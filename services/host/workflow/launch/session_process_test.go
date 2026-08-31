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
	"pix/host/sys"
)

// awaitRelease is how every fixture that holds a "session" open waits for the
// test to let it go. Both guards exist because these shells are DESIGNED to be
// orphaned — the kill tests SIGKILL the process that spawned them — and an
// orphan that waits forever forks `sleep` ~50x/sec for the life of the machine.
// That is not a theoretical cost: on macOS each exec writes a BSM audit record,
// and enough leaked fixtures will pin a core in whatever daemon tails the audit
// trail. The two interrupted-run shapes need different guards:
//
//   - test binary exits without running cleanup (Ctrl-C, `go test` timeout):
//     TempDir cleanup DID run, so $d is gone and no release can ever arrive.
//   - test binary is SIGKILLed: cleanup did NOT run, so $d survives with no
//     release in it and the dir check alone would never fire — hence the count.
//
// The bound is 6000 iterations, not a duration: each one forks `sleep`, so it
// costs ~34ms rather than the nominal 20ms and the ceiling lands near 200s
// (measured). That is deliberately loose — far above any legitimate wait here
// (the longest is waitForRecordedCreateState's 20s) and far below "until
// reboot", which is the only bound this loop used to have.
const awaitRelease = `
	i=0
	while [ ! -f "$d/release" ]; do
		[ -d "$d" ] || exit 0
		i=$((i + 1))
		if [ "$i" -gt 6000 ]; then exit 0; fi
		sleep 0.02
	done
	touch "$d/exited"`

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
env)
	# the cutover create: sbx env create <effective> returns as soon as the
	# sandbox exists; the SESSION is a separate exec below.
	touch "$d/created"
	exit 0
	;;
run)
	touch "$d/created"` + awaitRelease + `
	exit 0
	;;
