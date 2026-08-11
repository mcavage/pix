// daemon.go — DaemonService: one supervised long-running HOST process that
// already speaks its own protocol on a loopback port.
//
// It is the sibling of GoPluginService, and it exists because that one is the
// wrong shape for this job. A go-plugin unit is dispensed: the supervisor
// handshakes with it over net/rpc and holds a typed client. A daemon like
// `snow-proxy` is reached by something else entirely — an in-sandbox wrapper
// speaking HTTP to a loopback port — so making it a go-plugin would mean
// replacing a working transport with net/rpc to gain a handshake nobody uses.
//
// What it shares with GoPluginService is everything that actually matters, and
// that sharing is the point: staged copy-then-verify against the SHA pin, the
// env allowlist, the restart budgets and Suture's backoff, the permanent-vs-
// transient split, and the rule that "the process is up" is NOT health. The only
// thing that differs is how a unit is started and how it is asked whether it is
// working.
package supervise

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/thejerf/suture/v4"
)

// DaemonSpec is the launch + health contract for one supervised daemon. It is
// derived from a pack's [[services]] entry by the composition root; this package
// never reads a manifest.
type DaemonSpec struct {
	Unit UnitSpec
	// Listen defaults to 127.0.0.1. Loopback only — a daemon that binds a
	// routable interface is a service on the network, which is a different
	// consent conversation than the one a pack's adoption screen had.
	Listen string
	Port   int
	// Health is "tcp" (a successful dial) or an absolute HTTP path. A daemon
	// with neither is refused at load: a process nothing can probe is a process
	// nothing can evict, and eviction is most of what this buys over launchd.
	Health string
}

func (d DaemonSpec) addr() string {
	host := d.Listen
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(d.Port))
}

// DaemonService supervises one daemon unit.
type DaemonService struct {
	spec DaemonSpec
	tree *Tree

	cmd *exec.Cmd

	// ready reports the FIRST start attempt's outcome back to Add, at most once,
	// so a misconfigured unit fails `serve` loudly at startup instead of looking
	// like it came up and then quietly flapping.
	ready     chan error
	readyDone bool
}

// String is what Suture uses to name this service in its events.
func (s *DaemonService) String() string { return "daemon." + s.spec.Unit.Name }

// Serve runs one generation: launch, wait for the declared health check to pass,
// then hold until the process dies or ctx is cancelled.
func (s *DaemonService) Serve(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b := s.tree.budgets
	name := s.spec.Unit.Name
	s.tree.transition(name, func(st *UnitStatus) { st.State = UnitStarting; st.HealthOK = false })

	cmd, err := s.command()
	if err != nil {
		// A stale pin, an invalid spec or a missing binary is OPERATOR state,
		// not a transient fault: this subtree stops and its siblings keep
		// serving. Restarting it in a loop would only spin.
		err = fmt.Errorf("%w: %w", err, suture.ErrDoNotRestart)
		s.tree.transition(name, func(st *UnitStatus) {
			st.State, st.PID, st.LastError = UnitFailed, 0, err.Error()
		})
		s.tree.emit(Event{Unit: name, Type: EventDoNotRestart, Message: "permanent failure", Err: err.Error()})
		s.signal(err)
		return err
	}
	if err := cmd.Start(); err != nil {
		s.tree.fail(name, err)
		s.signal(err)
		return err
	}
	s.cmd = cmd
	pid := cmd.Process.Pid

	// The child's exit is watched on its own goroutine so Serve can wait on
	// either that or ctx, and so Wait is always called exactly once (a daemon
	// that is never reaped is a zombie the next probe would count as alive).
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// A unit that cannot pass its FIRST health check is not started, it is
	// broken, and `serve` must say so at startup rather than reporting a running
	// pid for a process that never became usable.
	if err := s.waitHealthy(ctx, b.HealthTimeout); err != nil {
		s.stopChild()
		<-exited
		s.tree.fail(name, err)
		s.signal(err)
		return err
	}

	s.tree.transition(name, func(st *UnitStatus) {
		st.State, st.PID, st.HealthOK, st.LastError = UnitRunning, pid, true, ""
		if st.Generations++; st.Generations > 1 {
			st.Restarts++
		}
	})
	s.tree.emit(Event{Unit: name, Type: EventStarted, Message: fmt.Sprintf("pid %d", pid)})
	s.signal(nil)

	// Health is polled for as long as the daemon lives. A daemon that stops
	// answering is REPLACED, which is the whole difference between this and a
	// LaunchAgent's KeepAlive: launchd restarts a process that exits, and has no
	// opinion about one that is wedged.
	ticker := time.NewTicker(b.HealthInterval)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			s.stopChild()
			<-exited
			s.tree.transition(name, func(st *UnitStatus) { st.State, st.HealthOK = UnitStopped, false })
			return ctx.Err()
		case werr := <-exited:
			err := fmt.Errorf("daemon %s exited: %w", name, werr)
			s.tree.fail(name, err)
			return err
		case <-ticker.C:
			if perr := s.probe(b.HealthTimeout); perr != nil {
				if fails++; fails >= b.HealthFailures {
					s.stopChild()
					<-exited
					err := fmt.Errorf("daemon %s failed %d health probes: %w", name, fails, perr)
					s.tree.fail(name, err)
					return err
				}
				s.tree.transition(name, func(st *UnitStatus) { st.HealthOK = false })
				continue
			}
			fails = 0
			s.tree.transition(name, func(st *UnitStatus) { st.HealthOK = true })
		}
	}
}

