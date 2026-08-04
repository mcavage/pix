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
