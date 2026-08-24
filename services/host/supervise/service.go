// service.go — GoPluginService: one supervised go-plugin subprocess as a suture.Service. Serve() owns ONE generation of the child: reattach-or-spawn, dispense, health-probe until it dies, then drain and stop inside the pinned budgets (restart policy is Suture's; this is the process and its identity).

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
	"strconv"
	"strings"
	"syscall"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/thejerf/suture/v4"
)

// HealthFunc is the unit's OWN notion of health (memory's Health(), the broker's Check()); "the process is up" is not health, and a unit failing HealthFailures probes in a row is replaced.
type HealthFunc func(impl any) error

// GoPluginService supervises one go-plugin unit.
type GoPluginService struct {
	spec   UnitSpec
	health HealthFunc
	holder *Holder
	tree   *Tree

	// ready+readyDone report the FIRST start attempt's outcome back to Add, at most once, so a misconfigured unit fails `serve` loudly at startup.
	ready     chan error
	readyDone bool
}

// String is what Suture uses to name this service in its events.
func (s *GoPluginService) String() string { return "unit." + s.spec.Name }

// Serve runs one generation, returning when it dies (Suture backs off and restarts) or ctx is cancelled (a clean stop).
func (s *GoPluginService) Serve(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	b := s.tree.budgets
	s.tree.transition(s.spec.Name, func(st *UnitStatus) { st.State = UnitStarting; st.HealthOK = false })

	client, impl, reattached, err := s.start()
	if err != nil {
		// A stale pin, an invalid spec or a missing binary is OPERATOR state, not a transient fault: this subtree stops, siblings keep serving.
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

	// A unit that cannot answer its first probe is not started, it is broken, and `serve` must say so at startup.
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
			// Clean stop: drain in-flight work, then kill inside the budget.
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

// probe runs the unit's health check under a hard timeout; on timeout the goroutine is abandoned (an rpc call cannot be cancelled mid-flight), and its buffered channel means it never blocks or touches shared state afterwards.
func (s *GoPluginService) probe(budget time.Duration) error {
	if s.health == nil {
		return nil
	}
	// Time EVERY probe, failures included: the latency of the probe that timed out is the number that explains the eviction that follows it.
	start := time.Now()
	defer func() {
		us := time.Since(start).Microseconds()
		s.tree.transition(s.spec.Name, func(st *UnitStatus) { st.LastProbeUS = us })
	}()
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
func permanent(err error) bool       { var p permanentErr; return errors.As(err, &p) }

// start reattaches to a surviving child naming THIS unit, alive; otherwise it spawns a fresh one.
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

// command builds the child process: a staged, freshly-verified copy for an external unit (re-verified on EVERY start, catching a swap under a running unit at its next restart), or a self-exec of this one.
func (s *GoPluginService) command() (*exec.Cmd, error) {
	if err := s.spec.Validate(); err != nil {
		return nil, permanentErr{err}
	}
	path := s.tree.selfPath
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
	if drain && !s.holder.Drain(s.tree.budgets.Drain) {
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventDrainTimeout,
			Message: fmt.Sprintf("in-flight calls outlived the %v drain budget", s.tree.budgets.Drain)})
	}
	done := make(chan struct{})
	go func() { client.Kill(); close(done) }()
	select {
	case <-done:
		s.clearReattach()
	case <-time.After(s.tree.budgets.Stop):
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventStopTimeout,
			Message: fmt.Sprintf("child did not stop within %v", s.tree.budgets.Stop)})
		// The kill never came back, so the child may still be RUNNING — and this
		// record is the only thing the next supervisor can find it by, to
		// reattach or to reap. Revoking it here is exactly how a child that
		// outlives its stop budget becomes an unreachable orphan on the store
		// flock. A record kept for a dead pid costs nothing (the next
		// tryReattach reads "process is gone" and drops it); one dropped for a
		// live pid costs the daemon.
		if client.Exited() {
			s.clearReattach()
		}
	}
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

