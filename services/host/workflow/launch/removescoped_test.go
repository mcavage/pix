package launch

import (
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// removescoped_test.go pins the coexistence half of the ONE forced removal
// seam. sandbox.PlanForceRemove proves a name is pix-owned and argv-safe; it
// cannot prove the name belongs to THIS stack, because it has no stack id to
// compare against. Every caller that reaches the forced seam from a RECORDED
// name (a task's metadata, written by whichever PIX_HOME created it) must
// therefore make that comparison first, or one home's `pix task rm` can
// force-remove another home's sandbox.

const scopedTestStackID = "0123456789abcdef"

// TestRemoveScopedPixSandbox_RefusesAForeignStacksName is the finding: a
// pix-* name scoped to a DIFFERENT stack is refused, and nothing is executed.
func TestRemoveScopedPixSandbox_RefusesAForeignStacksName(t *testing.T) {
	fake := &systest.Fake{RunFn: func(name string, args ...string) (string, error) { return "", nil }}
	env := hostenv.Env{System: fake}
	foreign := "pix-fedcba9876543210-taskbox"
	err := RemoveScopedPixSandbox(env, scopedTestStackID, foreign)
	if err == nil {
		t.Fatal("a foreign stack's sandbox must never be force-removed")
	}
	if !strings.Contains(err.Error(), foreign) {
		t.Errorf("the refusal must name the sandbox, got: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("nothing may be executed for a refused name, got: %v", fake.Calls)
	}
}

// TestRemoveScopedPixSandbox_RefusesAnUnscopedLegacyName: a pre-scoping
// "pix-<basename>-<digest>" name carries no stack id at all, so no home can
// claim it. Refuse rather than adopt it.
func TestRemoveScopedPixSandbox_RefusesAnUnscopedLegacyName(t *testing.T) {
	fake := &systest.Fake{RunFn: func(name string, args ...string) (string, error) { return "", nil }}
	err := RemoveScopedPixSandbox(hostenv.Env{System: fake}, scopedTestStackID, "pix-taskbox-deadbeef")
	if err == nil {
		t.Fatal("an unscoped legacy name must not be force-removed by this stack")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("nothing may be executed for a refused name, got: %v", fake.Calls)
	}
}

// TestRemoveScopedPixSandbox_RemovesThisStacksOwnSandbox: the scope check is
// a filter, not a wall. This stack's own name still reaches the exact forced
// argv sandbox.PlanForceRemove composes.
func TestRemoveScopedPixSandbox_RemovesThisStacksOwnSandbox(t *testing.T) {
	fake := &systest.Fake{RunFn: func(name string, args ...string) (string, error) { return "", nil }}
	mine := "pix-" + scopedTestStackID + "-taskbox"
	if err := RemoveScopedPixSandbox(hostenv.Env{System: fake}, scopedTestStackID, mine); err != nil {
		t.Fatalf("RemoveScopedPixSandbox: %v", err)
	}
	if want := "sbx rm -f " + mine; !fake.Ran(want) {
		t.Errorf("Calls = %v, want %q", fake.Calls, want)
	}
}

// TestRemoveScopedPixSandbox_RefusesAMalformedStackID: an unusable id can
// never authorize a removal, since IsScopedSandboxName cannot compose a
// prefix from it.
func TestRemoveScopedPixSandbox_RefusesAMalformedStackID(t *testing.T) {
	fake := &systest.Fake{RunFn: func(name string, args ...string) (string, error) { return "", nil }}
	if err := RemoveScopedPixSandbox(hostenv.Env{System: fake}, "not-an-id", "pix-anything"); err == nil {
		t.Fatal("a malformed stack id must refuse")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("nothing may be executed, got: %v", fake.Calls)
	}
}
