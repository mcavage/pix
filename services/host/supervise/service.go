// service.go — GoPluginService: one supervised go-plugin subprocess expressed
// as a suture.Service. Serve() owns the whole life of one generation of the
// child: reattach-or-spawn, dispense, health-probe until it dies, then drain
// and stop within the pinned budgets. Restart policy is Suture's; everything
// here is about the process and its identity.

package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/thejerf/suture/v4"
)

// HealthFunc probes a dispensed implementation. It is the unit's OWN notion of
// health (memory's Health(), the broker's Check()) — "the process is up" is not
// health, and a unit that fails HealthFailures probes in a row is replaced.
type HealthFunc func(impl any) error

// GoPluginService supervises one go-plugin unit.
type GoPluginService struct {
	spec   UnitSpec
	health HealthFunc
	holder *Holder
	tree   *Tree

	// ready reports the outcome of the FIRST start attempt back to Add, so a
	// misconfigured unit fails `serve` loudly at startup instead of silently
	// flapping. Sent at most once (readyDone).
	ready     chan error
	readyDone bool
}

// String is what Suture uses to name this service in its events.
func (s *GoPluginService) String() string { return "unit." + s.spec.Name }

// Serve runs one generation of the unit and returns when it dies (Suture then
// applies backoff and restarts) or when ctx is cancelled (a clean stop).
func (s *GoPluginService) Serve(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b := s.tree.budgets
	s.tree.transition(s.spec.Name, func(st *UnitStatus) { st.State = UnitStarting; st.HealthOK = false })

	client, impl, reattached, err := s.start()
	if err != nil {
		// A pin that no longer matches, an invalid spec or a missing binary is
		// OPERATOR state, not a transient fault: restarting cannot fix it, so the
		// unit is removed from the tree (its subtree stops, the root and every
		// sibling keep serving).
		if permanent(err) {
			err = fmt.Errorf("%w: %w", err, suture.ErrDoNotRestart)
			s.tree.transition(s.spec.Name, func(st *UnitStatus) {
				st.State, st.PID, st.LastError = UnitFailed, 0, err.Error()
			})
			s.tree.emit(Event{Unit: s.spec.Name, Type: EventDoNotRestart, Message: "permanent failure", Err: err.Error()})
		} else {
			s.tree.fail(s.spec.Name, err)
		}
		s.signal(err)
		return err
	}

	pid := clientPID(client)
	s.holder.Set(impl, client)
	s.tree.transition(s.spec.Name, func(st *UnitStatus) {
		st.State, st.PID, st.Reattached, st.LastError = UnitRunning, pid, reattached, ""
		if st.Generations++; st.Generations > 1 {
			st.Restarts++
		}
	})
	evt := EventStarted
	if reattached {
		evt = EventReattached
	}
	s.tree.emit(Event{Unit: s.spec.Name, Type: evt, Message: fmt.Sprintf("pid %d", pid)})

	// First probe inside the handshake budget: a unit that cannot answer is not
	// started, it is broken, and `serve` must say so at startup.
	if err := s.probe(b.HealthTimeout); err != nil {
		err = fmt.Errorf("unit %s: first health probe failed: %w", s.spec.Name, err)
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventHealthFailed, Err: err.Error()})
		s.stopChild(client, false)
		s.tree.fail(s.spec.Name, err)
		s.signal(err)
		return err
	}
	s.tree.transition(s.spec.Name, func(st *UnitStatus) { st.HealthOK = true })
	s.signal(nil)

	ticker := time.NewTicker(b.HealthInterval)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			// Clean stop: drain in-flight work, then kill inside the stop budget.
			s.stopChild(client, true)
			s.tree.transition(s.spec.Name, func(st *UnitStatus) {
				st.State, st.PID, st.HealthOK = UnitStopped, 0, false
			})
			s.tree.emit(Event{Unit: s.spec.Name, Type: EventStopped})
			return nil
		case <-ticker.C:
			if client.Exited() {
				err := fmt.Errorf("unit %s: plugin process (pid %d) exited", s.spec.Name, pid)
				s.holder.Clear()
				s.clearReattach()
				s.tree.emit(Event{Unit: s.spec.Name, Type: EventExited, Err: err.Error()})
				s.tree.fail(s.spec.Name, err)
				return err
			}
			if err := s.probe(b.HealthTimeout); err != nil {
				fails++
				s.tree.transition(s.spec.Name, func(st *UnitStatus) {
					st.State, st.HealthOK, st.LastError = UnitDegraded, false, err.Error()
				})
				s.tree.emit(Event{Unit: s.spec.Name, Type: EventHealthFailed, Err: err.Error(),
					Message: fmt.Sprintf("%d/%d", fails, b.HealthFailures)})
				if fails >= b.HealthFailures {
					err = fmt.Errorf("unit %s: %d consecutive failed health probes: %w", s.spec.Name, fails, err)
					s.stopChild(client, false)
					s.tree.fail(s.spec.Name, err)
					return err
				}
				continue
			}
			fails = 0
			s.tree.transition(s.spec.Name, func(st *UnitStatus) { st.State, st.HealthOK = UnitRunning, true })
		}
	}
}

