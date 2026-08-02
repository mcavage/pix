package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/rpc"
)

// starterRec records what a fake serveStarter did, and drives a fake clock.
type starterRec struct {
	spawns      int
	marked      bool
	markedPid   int // the pid markLazy received (H4: the marker carries it)
	recordedPid int // the pid recordPid received (H3: written before unlock)
	order       []string
	buf         bytes.Buffer
	clock       time.Time
	up          bool // the "port answers" state the fake dial reads
	upAfter     int  // dials remaining until up flips true (0 = never auto-flip)
	dials       int
	released    bool // handle.release() was called (round 3: gated on recordPid success)
}

// starter builds a serveStarter over rec with an ABSENT pidfile and a
// pass-through lock. Tests mutate fields for their scenario.
func (rec *starterRec) starter() serveStarter {
	return serveStarter{
		hostBin: func() (string, error) { return "/fake/pix-host", nil },
		dial: func(port int) bool {
			rec.dials++
			if rec.upAfter > 0 {
				rec.upAfter--
				if rec.upAfter == 0 {
					rec.up = true
				}
			}
			return rec.up
		},
		spawn: func(bin string, args []string, logPath string) (serveChildHandle, error) {
			rec.spawns++
			rec.order = append(rec.order, "spawn")
			return serveChildHandle{
				pid: 4242,
				kill: func() error {
					rec.order = append(rec.order, "kill")
					return nil
				},
				wait: func() error {
					rec.order = append(rec.order, "wait")
					return nil
				},
				release: func() {
					rec.released = true
					rec.order = append(rec.order, "release")
				},
			}, nil
		},
		tryLock: func(fn func() error) (bool, error) {
			rec.order = append(rec.order, "lock")
			err := fn()
			rec.order = append(rec.order, "unlock")
			return true, err
		},
		recordPid: func(pid int) error {
			rec.recordedPid = pid
			rec.order = append(rec.order, "recordPid")
			return nil
		},
		markLazy: func(pid int) {
			rec.marked = true
			rec.markedPid = pid
			rec.order = append(rec.order, "markLazy")
		},
		ctl: serveCtl{
			pidPath: func() string { return "/nonexistent/serve.pid" },
			readPid: func(string) (string, error) { return "", os.ErrNotExist },
			kill:    func(int, syscall.Signal) error { return syscall.ESRCH },
			verify:  func(int) (bool, bool) { return false, false },
			sleep:   func(time.Duration) {},
		},
		sleep:   func(d time.Duration) { rec.clock = rec.clock.Add(d) },
		now:     func() time.Time { return rec.clock },
		logPath: func() string { return "/fake/state/serve.log" },
		tailLog: func(string, int) string { return "fake-log-tail" },
		getenv:  func(string) string { return "" },
		stderr:  &rec.buf,
	}
}

func starterCfg() *config.Config {
	return &config.Config{Services: []string{"memory", "knowledge"}}
}

// Fast path: everything already up -> nil, no spawn, SILENT.
func TestEnsureServeFastPathAlreadyUp(t *testing.T) {
	rec := &starterRec{up: true}
	if err := ensureServe(rec.starter(), starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("ensureServe: %v", err)
	}
	if rec.spawns != 0 {
		t.Errorf("spawned %d times, want 0", rec.spawns)
	}
	if rec.buf.Len() != 0 {
		t.Errorf("expected silence when already up, got %q", rec.buf.String())
	}
}

// Down -> spawn once, mark lazy, wait until up, legible messages.
func TestEnsureServeSpawnsAndBecomesReady(t *testing.T) {
	rec := &starterRec{upAfter: 8}
	if err := ensureServe(rec.starter(), starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("ensureServe: %v", err)
	}
	if rec.spawns != 1 {
		t.Errorf("spawned %d times, want 1", rec.spawns)
	}
	if !rec.marked {
		t.Error("serve.lazy marker not written after spawn")
	}
	if rec.markedPid != 4242 {
		t.Errorf("marker pid = %d, want the spawned pid 4242 (H4)", rec.markedPid)
	}
	out := rec.buf.String()
	if !strings.Contains(out, "starting pix services (memory:11435, knowledge:11436)") {
		t.Errorf("missing starting message, got %q", out)
	}
	if !strings.Contains(out, "pix services ready") {
		t.Errorf("missing ready message, got %q", out)
	}
}