// reattachState survives a HARD supervisor death (SIGKILL: no shutdown, no cleanup, orphaned children). It records how to reconnect AND who the child is supposed to be — a pid is not an identity, and reattaching to whatever now holds one is how a supervisor adopts a stranger.
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

// SaveReattach persists a unit's reattach state (0600, dir 0700). Exported so a test produces exactly what the supervisor consumes. protocolVersion is the version negotiated with the child (go-plugin's ReattachConfig() leaves it zero); a reattach across a protocol bump must be refused.
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

// tryReattach adopts a surviving child ONLY when the persisted state names this unit, kind and exact admission fingerprint (identity: executable, pin, argv, env surface), the pid is alive AND OURS, the endpoint is a unix socket WE own, and the child still speaks our protocol (dispense over the socket is the handshake); anything else drops the state and spawns fresh.
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
	// Liveness and address come FIRST so the two supersede cases below inherit
	// a pid already proven alive and ours — the precondition their kill rests
	// on. (A dead pid also reads better reported as dead than as a rename.)
	reason, supersede := "", false
	switch {
	case st.Unit != s.spec.Name || st.Kind != s.spec.Kind:
		reason = "state names a different unit"
	case st.Pid <= 0 || !processAlive(st.Pid):
		reason = "recorded process is gone or not ours"
	case st.Address == "":
		reason = "no recorded address"
	// SUPERSEDED, not foreign: the state named this unit and kind above, so the
	// live pid it records is our OWN earlier generation, launched from a
	// surface we have since changed (the ordinary upgrade: pix-host rebuilt
	// under a hard-killed supervisor's surviving child). Refusing to adopt it
	// is right; dropping its record and walking away is the bug — that leaves
	// it running, unsupervised and now unfindable, holding what the unit owns
	// exclusively. For memory that is the store flock, so every later spawn
	// dies on it and `serve` never starts again until someone finds the pid.
	case st.Identity != s.spec.identity():
		reason, supersede = "executable identity changed", true
	case st.ProtocolVersion != int(s.tree.handshake.ProtocolVersion):
		reason, supersede = "plugin protocol version changed", true
	}
	if reason == "" {
		reason = verifyReattachTarget(st.Network, st.Address)
	}
	if supersede {
		reason += s.reapSupersededOrphan(st, reason)
	}
	if reason == "" {
		// Snapshot the pid's kernel-reported start time NOW, at the moment every
		// ownership check above has just passed, so a revalidation right before
		// any kill decision below has something trustworthy to compare against
		// (see revalidateOrphan): the client construction + dispense RPC that
		// follows can take up to the handshake budget, real time in which the
		// pid could exit and be reused by an unrelated process.
		startBefore, startKnown := processStartTime(st.Pid)
		client := goplugin.NewClient(&goplugin.ClientConfig{
			HandshakeConfig: s.tree.handshake,
			Plugins:         s.tree.plugins,
			Reattach: &goplugin.ReattachConfig{
				Protocol: goplugin.Protocol(st.Protocol), ProtocolVersion: st.ProtocolVersion,
				Addr: &net.UnixAddr{Name: st.Address, Net: "unix"}, Pid: st.Pid,
			},
			StartTimeout: s.tree.budgets.Handshake,
		})
		impl, derr := dispense(client, s.spec.Kind)
		if derr == nil {
			return client, impl, true
		}
		client.Kill()
		reason = "reattach failed: " + derr.Error()
		// client.Kill() above reaches the OS process only when go-plugin's own
		// reattach dial succeeded (it sets a runner only then). A unit that
		// stopped answering its socket entirely — the failure that would
		// otherwise leave memory's store flock held forever — dials to
		// ErrProcessNotFound and leaves that Kill a no-op, so the reaper is
		// what actually ends it. startBefore is what lets it tell the pid it
		// verified from a successor that reused the number meanwhile.
		reason += s.reapVerifiedOrphan(st, startBefore, startKnown, reason)
	}
	s.tree.emit(Event{Unit: s.spec.Name, Type: EventReattachRejected, Message: reason})
	s.clearReattach()
	return nil, nil, false
}

