//go:build unix

// worker.go — starting, adopting, and stopping a session's `pix-host
// uat-worker`: the process `pix run --dev` launches from the operator's own
// interactive shell so it inherits the authenticated sbx/Docker/browser
// context a gateway-spawned process never sees
// (docs/design/self-development-uat.md). EnsureWorker is the dial-first
// adopt-or-start decision two racing attachers must agree on without either
// one unlinking or replacing a live worker; StopWorker is the one place that
// ever signals a recorded worker pid, and it never does so without
// positively verifying the pid is still that session's uat-worker first —
// the same standard service/ctl.go's verifyServeProc applies to `pix-host
// serve`.
package uat

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// WorkerReadyAttempts/WorkerReadyDelay bound how long EnsureWorker waits for
// a freshly spawned uat-worker's socket to accept a connection before
// declaring the launch failed closed — the same order of magnitude as
// uat-mcp's own gateway-side retry (uatConnectAttempts/uatConnectDelay), just
// owned here since this is the side that actually starts the process.
const (
	WorkerReadyAttempts = 25
	WorkerReadyDelay    = 200 * time.Millisecond
	workerTermTimeout   = 5 * time.Second
	workerKillTimeout   = 3 * time.Second
	workerPollInterval  = 50 * time.Millisecond
)

// WorkerPIDFileName records the session's live uat-worker inside its runner
// state directory: pid, plus the session id and socket it must still match
// before StopWorker ever signals it.
const WorkerPIDFileName = "worker.pid.json"

// WorkerRecord is what a launcher that just started a worker writes, and what
// StopWorker (possibly a LATER, unrelated pix-host invocation — an orphan
// sweep, or the shell that happens to win the last-reference proof) reads
// back to decide whether it may signal that pid at all.
type WorkerRecord struct {
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	Socket    string `json:"socket"`
}

func workerRecordPath(runnerState string) string {
	return filepath.Join(runnerState, WorkerPIDFileName)
}

func writeWorkerRecord(runnerState string, rec WorkerRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(workerRecordPath(runnerState), data, 0o600)
}

// ReadWorkerRecord returns the recorded uat-worker for a session's runner
// state directory, or nil if there is none. A symlinked record is refused,
// matching every other UAT session-state file's hardening.
func ReadWorkerRecord(runnerState string) (*WorkerRecord, error) {
	path := workerRecordPath(runnerState)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("uat-worker record %s must not be a symlink", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec WorkerRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("uat-worker record %s: %w", path, err)
	}
	return &rec, nil
}