// Spawn itself fails -> honest immediate failure, no health-wait, no marker.
func TestEnsureServeSpawnFailure(t *testing.T) {
	rec := &starterRec{}
	st := rec.starter()
	st.spawn = func(string, []string, string) (serveChildHandle, error) {
		rec.spawns++
		return serveChildHandle{}, os.ErrPermission
	}
	err := ensureServe(st, starterCfg(), ensureServeOpts{})
	if err == nil || !strings.Contains(err.Error(), "could not start pix services") {
		t.Fatalf("err = %v, want could-not-start", err)
	}
	if rec.marked {
		t.Error("marker written despite spawn failure")
	}
	if !strings.Contains(rec.buf.String(), "run `pix serve` to see the error") {
		t.Errorf("failure message not printed: %q", rec.buf.String())
	}
}

// Never becomes ready -> timeout error with the log tail + log path; the fake
// clock advances via sleep so the test never real-waits.
func TestEnsureServeHealthWaitTimeout(t *testing.T) {
	rec := &starterRec{} // ports never up
	err := ensureServe(rec.starter(), starterCfg(), ensureServeOpts{Timeout: 2 * time.Second})
	if err == nil {
		t.Fatal("want timeout error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "did not become ready in 2s") {
		t.Errorf("missing timeout budget in %q", msg)
	}
	if !strings.Contains(msg, "fake-log-tail") || !strings.Contains(msg, "/fake/state/serve.log") {
		t.Errorf("missing log tail / path in %q", msg)
	}
	if rec.spawns != 1 {
		t.Errorf("spawned %d times, want 1", rec.spawns)
	}
}

// PIX_NO_AUTOSERVE set -> no spawn, sentinel error, clear message.
func TestEnsureServeOptOutEnv(t *testing.T) {
	rec := &starterRec{}
	st := rec.starter()
	st.getenv = func(k string) string {
		if k == autoserveEnvVar {
			return "1"
		}
		return ""
	}
	if err := ensureServe(st, starterCfg(), ensureServeOpts{}); err != errAutoserveDisabled {
		t.Fatalf("err = %v, want errAutoserveDisabled", err)
	}
	if rec.spawns != 0 {
		t.Errorf("spawned despite opt-out")
	}
	if !strings.Contains(rec.buf.String(), "auto-start is disabled") {
		t.Errorf("opt-out message missing: %q", rec.buf.String())
	}
}

// host.autoserve = false -> same opt-out.
func TestEnsureServeOptOutConfig(t *testing.T) {
	rec := &starterRec{}
	cfg := starterCfg()
	no := false
	cfg.Host.Autoserve = &no
	if err := ensureServe(rec.starter(), cfg, ensureServeOpts{}); err != errAutoserveDisabled {
		t.Fatalf("err = %v, want errAutoserveDisabled", err)
	}
	if rec.spawns != 0 {
		t.Errorf("spawned despite host.autoserve=false")
	}
}

// Opt-out is irrelevant when everything is already up: fast-path nil.
func TestEnsureServeOptOutButAlreadyUp(t *testing.T) {
	rec := &starterRec{up: true}
	st := rec.starter()
	st.getenv = func(string) string { return "1" }
	if err := ensureServe(st, starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("ensureServe: %v", err)
	}
}

// A live, VERIFIED-ours pidfile means a start is in progress: wait, never
// double-spawn.
func TestEnsureServePidfileOwnedNoDoubleSpawn(t *testing.T) {
	rec := &starterRec{upAfter: 5}
	st := rec.starter()
	st.ctl = serveCtl{
		pidPath: func() string { return "/x/serve.pid" },
		readPid: func(string) (string, error) { return "4242\n", nil },
		kill:    func(int, syscall.Signal) error { return nil },
		verify:  func(int) (bool, bool) { return true, true },
		sleep:   func(time.Duration) {},
	}
	if err := ensureServe(st, starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("ensureServe: %v", err)
	}
	if rec.spawns != 0 {
		t.Errorf("double-spawned despite an owned live pidfile")
	}
	if !strings.Contains(rec.buf.String(), "pix services ready") {
		t.Errorf("missing ready message: %q", rec.buf.String())
	}
}

// A pidfile pointing at a NOT-ours pid does not block the spawn.
func TestEnsureServeHijackedPidfileStillSpawns(t *testing.T) {
	rec := &starterRec{upAfter: 5}
	st := rec.starter()
	st.ctl.readPid = func(string) (string, error) { return "99\n", nil }
	st.ctl.kill = func(int, syscall.Signal) error { return nil }
	st.ctl.verify = func(int) (bool, bool) { return false, true }
	if err := ensureServe(st, starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("ensureServe: %v", err)
	}
	if rec.spawns != 1 {
		t.Errorf("spawned %d times, want 1 (unowned pidfile must not suppress)", rec.spawns)
	}
}

// Double-checked locking: a racer starts serve while we wait on the lock; the
// re-probe under the lock sees it up and we do NOT spawn.
func TestEnsureServeRaceLoserDoesNotSpawn(t *testing.T) {
	rec := &starterRec{}
	st := rec.starter()
	st.tryLock = func(fn func() error) (bool, error) {
		rec.up = true // the racing process bound the ports while we blocked
		return true, fn()
	}
	if err := ensureServe(st, starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("ensureServe: %v", err)
	}
	if rec.spawns != 0 {
		t.Errorf("race loser spawned anyway")
	}
}

// H3: the spawned child's pid is recorded in the pidfile (and the lazy marker)
// BEFORE the spawn lock releases, so a cold-init racer that acquires the lock
// next sees the pidfile instead of forking a second daemon. Round 3 (H10):
// release() only runs AFTER recordPid succeeds, and strictly before markLazy.
func TestEnsureServeRecordsPidBeforeUnlock(t *testing.T) {
	rec := &starterRec{upAfter: 5}
	if err := ensureServe(rec.starter(), starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("ensureServe: %v", err)
	}
	if rec.recordedPid != 4242 {
		t.Fatalf("recorded pid = %d, want the spawned pid 4242", rec.recordedPid)
	}
	if !rec.released {
		t.Error("handle.release() was never called after a successful recordPid")
	}
	want := []string{"lock", "spawn", "recordPid", "release", "markLazy", "unlock"}
	if strings.Join(rec.order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v (pidfile MUST land before the lock releases)", rec.order, want)
	}
}

// H3 end-to-end shape: caller A spawns (daemon pidfile not yet written by the
// child); caller B runs in the window AFTER A released the lock but BEFORE the
// daemon's own writeServePidFile — B must see A's launcher-recorded pid via
// readLiveServePid and WAIT, never spawn a second daemon.
func TestEnsureServeColdInitTwoCallersSingleSpawn(t *testing.T) {
	// Shared "filesystem": the launcher-recorded pidfile contents.
	pidfile := ""
	mkStarter := func(rec *starterRec) serveStarter {
		st := rec.starter()
		st.recordPid = func(pid int) error { pidfile = "4242\n"; return nil }
		st.ctl = serveCtl{
			pidPath: func() string { return "/x/serve.pid" },
			readPid: func(string) (string, error) {
				if pidfile == "" {
					return "", os.ErrNotExist
				}
				return pidfile, nil
			},
			kill:   func(int, syscall.Signal) error { return nil }, // recorded pid is alive
			verify: func(int) (bool, bool) { return true, true },   // and verified-ours
			sleep:  func(time.Duration) {},
		}
		return st
	}

	recA := &starterRec{upAfter: 5}
	if err := ensureServe(mkStarter(recA), starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("caller A: %v", err)
	}
	if recA.spawns != 1 {
		t.Fatalf("caller A spawned %d times, want 1", recA.spawns)
	}

	// Caller B: daemon process alive (pidfile recorded by A) but its ports are
	// still down (cold init: config load / store open / indexing in progress).
	recB := &starterRec{upAfter: 5}
	if err := ensureServe(mkStarter(recB), starterCfg(), ensureServeOpts{}); err != nil {
		t.Fatalf("caller B: %v", err)
	}
	if recB.spawns != 0 {
		t.Errorf("caller B spawned %d times, want 0 (must wait on A's recorded pid)", recB.spawns)
	}
	if !strings.Contains(recB.buf.String(), "pix services ready") {
		t.Errorf("caller B should have waited to ready: %q", recB.buf.String())
	}
}

// Round 2 (H8) / Round 3 (H10) (a): recordPid failing must NOT release the
// spawn lock as success. When the cleanup kill SUCCEEDS, the just-spawned
// child is Kill()ed AND Wait()ed/reaped (round 3 closes the leaked-zombie /
// unconfirmed-kill gap: the old code discarded the kill error and never
// reaped, because spawnDetachedServe had already Release()d the process,
// making it un-Wait()able). release() must never run on this path (it would
// foreclose the Wait()), markLazy must not run, and the error surfaces so a
// racing second caller can never end up with two live, unrecorded daemons.
func TestEnsureServeRecordPidFailureKillSucceeds(t *testing.T) {
	rec := &starterRec{upAfter: 5}
	st := rec.starter()
	st.recordPid = func(pid int) error { return errors.New("disk full") }
	killed, waited, released := false, false, false
	st.spawn = func(string, []string, string) (serveChildHandle, error) {
		rec.spawns++
		return serveChildHandle{
			pid:     4242,
			kill:    func() error { killed = true; return nil },
			wait:    func() error { waited = true; return nil },
			release: func() { released = true },
		}, nil
	}
	err := ensureServe(st, starterCfg(), ensureServeOpts{})
	if err == nil {
		t.Fatal("want an error when recordPid fails")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error should surface the underlying cause: %v", err)
	}
	if rec.spawns != 1 {
		t.Fatalf("spawns = %d, want 1 (spawn itself succeeded)", rec.spawns)
	}
	if !killed {
		t.Error("the just-spawned child was never killed after a recordPid failure")
	}
	if !waited {
		t.Error("the killed child was never Wait()ed/reaped (a released process can't be reaped, so this must happen BEFORE release)")
	}
	if released {
		t.Error("release() must NOT run on the recordPid-failure path")
	}
	if rec.marked {
		t.Error("markLazy must not run after a failed pid record")
	}
	if strings.Contains(err.Error(), "could not start pix services") {
		t.Errorf("error should be the pid-record failure, not the generic spawn-failed message: %v", err)
	}
}

// Round 3 (H10) (b): if the CLEANUP kill itself fails, that failure must not be
// swallowed (the old code did `_ = st.ctl.kill(...)`) — it must surface in the
// returned error, since the child may still be alive, unrecorded, and
// undetectable by a racing second launcher. Wait() must not be attempted on a
// child whose kill failed (it may not be dead, so Wait would hang/misreport).
func TestEnsureServeRecordPidFailureKillFails(t *testing.T) {
	rec := &starterRec{upAfter: 5}
	st := rec.starter()
	st.recordPid = func(pid int) error { return errors.New("disk full") }
	waited := false
	st.spawn = func(string, []string, string) (serveChildHandle, error) {
		rec.spawns++
		return serveChildHandle{
			pid:     4242,
			kill:    func() error { return errors.New("operation not permitted") },
			wait:    func() error { waited = true; return nil },
			release: func() { t.Error("release() must not run when the cleanup kill failed") },
		}, nil
	}
	err := ensureServe(st, starterCfg(), ensureServeOpts{})
	if err == nil {
		t.Fatal("want an error when recordPid AND the cleanup kill both fail")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error should still mention the recordPid cause: %v", err)
	}
	if !strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("error must mention the kill failure: %v", err)
	}
	if waited {
		t.Error("must not Wait() a child whose kill failed (it may still be alive)")
	}
	if rec.marked {
		t.Error("markLazy must not run after a failed pid record")
	}
}

