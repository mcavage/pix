//go:build unix

package launch

// TestStartSbxSession_SettlesAfterFastExitBeforeFirstAppearance and
// TestStartSbxSession_NeverAppearsSettlesBounded are the fix for a fast,
// successful `sbx run`: the child can exit before StartSbxSession's own
// first tick, and sbx v0.38 may not have published the now-stopped sandbox
// into `sbx ls` at that exact instant. The old single immediate probe taken
// at exit saw nothing, so Appeared stayed false and a caller following
// startSessionTransition's "spec.Creating && child.Appeared" gate never
// recorded or kept the lease — the sandbox it just created got torn down
// out from under it. settleAfterExit (run.go) keeps polling on the SAME
// fixture for a short, bounded window instead of trusting that one probe.
//
// Both tests drive a REAL fixture `sbx` and a REAL child process (the same
// style as session_process_test.go), because the race is about ORDERING
// between a process actually exiting and a probe actually reporting —
// nothing a mocked Probe function can misrepresent by accident the way a
// hand-rolled state machine could.

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"pix/host/lease"
	"pix/host/sandbox"
)

// settleFixture: `run` exits IMMEDIATELY (the fast, successful create this
// bug is about — no barrier file, no held-open session), and `ls` reports
// the sandbox present only from its settleAt'th invocation on, so the first
// several probes genuinely see nothing. settleAt is substituted per test.
const settleFixtureTemplate = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	n=0
	if [ -f "$d/ls_count" ]; then n=$(cat "$d/ls_count"); fi
	n=$((n+1))
	echo "$n" > "$d/ls_count"
	if [ "$n" -ge %d ]; then
		if [ "$2" = "--json" ]; then
			echo '[{"name":"pix-demo","state":"stopped","instance_id":"inst-1"}]'
		else
			echo "pix-demo  img  stopped"
		fi
	fi
	exit 0
	;;
run)
	touch "$d/created"
	exit 0
	;;
esac
exit 0
`

// TestStartSbxSession_SettlesAfterFastExitBeforeFirstAppearance is the
// deterministic proof that a receipt delayed a few polls behind a fast exit
// is not lost: the child is gone (child.Wait already drained, exitErr nil)
// long before `ls` starts reporting it, and StartSbxSession must still come
// back with Appeared=true once the settle poll catches up — exactly what
// unblocks RecordSessionCreation + setSessionKeep the way
// startSessionTransition drives them.
func TestStartSbxSession_SettlesAfterFastExitBeforeFirstAppearance(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sprintfSettleFixture(4))
	name := "pix-demo"

	poll := CreatePoll{
		Probe:          func(n string) SbxState { return ProbeTaskSandbox(realEnv(), n) },
		Interval:       10 * time.Millisecond,
		Timeout:        30 * time.Second,
		PostExitSettle: 2 * time.Second, // bounded, nowhere near the 15m default
	}

	start := time.Now()
	child, err := StartSbxSession(fixtureSpawn(t)([]string{"run", "--name", name}), poll, true, name)
	if err != nil {
		t.Fatalf("StartSbxSession: %v", err)
	}
	elapsed := time.Since(start)

	waitForFile(t, filepath.Join(fixture, "created"), 2*time.Second)

	if werr := child.Wait(); werr != nil {
		t.Fatalf("child exit result not preserved: Wait() = %v, want nil", werr)
	}
	if !child.Appeared {
		t.Fatal("Appeared = false, want true: the settle poll must catch the delayed receipt")
	}
	if elapsed >= 2*time.Second {
		t.Errorf("settle took %s, expected to resolve well inside the 2s budget once ls catches up", elapsed)
	}

	// Prove the thing this bug actually breaks: with Appeared true, the
	// record+keep write that startSessionTransition gates on it succeeds.
	key := SessionName(t.TempDir())
	fp := sandbox.Fingerprint{"static_mcp": "slack"}
	recorded, rerr := RecordSessionCreation(realEnv(), key, name, fp, []string{"--model", "m"})
	if rerr != nil {
		t.Fatalf("RecordSessionCreation: %v", rerr)
	}
	if !recorded {
		t.Fatal("RecordSessionCreation did not record, want a written lease")
	}
	if err := setSessionKeep(key); err != nil {
		t.Fatalf("setSessionKeep: %v", err)
	}
	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatalf("leaseDirFor: %v", err)
	}
	if _, kept, kerr := lease.ReadKeep(dir); kerr != nil || !kept {
		t.Fatalf("ReadKeep = (kept=%v, err=%v), want a keep record on disk", kept, kerr)
	}
}

// TestStartSbxSession_NeverAppearsSettlesBounded is the other half: a
// sandbox that genuinely never shows up (ls never reports it, ever) must not
// hang StartSbxSession for anywhere near the full 15-minute create Timeout —
// the settle window is its own short, separate bound.
func TestStartSbxSession_NeverAppearsSettlesBounded(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, sprintfSettleFixture(1<<30)) // ls never crosses the threshold
	name := "pix-demo"

	poll := CreatePoll{
		Probe:          func(n string) SbxState { return ProbeTaskSandbox(realEnv(), n) },
		Interval:       10 * time.Millisecond,
		Timeout:        30 * time.Second,
		PostExitSettle: 200 * time.Millisecond,
	}

	start := time.Now()
	child, err := StartSbxSession(fixtureSpawn(t)([]string{"run", "--name", name}), poll, true, name)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("StartSbxSession: %v", err)
	}
	if child.Appeared {
		t.Fatal("Appeared = true, want false: ls never reported this sandbox")
	}
	if werr := child.Wait(); werr != nil {
		t.Fatalf("child exit result not preserved: Wait() = %v, want nil", werr)
	}
	if elapsed < poll.PostExitSettle {
		t.Errorf("returned after %s, before the settle budget (%s) even elapsed", elapsed, poll.PostExitSettle)
	}
	if elapsed > 5*time.Second {
		t.Errorf("returned after %s — the never-appears case must be bounded by PostExitSettle, not the 15m create Timeout", elapsed)
	}
}

func sprintfSettleFixture(settleAt int) string {
	return fmt.Sprintf(settleFixtureTemplate, settleAt)
}