// probe runs the unit's health check under a hard timeout. The probe goroutine
// is abandoned on timeout (an rpc call cannot be cancelled mid-flight); the
// buffered channel means it never blocks or touches shared state afterwards.
func (s *GoPluginService) probe(budget time.Duration) error {
	if s.health == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- s.holder.Use(s.health) }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("health probe exceeded %v", budget)
	}
}

// signal reports the first start outcome to Add exactly once.
func (s *GoPluginService) signal(err error) {
	if s.readyDone {
		return
	}
	s.readyDone = true
	select {
	case s.ready <- err:
	default:
	}
}

// permanentErr marks a failure that a restart cannot fix.
type permanentErr struct{ err error }

func (e permanentErr) Error() string { return e.err.Error() }
func (e permanentErr) Unwrap() error { return e.err }

func permanent(err error) bool {
	var p permanentErr
	return errors.As(err, &p)
}

// start reattaches to a surviving child if the persisted state still names THIS
// unit and that process is alive; otherwise it spawns a fresh one.
func (s *GoPluginService) start() (*goplugin.Client, any, bool, error) {
	if client, impl, ok := s.tryReattach(); ok {
		return client, impl, true, nil
	}
	cmd, err := s.command()
	if err != nil {
		return nil, nil, false, err
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: s.tree.handshake,
		Plugins:         s.tree.plugins,
		Cmd:             cmd,
		StartTimeout:    s.tree.budgets.Handshake,
		SkipHostEnv:     true, // env comes from FilterEnv, never the raw parent
	})
	impl, err := dispense(client, s.spec.Kind)
	if err != nil {
		client.Kill()
		return nil, nil, false, err
	}
	s.saveReattach(client)
	return client, impl, false, nil
}

// command builds the child process: a staged, freshly-verified copy for an
// external unit (re-verified on EVERY start, so a binary swapped under a
// running unit is caught at its next restart), or a self-exec of this binary.
func (s *GoPluginService) command() (*exec.Cmd, error) {
	if err := s.spec.Validate(); err != nil {
		return nil, permanentErr{err}
	}
	path := s.tree.SelfPath()
	argv := append([]string{"plugin", s.spec.Kind}, s.spec.Argv...)
	if !s.spec.SelfExec {
		staged, err := StageExecutable(s.tree.stageDir, s.spec.Name, s.spec.Path, s.spec.SHA)
		if err != nil {
			return nil, permanentErr{err}
		}
		path, argv = staged, s.spec.Argv
	}
	if path == "" {
		return nil, permanentErr{fmt.Errorf("unit %s: no executable (self-exec unit with an unknown self path)", s.spec.Name)}
	}
	cmd := exec.Command(path, argv...)
	cmd.Env = FilterEnv(s.spec.EnvAllow, s.spec.EnvGrant)
	return cmd, nil
}

// stopChild drains (optionally) and kills the child inside the stop budget.
func (s *GoPluginService) stopChild(client *goplugin.Client, drain bool) {
	s.holder.Clear()
	if drain && !s.holder.drain(s.tree.budgets.Drain) {
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventDrainTimeout,
			Message: fmt.Sprintf("in-flight calls outlived the %v drain budget", s.tree.budgets.Drain)})
	}
	done := make(chan struct{})
	go func() { client.Kill(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.tree.budgets.Stop):
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventStopTimeout,
			Message: fmt.Sprintf("child did not stop within %v", s.tree.budgets.Stop)})
	}
	s.clearReattach()
}

func dispense(client *goplugin.Client, kind string) (any, error) {
	rpc, err := client.Client()
	if err != nil {
		return nil, err
	}
	return rpc.Dispense(kind)
}

func clientPID(c *goplugin.Client) int {
	if rc := c.ReattachConfig(); rc != nil {
		return rc.Pid
	}
	return 0
}

// --- reattach state ---------------------------------------------------------

// reattachState is what survives a HARD supervisor death (SIGKILL: no
// shutdown, no cleanup, orphaned children). It records both how to reconnect
// and WHO the child is supposed to be, because a pid is not an identity — pids
// are recycled, and reattaching to whatever now holds one is how a supervisor
// adopts a stranger.
type reattachState struct {
	Unit            string    `json:"unit"`
	Kind            string    `json:"kind"`
	Identity        string    `json:"identity"`
	Pid             int       `json:"pid"`
	Network         string    `json:"network"`
	Address         string    `json:"address"`
	Protocol        string    `json:"protocol"`
	ProtocolVersion int       `json:"protocolVersion"`
	SavedAt         time.Time `json:"savedAt"`
}

