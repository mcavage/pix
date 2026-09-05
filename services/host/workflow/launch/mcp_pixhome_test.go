package launch

import "testing"

// The fingerprint resolver must key the value the launch actually used. If
// it fell through to the host environment while the argv expansion used
// Pix's own home, an unchanged environment would fingerprint differently on
// a host that exported nothing.
func TestResolveInterpolation_PixHomeMatchesTheArgvExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)

	got := resolveInterpolation(func(string) (string, bool) { return "", false }, "PIX_HOME", nil)
	if got != home {
		t.Fatalf("resolveInterpolation(PIX_HOME) = %q, want %q", got, home)
	}
}
