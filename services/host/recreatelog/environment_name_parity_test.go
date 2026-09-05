//go:build unix

package recreatelog

import (
	"strings"
	"testing"

	"pix/host/config"
)

// TestEnvironmentNameShapeMatchesConfig pins THIS package's envNameRE
// (validateEnvironment, used by Append) and config.AddEnvironment's
// validEnvironmentName to the SAME safe shape: start alnum, then alnum plus
// '.', '_', '-', capped at 128 bytes.
//
// config is L0 (foundation) and recreatelog is L1 (capability); an L0
// package may never import a capability (see ../arch_test.go), so config
// cannot import this package's regex to share it, and this file duplicates
// the pattern in config/environment.go rather than the reverse. But L1 MAY
// legally import L0 (down-only imports are the whole rule), so THIS package
// is where the two definitions can be compared directly instead of trusting
// two independently hand-written regexes to stay byte-for-byte identical
// forever. A name that this test does not cover diverging silently is
// exactly the class of bug a hand exact copy invites.
func TestEnvironmentNameShapeMatchesConfig(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"environments", true},
		{"home", true},
		{"a", true},
		{"A9", true},
		{"env.name-1_2", true},
		{strings.Repeat("a", 128), true},
		{"", false},
		{"   ", false},
		{" home", false},
		{"home ", false},
		{"has space", false},
		{"has/slash", false},
		{"-leading-dash", false},
		{".leading-dot", false},
		{"_leading-underscore", false},
		{"control\tchar", false},
		{"control\nchar", false},
		{strings.Repeat("a", 129), false},
	}
	for _, tc := range cases {
		gotRecreatelog := validateEnvironment(tc.name) == nil
		if gotRecreatelog != tc.want {
			t.Errorf("recreatelog validateEnvironment(%q) accepted=%v, want %v", tc.name, gotRecreatelog, tc.want)
		}

		cfg := &config.Config{}
		_, err := cfg.AddEnvironment(tc.name, "/abs/parity-path")
		gotConfig := err == nil
		// AddEnvironment trims surrounding whitespace before validating, so an
		// all-whitespace or leading/trailing-whitespace-only name collapses to
		// the empty/trimmed case there in a way recreatelog's validateEnvironment
		// (no trimming) does not. That divergence is a documented, deliberate
		// difference in the TRIM step, not in the safe-shape regex itself, so
		// it is excluded from the direct comparison below.
		trimmed := strings.TrimSpace(tc.name)
		if trimmed != tc.name {
			continue
		}
		if gotConfig != tc.want {
			t.Errorf("config AddEnvironment(%q, ...) accepted=%v, want %v", tc.name, gotConfig, tc.want)
		}
	}
}