exec)
	if [ "$2" = "-it" ]; then touch "$d/attached-it"; else touch "$d/attached"; fi` + awaitRelease + `
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
		_, fpFound := readSessionFingerprint(sessionKey)
		_, invFound := readSessionInvocation(sessionKey)
		_, keepSet, kerr := lease.ReadKeep(leaseDir)
		if rerr == nil && fpFound && invFound && (!wantKeep || (kerr == nil && keepSet)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for the full create-time record (record=%v fingerprint=%v invocation=%v keep=%v/%v)", within, rerr, fpFound, invFound, keepSet, kerr)
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
			EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
			// Invocation is what gets RECORDED (audit only, QA re-review F1):
			// deliberately a DIFFERENT value from the attach's own
			// DefaultInvocation below, so a regression to "attach replays
			// whatever got stored" fails loudly instead of passing by
			// coincidence.
			Fingerprint: fp, Invocation: []string{"--model", "stale-create-time-model"},
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
			AttachArgs: []string{"run", "--name", "pix-demo"}, Fingerprint: fp,
			// DefaultInvocation is THIS attach's own current, freshly resolved
			// invocation (e.g. a new --model/--resume this run asked for) —
			// it must reach the exec argv, never the stale stored one above.
			DefaultInvocation: []string{"--model", "m"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t)})
	}()

	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	recordSeen := waitForFile(t, filepath.Join(leaseDir, "record.json"), 20*time.Second)
	waitForRecordedCreateState(t, leaseDir, key, false, 20*time.Second)

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
	if inv, found := readSessionInvocation(key); !found || strings.Join(inv, " ") != "--model stale-create-time-model" {
		t.Errorf("invocation not recorded before the session ended: %v (found %v)", inv, found)
	}
	select {
	case err := <-created:
		t.Fatalf("the creating session already returned (%v) — the record must be written while it is STILL RUNNING", err)
	default:
	}

	// (2) The blocked attach proceeds off the RECORD, not the session's end.
	// The INTERACTIVE attach's own exec (`exec -it`), not the create's own
	// session exec (`exec -i`, which this cutover also runs).
	attachSeen := waitForFile(t, filepath.Join(fixture, "attached-it"), 10*time.Second)
	if d := attachSeen.Sub(recordSeen); d > 250*time.Millisecond {
		t.Errorf("second attach started %s after the first record; want <250ms (the lifecycle lock must cover the transition, not the session)", d)
	}

	// (3) The attach used its OWN current invocation (DefaultInvocation),
	// NEVER the stale one the create recorded (QA re-review F1). After the
	// cutover the CREATE also execs (`sbx env create` then `sbx exec` —
	// this spec asked for no TTY, so its own line is `exec -i`), so the
	// assertion is that the interactive attach line is present, not that
	// it is the only exec on the wire.
	var execLines []string
	sawAttachExec := false
	for _, l := range argvLines(t, fixture) {
		if strings.HasPrefix(l, "exec ") {
			execLines = append(execLines, l)
			if l == "exec -it pix-demo -- pi --model m" {
				sawAttachExec = true
			}
		}
	}
	if !sawAttachExec {
		t.Errorf("attach argv = %q, want one of them to be %q", execLines, "exec -it pix-demo -- pi --model m")
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
			AttachArgs: []string{"run", "--name", "pix-demo"},
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
	if execLine != "exec -i pix-demo -- pi --skill /opt/skills" {
		t.Errorf("unowned attach argv = %q, want %q", execLine, "exec -i pix-demo -- pi --skill /opt/skills")
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
			EnvCreateArgs: want, Invocation: []string{"--model", "m"},
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
		EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
		Fingerprint:   sandbox.Fingerprint{"static_mcp": "slack"},
		Invocation:    []string{"--model", "m"},
		Keep:          true,
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
		// The creator is deliberately killed below, so its sbx child is
		// orphaned. Wait for that child to observe the release barrier before
		// testing.TempDir removes the fixture directory; otherwise cleanup can
		// race the child's final path lookup and flake with "directory not empty".
		waitForFile(t, filepath.Join(fixture, "exited"), 5*time.Second)
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

// --- U-fix-lifecycle: a started child must never continue unreferenced ---

// TestRunSession_RefLeaseFailsAfterChildStarts_KillsRatherThanLeavesUnreferenced
// forces the exact gap lease.AttachRefUnderLifecycle's fresh-context fix
// closes: the transition (fn) succeeds and starts the child, but the refs
// SHARED acquire that runs AFTER it cannot — here, sustained refs contention
// past a shrunk RefAcquireTimeout, standing in for "the expired original ctx
// + brief refs contention" case the lease-package tests prove directly. The
// session must come out of this DEAD, not live and unreferenced: a future
// reaper's zero-holder proof could not otherwise tell it apart from an
// orphan it is safe to reap out from under a real user.
func TestRunSession_RefLeaseFailsAfterChildStarts_KillsRatherThanLeavesUnreferenced(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)

	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}

	restore := lease.RefAcquireTimeout
	lease.RefAcquireTimeout = 50 * time.Millisecond
	t.Cleanup(func() { lease.RefAcquireTimeout = restore })

	// Hold refs.lock EXCLUSIVE for the whole test: the refs acquire that runs
	// after the transition can never succeed.
	holder, err := lease.OpenRefLease(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}

	var cmd *exec.Cmd
	spawn := fixtureSpawn(t)
	captureSpawn := func(argv []string) *exec.Cmd {
		cmd = spawn(argv)
		return cmd
	}

	done := make(chan error, 1)
	go func() {
		done <- RunSession(SessionSpec{
			Key: key, Name: "pix-demo", Creating: true,
			EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
			Fingerprint:   sandbox.Fingerprint{"static_mcp": "slack"},
			Invocation:    []string{"--model", "m"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: captureSpawn})
	}()

	// The child genuinely started (the fixture's `run` ran and became
	// visible) before the refs acquire could even begin.
	waitForFile(t, filepath.Join(fixture, "created"), 10*time.Second)

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSession never returned after its reference lease could not be acquired")
	}
	if runErr == nil {
		t.Fatal("RunSession = nil error, want the failed reference-lease acquire surfaced")
	}

	// The child must be DEAD, not left running unreferenced: it was killed,
	// not waited out to a clean exit the test never asked for (the fixture
	// only exits on its own once `release` is written, and this test never
	// writes it).
	if cmd == nil || cmd.ProcessState == nil {
		t.Fatal("the started child was never reaped")
	}
	if cmd.ProcessState.Success() {
		t.Error("the child exited cleanly; want it killed instead")
	}
	if _, serr := os.Stat(filepath.Join(fixture, "release")); serr == nil {
		t.Fatal("the release barrier was written; the child was supposed to be killed, not let run to completion")
	}

	// The create-time record still exists — the transition itself completed
	// and wrote it before the refs acquire even ran — but nothing holds a
	// reference to it any more, so once the artificial contention clears a
	// reaper is free to act on it.
	if !SessionRecorded(key) {
		t.Error("the create-time record must still exist; only the reference lease failed")
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if perr := lease.TryReapProof(dir, func() error { return nil }); perr != nil {
		t.Errorf("TryReapProof after the killed, unreferenced session = %v, want nil (no leaked holder)", perr)
	}
}

// TestRunSession_RefLeaseFailsAfterChildStarts_AlsoTearsDown proves the OTHER
// half of the same fix: killing the started child only ends this shell's OWN
// `sbx` invocation, it does not touch the sandbox that invocation started on
// the sbx runtime. Left there, that sandbox would sit unreferenced until some
// future orphan sweep happened to run. RunSession must instead hand it,
// immediately, to the very same proof-gated TeardownSandbox a normal session
// exit uses — proven here by the journalled verdict that call leaves behind.
// This test's own holder still has refs.lock EX, so the correct verdict is
// "kept-busy", not a removal: the point is that TeardownSandbox was reached
// and reported AT ALL, which the pre-fix code never did on this path.
func TestRunSession_RefLeaseFailsAfterChildStarts_AlsoTearsDown(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)

	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}

	restore := lease.RefAcquireTimeout
	lease.RefAcquireTimeout = 50 * time.Millisecond
	t.Cleanup(func() { lease.RefAcquireTimeout = restore })

	// Hold refs.lock EXCLUSIVE for the whole test: the refs acquire that runs
	// after the transition can never succeed, and neither can TeardownSandbox's
	// own zero-holder proof — it must come back "kept-busy", not removed.
	holder, err := lease.OpenRefLease(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })

	var warn strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- RunSession(SessionSpec{
			Key: key, Name: "pix-demo", Creating: true,
			EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
			Fingerprint:   sandbox.Fingerprint{"static_mcp": "slack"},
			Invocation:    []string{"--model", "m"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: &warn, Spawn: fixtureSpawn(t)})
	}()

	waitForFile(t, filepath.Join(fixture, "created"), 10*time.Second)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunSession = nil error, want the failed reference-lease acquire surfaced")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunSession never returned after its reference lease could not be acquired")
	}

	path, perr := teardownJournalPath()
	if perr != nil {
		t.Fatal(perr)
	}
	entries := readJournalFile(t, path)
	if len(entries) == 0 {
		t.Fatal("the ref-lease failure never reached TeardownSandbox: the teardown journal has no entry for it")
	}
	last := entries[len(entries)-1]
	if last.Sandbox != "pix-demo" || last.Key != key || last.Trigger != TriggerSession {
		t.Errorf("teardown journal entry = %+v, want sandbox=pix-demo key=%s trigger=%s", last, key, TriggerSession)
	}
	if last.Verdict != TeardownKeptBusy {
		t.Errorf("teardown verdict = %q, want %q (this test's holder still has refs.lock EX)", last.Verdict, TeardownKeptBusy)
	}
	if !strings.Contains(warn.String(), "kept pix-demo") {
		t.Errorf("warn output = %q, want it to report the kept teardown", warn.String())
	}
}

