package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanRemove_ValidPixName(t *testing.T) {
	argv, err := PlanRemove("pix-demo")
	if err != nil {
		t.Fatalf("PlanRemove: %v", err)
	}
	if want := []string{"rm", "pix-demo"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("PlanRemove = %v, want %v", argv, want)
	}
	for _, a := range argv {
		if a == "-f" || a == "--force" {
			t.Fatalf("PlanRemove argv %v contains a force flag; PlanRemove must never plan a force remove", argv)
		}
	}
}

func TestPlanRemove_RefusesOutOfScopeName(t *testing.T) {
	_, err := PlanRemove("some-other-box")
	if err == nil {
		t.Fatalf("PlanRemove(non pix-* name) = nil error, want one")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("error = %v, want it to mention the namespace scope refusal", err)
	}
}

func TestPlanRemove_RefusesEmptyName(t *testing.T) {
	if _, err := PlanRemove(""); err == nil {
		t.Fatalf("PlanRemove(\"\") = nil error, want one")
	}
}

func TestPlanRemove_RefusesUnsafeCharacters(t *testing.T) {
	for _, bad := range []string{
		"pix-" + "; rm -rf /",
		"pix-foo/../bar",
		"pix-foo bar",
		"pix-$(whoami)",
	} {
		if _, err := PlanRemove(bad); err == nil {
			t.Fatalf("PlanRemove(%q) = nil error, want one (unsafe characters)", bad)
		}
	}
}

func TestPlanRemove_RefusesOverlongName(t *testing.T) {
	long := "pix-" + strings.Repeat("a", MaxNameLen)
	if _, err := PlanRemove(long); err == nil {
		t.Fatalf("PlanRemove(overlong name) = nil error, want one")
	}
}

func TestPlanForceRemove_ValidPixName(t *testing.T) {
	argv, err := PlanForceRemove("pix-demo")
	if err != nil {
		t.Fatalf("PlanForceRemove: %v", err)
	}
	if want := []string{"rm", "-f", "pix-demo"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("PlanForceRemove = %v, want %v", argv, want)
	}
}

// TestPlanForceRemove_SharesExactScopeWithPlanRemove proves the two planners
// cannot diverge on WHAT they will remove — only on whether they pass `-f` —
// by running every PlanRemove refusal fixture through PlanForceRemove too.
func TestPlanForceRemove_SharesExactScopeWithPlanRemove(t *testing.T) {
	cases := []string{
		"",
		"some-other-box",
		"pix-" + "; rm -rf /",
		"pix-foo/../bar",
		"pix-foo bar",
		"pix-$(whoami)",
		"pix-" + strings.Repeat("a", MaxNameLen),
	}
	for _, name := range cases {
		_, wantErr := PlanRemove(name)
		_, gotErr := PlanForceRemove(name)
		if (wantErr == nil) != (gotErr == nil) {
			t.Errorf("PlanRemove(%q) err=%v, PlanForceRemove(%q) err=%v — the two planners disagree on scope", name, wantErr, name, gotErr)
		}
	}
}

func TestPlanForceRemove_RefusesOutOfScopeName(t *testing.T) {
	_, err := PlanForceRemove("some-other-box")
	if err == nil {
		t.Fatalf("PlanForceRemove(non pix-* name) = nil error, want one")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("error = %v, want it to mention the namespace scope refusal", err)
	}
}

func TestPlanForceRemove_RefusesUnsafeCharacters(t *testing.T) {
	for _, bad := range []string{
		"pix-" + "; rm -rf /",
		"pix-foo/../bar",
		"pix-foo bar",
		"pix-$(whoami)",
	} {
		if _, err := PlanForceRemove(bad); err == nil {
			t.Fatalf("PlanForceRemove(%q) = nil error, want one (unsafe characters)", bad)
		}
	}
}