func removeWorkerRecord(runnerState string) error {
	err := os.Remove(workerRecordPath(runnerState))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// EnsureWorkerDeps are EnsureWorker's injectable OS seams. Real callers use
// DefaultEnsureWorkerDeps(); tests substitute fakes so the dial-first-adopt
// decision, the exact spawned argv, inherited env, and a bounded readiness
// failure are all provable without forking a real pix-host.
type EnsureWorkerDeps struct {
	Dial          func(path string, attempts int, delay time.Duration) (net.Conn, error)
	Spawn         func(argv []string) (*exec.Cmd, error)
	ReadyAttempts int
	ReadyDelay    time.Duration
}

// DefaultEnsureWorkerDeps wires the real dial/spawn seams at their production
// bounds.
func DefaultEnsureWorkerDeps() EnsureWorkerDeps {
	return EnsureWorkerDeps{
		Dial:          DialSocket,
		Spawn:         defaultWorkerSpawn,
		ReadyAttempts: WorkerReadyAttempts,
		ReadyDelay:    WorkerReadyDelay,
	}
}

// defaultWorkerSpawn is the real process start used when a caller does not
// need the child's own stdout/stderr surfaced anywhere (its diagnostics are
// simply discarded). cmd.Env is deliberately left nil: the child inherits
// this launcher process's exact environment, unchanged. That inheritance is
// the entire reason uat-worker is started here — from the operator's own
// authenticated interactive shell — instead of by the sbx gateway
// (docs/design/self-development-uat.md). Setpgid gives it its own process
// group so StopWorker/escalateSignal can terminate it and anything it goes
// on to spawn (docker, sbx, a browser) as one unit.
func defaultWorkerSpawn(argv []string) (*exec.Cmd, error) {
	return newWorkerSpawnCmd(argv, io.Discard, io.Discard)
}

// WorkerSpawn builds a Spawn func that wires the child's stdout/stderr to the
// given writers, so its diagnostics are not just discarded. This package is
// below the command layer and must never name os.Stdout/os.Stderr itself
// (processboundary_test.go); the caller — cmd/pix, which owns the process
// boundary — passes its own os.Stderr in.
func WorkerSpawn(stdout, stderr io.Writer) func(argv []string) (*exec.Cmd, error) {
	return func(argv []string) (*exec.Cmd, error) { return newWorkerSpawnCmd(argv, stdout, stderr) }
}

func newWorkerSpawnCmd(argv []string, stdout, stderr io.Writer) (*exec.Cmd, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// WorkerArgv is the exact argv `pix run --dev` starts a session's uat-worker
// with, exported so both the real spawn and a test can assert precisely this
// — never an approximation of it.
func WorkerArgv(hostBinary, repoRoot, runnerState, sessionID string) []string {
	return []string{hostBinary, "uat-worker", "--repo", repoRoot, "--state", runnerState, "--session", sessionID}
}

// EnsureWorker makes sure a live uat-worker is listening on the session's
// socket before its caller lets anything — a gateway relay, a session
// attach — depend on it. It dials FIRST: a live worker is adopted, never
// unlinked or replaced, so two attachers racing here (a create and a
// concurrent attach, or two attaches) can never step on one another; only an
// absent or stale one is started fresh. started reports whether THIS call
// spawned the process — only a caller that started it owns rolling it back
// on a later launch error, an adopted worker is never this call's to stop.
// A worker that never becomes ready is killed and reported as an error: this
// never returns success while the relay is dead.
func EnsureWorker(deps EnsureWorkerDeps, hostBinary, repoRoot, runnerState, sessionID string) (started bool, err error) {
	if !filepath.IsAbs(runnerState) {
		return false, fmt.Errorf("uat-worker: runner state must be an absolute path")
	}
	if err := ValidateID(sessionID); err != nil {
		return false, fmt.Errorf("uat-worker: invalid session: %w", err)
	}
	socket := SessionSocketPath(runnerState)
	if c, derr := deps.Dial(socket, 1, 0); derr == nil {
		_ = c.Close()
		return false, nil // a live worker answered: adopt it, start nothing
	}

	// Only reached when there is no live worker to adopt, so a caller that
	// merely needs to ADOPT one (a plain attach to an already-running session)
	// never has to resolve a repo root or host binary path it will not use.
	if !filepath.IsAbs(hostBinary) || !filepath.IsAbs(repoRoot) {
		return false, fmt.Errorf("uat-worker: host binary and repo root must be absolute paths to start a replacement worker")
	}
	argv := WorkerArgv(hostBinary, repoRoot, runnerState, sessionID)
	cmd, serr := deps.Spawn(argv)
	if serr != nil {
		return false, fmt.Errorf("start uat-worker: %w", serr)
	}
	pid := cmd.Process.Pid
	waitDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waitDone) }()

	attempts, delay := deps.ReadyAttempts, deps.ReadyDelay
	if attempts <= 0 {
		attempts = WorkerReadyAttempts
	}
	if delay <= 0 {
		delay = WorkerReadyDelay
	}
	c, derr := deps.Dial(socket, attempts, delay)
	if derr != nil {
		_ = escalateSignal(pid, waitDone)
		return false, fmt.Errorf("uat-worker did not become ready on %s: %w", socket, derr)
	}
	_ = c.Close()

	if werr := writeWorkerRecord(runnerState, WorkerRecord{PID: pid, SessionID: sessionID, Socket: socket}); werr != nil {
		_ = escalateSignal(pid, waitDone)
		return false, fmt.Errorf("record uat-worker: %w", werr)
	}
	return true, nil
}

