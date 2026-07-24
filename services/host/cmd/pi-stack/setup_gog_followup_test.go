// setup_gog_followup_test.go — `pi-stack setup --account` never runs the gog
// browser/OAuth flow itself; it must at least print a clear follow-up telling
// the user to run `pi-stack gog setup --account ...` when OAuth/headless
// health is not already confirmed healthy.
package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestSetupHostPhase_AccountSet_PrintsGogSetupFollowUp_WhenNotHealthy(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	baseRun := env.run
	env.run = func(name string, args ...string) (string, error) {
		if name == "gog" {
			return "", fmt.Errorf("not authorized")
		}
		return baseRun(name, args...)
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes", "--account", "you@example.com"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "pi-stack gog setup --account you@example.com") {
		t.Errorf("expected a gog setup follow-up command, got:\n%s", out.String())
	}
	// It must never claim ready, and never itself attempt a browser flow — no
	// "gog auth" invocation of its own beyond the read-only health probes
	// setup already made (auth status / auth doctor), which stepEnv's
	// interception above turns into a failure, not a launched browser.
}

func TestSetupHostPhase_AccountSet_NoFollowUp_WhenHealthy(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	baseRun := env.run
	env.run = func(name string, args ...string) (string, error) {
		if name == "gog" {
			return "", nil // gogAuthed's `gog --account ... auth status` succeeds
		}
		return baseRun(name, args...)
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes", "--account", "you@example.com"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "pi-stack gog setup --account") {
		t.Errorf("must not print the follow-up when gog auth is already healthy, got:\n%s", out.String())
	}
}

func TestSetupHostPhase_NoAccount_NoGogFollowUp(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "pi-stack gog setup --account") {
		t.Errorf("must not mention gog setup when no --account was given, got:\n%s", out.String())
	}
}