// killedTeardownRemovesFixture is sessionFixture's run/ls plus removableFixture's
// rm (v0.38-shaped: a bare rm refuses, -f removes), in one script: the
// failed-ref path's teardown must reach an ACTUAL removal (not merely a
// "kept-busy" verdict) for the ordering test below to exercise anything.
const killedTeardownRemovesFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	if [ -f "$d/removed" ]; then
		exit 0
	fi
	if [ -f "$d/created" ]; then
		if [ "$2" = "--json" ]; then
			echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
		else
			echo "pix-demo  img  running"
		fi
	fi
	exit 0
	;;
env)
	# the cutover create: sbx env create <effective> returns as soon as the
	# sandbox exists; the SESSION is a separate exec below.
	touch "$d/created"
	exit 0
	;;
exec)
	touch "$d/attached"` + awaitRelease + `
	exit 0
	;;
run)
	touch "$d/created"` + awaitRelease + `
	exit 0
	;;
rm)
	if [ "$2" != "-f" ]; then
		echo "fixture: sbx v0.38 refuses a bare rm with no TTY attached" >&2
		exit 3
	fi
	touch "$d/removed"
	exit 0
	;;
esac
exit 0
`

// reapCheckingSystem wraps a real sys.System and, for exactly the "sbx rm"
// call removeAndConfirm drives TeardownSandbox's actual removal through,
// records whether the killed session child had ALREADY been reaped
// (cmd.Wait returned, cmd.ProcessState set) at the moment that call ran —
// the ordering the child.Wait-after-Kill fix guarantees on the failed-ref
// path. Every other call passes straight through to the real System.
type reapCheckingSystem struct {
	sys.System
	cmd    func() *exec.Cmd
	result chan<- bool
}

func (s reapCheckingSystem) RunWithin(d time.Duration, name string, args ...string) (string, bool, error) {
	if name == "sbx" && len(args) > 0 && args[0] == "rm" {
		c := s.cmd()
		select {
		case s.result <- (c != nil && c.ProcessState != nil):
		default:
		}
	}
	return s.System.RunWithin(d, name, args...)
}

// TestKillUnreferencedAndTeardown_WaitsForReapBeforeRemoving is the
// deterministic ordering proof for RunSession's failed-ref path: it drives
// killUnreferencedAndTeardown directly — the exact function that path calls —
// against a REAL, still-running session child and a REAL recorded lease, with
// NO lock contention or timing margin anywhere: nothing else holds refs.lock,
// so TeardownSandbox's own zero-holder proof succeeds immediately and its
// removal genuinely runs, every time, not merely on a lucky race window.
// killUnreferencedAndTeardown kills the child (child.Kill, which blocks on
// its own wait channel) and calls the explicit child.Wait this fix adds
// before ever reaching TeardownSandbox; the assertion sits directly on the
// System.RunWithin call TeardownSandbox's removal is driven through, so it
// catches a REGRESSION — say, a future edit that fires the removal off a
// goroutine, or moves it before Kill — not just today's happy path. Getting
// this order backwards is the "remove-vs-dying-sbx" race: asking the sbx
// runtime to remove a sandbox while its own creator process is still, from
// the kernel's point of view, dying rather than fully reaped.
func TestKillUnreferencedAndTeardown_WaitsForReapBeforeRemoving(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, killedTeardownRemovesFixture)
	ws := t.TempDir()
	key := SessionName(ws)
	name := "pix-demo"

	var cmd *exec.Cmd
	spawn := fixtureSpawn(t)
	captureSpawn := func(argv []string) *exec.Cmd {
		cmd = spawn(argv)
		return cmd
	}

	child, err := StartSbxSession(captureSpawn([]string{"run", "--name", name}), fastPoll(), true, name)
	if err != nil {
		t.Fatalf("StartSbxSession: %v", err)
	}
	waitForFile(t, filepath.Join(fixture, "created"), 10*time.Second)
	if !child.Appeared {
		t.Fatal("the fixture session never became visible")
	}

	// Record exactly what a real create's transition would have written —
	// TeardownSandbox's ownership check (TriggerSession requires it) needs a
	// valid record, fingerprint and invocation on disk, same as production.
	fp := sandbox.Fingerprint{"static_mcp": "slack"}
	if _, rerr := RecordSessionCreation(realEnv(), key, name, fp, []string{"--model", "m"}); rerr != nil {
		t.Fatalf("RecordSessionCreation: %v", rerr)
	}

	reapedAtRemoval := make(chan bool, 1)
	env := realEnv()
	env.System = reapCheckingSystem{System: env.System, cmd: func() *exec.Cmd { return cmd }, result: reapedAtRemoval}

	var warn strings.Builder
	spec := SessionSpec{Key: key, Name: name, Creating: true, Fingerprint: fp}
	deps := SessionDeps{Env: env, Warn: &warn}

	// This is the call under test: RunSession's failed-ref branch calls
	// exactly this, with exactly this shape of argument (a child already
	// live, no reference lease held for it).
	killUnreferencedAndTeardown(spec, deps, child, fmt.Errorf("injected: refs lease acquire failed"))

	if cmd.ProcessState == nil {
		t.Fatal("the session child was never reaped")
	}
	if _, serr := os.Stat(filepath.Join(fixture, "removed")); serr != nil {
		t.Fatalf("teardown never actually removed the sandbox (fixture's removed marker is missing): %v; warn=%q", serr, warn.String())
	}
	select {
	case reapedFirst := <-reapedAtRemoval:
		if !reapedFirst {
			t.Fatal("`sbx rm` ran BEFORE the killed session child was reaped — the remove-vs-dying-sbx race")
		}
	default:
		t.Fatal("the instrumented `sbx rm` call was never observed — the removal path was not exercised")
	}
}
