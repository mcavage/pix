// setup_ux_copy_test.go: UX-copy invariants for setup's provider-key failure
// message. op/1Password is the only key source, so a failure must point at the
// fix printed above rather than claim one fixed rerun command always works.
package main

import (
	"bytes"
	"strings"
	"testing"
)

// The generic setupHostPhase error must point at "the fix printed above",
// never claim the same setup command always fixes it (sometimes the fix is a
// different command, e.g. `pi-stack secret set` for a missing ref).
func TestSetupHostPhase_GenericKeyFailure_PointsAtFixAbove(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai", "sk-val") // no refs configured, mode unset
	var out bytes.Buffer
	err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected setupHostPhase to fail")
	}
	if !strings.Contains(err.Error(), "follow the fix printed above") {
		t.Errorf("error must point at the fix printed above, got: %v", err)
	}
	if strings.Contains(err.Error(), "re-run the same setup command") {
		t.Errorf("error must not claim the same setup command always fixes it, got: %v", err)
	}
}