// command builds the launch, staging and verifying a pinned executable exactly
// as a go-plugin unit's is. A PATH-resolved command skips staging because there
// is nothing to verify it against — see ServiceRuntimeDaemon for why that is
// permitted and how it is disclosed.
func (s *DaemonService) command() (*exec.Cmd, error) {
	u := s.spec.Unit
	path := ""
	switch {
	case u.Path != "":
		staged, err := StageExecutable(s.tree.stageDir, u.Name, u.Path, u.SHA)
		if err != nil {
			return nil, err
		}
		path = staged
	case u.Command != "":
		resolved, err := exec.LookPath(u.Command)
		if err != nil {
			return nil, fmt.Errorf("unit %s: %q is not on PATH (the pack's setup step installs it)", u.Name, u.Command)
		}
		path = resolved
	default:
		return nil, fmt.Errorf("unit %s: no executable (neither a pinned path nor a command)", u.Name)
	}
	cmd := exec.Command(path, u.Argv...)
	cmd.Env = FilterEnv(u.EnvAllow, u.EnvGrant)
	// Its own process group, so stopChild can signal the daemon AND anything it
	// spawned. snow-proxy execs the vendor CLI per request; leaving those behind
	// would keep the port bound and make the next start fail to bind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// waitHealthy polls until the declared check passes or the budget expires. A
// daemon needs a moment to bind its port, so a single immediate probe would
// reject every healthy start.
func (s *DaemonService) waitHealthy(ctx context.Context, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if last = s.probe(time.Second); last == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon %s did not become healthy within %s: %w", s.spec.Unit.Name, budget, last)
}

// probe answers the ONE question that counts: is this daemon serving? A dial for
// "tcp", an HTTP GET for a path. Bounded, and it never follows a redirect or
// reads a body — the status line is the whole answer, and a daemon that returns
// a megabyte on /healthz should not be able to stall its own supervisor.
func (s *DaemonService) probe(budget time.Duration) error {
	addr := s.spec.addr()
	if s.spec.Health == "tcp" {
		conn, err := net.DialTimeout("tcp", addr, budget)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	client := &http.Client{
		Timeout:       budget,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get("http://" + addr + s.spec.Health)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("health %s returned %s", s.spec.Health, resp.Status)
	}
	return nil
}

// stopChild asks the process group to exit, then kills it inside the stop
// budget. A daemon that ignores SIGTERM must not be able to hold up a shutdown
// or keep its port bound against the replacement.
func (s *DaemonService) stopChild() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	pid := s.cmd.Process.Pid
	// Negative pid signals the whole group (see Setpgid in command()).
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(s.tree.budgets.Stop)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// signal reports the first start attempt's outcome to Add, exactly once.
func (s *DaemonService) signal(err error) {
	if s.ready == nil || s.readyDone {
		return
	}
	s.readyDone = true
	select {
	case s.ready <- err:
	default:
	}
}

var _ suture.Service = (*DaemonService)(nil)
