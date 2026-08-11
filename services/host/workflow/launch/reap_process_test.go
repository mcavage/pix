//go:build unix

package launch

// U04d's teardown is a claim about ORDER ACROSS PROCESSES — "the LAST shell
// out removes the sandbox, and only the last" — so these tests use REAL OS
// processes holding REAL flocks, plus the real `sbx` fixture script, exactly
// like session_process_test.go. Two properties cannot be observed any other
// way:
//
//   - a session that ends while ANOTHER PROCESS still holds a reference must
//     leave the sandbox alone (kept-busy) — and the proof must be
//     NON-BLOCKING, so the exiting shell returns promptly rather than waiting
//     out the other shell's session;
//   - the SAME session, once that other process is gone, removes the sandbox
//     with a non-force `sbx rm` and clears its state.
//
// The first also proves the ordering inside RunSession: the exiting shell drops
// its OWN refs SHARED lock before attempting the proof. If it did not, the
// single-shell case below would report kept-busy against itself, every time.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/lease"
	"pix/host/sandbox"
)

// teardownFixture is the session fixture with a REMOVABLE sandbox: `run`
// creates and keeps the session alive until released, `ls` reports it until
// `rm -f` removes it. It models sbx v0.38: a BARE `rm` (no `-f`) fails loudly
// here — standing in for sbx's own non-interactive confirmation refusal — so
// any teardown path that ever plans a bare rm instead of routing through
// sandbox.PlanForceRemove fails these tests instead of passing quietly.
const teardownFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	if [ -f "$d/created" ] && [ ! -f "$d/removed" ]; then
		if [ "$2" = "--json" ]; then
			echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
		else
			echo "pix-demo  img  running"
		fi
	elif [ "$2" = "--json" ]; then
		echo '[]'
	fi
	exit 0
	;;
run)
	touch "$d/created"
	while [ ! -f "$d/release" ]; do sleep 0.02; done
	exit 0
	;;
rm)
	if [ "$2" != "-f" ]; then
		echo "fixture: sbx v0.38 refuses a bare rm with no TTY attached; pass --force to skip confirmation" >&2
		exit 3
	fi
	touch "$d/removed"
	exit 0
	;;
