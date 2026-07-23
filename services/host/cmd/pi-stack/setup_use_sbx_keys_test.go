// setup_use_sbx_keys_test.go: the owner-correction cases. `--use-sbx-keys`
// (explicit) and the interactive convenience prompt (implicit) both let
// `pi-stack setup` trust a COMPLETE existing sbx key set instead of the
// strict 1Password flow. Both require the EXACT sbx probe to report all
// three provider keys; neither ever touches op, op-refs.env, hostmode.env, or
// the synced-ref record. See setup.go's setupProvisionKeys doc comment.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// assertNoSkipSideEffects fails the test if the skip path left ANY trace: no
// op invocation, no sbx secret set, no ref file writes visible through env's
// own readFile, and no synced-ref record for any provider.
func assertNoSkipSideEffects(t *testing.T, env shellEnv, calls []string) {
	t.Helper()
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "op ") || strings.HasPrefix(joined, "op") {
		t.Errorf("skip path must never invoke op, got calls:\n%s", joined)
	}
	if strings.Contains(joined, "secret set") {
		t.Errorf("skip path must never call sbx secret set, got calls:\n%s", joined)
	}
	if c, err := env.readFile(defaultOpRefsPath(env)); err == nil && strings.TrimSpace(c) != "" {
		t.Errorf("skip path must never write op-refs.env, got:\n%s", c)
	}
	if c, err := env.readFile(hostModeRefsPath(env)); err == nil && strings.TrimSpace(c) != "" {
		t.Errorf("skip path must never write hostmode.env, got:\n%s", c)
	}
	for _, envVar := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
		if _, ok := syncedRef(envVar); ok {
			t.Errorf("skip path must never write a synced-ref record for %s", envVar)
		}
	}
}

// --- interactive convenience prompt ----------------------------------------

// Interactive, no flag, sbx already has all three keys, no ref configured
// yet, bare Enter (default): accepts immediately, no strict flow, no op/ref
// side effects.
func TestSetupProvisionKeys_InteractiveConveniencePrompt_DefaultYesSkips(t *testing.T) {
	env, calls := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	if !setupProvisionKeys(env, strings.NewReader("\n"), &out, true, false, false) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	got := out.String()
	if !strings.Contains(got, sbxKeysConveniencePrompt) {
		t.Errorf("must ask the exact convenience prompt, got:\n%s", got)
	}
	if strings.Contains(got, "paste a 1Password ref") {
		t.Errorf("accepting the convenience prompt must never fall into the strict flow, got:\n%s", got)
	}
	if strings.Contains(got, "provider keys (host secrets") {
		t.Errorf("accepting the convenience prompt must print a compact status, not the full reportProviderKeys listing, got:\n%s", got)
	}
	assertNoSkipSideEffects(t, env, *calls)
}

