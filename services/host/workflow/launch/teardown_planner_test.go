//go:build unix

// teardown_planner_test.go — E2.4 BLOCK fix: TeardownOptions.Planner is the
// seam that lets environment-scoped removal argv (E2.5's `sbx env rm -f
// <effective>`, composed by PlanEnvRemoveSeam) actually reach env.RunWithin,
// and these tests prove it can do that ONLY after this file's own
// zero-holder/keep/instance-id/fresh-probe proof chain — never instead of
// it, and never before it.
package launch

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/sandbox"
)

// envRemovableFixture models an sbx that lists one running, schema-verified
// sandbox until `env rm -f <path>` removes it — the E2.5 shape, alongside
// removableFixture's `rm -f <name>` shape, both fixtures a real
// TeardownOptions.Planner must be able to drive through the SAME proof
// chain.
const envRemovableFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	[ -f "$d/removed" ] && exit 0
	if [ "$2" = "--json" ]; then
		echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
	else
		echo "pix-demo  img  running"
	fi
	exit 0
	;;
env)
	if [ "$2" = "rm" ] && [ "$3" = "-f" ]; then
		touch "$d/removed"
		exit 0
	fi
	echo "fixture: unexpected env argv: $@" >&2
	exit 3
	;;
esac
exit 0
`

// assertNoArgvReachedSbx fails if the fixture logged ANY call at all — the
// assertion the proof-failure test needs: not merely "no rm", but that
// execution never got anywhere near sbx (a failed proof stops BEFORE the
// fresh-probe `ls` this file's own proof chain would otherwise run).
func assertNoArgvReachedSbx(t *testing.T, fixture string) {
	t.Helper()
	if lines := sbxArgv(t, fixture); len(lines) != 0 {
		t.Fatalf("execution reached sbx despite no authorization: %v", lines)
	}
}

// assertNoRemovalReachedSbx tolerates the fresh-probe `ls` this file's own
// proof chain runs BEFORE the planner is ever consulted, but fails if any
// `rm`/`env` removal call reached sbx — the assertion a planner REFUSAL
// needs: the proof chain may look, but a refused plan must never execute.
func assertNoRemovalReachedSbx(t *testing.T, fixture string) {
	t.Helper()
	for _, l := range sbxArgv(t, fixture) {
		if strings.HasPrefix(l, "rm") || strings.HasPrefix(l, "env") {
			t.Fatalf("a refused plan still reached a removal call: %q", l)
		}
	}
}

// TestTeardown_PlannerNeverCalledWhenProofFails is the load-bearing new
// property: a live reference held by ANOTHER process fails the zero-holder
// proof (kept-busy), and neither the injected planner nor sbx itself is ever
// called — proving planning is gated behind the SAME proof as execution, not
// merely execution alone.
func TestTeardown_PlannerNeverCalledWhenProofFails(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	_, stdin := startRefHolder(t, dir)

	opts := fastTeardown(t)
	var calls int
	opts.Planner = func(name string) (EnvRemovalPlan, error) {
		calls++
		return EnvRemovalPlan{}, fmt.Errorf("must never be called while a reference is held")
	}

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownKeptBusy {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptBusy)
	}
	if calls != 0 {
		t.Errorf("planner called %d times while the zero-holder proof failed, want 0", calls)
	}
	assertNoArgvReachedSbx(t, fixture)

	fmt.Fprintln(stdin, "release")
}

// TestTeardown_ExecutesInjectedPlannerArgvAfterProofs is E2.5's shape: a
// planner built from sandbox.PlanEnvRemove (via PlanEnvRemoveSeam) composes
// `env rm -f <effectivePath>`, and once every proof below passes, that EXACT
// argv — not a re-derived one — is what reaches env.RunWithin.
func TestTeardown_ExecutesInjectedPlannerArgvAfterProofs(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, envRemovableFixture)
	key := "pix-demo"
	seedRecordedSession(t, key, "inst-1")
	effectivePath := writeEffectiveFixture(t, t.TempDir(), "pix-demo")

	opts := fastTeardown(t)
	var calls int
	opts.Planner = func(name string) (EnvRemovalPlan, error) {
		calls++
		return PlanEnvRemoveSeam(effectivePath, name, true)
	}

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownRemoved {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownRemoved)
	}
	if calls != 1 {
		t.Errorf("planner called %d times, want exactly 1", calls)
	}

	want := "env rm -f " + effectivePath
	var sawEnvRm bool
	for _, l := range argvLines(t, fixture) {
		if strings.HasPrefix(l, "env rm") {
			sawEnvRm = true
			if l != want {
				t.Errorf("argv = %q, want exactly %q", l, want)
			}
		}
		if strings.HasPrefix(l, "rm ") {
			t.Errorf("a generic `rm` also reached sbx (%q); the environment-scoped argv must be the ONLY removal call", l)
		}
	}
	if !sawEnvRm {
		t.Fatalf("no `env rm` reached sbx; argv was %v", argvLines(t, fixture))
	}
}

// TestTeardown_PlannerRefusalKeepsUnownedWithoutExecuting proves the
// mismatch/non-pix-scoped refusals PlanEnvRemoveSeam already enforces
// propagate through TeardownOptions.Planner as a kept-unowned verdict, with
// NOTHING executed — the refusal, not a caller, decides.
func TestTeardown_PlannerRefusalKeepsUnownedWithoutExecuting(t *testing.T) {
	t.Run("instance-mismatch", func(t *testing.T) {
		isolateState(t)
		fixture := installFakeSbx(t, envRemovableFixture)
		key := "pix-demo"
		seedRecordedSession(t, key, "inst-1")
		// The effective file names a DIFFERENT instance than the one this
		// teardown is running against.
		effectivePath := writeEffectiveFixture(t, t.TempDir(), "pix-mismatched-instance")

		opts := fastTeardown(t)
		opts.Planner = func(name string) (EnvRemovalPlan, error) {
			return PlanEnvRemoveSeam(effectivePath, name, true)
		}

		res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
		if res.Verdict != TeardownKeptUnowned {
			t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptUnowned)
		}
		if !strings.Contains(res.Detail, "does not match recorded instance") {
			t.Errorf("detail = %q, want it to name the mismatch", res.Detail)
		}
		assertNoRemovalReachedSbx(t, fixture)
	})

	t.Run("non-pix-scoped-effective-name", func(t *testing.T) {
		isolateState(t)
		fixture := installFakeSbx(t, envRemovableFixture)
		key := "pix-demo"
		seedRecordedSession(t, key, "inst-1")
		effectivePath := writeEffectiveFixture(t, t.TempDir(), "not-pix-scoped")

		opts := fastTeardown(t)
		opts.Planner = func(name string) (EnvRemovalPlan, error) {
			return PlanEnvRemoveSeam(effectivePath, name, true)
		}

		res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
		if res.Verdict != TeardownKeptUnowned {
			t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownKeptUnowned)
		}
		assertNoRemovalReachedSbx(t, fixture)
	})
}

// TestTeardown_FallbackPlanExecutesGenericArgvAndReportsCleanupSkipped is
// §10.3's fallback: no effective file at the recorded path, so
// PlanEnvRemoveSeam falls back to the ordinary name-based argv, which still
// executes exactly like the pre-E2.5 default — but the result's Detail
// explicitly says environment-scoped secret cleanup could not run, per
// docs/design/environments.md §10.3 ("it never guesses or prunes shared
// state").
func TestTeardown_FallbackPlanExecutesGenericArgvAndReportsCleanupSkipped(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	seedRecordedSession(t, key, "inst-1")
	missingEffective := filepath.Join(t.TempDir(), "effective.sbxenv.yaml")

	opts := fastTeardown(t)
	opts.Planner = func(name string) (EnvRemovalPlan, error) {
		return PlanEnvRemoveSeam(missingEffective, name, true)
	}

	res := TeardownSandbox(realEnv(), key, "pix-demo", TriggerSession, opts)
	if res.Verdict != TeardownRemoved {
		t.Fatalf("verdict = %q (%s), want %q", res.Verdict, res.Detail, TeardownRemoved)
	}
	assertSawRmArgv(t, fixture, "rm -f pix-demo")
	if !strings.Contains(res.Detail, "environment-scoped secret cleanup could not run") {
		t.Errorf("detail = %q, want the fallback cleanup-not-run report", res.Detail)
	}
	if !strings.Contains(res.Detail, "pix-demo") {
		t.Errorf("detail = %q, want it to name the instance falling back", res.Detail)
	}
	for _, l := range argvLines(t, fixture) {
		if strings.Contains(l, "prune") {
			t.Fatalf("argv %q mentions pruning; the fallback must never prune", l)
		}
	}
}

// TestTeardownOptions_DefaultPlannerIsByteCompatible pins the non-negotiable
// regression bar: with no Planner set, withDefaults produces the EXACT
// pre-E2.5 argv shape (sandbox.PlanForceRemove's own), and no Report.
func TestTeardownOptions_DefaultPlannerIsByteCompatible(t *testing.T) {
	o := TeardownOptions{}.withDefaults()
	got, err := o.Planner("pix-demo")
	if err != nil {
		t.Fatalf("default planner: %v", err)
	}
	want, werr := sandbox.PlanForceRemove("pix-demo")
	if werr != nil {
		t.Fatalf("sandbox.PlanForceRemove: %v", werr)
	}
	if len(got.Argv) != len(want) {
		t.Fatalf("Argv = %v, want %v", got.Argv, want)
	}
	for i := range want {
		if got.Argv[i] != want[i] {
			t.Fatalf("Argv = %v, want %v", got.Argv, want)
		}
	}
	if got.Report != "" {
		t.Errorf("Report = %q, want empty on the default byte-compatible path", got.Report)
	}
}