// escalateSignal is the bounded TERM-then-KILL a process group gets, whether
// the caller has a live *os/exec.Cmd to Wait() on (EnsureWorker's own
// rollback of a worker it just spawned) or only a poll loop over a recorded
// pid from another process entirely (StopWorker, possibly long after the
// launcher that started it exited). Either way it never returns until the
// group is confirmed gone or the bound is exhausted.
func escalateSignal(pid int, done <-chan struct{}) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(workerTermTimeout):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-time.After(workerKillTimeout):
		return fmt.Errorf("uat-worker pid %d did not exit after SIGKILL", pid)
	}
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func pollUntilDead(pid int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for processAlive(pid) {
			time.Sleep(workerPollInterval)
		}
		close(done)
	}()
	return done
}

// verifyWorkerProc reports whether pid is really a `pix-host uat-worker`
// serving sessionID, via `ps` — run is injected so a test proves the
// argv-parsing logic itself without a real process. known=false on anything
// ps could not answer, which every caller here treats as "do not signal".
func verifyWorkerProc(pid int, sessionID string, run func(name string, args ...string) ([]byte, error)) (ours, known bool) {
	out, err := run("ps", "-o", "args=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false, false
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return false, false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || filepath.Base(fields[0]) != "pix-host" {
		return false, true
	}
	hasWorker, hasSession := false, false
	for i, f := range fields {
		if f == "uat-worker" {
			hasWorker = true
		}
		if f == "--session" && i+1 < len(fields) && fields[i+1] == sessionID {
			hasSession = true
		}
	}
	return hasWorker && hasSession, true
}

func defaultVerifyWorkerProc(pid int, sessionID string) (ours, known bool) {
	return verifyWorkerProc(pid, sessionID, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	})
}

// stopDeps is StopWorker's injectable seam, so a test proves the identity
// gate (never signal an unverified pid) without touching a real process.
type stopDeps struct {
	alive  func(pid int) bool
	verify func(pid int, sessionID string) (ours, known bool)
	term   func(pid int) error
}

func defaultStopDeps() stopDeps {
	return stopDeps{
		alive:  processAlive,
		verify: defaultVerifyWorkerProc,
		term:   func(pid int) error { return escalateSignal(pid, pollUntilDead(pid)) },
	}
}

// StopWorker terminates the uat-worker recorded for a session's runner state,
// if any. It is called from the same places that already remove the
// session's registration and state (DeleteRegistration), so a worker is
// always stopped BEFORE its socket and pid record disappear out from under
// it — including the hard-crash/orphan sweep path, where the launcher that
// started the worker is long gone. A missing record or an already-dead pid
// just clears the record. A record whose session or socket no longer
// matches, or a pid `ps` cannot positively verify as this session's
// uat-worker, is left alone: this never signals a pid it has not verified as
// its own.
func StopWorker(runnerState, sessionID string) error {
	return stopWorker(defaultStopDeps(), runnerState, sessionID)
}

func stopWorker(deps stopDeps, runnerState, sessionID string) error {
	rec, err := ReadWorkerRecord(runnerState)
	if err != nil {
		return fmt.Errorf("read uat-worker record: %w", err)
	}
	if rec == nil {
		return nil
	}
	if rec.SessionID != sessionID || rec.Socket != SessionSocketPath(runnerState) || rec.PID <= 0 {
		return fmt.Errorf("uat-worker record in %s does not match session %s; refusing to signal pid %d", runnerState, sessionID, rec.PID)
	}
	if !deps.alive(rec.PID) {
		return removeWorkerRecord(runnerState)
	}
	ours, known := deps.verify(rec.PID, sessionID)
	if !known {
		return fmt.Errorf("could not verify uat-worker pid %d identity; refusing to signal it", rec.PID)
	}
	if !ours {
		// pid reuse: something else now holds this pid. Never signal it; the
		// record is stale and safe to drop.
		return removeWorkerRecord(runnerState)
	}
	if terr := deps.term(rec.PID); terr != nil {
		return terr
	}
	return removeWorkerRecord(runnerState)
}