// Interactive, no flag, same setup, explicit "n": falls through to the full
// strict 1Password flow (never retries the convenience question, never
// treats "no" as three empty-ref retries).
func TestSetupProvisionKeys_InteractiveConveniencePrompt_NoStaysStrict(t *testing.T) {
	env, calls := stepEnv(t, "", "anthropic openai google", "sk-val")
	// After declining: STEP 1 prompts fresh for all three refs (none configured
	// yet), then reconcile's batched overwrite prompt (sbx already has all
	// three names, but nothing was ever recorded as synced) defaults to yes on
	// a blank line.
	in := strings.NewReader("n\nop://V/anthropic/key\nop://V/openai/key\nop://V/gemini/key\n\n")
	var out bytes.Buffer
	if !setupProvisionKeys(env, in, &out, true, false, false) {
		t.Fatalf("expected eventual success via the strict flow, got:\n%s", out.String())
	}
	got := out.String()
	if n := strings.Count(got, sbxKeysConveniencePrompt); n != 1 {
		t.Errorf("must ask the convenience prompt exactly once, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "provider keys (host secrets") {
		t.Errorf("declining must fall through to the full strict report, got:\n%s", got)
	}
	if !strings.Contains(got, "paste a 1Password ref") {
		t.Errorf("declining must proceed to prompt for refs (strict flow), got:\n%s", got)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "op read") {
		t.Errorf("strict flow must actually read refs via op, got:\n%s", joined)
	}
}

// An existing ref for even ONE provider suppresses the convenience prompt
// entirely (the user already made a 1Password choice), but the EXPLICIT
// --use-sbx-keys flag still overrides and skips, ignoring that existing ref.
func TestSetupProvisionKeys_ExistingRefSuppressesPromptButFlagOverrides(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\n"

	// No flag: the existing anthropic ref suppresses the convenience prompt,
	// so setup goes straight to the strict flow.
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	in := strings.NewReader("op://V/openai/key\nop://V/gemini/key\n\n")
	var out bytes.Buffer
	if !setupProvisionKeys(env, in, &out, true, false, false) {
		t.Fatalf("expected eventual success via the strict flow, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), sbxKeysConveniencePrompt) {
		t.Errorf("an existing ref must suppress the convenience prompt, got:\n%s", out.String())
	}

	// Explicit flag: same existing ref, same complete sbx. The flag still
	// wins and skips everything, proving it overrides the "existing ref"
	// suppression rule that only applies to the implicit convenience prompt.
	// The pre-existing ref is left exactly as it was (not deleted, not touched).
	env2, calls2 := stepEnv(t, refs, "anthropic openai google", "sk-val")
	var out2 bytes.Buffer
	if !setupProvisionKeys(env2, strings.NewReader(""), &out2, true, false, true) {
		t.Fatalf("--use-sbx-keys must win even with an existing ref, got:\n%s", out2.String())
	}
	if strings.Contains(out2.String(), "paste a 1Password ref") {
		t.Errorf("--use-sbx-keys must never enter the strict flow, got:\n%s", out2.String())
	}
	joined2 := strings.Join(*calls2, "\n")
	if strings.Contains(joined2, "op ") || strings.Contains(joined2, "secret set") {
		t.Errorf("--use-sbx-keys must never invoke op or sbx secret set, got calls:\n%s", joined2)
	}
	if c, err := env2.readFile(defaultOpRefsPath(env2)); err != nil || c != refs {
		t.Errorf("the pre-existing ref must be left exactly as it was, got %q (err=%v), want %q", c, err, refs)
	}
	for _, envVar := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
		if _, ok := syncedRef(envVar); ok {
			t.Errorf("--use-sbx-keys must never write a synced-ref record for %s", envVar)
		}
	}
}

// --- explicit --use-sbx-keys flag ------------------------------------------

// The flag works identically on a TTY and headless (non-interactive): sbx has
// all three, it succeeds immediately with a compact status, no prompts.
func TestSetupProvisionKeys_ExplicitFlag_TTYAndHeadless(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
	}{
		{"tty", true},
		{"headless", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, calls := stepEnv(t, "", "anthropic openai google", "sk-val")
			var out bytes.Buffer
			if !setupProvisionKeys(env, strings.NewReader(""), &out, tc.interactive, false, true) {
				t.Fatalf("expected success, got:\n%s", out.String())
			}
			if strings.Contains(out.String(), sbxKeysConveniencePrompt) {
				t.Errorf("the explicit flag must never ask the convenience prompt, got:\n%s", out.String())
			}
			assertNoSkipSideEffects(t, env, *calls)
		})
	}
}

// The flag with an incomplete sbx (missing one of three) FAILS with a clear
// message, regardless of interactivity.
func TestSetupProvisionKeys_ExplicitFlag_IncompleteFails(t *testing.T) {
	env, calls := stepEnv(t, "", "anthropic openai", "sk-val") // google missing
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false, true) {
		t.Fatal("an incomplete sbx must fail --use-sbx-keys")
	}
	// Generic, source-agnostic copy: names the missing provider and offers
	// both a cheap sbx fix and the 1Password alternative — never "drop the
	// flag", since acceptExistingSbxKeys' wording is shared with the
	// persisted-mode and convenience-prompt call sites, which never had a flag
	// to drop.
	if !strings.Contains(out.String(), "missing provider key(s): google") {
		t.Errorf("must name the missing provider, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "sbx secret set -g google") || !strings.Contains(out.String(), "pi-stack setup --use-1password") {
		t.Errorf("must offer both the sbx fix and the 1Password alternative, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "drop the flag") {
		t.Errorf("must never say 'drop the flag', got:\n%s", out.String())
	}
	assertNoSkipSideEffects(t, env, *calls)
}

// The flag when sbx's control plane errors (`sbx secret ls` fails) FAILS
// closed with a clear message, never silently trusting an unverifiable sbx.
func TestSetupProvisionKeys_ExplicitFlag_SbxErrorFails(t *testing.T) {
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		readFile: func(string) (string, error) { return "", os.ErrNotExist },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" {
				return "", fmt.Errorf("control plane down")
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false, true) {
		t.Fatal("an sbx probe error must fail --use-sbx-keys")
	}
	if !strings.Contains(out.String(), "could not verify") {
		t.Errorf("must explain the probe failed, got:\n%s", out.String())
	}
}

