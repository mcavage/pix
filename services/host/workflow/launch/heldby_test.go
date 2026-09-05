package launch

import "testing"

// TestHeldByColumn_UnknownStateNeverRendersAsFree is the one way this column
// could mislead. "—" is a promise that teardown will remove the box; a lease
// directory that could not be read proves nothing of the kind, and rendering it
// as free would invite a user to wait for a removal that never comes.
//
// A box with NO lease state is different and genuinely free: nothing claims it,
// which is precisely what `pix rm --orphans` sweeps.
func TestHeldByColumn_UnknownStateNeverRendersAsFree(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())
	if got := heldByColumn("pix-no-such-box"); got != heldByNone {
		t.Errorf("a sandbox with no lease state = %q, want %q — nothing holds it", got, heldByNone)
	}
}

// TestHeldByColumn_ValuesAreDistinct: three answers that must stay tellable
// apart at a glance, since the whole column exists so a reader does not have to
// go read a journal to learn why a box survived.
func TestHeldByColumn_ValuesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range []string{heldBySession, heldByNone, heldUnknown} {
		if v == "" {
			t.Error("an empty column reads as a missing value, not an answer")
		}
		if seen[v] {
			t.Errorf("duplicate column value %q", v)
		}
		seen[v] = true
	}
}