func reattachPath(stateDir, unit string) string {
	return filepath.Join(stateDir, "units", unit+".reattach.json")
}

// SaveReattach persists a unit's reattach state (0600, dir 0700). Exported so a
// test — and any future out-of-tree supervisor front-end — can produce exactly
// what the supervisor consumes. protocolVersion is the handshake version that
// was negotiated with the child: go-plugin's own ReattachConfig() leaves that
// field zero, and a reattach across a protocol bump must be refused, so the
// caller supplies it.
func SaveReattach(stateDir string, spec UnitSpec, rc *goplugin.ReattachConfig, protocolVersion int) error {
	if rc == nil || stateDir == "" {
		return nil
	}
	if rc.ProtocolVersion != 0 {
		protocolVersion = rc.ProtocolVersion
	}
	st := reattachState{
		Unit: spec.Name, Kind: spec.Kind, Identity: spec.identity(), Pid: rc.Pid,
		Protocol: string(rc.Protocol), ProtocolVersion: protocolVersion, SavedAt: time.Now(),
	}
	if rc.Addr != nil {
		st.Network, st.Address = rc.Addr.Network(), rc.Addr.String()
	}
	path := reattachPath(stateDir, spec.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func (s *GoPluginService) saveReattach(client *goplugin.Client) {
	if err := SaveReattach(s.tree.stateDir, s.spec, client.ReattachConfig(), int(s.tree.handshake.ProtocolVersion)); err != nil {
		s.tree.logf("supervise: could not persist reattach state for %s: %v", s.spec.Name, err)
	}
}

func (s *GoPluginService) clearReattach() {
	if s.tree.stateDir == "" {
		return
	}
	if err := os.Remove(reattachPath(s.tree.stateDir, s.spec.Name)); err != nil && !os.IsNotExist(err) {
		s.tree.logf("supervise: could not clear reattach state for %s: %v", s.spec.Name, err)
	}
}

// tryReattach adopts a surviving child ONLY when the persisted state names this
// unit, this kind, this exact executable identity, and that pid is still alive
// and still speaks our protocol. Anything else: drop the state, spawn fresh.
func (s *GoPluginService) tryReattach() (*goplugin.Client, any, bool) {
	if s.tree.stateDir == "" {
		return nil, nil, false
	}
	raw, err := os.ReadFile(reattachPath(s.tree.stateDir, s.spec.Name))
	if err != nil {
		return nil, nil, false
	}
	var st reattachState
	if err := json.Unmarshal(raw, &st); err != nil {
		s.clearReattach()
		return nil, nil, false
	}
	reason := ""
	switch {
	case st.Unit != s.spec.Name || st.Kind != s.spec.Kind:
		reason = "state names a different unit"
	case st.Identity != s.spec.identity():
		reason = "executable identity changed"
	case st.ProtocolVersion != int(s.tree.handshake.ProtocolVersion):
		reason = "plugin protocol version changed"
	case st.Pid <= 0 || !processAlive(st.Pid):
		reason = "recorded process is gone"
	case st.Address == "":
		reason = "no recorded address"
	}
	if reason != "" {
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventReattachRejected, Message: reason})
		s.clearReattach()
		return nil, nil, false
	}
	addr, err := resolveAddr(st.Network, st.Address)
	if err != nil {
		s.clearReattach()
		return nil, nil, false
	}
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: s.tree.handshake,
		Plugins:         s.tree.plugins,
		Reattach: &goplugin.ReattachConfig{
			Protocol: goplugin.Protocol(st.Protocol), ProtocolVersion: st.ProtocolVersion,
			Addr: addr, Pid: st.Pid,
		},
		StartTimeout: s.tree.budgets.Handshake,
	})
	impl, err := dispense(client, s.spec.Kind)
	if err != nil {
		client.Kill()
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventReattachRejected, Message: "reattach failed: " + err.Error()})
		s.clearReattach()
		return nil, nil, false
	}
	return client, impl, true
}

func resolveAddr(network, address string) (net.Addr, error) {
	switch network {
	case "unix", "unixgram", "unixpacket":
		return &net.UnixAddr{Name: address, Net: network}, nil
	case "tcp", "tcp4", "tcp6":
		return net.ResolveTCPAddr(network, address)
	default:
		return nil, fmt.Errorf("unsupported plugin address network %q", network)
	}
}

// processAlive reports whether a pid still exists (signal 0 probe).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