// The flag when sbx is entirely absent (not on PATH) FAILS. --use-sbx-keys
// is a request to trust sbx, so sbx not being there at all is a hard failure
// here, never the strict flow's fail-OPEN portability behavior.
func TestSetupProvisionKeys_ExplicitFlag_SbxAbsentFails(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", os.ErrNotExist },
		readFile: func(string) (string, error) { return "", os.ErrNotExist },
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false, true) {
		t.Fatal("sbx entirely absent must fail --use-sbx-keys, not fail open")
	}
	// Generic, source-agnostic copy: explains sbx itself is unavailable and
	// offers the 1Password alternative, without presuming the user typed a
	// flag this run (the same message fires for a persisted sbx mode too).
	if !strings.Contains(out.String(), "sbx isn't installed or reachable") {
		t.Errorf("must explain sbx is unavailable, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pi-stack setup --use-1password") {
		t.Errorf("must offer the 1Password alternative, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "drop the flag") {
		t.Errorf("must never say 'drop the flag', got:\n%s", out.String())
	}
}

// --- noninteractive, no flag: stays strict ---------------------------------

// Non-interactive, no flag, no --yes: even though sbx already has all three
// keys, setup NEVER offers or takes the convenience skip non-interactively,
// it goes straight to the strict flow and fails on missing refs exactly as
// before this feature.
func TestSetupProvisionKeys_NonInteractiveNoFlag_StaysStrictEvenWithCompleteSbx(t *testing.T) {
	env, calls := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false, false) {
		t.Fatal("non-interactive with no configured refs must still fail (strict flow), a complete sbx must not silently substitute")
	}
	got := out.String()
	if strings.Contains(got, sbxKeysConveniencePrompt) {
		t.Errorf("must never show the convenience prompt non-interactively, got:\n%s", got)
	}
	for _, want := range []string{
		"pi-stack secret set ANTHROPIC_API_KEY op://Vault/Item/field",
		"pi-stack secret set OPENAI_API_KEY op://Vault/Item/field",
		"pi-stack secret set GEMINI_API_KEY op://Vault/Item/field",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("must print the exact fix command %q, got:\n%s", want, got)
		}
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "secret set") {
		t.Errorf("the strict non-interactive failure must never touch sbx, got:\n%s", joined)
	}
}

// --yes does NOT imply --use-sbx-keys: assumeYes alone must not trigger the
// convenience skip (assumeYes makes setupInteractivePrompts report
// non-interactive, so this is really the same guarantee as the test above,
// exercised through the explicit assumeYes flag rather than a non-TTY).
func TestSetupProvisionKeys_AssumeYesAloneDoesNotImplySkip(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, true, false) {
		t.Fatal("--yes alone (no --use-sbx-keys) must still require 1Password refs to already resolve")
	}
	if strings.Contains(out.String(), sbxKeysConveniencePrompt) {
		t.Errorf("--yes must never trigger the convenience prompt (it suppresses ALL prompts), got:\n%s", out.String())
	}
}

// --- setupHostPhase threads the flags through -------------------------------

// stubHostProvisioning replaces setupHostMode's real provisioning seams with
// no-op fakes for the duration of the test, so a full setupHostPhase run
// never touches real host state or makes a real `pi install` network call
// regardless of what's on the test runner's PATH.
func stubHostProvisioning(t *testing.T) {
	t.Helper()
	origSetup, origProvisioned := runHostSetupFn, hostProvisionedFn
	runHostSetupFn = func(*os.File) error { return nil }
	hostProvisionedFn = func() bool { return true }
	t.Cleanup(func() {
		runHostSetupFn = origSetup
		hostProvisionedFn = origProvisioned
	})
}

// parseOnboardArgs (shared by setup and onboard) accepts --use-sbx-keys and
// setupHostPhase forwards it to provisionProviderKeysFn.
func TestSetupHostPhase_ForwardsUseSbxKeysFlag(t *testing.T) {
	orig := provisionProviderKeysFn
	t.Cleanup(func() { provisionProviderKeysFn = orig })
	stubHostProvisioning(t)

	var gotUseSbxKeys, gotUse1Password bool
	provisionProviderKeysFn = func(_ shellEnv, _ io.Reader, _ io.Writer, _, _, useSbxKeys, use1Password bool, _ string) providerKeySetupResult {
		gotUseSbxKeys = useSbxKeys
		gotUse1Password = use1Password
		return providerKeySetupResult{OK: true, Mode: "sbx"}
	}

	env, _ := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--use-sbx-keys", "--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if !gotUseSbxKeys {
		t.Error("setupHostPhase must forward --use-sbx-keys to provisionProviderKeysFn")
	}
	if gotUse1Password {
		t.Error("--use-sbx-keys alone must not also forward use1Password=true")
	}
}

// --- setupHostMode: a local-only result is legitimate, not a bug ----------

