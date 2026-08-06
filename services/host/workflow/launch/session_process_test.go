//go:build unix

package launch

// These tests drive RunSession against a REAL `sbx` fixture script and REAL
// child processes, with barrier files instead of sleeps, because the four
// properties U04c2 exists to guarantee are all about ORDERING ACROSS
// PROCESSES and cannot be observed from a mock:
//
//   - the create-time record is complete while the first session is STILL
//     RUNNING (not written at exit, where a crash would lose it),
//   - a concurrent second attach is unblocked by the FIRST RECORD, not by the
//     first session ending (the lifecycle lock covers the transition only),
//   - a creator SIGKILLed mid-session still leaves that record behind, and
//     leaves no lock holder (the kernel releases flock on the fd, and the
//     child shell never inherited it — CLOEXEC),
//   - the argv actually handed to sbx is exact, including -it vs -i.
//
// The barrier protocol the fixture implements:
//   <dir>/argv.log   every invocation's argv, one line each
//   <dir>/created    written by `sbx run`: the sandbox is now visible to `ls`
//   <dir>/attached   written by `sbx exec`: an attach's child actually started
//   <dir>/release    written by the TEST: lets a live "session" exit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"pix/host/lease"
	"pix/host/sandbox"
)

// sessionFixture is the fixture sbx: a real script that records argv, becomes
// visible to `ls` only after `run` has started, and keeps a "session" alive
// until the test releases it.
const sessionFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	if [ -f "$d/created" ]; then
		if [ "$2" = "--json" ]; then
			echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
		else
			echo "pix-demo  img  running"
		fi
	fi
	exit 0
	;;
run)
	touch "$d/created"
	while [ ! -f "$d/release" ]; do sleep 0.02; done
	exit 0
	;;
exec)
	touch "$d/attached"
	while [ ! -f "$d/release" ]; do sleep 0.02; done
	exit 0
	;;
