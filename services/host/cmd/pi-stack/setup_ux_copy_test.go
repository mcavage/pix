// setup_ux_copy_test.go: item 4's UX-copy invariants for incomplete-sbx
// guidance. acceptExistingSbxKeys and the new partial-sbx gate in
// providerKeyFlow must never presume the user typed a flag they didn't (a
// persisted mode or an accepted convenience prompt supplies no flag at all),
// and a partial sbx with no ref configured yet must fail early with actionable
// choices rather than being silently handed to the full strict 1Password flow.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// A persisted provider_key_mode=sbx with an INCOMPLETE sbx (one key missing)
// must print generic, source-agnostic guidance: name the missing provider,
// offer the sbx restore command, the 1Password alternative, AND (since a
// mode really is persisted here) the config-unset escape hatch — never
// "drop the flag" (nothing was passed this run).
func TestProviderKeyFlow_PersistedSbxMode_MissingKey_GenericActionableOutput(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai", "sk-val") // google missing from sbx
	var out bytes.Buffer
	ok, _ := providerKeyFlow(env, strings.NewReader(""), &out, false, false, false, false, config.ProviderKeyModeSbx)
	if ok {
		t.Fatal("a persisted sbx mode with an incomplete sbx must fail")
	}
	got := out.String()
	if strings.Contains(got, "drop the flag") {
		t.Errorf("must never say 'drop the flag' for a persisted mode (nothing was passed this run), got:\n%s", got)
	}
	if strings.Contains(got, "--use-sbx-keys") {
		t.Errorf("must not imply the user passed --use-sbx-keys this run, got:\n%s", got)
	}
	if !strings.Contains(got, "missing provider key(s): google") {
		t.Errorf("must name the missing provider, got:\n%s", got)
	}
	if !strings.Contains(got, "sbx secret set -g google") {
		t.Errorf("must offer the sbx restore command for the missing provider, got:\n%s", got)
	}
	if !strings.Contains(got, "pi-stack setup --use-1password") {
		t.Errorf("must offer the 1Password alternative, got:\n%s", got)
	}
	if !strings.Contains(got, "pi-stack config unset provider_key_mode") {
		t.Errorf("a persisted-mode failure must mention how to stop auto-using sbx, got:\n%s", got)
	}
}

// The same incomplete-sbx failure via an EXPLICIT --use-sbx-keys flag never
// mentions the config-unset escape hatch (nothing was persisted this run to
// unset), and never says "drop the flag" either.
func TestProviderKeyFlow_ExplicitUseSbxKeys_MissingKey_NoUnsetHatchNoDropFlag(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai", "sk-val") // google missing
	var out bytes.Buffer
	ok, _ := providerKeyFlow(env, strings.NewReader(""), &out, false, false, true, false, "")
	if ok {
		t.Fatal("an explicit --use-sbx-keys with an incomplete sbx must fail")
	}
	got := out.String()
	if strings.Contains(got, "drop the flag") {
		t.Errorf("must never say 'drop the flag', got:\n%s", got)
	}
	if strings.Contains(got, "pi-stack config unset provider_key_mode") {
		t.Errorf("an explicit flag (nothing persisted) must not suggest unsetting a mode, got:\n%s", got)
	}
	if !strings.Contains(got, "missing provider key(s): google") || !strings.Contains(got, "pi-stack setup --use-1password") {
		t.Errorf("must still name the missing provider and the 1Password alternative, got:\n%s", got)
	}
}