esac
exit 0
`

const reapHelperEnv = "LAUNCH_REAP_HELPER"

// TestReapHolderHelperProcess is the re-exec'd second shell: it takes a REAL
// refs SHARED reference through the production helper (AttachRefUnderLifecycle,
// same call an attach makes), announces it, and holds it until stdin says
// otherwise. A no-op under a normal `go test` run.
func TestReapHolderHelperProcess(t *testing.T) {
	if os.Getenv(reapHelperEnv) != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rl, err := lease.AttachRefUnderLifecycle(ctx, os.Getenv("HELPER_LEASE_DIR"), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper AttachRef: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ACQUIRED")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if line == "release\n" {
		rl.Close()
		os.Exit(0)
	}
	select {} // wait to be killed, so the KERNEL is what releases the hold
}

// startRefHolder starts the helper and returns once it has really acquired the
// refs SHARED lock, plus the pipe that releases it.
func startRefHolder(t *testing.T, leaseDir string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestReapHolderHelperProcess")
	cmd.Env = append(os.Environ(), reapHelperEnv+"=1", "HELPER_LEASE_DIR="+leaseDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the ref holder: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ACQUIRED" {
		t.Fatalf("ref holder did not acquire: %q (%v)", line, err)
	}
	return cmd, stdin
}

// TestRunSession_LastShellOut_KeptWhileAnotherProcessHoldsARef: the session
// exits while a SECOND PROCESS holds a reference. The sandbox survives, the
// verdict is kept-busy, the state is intact, and the exiting shell does not
// block waiting for the other shell.
func TestRunSession_LastShellOut_KeptWhileAnotherProcessHoldsARef(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, teardownFixture)
	ws := t.TempDir()
	key := "pix-demo"
	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	_, stdin := startRefHolder(t, leaseDir)

	opts := fastTeardown(t)
	var warn strings.Builder
	// The fixture session exits as soon as it is released; release first so
	// RunSession's Wait returns promptly and we time the TEARDOWN, not the
	// session.
	release(t, fixture)
	start := time.Now()
	if err := runFixtureSession(t, key, ws, &warn, opts); err != nil {
		t.Fatalf("RunSession: %v (warn %q)", err, warn.String())
	}
	elapsed := time.Since(start)

	if _, err := os.Stat(filepath.Join(fixture, "removed")); err == nil {
		t.Fatal("the sandbox was removed while another process still held a reference")
	}
	if entry := lastVerdict(t, opts); entry.Verdict != TeardownKeptBusy {
		t.Errorf("journal verdict = %q (%s), want %q", entry.Verdict, entry.Detail, TeardownKeptBusy)
	}
	if !strings.Contains(warn.String(), "kept pix-demo") {
		t.Errorf("warn stream = %q, want it to say the sandbox was kept", warn.String())
	}
	if _, err := lease.ReadRecord(leaseDir); err != nil {
		t.Errorf("state must survive a busy verdict: %v", err)
	}
	// The proof is NON-BLOCKING: the exiting shell may not wait out the other
	// shell's reference. Generous, because this asserts "not a lock wait" (the
	// SessionLockTimeout is 30 s), not a performance number.
	if elapsed > 15*time.Second {
		t.Errorf("teardown took %s; the zero-holder proof must be non-blocking", elapsed)
	}

	// Now the other shell leaves: the same teardown, now genuinely last out,
	// removes the sandbox with a PLAIN rm and clears the state.
	fmt.Fprintln(stdin, "release")
	deadline := time.Now().Add(10 * time.Second)
	var res TeardownResult
	for {
		res = TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
		if res.Verdict != TeardownKeptBusy || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if res.Verdict != TeardownRemoved {
		t.Fatalf("after the last reference left, verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownRemoved)
	}
	// The wire argv carries `-f` (sbx v0.38's confirmation skip, planned via
	// sandbox.PlanForceRemove), but pix's OWN authorization for reaching this
	// argv at all is exactly the zero-holder proof this test just exercised —
	// the reaper never had, and never needed, pix's own --force bypass.
	var sawRm bool
	for _, l := range sbxArgv(t, fixture) {
		if strings.HasPrefix(l, "rm") {
			sawRm = true
			if l != "rm -f pix-demo" {
				t.Fatalf("removal argv = %q, want exactly %q", l, "rm -f pix-demo")
			}
		}
	}
	if !sawRm {
		t.Fatal("no rm reached sbx after the last reference left")
	}
	assertLeaseStateCleared(t, leaseDir)
}

// TestRunSession_LastShellOut_RemovesWhenAlone: the single-shell case, which is
// the ordering proof — RunSession closes its OWN refs SHARED lock before
// attempting the proof, so the very process that held the reference is the one
// that reaps. A teardown attempted before that Close would be kept-busy
// against itself.
func TestRunSession_LastShellOut_RemovesWhenAlone(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, teardownFixture)
	ws := t.TempDir()
	key := "pix-demo"
	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	opts := fastTeardown(t)
	var warn strings.Builder
	release(t, fixture)
	if err := runFixtureSession(t, key, ws, &warn, opts); err != nil {
		t.Fatalf("RunSession: %v (warn %q)", err, warn.String())
	}
	if _, err := os.Stat(filepath.Join(fixture, "removed")); err != nil {
		t.Fatalf("the last shell out did not remove the sandbox: %v (warn %q)", err, warn.String())
	}
	if entry := lastVerdict(t, opts); entry.Verdict != TeardownRemoved {
		t.Errorf("journal verdict = %q (%s), want %q", entry.Verdict, entry.Detail, TeardownRemoved)
	}
	assertLeaseStateCleared(t, leaseDir)
	if warn.Len() != 0 {
		t.Errorf("warn stream = %q, routine removal must stay silent", warn.String())
	}
}

// TestRunSession_LastShellOut_KeepSurvives: `pix run -k` is no longer inert —
// the keep it binds is what stops this teardown, and the sandbox is still there
// after the session exits.
func TestRunSession_LastShellOut_KeepSurvives(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, teardownFixture)
	ws := t.TempDir()
	key := "pix-demo"
	opts := fastTeardown(t)
	var warn strings.Builder
	release(t, fixture)
	if err := runFixtureSessionKeep(t, key, ws, &warn, opts); err != nil {
		t.Fatalf("RunSession: %v (warn %q)", err, warn.String())
	}
	if _, err := os.Stat(filepath.Join(fixture, "removed")); err == nil {
		t.Fatal("-k/--keep did not stop the last-shell teardown")
	}
	if entry := lastVerdict(t, opts); entry.Verdict != TeardownKeptKeep {
		t.Errorf("journal verdict = %q (%s), want %q", entry.Verdict, entry.Detail, TeardownKeptKeep)
	}
}

// runFixtureSession / runFixtureSessionKeep run a real create session against
// the fixture sbx, with the production ordering (RunSession) and the test's
// teardown bounds.
func runFixtureSession(t *testing.T, key, ws string, warn io.Writer, opts TeardownOptions) error {
	return runFixtureSessionSpec(t, key, ws, warn, opts, false)
}

func runFixtureSessionKeep(t *testing.T, key, ws string, warn io.Writer, opts TeardownOptions) error {
	return runFixtureSessionSpec(t, key, ws, warn, opts, true)
}

func runFixtureSessionSpec(t *testing.T, key, ws string, warn io.Writer, opts TeardownOptions, keep bool) error {
	t.Helper()
	return RunSession(SessionSpec{
		Key: key, Name: "pix-demo", Creating: true, Keep: keep,
		CreateArgs:  []string{"run", "--name", "pix-demo"},
		Fingerprint: sandbox.Fingerprint{"static_mcp": "slack"},
		Invocation:  []string{"--model", "m"},
	}, SessionDeps{
		Env: realEnv(), Poll: fastPoll(), Warn: warn,
		Spawn:    fixtureSpawn(t),
		Teardown: opts,
	})
}