// reapVerifiedOrphan is the ONE path from "we refused to reattach to a live
// child we recorded" to signaling it, and killVerifiedOrphan's only caller. It
// re-proves through revalidateOrphan, immediately before the kill, that the pid
// and socket are STILL the target the caller verified (a stale or partial
// answer refuses, rather than risk a reused pid), and emits a typed event —
// EventOrphanKilled only for a CONFIRMED death. why is the rejection that led
// here, carried into the event. Returns "" when the orphan was signaled, else a
// note for the caller's reason: a refusal leaves a process an operator has to
// find by hand, so it must never be silent.
func (s *GoPluginService) reapVerifiedOrphan(st reattachState, startBefore uint64, startKnown bool, why string) string {
	ok, revReason := revalidateOrphan(st.Pid, st.Network, st.Address, startBefore, startKnown)
	if !ok {
		return " (verified orphan NOT killed: " + revReason + ")"
	}
	switch killVerifiedOrphan(st.Pid) {
	case orphanKillConfirmedDead:
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventOrphanKilled, Message: fmt.Sprintf("pid %d: %s", st.Pid, why)})
	case orphanKillSignalFailed:
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventOrphanKillFailed, Message: fmt.Sprintf("pid %d: kill signal not delivered: %s", st.Pid, why)})
	case orphanKillNotConfirmed:
		s.tree.emit(Event{Unit: s.spec.Name, Type: EventOrphanKillFailed, Message: fmt.Sprintf("pid %d: still alive %v after SIGKILL: %s", st.Pid, orphanKillWait, why)})
	}
	return ""
}

// reapSupersededOrphan reaps the child of a generation we refused to adopt
// because WE changed, not because anything about the child went wrong. No RPC
// was attempted, so unlike the dispense-failure path there is no reuse window
// to close; the snapshot and revalidation happen anyway, so every kill in this
// package goes through exactly one set of proofs.
func (s *GoPluginService) reapSupersededOrphan(st reattachState, why string) string {
	startBefore, startKnown := processStartTime(st.Pid)
	return s.reapVerifiedOrphan(st, startBefore, startKnown, why)
}

// orphanKillWait bounds how long killVerifiedOrphan waits for a verified
// orphan to actually exit after SIGKILL. It bounds the WAIT, not the kill
// (the signal is sent unconditionally, once, before this loop): a slow-to-
// reap orphan just means the fresh spawn that follows may still lose one lock
// race, not that tryReattach hangs indefinitely.
const orphanKillWait = 2 * time.Second

// orphanKillResult distinguishes the three outcomes a caller of
// killVerifiedOrphan must NOT conflate: the process actually confirmed dead
// (the only case that earns EventOrphanKilled), the kill signal itself
// failing to deliver, and the signal being delivered but the process still
// being alive after orphanKillWait (also not a confirmed kill).
type orphanKillResult int

const (
	orphanKillConfirmedDead orphanKillResult = iota // process is actually gone
	orphanKillSignalFailed                          // p.Kill() itself returned an error
	orphanKillNotConfirmed                          // signaled, but still alive after the wait
)

// killVerifiedOrphan terminates pid, which the caller has ALREADY established
// (via revalidateOrphan, immediately beforehand) is our own previously-
// launched unit process, and waits briefly for it to actually exit so the
// fresh spawn that follows has a real chance at whatever exclusive resource
// (a flock) the orphan was holding. It reports which of the three outcomes
// above occurred; the caller emits EventOrphanKilled ONLY for
// orphanKillConfirmedDead.
func killVerifiedOrphan(pid int) orphanKillResult {
	// Guard the PRIMITIVE, not just its callers: on unix pid 0 signals our whole
	// process group and a negative pid another group (-1: everything this uid
	// can reach), so one arriving here turns "reap an orphan" into "kill the
	// supervisor and all it started". No caller passes one; it stops here anyway.
	if pid <= 0 {
		return orphanKillSignalFailed
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return orphanKillConfirmedDead // nothing to signal: already gone
	}
	if err := p.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return orphanKillConfirmedDead
		}
		return orphanKillSignalFailed
	}
	deadline := time.Now().Add(orphanKillWait)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		return orphanKillNotConfirmed
	}
	return orphanKillConfirmedDead
}

