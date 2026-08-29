package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

// TestPlanEnvRemove_F8EffectiveNameMustEqualRecordedInstance is F8
// (architecture.md's own table): the composed effective name must equal
// the recorded pix-* instance name BEFORE any removal argv is composed.
// Equal here succeeds and returns `env rm -f <effectivePath>`.
func TestPlanEnvRemove_F8EffectiveNameMustEqualRecordedInstance(t *testing.T) {
	const (
		effectivePath = "/state/environments/pix-work-abc12345/effective.sbxenv.yaml"
		instance      = "pix-work-abc12345"
	)
	argv, err := PlanEnvRemove(effectivePath, instance, instance)
	if err != nil {
		t.Fatalf("PlanEnvRemove: %v", err)
	}
	want := []string{"env", "rm", "-f", effectivePath}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("PlanEnvRemove = %v, want %v", argv, want)
	}
}

// TestPlanEnvRemove_RefusesNonPixScopedEffectiveName is section 10's case
// 1: an effective name outside pix-* scope is refused before any removal
// argv is issued, regardless of what the recorded instance is.
func TestPlanEnvRemove_RefusesNonPixScopedEffectiveName(t *testing.T) {
	argv, err := PlanEnvRemove("/state/environments/not-pix-scoped-env/effective.sbxenv.yaml",
		"not-pix-scoped-env", "pix-uatenv-fixture-rm-scope")
	if err == nil {
		t.Fatalf("PlanEnvRemove(non pix-* effective name) = nil error, want a refusal")
	}
	if argv != nil {
		t.Fatalf("PlanEnvRemove(non pix-* effective name) = argv %v, want no argv on refusal", argv)
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("error = %v, want it to mention the namespace scope refusal", err)
	}
}

// TestPlanEnvRemove_RefusesInstanceMismatch is section 10's case 2: a
// pix-*-scoped effective name that does not match the recorded instance is
// refused even though the scope check alone would pass.
func TestPlanEnvRemove_RefusesInstanceMismatch(t *testing.T) {
	const recorded = "pix-uatenv-fixture-rm-scope"
	mismatched := "pix-uatenv-fixture-rm-scope-mismatch"
	argv, err := PlanEnvRemove("/state/environments/"+mismatched+"/effective.sbxenv.yaml",
		mismatched, recorded)
	if err == nil {
		t.Fatalf("PlanEnvRemove(mismatched instance) = nil error, want a refusal")
	}
	if argv != nil {
		t.Fatalf("PlanEnvRemove(mismatched instance) = argv %v, want no argv on refusal", argv)
	}
	if !strings.Contains(err.Error(), recorded) || !strings.Contains(err.Error(), mismatched) {
		t.Fatalf("error = %v, want it to name both the effective name and the recorded instance", err)
	}
}

// TestPlanEnvRemove_RefusesUnsafeCharacters shares PlanRemove/
// PlanForceRemove's own unsafe-character fixtures: an environment-scoped
// planner must not open a second, looser gate an integrator could reach for
// instead.
func TestPlanEnvRemove_RefusesUnsafeCharacters(t *testing.T) {
	for _, bad := range []string{
		"pix-" + "; rm -rf /",
		"pix-foo/../bar",
		"pix-foo bar",
		"pix-$(whoami)",
	} {
		if _, err := PlanEnvRemove("/state/environments/x/effective.sbxenv.yaml", bad, bad); err == nil {
			t.Fatalf("PlanEnvRemove(%q) = nil error, want one (unsafe characters)", bad)
		}
	}
}

// TestPlanEnvRemove_RefusesEmptyEffectivePath proves a valid, matching name
// pair is still not enough: an empty effective path has nothing to name in
// `sbx env rm -f <effective>` and must not silently degrade into a
// name-based removal from inside this function (that degrade is the
// CALLER's fallback responsibility, never this planner's).
func TestPlanEnvRemove_RefusesEmptyEffectivePath(t *testing.T) {
	const instance = "pix-work-abc12345"
	if _, err := PlanEnvRemove("", instance, instance); err == nil {
		t.Fatalf("PlanEnvRemove(empty effective path) = nil error, want one")
	}
}

// TestPlanEnvRemove_NeverAppendsPruneBindings is the argv-matrix test: no
// argv PlanEnvRemove can produce, across a spread of paths and pix-*
// names, ever contains --prune-bindings (or a bare --prune) — A3's
// nonclaim about binding/MCP-registration preservation stays exactly what
// it was; this only proves the narrower, checkable half: this function
// never composes an argv that would ASK sbx to prune.
func TestPlanEnvRemove_NeverAppendsPruneBindings(t *testing.T) {
	cases := []struct {
		effectivePath string
		effectiveName string
		recorded      string
	}{
		{"/state/environments/pix-a-11111111/effective.sbxenv.yaml", "pix-a-11111111", "pix-a-11111111"},
		{"/state/environments/pix-b-22222222/effective.sbxenv.yaml", "pix-b-22222222", "pix-b-22222222"},
		{"relative/effective.sbxenv.yaml", "pix-c-33333333", "pix-c-33333333"},
		{"/state/environments/pix-work/effective.sbxenv.yaml with spaces", "pix-work", "pix-work"},
	}
	for _, c := range cases {
		argv, err := PlanEnvRemove(c.effectivePath, c.effectiveName, c.recorded)
		if err != nil {
			t.Fatalf("PlanEnvRemove(%+v): %v", c, err)
		}
		for _, a := range argv {
			if a == "--prune-bindings" || a == "--prune" || strings.Contains(a, "prune") {
				t.Fatalf("PlanEnvRemove(%+v) argv %v mentions pruning; must never plan a binding prune", c, argv)
			}
		}
	}
	// And the refusal paths, which must compose no argv (nil) at all —
	// nothing to scan for a prune flag because nothing is planned.
	refusals := []struct{ path, name, recorded string }{
		{"/x/effective.sbxenv.yaml", "not-pix-scoped", "pix-a"},
		{"/x/effective.sbxenv.yaml", "pix-a", "pix-b"},
	}
	for _, r := range refusals {
		if argv, err := PlanEnvRemove(r.path, r.name, r.recorded); err == nil {
			t.Fatalf("PlanEnvRemove(%+v) unexpectedly succeeded with argv %v", r, argv)
		}
	}
}

// TestPlanEnvRemove_SharesScopeCheckWithPlanRemove proves PlanEnvRemove
// cannot diverge from PlanRemove/PlanForceRemove on WHAT counts as a valid
// pix-* name: every name-safety refusal fixture the two existing planners
// already share is refused here too, when recordedInstanceName equals the
// candidate name (isolating the name-safety check from the equality
// check this function adds on top of it).
func TestPlanEnvRemove_SharesScopeCheckWithPlanRemove(t *testing.T) {
	cases := []string{
		"",
		"some-other-box",
		"pix-" + "; rm -rf /",
		"pix-foo/../bar",
		"pix-foo bar",
		"pix-$(whoami)",
		"pix-" + strings.Repeat("a", MaxNameLen),
		"pix-demo",
	}
	for _, name := range cases {
		_, wantErr := PlanRemove(name)
		_, gotErr := PlanEnvRemove("/state/environments/x/effective.sbxenv.yaml", name, name)
		if (wantErr == nil) != (gotErr == nil) {
			t.Errorf("PlanRemove(%q) err=%v, PlanEnvRemove(%q, recorded=same) err=%v — the two planners disagree on name safety/scope", name, wantErr, name, gotErr)
		}
	}
}