// Mode unset, no explicit flag, no provider ref configured yet, sbx reports
// SOME but not all three keys: providerKeyFlow must fail EARLY with
// actionable guidance (name what's missing, offer the sbx fix and the
// 1Password alternative) rather than silently falling into the full strict
// 1Password flow, which would demand a fresh ref even for providers sbx
// already has. This applies BOTH interactively and headlessly.
func TestProviderKeyFlow_ModeUnsetPartialSbx_FailsEarlyBothInteractiveAndHeadless(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		t.Run(fmt.Sprintf("interactive=%v", interactive), func(t *testing.T) {
			env, calls := stepEnv(t, "", "anthropic openai", "sk-val") // google missing, no refs configured
			var out bytes.Buffer
			ok, _ := providerKeyFlow(env, strings.NewReader(""), &out, interactive, false, false, false, "")
			if ok {
				t.Fatalf("a partial sbx with mode unset must fail (interactive=%v), got:\n%s", interactive, out.String())
			}
			got := out.String()
			if !strings.Contains(got, "sbx already has some provider keys") {
				t.Errorf("must explain sbx is partial, got:\n%s", got)
			}
			if !strings.Contains(got, "missing: google") {
				t.Errorf("must name the missing provider, got:\n%s", got)
			}
			if !strings.Contains(got, "sbx secret set -g google") {
				t.Errorf("must offer the sbx restore command, got:\n%s", got)
			}
			if !strings.Contains(got, "pi-stack setup --use-1password") {
				t.Errorf("must offer the 1Password alternative, got:\n%s", got)
			}
			if strings.Contains(got, "Model keys must come from 1Password") {
				t.Errorf("must not blindly fall into the old strict-flow phrasing, got:\n%s", got)
			}
			joined := strings.Join(*calls, "\n")
			if strings.Contains(joined, "op ") {
				t.Errorf("must fail before ever touching op, got calls:\n%s", joined)
			}
		})
	}
}

// An explicit --use-1password (or persisted 1password mode) BYPASSES the
// partial-sbx gate entirely and goes straight to the strict flow, even when
// sbx happens to be partially populated.
func TestProviderKeyFlow_ExplicitUse1Password_BypassesPartialSbxGate(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai", "sk-val") // google missing from sbx
	env.lookPath = func(name string) (string, error) {
		if name == "op" {
			return "", os.ErrNotExist // hit the strict flow's own hard precondition
		}
		return "/usr/bin/" + name, nil
	}
	var out bytes.Buffer
	ok, _ := providerKeyFlow(env, strings.NewReader(""), &out, false, false, false, true, "")
	if ok {
		t.Fatal("expected failure (op not installed)")
	}
	got := out.String()
	if strings.Contains(got, "sbx already has some provider keys") {
		t.Errorf("--use-1password must bypass the partial-sbx gate entirely, got:\n%s", got)
	}
	if !strings.Contains(got, "1Password provider setup requires") {
		t.Errorf("must reach the strict flow's own precondition message, got:\n%s", got)
	}
}

// A persisted 1password mode bypasses the partial-sbx gate exactly like the
// explicit flag.
func TestProviderKeyFlow_Persisted1PasswordMode_BypassesPartialSbxGate(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai", "sk-val")
	env.lookPath = func(name string) (string, error) {
		if name == "op" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	var out bytes.Buffer
	ok, _ := providerKeyFlow(env, strings.NewReader(""), &out, true, false, false, false, config.ProviderKeyMode1Password)
	if ok {
		t.Fatal("expected failure (op not installed)")
	}
	if strings.Contains(out.String(), "sbx already has some provider keys") {
		t.Errorf("a persisted 1password mode must bypass the partial-sbx gate entirely, got:\n%s", out.String())
	}
}

// If ANY provider ref is already configured, the partial-sbx gate never fires
// at all — the strict path remains exactly as before this feature, regardless
// of how complete sbx happens to be.
func TestProviderKeyFlow_RefsExist_PartialSbx_SkipsPartialGate(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\n"
	env, _ := stepEnv(t, refs, "anthropic openai", "sk-val") // google missing from sbx; a ref already exists
	var out bytes.Buffer
	// Non-interactive: we only care the gate never fires, not whether the rest
	// of the strict flow ultimately succeeds.
	providerKeyFlow(env, strings.NewReader(""), &out, false, false, false, false, "")
	if strings.Contains(out.String(), "sbx already has some provider keys") {
		t.Errorf("an existing ref must skip the partial-sbx gate entirely (strict path remains), got:\n%s", out.String())
	}
}

// The generic setupHostPhase error must point at "the fix printed above",
// never claim the same setup command always fixes it (sometimes the fix is a
// different command, e.g. `pi-stack setup --use-1password` or `sbx secret
// set`).
func TestSetupHostPhase_GenericKeyFailure_PointsAtFixAbove(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai", "sk-val") // google missing, no refs, mode unset
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