// revalidateOrphan re-confirms, immediately before a kill decision, that pid
// and its recorded socket are STILL the exact target already verified
// earlier in tryReattach — not a different process or endpoint that appeared
// in the window opened by constructing the reattach client and running the
// failed dispense RPC (bounded by the handshake budget, which can be real
// seconds). It binds identity to the pid's kernel-reported START TIME where
// Linux exposes one (/proc/pid/stat): a pid-reusing successor process can
// share processAlive's uid check but can never share the original process's
// start time down to the clock tick. Where no start-time source exists
// (non-Linux, or /proc unreadable), this refuses to kill conservatively:
// alive-and-ours is necessary but not sufficient to prove it is the SAME
// process, so it is not sufficient to kill. Any stale or partial result here
// — not just an outright mismatch — means "do not kill".
func revalidateOrphan(pid int, network, address string, startBefore uint64, startKnown bool) (ok bool, reason string) {
	if pid <= 0 {
		return false, "no recorded pid to identify a process by"
	}
	if !processAlive(pid) {
		return false, "process is gone or no longer ours as of the kill decision"
	}
	if r := verifyReattachTarget(network, address); r != "" {
		return false, "socket no longer verifies: " + r
	}
	if !startKnown {
		return false, "no process start-time source available to rule out pid reuse"
	}
	startNow, ok2 := processStartTime(pid)
	if !ok2 {
		return false, "process start time unavailable as of the kill decision"
	}
	if startNow != startBefore {
		return false, "process start time changed: the pid was reused"
	}
	return true, ""
}

// processStartTime reports pid's kernel-recorded START TIME as a stable
// identity value: a fact a pid-reusing successor process can never share
// with the process it replaced, unlike the pid number itself. Its source is
// platform-specific (see processtime_linux.go, processtime_darwin.go,
// processtime_other.go); ok is false wherever this platform exposes no such
// source, or the OS refuses to answer for pid, and the caller must then
// refuse to bind identity by start time rather than treat that as proof of
// anything.

// verifyReattachTarget admits UNIX SOCKETS ONLY (a tcp target is whatever process got the port back after a reboot; a unix socket path carries an owner we can check), and only one owned by OUR uid: a regular file wearing the recorded path, or a socket created by another user, is a stranger's endpoint, not our child's. Returns a rejection reason, or "" to proceed.
func verifyReattachTarget(network, address string) string {
	if network != "unix" {
		return fmt.Sprintf("refusing reattach over %q (unix sockets only)", network)
	}
	fi, err := os.Lstat(address)
	if err != nil {
		return "reattach socket: " + err.Error()
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Sprintf("reattach address %s is not a unix socket", address)
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Sprintf("reattach socket %s is not owned by uid %d", address, os.Getuid())
	}
	return ""
}

// processAlive: the recorded pid must exist AND be ours. A signal-0 EPERM proves only that SOME process wears the pid — after pid reuse that is anybody — so EPERM is a refusal, not proof; where /proc exposes the owner, the real uid must equal ours.
func processAlive(pid int) bool {
	// 0 and negatives address process GROUPS: signal 0 to one we belong to
	// succeeds, reporting "alive and ours" for a pid naming no process at all.
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil || p.Signal(syscall.Signal(0)) != nil {
		return false
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		// No /proc (darwin): a NON-root signal-0 success already proved real/saved-uid equality; root can signal anyone, so with nothing to check it refuses to vouch.
		return os.Getuid() != 0
	}
	_, after, ok := strings.Cut(string(raw), "\nUid:\t")
	f := strings.Fields(after)
	return ok && len(f) > 0 && f[0] == strconv.Itoa(os.Getuid())
}