esac
exit 0
`

// fastPoll is the production poll shape on a test budget: the same Probe, a
// 5 ms interval instead of 500 ms.
func fastPoll() CreatePoll {
	return CreatePoll{
		Probe:    func(name string) SbxState { return ProbeTaskSandbox(realEnv(), name) },
		Interval: 5 * time.Millisecond,
		Timeout:  30 * time.Second,
	}
}

func waitForFile(t *testing.T, path string, within time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return time.Now()
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, path)
	return time.Time{}
}

func argvLines(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		t.Fatalf("reading argv.log: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func release(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitForRecordedCreateState blocks until every create-time artifact this
// test asserts on after the kill is on disk: the record, the invocation, and
// (when the spec asked for it) the keep binding. RecordSessionCreation and
// setSessionKeep write these SEQUENTIALLY, in that order, still under the
// lifecycle lock — but they are separate files committed by separate
// rename(2)s, so a barrier on record.json alone left a window before the
// LATER writes landed. Killing inside that window raced whichever write
// hadn't happened yet, which is exactly the flake this barrier closes: it
// waits (polling, not sleeping a fixed duration) for the LAST write in the
// sequence, which proves the earlier ones already committed.
func waitForRecordedCreateState(t *testing.T, leaseDir, sessionKey string, wantKeep bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		_, rerr := lease.ReadRecord(leaseDir)
		_, invFound := readSessionInvocation(sessionKey)
		_, keepSet, kerr := lease.ReadKeep(leaseDir)
		if rerr == nil && invFound && (!wantKeep || (kerr == nil && keepSet)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for the full create-time record (record=%v invocation=%v keep=%v/%v)", within, rerr, invFound, keepSet, kerr)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRunSession_RecordsBeforeWaiting_AndUnblocksAttachOnRecord proves the two
// central ordering claims at once, because they are two halves of the same
// window: while the first session is still running, its record is already
// complete on disk; and a second attach that was blocked on the lifecycle lock
// completes within 250 ms OF THAT RECORD — not of the first session's exit,
// which does not happen until this test releases it at the end.
func TestRunSession_RecordsBeforeWaiting_AndUnblocksAttachOnRecord(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)
	fp := sandbox.Fingerprint{"static_mcp": "slack"}

	created := make(chan error, 1)
	go func() {
		created <- RunSession(SessionSpec{
			Key: key, Name: "pix-demo", Creating: true,
			CreateArgs:  []string{"run", "--name", "pix-demo"},
			Fingerprint: fp, Invocation: []string{"--model", "m"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t)})
	}()

	// The creator is inside its transition (lifecycle EX held) the moment the
	// fixture's `run` has started. Launch the second attach NOW so it is
	// genuinely blocked on the lifecycle lock rather than racing to it.
	waitForFile(t, filepath.Join(fixture, "created"), 10*time.Second)
	attached := make(chan error, 1)
	go func() {
		attached <- RunSession(SessionSpec{
			Key: key, Name: "pix-demo", AttachExec: true, AttachTTY: true,
			CreateArgs: []string{"run", "--name", "pix-demo"}, Fingerprint: fp,
			DefaultInvocation: []string{"--unused-default"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t)})
	}()

	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	recordSeen := waitForFile(t, filepath.Join(leaseDir, "record.json"), 20*time.Second)

	// (1) The record is COMPLETE — instance id, fingerprint, invocation — and
	// the first session is still running: it cannot have exited, because
	// nothing has written the release barrier yet.
	rec, err := lease.ReadRecord(leaseDir)
	if err != nil || rec.InstanceID != "inst-1" {
		t.Fatalf("record = %+v (err %v), want instance inst-1", rec, err)
	}
	if diverged, found := CheckSessionFingerprint(key, fp); !found || len(diverged) > 0 {
		t.Errorf("fingerprint not recorded before the session ended: found=%v diverged=%v", found, diverged)
	}
	if inv, found := readSessionInvocation(key); !found || strings.Join(inv, " ") != "--model m" {
		t.Errorf("invocation not recorded before the session ended: %v (found %v)", inv, found)
	}
	select {
	case err := <-created:
		t.Fatalf("the creating session already returned (%v) — the record must be written while it is STILL RUNNING", err)
	default:
	}

	// (2) The blocked attach proceeds off the RECORD, not the session's end.
	attachSeen := waitForFile(t, filepath.Join(fixture, "attached"), 10*time.Second)
	if d := attachSeen.Sub(recordSeen); d > 250*time.Millisecond {
		t.Errorf("second attach started %s after the first record; want <250ms (the lifecycle lock must cover the transition, not the session)", d)
	}

	// (3) The attach replayed the STORED invocation verbatim, as `exec -it`.
	var execLine string
	for _, l := range argvLines(t, fixture) {
		if strings.HasPrefix(l, "exec ") {
			execLine = l
		}
	}
	if execLine != "exec -it pix-demo pi --model m" {
		t.Errorf("attach argv = %q, want %q", execLine, "exec -it pix-demo pi --model m")
	}

	release(t, fixture)
	if err := <-created; err != nil {
		t.Errorf("creating session: %v", err)
	}
	if err := <-attached; err != nil {
		t.Errorf("attaching session: %v", err)
	}
	// Both sessions ended: no reference holder is left, which is what a future
	// reaper's zero-holder proof depends on.
	if err := lease.TryReapProof(leaseDir, func() error { return nil }); err != nil {
		t.Errorf("after both sessions exited, TryReapProof = %v, want nil (no leaked holder)", err)
	}
}

// TestRunSession_AttachUnowned_UsesSafeDefaultArgv: a sandbox with NO record
// (legacy, or one this host never proved it created) attaches with the freshly
// recomputed safe default invocation — never refusing, never inventing an
// identity, and — having no record — never authorizing a removal. -k on such a
// session records no keep either (see TestRunSession_KeepNeedsARecord).
func TestRunSession_AttachUnowned_UsesSafeDefaultArgv(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	// Make the sandbox already visible: this is an attach, not a create.
	if err := os.WriteFile(filepath.Join(fixture, "created"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	key := SessionName(ws)

	done := make(chan error, 1)
	go func() {
		done <- RunSession(SessionSpec{
			Key: key, Name: "pix-demo", AttachExec: true, AttachTTY: false, Keep: true,
			CreateArgs: []string{"run", "--name", "pix-demo"},
			// A fingerprint that would DIVERGE if anything were recorded:
			// nothing is, so there is nothing to compare against.
			Fingerprint:       sandbox.Fingerprint{"static_mcp": "notion"},
			DefaultInvocation: []string{"--skill", "/opt/skills"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t)})
	}()

	waitForFile(t, filepath.Join(fixture, "attached"), 10*time.Second)
	release(t, fixture)
	if err := <-done; err != nil {
		t.Fatalf("unowned attach: %v", err)
	}

	var execLine string
	for _, l := range argvLines(t, fixture) {
		if strings.HasPrefix(l, "exec ") {
			execLine = l
		}
	}
	if execLine != "exec -i pix-demo pi --skill /opt/skills" {
		t.Errorf("unowned attach argv = %q, want %q", execLine, "exec -i pix-demo pi --skill /opt/skills")
	}
	if SessionRecorded(key) {
		t.Error("an unowned attach must not create a creation record")
	}
	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, set, kerr := lease.ReadKeep(leaseDir); set || kerr != nil {
		t.Errorf("keep set=%v err=%v; -k on an unowned session must record nothing", set, kerr)
	}
}

// TestRunSession_CreateArgvIsVerbatim: the composed create argv the command
// layer planned reaches sbx unchanged — RunSession rewrites argv for exactly
// one case (an exec-attach), never for a create.
func TestRunSession_CreateArgvIsVerbatim(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	want := []string{"run", "--name", "pix-demo", "--static-mcp", "slack", "--", "pi", "--model", "m"}

	done := make(chan error, 1)
	go func() {
		done <- RunSession(SessionSpec{
			Key: SessionName(ws), Name: "pix-demo", Creating: true,
			CreateArgs: want, Invocation: []string{"--model", "m"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t)})
	}()
	waitForFile(t, filepath.Join(fixture, "created"), 10*time.Second)
	release(t, fixture)
	if err := <-done; err != nil {
		t.Fatalf("create session: %v", err)
	}
	// argv.log also carries the read-only `ls` probes (the pre-lock decision is
	// re-checked UNDER the lock); the one mutating invocation is the create.
	var runLine string
	for _, l := range argvLines(t, fixture) {
		if strings.HasPrefix(l, "run ") {
			runLine = l
		}
	}
	if runLine != strings.Join(want, " ") {
		t.Errorf("create argv = %q, want %q", runLine, strings.Join(want, " "))
	}
}

// --- killed creator ------------------------------------------------------
//
// This one needs a REAL, separately killable process: the point is that the
// record survives a creator that never gets to run any cleanup at all.

const sessionHelperEnv = "LAUNCH_SESSION_HELPER"

// TestSessionCreateHelperProcess is the re-exec'd helper (see helperCreate).
// It is a no-op under a normal `go test` run.
func TestSessionCreateHelperProcess(t *testing.T) {
	if os.Getenv(sessionHelperEnv) != "1" {
		return
	}
	err := RunSession(SessionSpec{
		Key: os.Getenv("HELPER_KEY"), Name: "pix-demo", Creating: true,
		CreateArgs:  []string{"run", "--name", "pix-demo"},
		Fingerprint: sandbox.Fingerprint{"static_mcp": "slack"},
		Invocation:  []string{"--model", "m"},
		Keep:        true,
	}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: os.Stderr, Spawn: func(argv []string) *exec.Cmd {
		cmd := exec.Command("sbx", argv...)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		return cmd
	}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper RunSession: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestRunSession_KilledCreator_LeavesTheRecord: a creator SIGKILLed while its
// session is live — no defers, no cleanup, no chance to write anything —
// still leaves the complete record behind, because RunSession wrote it before
// releasing the lifecycle lock. And it leaves NO holder: the kernel released
// the refs lock on the dead fd, and the child sbx shell never inherited it
// (CLOEXEC), so a future reaper's zero-holder proof succeeds.
func TestRunSession_KilledCreator_LeavesTheRecord(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)
	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSessionCreateHelperProcess")
	cmd.Env = append(os.Environ(),
		sessionHelperEnv+"=1",
		"HELPER_KEY="+key,
		"HELPER_WS="+ws,
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	t.Cleanup(func() {
		release(t, fixture)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Wait for the WHOLE create-time sequence — record, invocation, and the
	// keep binding the helper's spec asks for — not just record.json, so the
	// kill lands strictly AFTER every write this test checks, never mid-write.
	waitForRecordedCreateState(t, leaseDir, key, true, 20*time.Second)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	state, err := cmd.Process.Wait()
	if err != nil {
		t.Fatalf("reaping the killed helper: %v", err)
	}
	if state.ExitCode() != -1 {
		t.Errorf("helper exit code = %d, want -1 (killed by signal, no cleanup ran)", state.ExitCode())
	}

	rec, rerr := lease.ReadRecord(leaseDir)
	if rerr != nil || rec.InstanceID != "inst-1" {
		t.Fatalf("record after SIGKILL = %+v (err %v), want instance inst-1", rec, rerr)
	}
	if inv, found := readSessionInvocation(key); !found || strings.Join(inv, " ") != "--model m" {
		t.Errorf("invocation after SIGKILL = %v (found %v)", inv, found)
	}
	if _, set, kerr := lease.ReadKeep(leaseDir); !set || kerr != nil {
		t.Errorf("keep set=%v err=%v; -k was recorded after the record and must survive the kill", set, kerr)
	}

	// No leaked holder, and the lifecycle lock is free again: proven with the
	// reaper's own non-blocking primitive, bounded so a leak fails the test
	// rather than hanging it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		perr := lease.TryReapProof(leaseDir, func() error { return nil })
		if perr == nil {
			break
		}
		if !errors.Is(perr, lease.ErrHeld) || time.Now().After(deadline) {
			t.Fatalf("after SIGKILL, TryReapProof = %v, want nil (kernel releases the fd; the child never inherited it)", perr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Sanity: the fixture session child really was still running when the
	// creator died — the record was written mid-session, not at exit.
	if _, serr := os.Stat(filepath.Join(fixture, "release")); serr == nil {
		t.Fatal("the release barrier was written before the kill; the session was not live")
	}
}