// Round 2 (H8) end-to-end: a real recordSpawnedServePid failure (unwritable
// pidfile dir) must not silently succeed — it now returns an error.
func TestRecordSpawnedServePidReturnsErrorOnFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", "")
	t.Setenv("XDG_STATE_HOME", blocker) // config.ServePidPath() nests under the STATE dir; a FILE where a dir is needed makes MkdirAll fail
	if err := recordSpawnedServePid(4242); err == nil {
		t.Fatal("want an error when the pidfile directory cannot be created")
	}
}

// M2: a wedged lock-holder cannot hang the caller — tryLock keeps reporting
// busy, and the SAME deadline that bounds the health wait bounds the retries.
func TestEnsureServeLockBusyBoundedByDeadline(t *testing.T) {
	rec := &starterRec{}
	st := rec.starter()
	tries := 0
	st.tryLock = func(fn func() error) (bool, error) {
		tries++
		return false, nil // always busy
	}
	err := ensureServe(st, starterCfg(), ensureServeOpts{Timeout: 2 * time.Second})
	if err == nil {
		t.Fatal("want a bounded-timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "start lock") {
		t.Errorf("error should name the busy start lock: %v", err)
	}
	if tries == 0 {
		t.Error("tryLock never attempted")
	}
	if rec.spawns != 0 {
		t.Error("spawned despite never holding the lock")
	}
}

// requiredServePorts: narrows to the requested subset, intersects with the
// ENABLED set, dedupes, ignores unknown names.
func TestRequiredServePorts(t *testing.T) {
	rec := &starterRec{}
	st := rec.starter()

	memOnly := &config.Config{Services: []string{"memory"}}
	if got := requiredServePorts(st, memOnly, []string{"knowledge"}); len(got) != 0 {
		t.Errorf("disabled service required: %v", got)
	}
	both := starterCfg()
	got := requiredServePorts(st, both, nil)
	if len(got) != 2 || got[0].name != "memory" || got[0].port != rpc.MemoryPortDefault ||
		got[1].name != "knowledge" || got[1].port != rpc.KnowledgePortDefault {
		t.Errorf("full set = %v", got)
	}
	if got := requiredServePorts(st, both, []string{"memory", "memory", "bogus"}); len(got) != 1 || got[0].name != "memory" {
		t.Errorf("dedupe/unknown = %v", got)
	}
	// Port env override flows through servePort.
	st.getenv = func(k string) string {
		if k == "MEMORY_PORT" {
			return "21435"
		}
		return ""
	}
	if got := requiredServePorts(st, both, []string{"memory"}); len(got) != 1 || got[0].port != 21435 {
		t.Errorf("env override = %v", got)
	}
}

// stopServe clears the serve.lazy marker on a successful stop.
func TestStopServeClearsLazyMarker(t *testing.T) {
	removedLazy := false
	proc := &fakeProc{pid: 321, alive: true}
	removed := false
	ctl := ctlFor("321", nil, proc, &removed, func(int) (bool, bool) { return true, true })
	ctl.removeLazy = func() { removedLazy = true }

	var out bytes.Buffer
	stopped, err := stopServe(ctl, &out)
	if err != nil || !stopped {
		t.Fatalf("stopServe = %v, %v", stopped, err)
	}
	if !removedLazy {
		t.Error("serve.lazy marker not cleared on successful stop")
	}
}
