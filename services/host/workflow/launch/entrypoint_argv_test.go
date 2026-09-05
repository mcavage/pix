package launch

import (
	"strings"
	"testing"

	"pix/host/sandbox"
)

// TestNameForIsDeterministicPerWorkspaceAndEnv proves two environments on
// one workspace are two sandboxes, and that naming is stable across
// spellings.
func TestNameForIsDeterministicPerWorkspaceAndEnv(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())
	a, err := sandbox.NameFor("/home/u/proj", "work")
	if err != nil {
		t.Fatalf("NameFor: %v", err)
	}
	b, err := sandbox.NameFor("/home/u/./proj", "work")
	if err != nil {
		t.Fatalf("NameFor: %v", err)
	}
	c, err := sandbox.NameFor("/home/u/proj", "home")
	if err != nil {
		t.Fatalf("NameFor: %v", err)
	}
	if a != b {
		t.Fatalf("the same workspace+env must derive one name: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("two environments on one workspace must not share a sandbox: %s", a)
	}
	if !strings.HasPrefix(a, "pix-") {
		t.Fatalf("derived names stay pix-* scoped, got %s", a)
	}
}
