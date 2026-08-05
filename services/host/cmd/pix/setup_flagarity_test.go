// setup_flagarity_test.go — provision.FlagTakesValue is setup's argument splitting, so
// its test lives with it even though the flags it names are onboarding's.
package main

import (
	"pix/host/workflow/provision"
	"testing"
)

// TestFlagTakesValue guards the onboard-flag arity setup uses to split DIR from
// value-bearing flags.
func TestFlagTakesValue(t *testing.T) {
	for _, f := range []string{"--account", "--knowledge", "--mcp", "--model"} {
		if !provision.FlagTakesValue(f) {
			t.Errorf("%s should take a value", f)
		}
	}
	for _, f := range []string{"--help", "-h", "--yes", "--account=x"} {
		if provision.FlagTakesValue(f) {
			t.Errorf("%s should NOT consume a following token", f)
		}
	}
}