// When cloud keys never get wired to hostmode.env (e.g. this setup run used
// --use-sbx-keys or accepted the convenience prompt), setupHostMode must
// report an honest local-only result, never a "this should not happen"
// defensive message, and it must not ask for refs.
func TestSetupHostMode_LocalOnlyIsLegitimateResult(t *testing.T) {
	stubHostProvisioning(t)
	env, _ := stepEnv(t, "", "", "sk-val") // no hostmode.env refs at all
	var out bytes.Buffer
	setupHostMode(env, &out, providerKeySetupResult{OK: true, Mode: "sbx"})
	got := out.String()
	if strings.Contains(got, "should not happen") {
		t.Errorf("a local-only result after skipping 1Password must not be called a should-not-happen defensive branch, got:\n%s", got)
	}
	if strings.Contains(got, "paste a 1Password ref") || strings.Contains(got, "?") {
		t.Errorf("setupHostMode must never prompt for refs, got:\n%s", got)
	}
}

// --- item 4: host refs truth: "validated" vs "configured but unverified" ---

// A strict (1password) run that lands cloud keys must say they were
// VALIDATED this run (op read actually happened); a run that skipped
// 1Password (sbx mode) must never claim that, even if hostmode.env happens to
// carry refs from a prior run — it must say "configured" and name that they
// were not re-verified this run.
func TestSetupHostMode_ReportsValidatedOnlyForStrictMode(t *testing.T) {
	stubHostProvisioning(t)
	hostmode := "ANTHROPIC_API_KEY=op://v/a/k\nOPENAI_API_KEY=op://v/o/k\nGEMINI_API_KEY=op://v/g/k\n"

	env, _ := stepEnv(t, "", "", "sk-val")
	if err := env.writeFile(hostModeRefsPath(env), []byte(hostmode), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	setupHostMode(env, &out, providerKeySetupResult{OK: true, Mode: "1password"})
	if !strings.Contains(out.String(), "validated this run") {
		t.Errorf("strict mode must claim validation, got:\n%s", out.String())
	}

	env2, _ := stepEnv(t, "", "", "sk-val")
	if err := env2.writeFile(hostModeRefsPath(env2), []byte(hostmode), 0o600); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	setupHostMode(env2, &out2, providerKeySetupResult{OK: true, Mode: "sbx"})
	got2 := out2.String()
	if strings.Contains(got2, "validated this run") {
		t.Errorf("sbx-mode skip must never claim validation, got:\n%s", got2)
	}
	if !strings.Contains(got2, "configured") || !strings.Contains(got2, "not verified this run") {
		t.Errorf("sbx-mode skip must say configured-but-unverified, got:\n%s", got2)
	}
}

// --- item 2: setupHostMode must never guess a claim it can't back up -------

// An unreadable hostmode.env (EACCES-shaped) must make setupHostMode report
// "credential state unreadable", never fall back to claiming "local-only" or
// "configured" — both would be a confident guess about state it could not
// actually read. Host mode itself (enabled+provisioned, reported earlier in
// the function) is unaffected.
func TestSetupHostMode_CredentialStateUnreadable_NeverClaimsLocalOrConfigured(t *testing.T) {
	stubHostProvisioning(t)
	env, _ := stepEnv(t, "", "", "sk-val")
	env.readFile = func(p string) (string, error) {
		if strings.HasSuffix(p, "hostmode.env") {
			return "", os.ErrPermission
		}
		return "", os.ErrNotExist
	}
	var out bytes.Buffer
	setupHostMode(env, &out, providerKeySetupResult{OK: true, Mode: "sbx"})
	got := out.String()
	if !strings.Contains(got, "credential state unreadable") {
		t.Errorf("must report credential state unreadable, got:\n%s", got)
	}
	if strings.Contains(got, "local/Ollama-only") {
		t.Errorf("must never claim local-only when the state couldn't be read, got:\n%s", got)
	}
	if strings.Contains(got, "cloud refs configured") || strings.Contains(got, "cloud keys validated") {
		t.Errorf("must never claim configured/validated when the state couldn't be read, got:\n%s", got)
	}
}

// --- parser -----------------------------------------------------------------

func TestParseOnboardArgs_UseSbxKeys(t *testing.T) {
	o, err := parseOnboardArgs([]string{"--use-sbx-keys"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !o.useSbxKeys {
		t.Errorf("parsed = %+v, want useSbxKeys=true", o)
	}
	o2, err := parseOnboardArgs(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o2.useSbxKeys {
		t.Error("useSbxKeys must default to false")
	}
}

// --- help / usage text -------------------------------------------------------

func TestSetupUsage_DocumentsUseSbxKeysAndDropsMandatoryClaim(t *testing.T) {
	if !strings.Contains(setupUsage, "--use-sbx-keys") {
		t.Error("setupUsage must document --use-sbx-keys")
	}
	if strings.Contains(setupUsage, "ALWAYS mandatory and ALWAYS") {
		t.Error("setupUsage must no longer claim provider keys are always mandatory from 1Password")
	}
}
