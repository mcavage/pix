package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// --- item 1: persisted-mode precedence (providerKeyFlow) -------------------

// A persisted mode of "sbx" auto-skips 1Password with NO prompt on a repeat
// run, using the same exact all-three probe as the explicit flag.
func TestProviderKeyFlow_PersistedSbxMode_AutoSkipsNoPrompt(t *testing.T) {
	env, calls := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	// No explicit flag, no input available at all (EOF) — if this fell through
	// to any prompt it would fail; it must not even try to read.
	ok, mode := providerKeyFlow(env, strings.NewReader(""), &out, true, false, false, false, config.ProviderKeyModeSbx)
	if !ok {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	if mode != config.ProviderKeyModeSbx {
		t.Errorf("mode = %q, want sbx", mode)
	}
	if strings.Contains(out.String(), sbxKeysConveniencePrompt) {
		t.Errorf("a persisted sbx mode must never show the convenience prompt, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "paste a 1Password ref") {
		t.Errorf("a persisted sbx mode must never enter the strict flow, got:\n%s", out.String())
	}
	assertNoSkipSideEffects(t, env, *calls)
}

// A persisted mode of "sbx" still requires the EXACT all-three probe on
// repeat: an incomplete sbx fails, it never silently degrades.
func TestProviderKeyFlow_PersistedSbxMode_StillRequiresExactProbe(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai", "sk-val") // google missing
	var out bytes.Buffer
	ok, _ := providerKeyFlow(env, strings.NewReader(""), &out, false, false, false, false, config.ProviderKeyModeSbx)
	if ok {
		t.Fatal("an incomplete sbx must fail even with a persisted sbx mode")
	}
}

// A persisted mode of "1password" skips the convenience prompt entirely (no
// prompt on repeat) and goes straight to the strict flow.
func TestProviderKeyFlow_Persisted1PasswordMode_SkipsPromptGoesStrict(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := recordSyncedRefWithDigest(envVar, ref, secretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	// Even though sbx ALSO has all three keys (which would normally trigger the
	// convenience prompt when mode is unset), a persisted 1password mode must
	// never offer it.
	ok, mode := providerKeyFlow(env, strings.NewReader(""), &out, true, false, false, false, config.ProviderKeyMode1Password)
	if !ok {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	if mode != config.ProviderKeyMode1Password {
		t.Errorf("mode = %q, want 1password", mode)
	}
	if strings.Contains(out.String(), sbxKeysConveniencePrompt) {
		t.Errorf("a persisted 1password mode must never show the convenience prompt, got:\n%s", out.String())
	}
}

// --- explicit flags always override a persisted mode ------------------------

func TestProviderKeyFlow_ExplicitFlagsOverridePersistedMode(t *testing.T) {
	// Persisted "1password", but this run explicitly asks --use-sbx-keys: the
	// explicit flag wins.
	env, calls := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	ok, mode := providerKeyFlow(env, strings.NewReader(""), &out, false, false, true, false, config.ProviderKeyMode1Password)
	if !ok || mode != config.ProviderKeyModeSbx {
		t.Fatalf("explicit --use-sbx-keys must win over a persisted 1password mode: ok=%v mode=%q\n%s", ok, mode, out.String())
	}
	assertNoSkipSideEffects(t, env, *calls)

	// Persisted "sbx", but this run explicitly asks --use-1password: the
	// explicit flag forces strict.
	refs := allRefs("", "", "")
	env2, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := recordSyncedRefWithDigest(envVar, ref, secretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	var out2 bytes.Buffer
	ok2, mode2 := providerKeyFlow(env2, strings.NewReader(""), &out2, false, false, false, true, config.ProviderKeyModeSbx)
	if !ok2 || mode2 != config.ProviderKeyMode1Password {
		t.Fatalf("explicit --use-1password must win over a persisted sbx mode: ok=%v mode=%q\n%s", ok2, mode2, out2.String())
	}
}

// --- mode unset, no explicit flag: original behavior preserved -------------

func TestProviderKeyFlow_ModeUnsetNoFlags_MatchesLegacyBehavior(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	ok, mode := providerKeyFlow(env, strings.NewReader("\n"), &out, true, false, false, false, "")
	if !ok || mode != config.ProviderKeyModeSbx {
		t.Fatalf("mode-unset + accepted convenience prompt should succeed with sbx mode: ok=%v mode=%q\n%s", ok, mode, out.String())
	}
}

// --- item 3 (EOF safety) integration: convenience prompt EOF is not consent -

func TestProviderKeyFlow_ConveniencePromptEOF_FailsNotConsent(t *testing.T) {
	env, calls := stepEnv(t, "", "anthropic openai google", "sk-val")
	var out bytes.Buffer
	ok, _ := providerKeyFlow(env, strings.NewReader(""), &out, true, false, false, false, "")
	if ok {
		t.Fatal("EOF on the convenience prompt must never be treated as consent (default yes)")
	}
	if !strings.Contains(out.String(), "no answer read (EOF)") {
		t.Errorf("must explain the EOF clearly, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "secret set") {
		t.Errorf("an EOF'd prompt must never proceed to touch sbx: %s", joined)
	}
}

// --- item 2: tri-state ref-read error must fail setup, never mask ---------

// A genuine read error on op-refs.env (not ENOENT) must fail the convenience
// gate outright, naming the path, rather than being silently treated as "no
// refs configured" (which would incorrectly offer the skip prompt while
// masking a real problem).
func TestProviderKeyFlow_UnreadableOpRefs_FailsNeverMasksAsNone(t *testing.T) {
	env, _ := stepEnv(t, "", "anthropic openai google", "sk-val")
	realRead := env.readFile
	env.readFile = func(p string) (string, error) {
		if strings.HasSuffix(p, "op-refs.env") {
			return "", os.ErrPermission
		}
		return realRead(p)
	}
	var out bytes.Buffer
	ok, _ := providerKeyFlow(env, strings.NewReader("\n"), &out, true, false, false, false, "")
	if ok {
		t.Fatal("an unreadable op-refs.env must fail setup, never silently proceed")
	}
	if !strings.Contains(out.String(), "op-refs.env") {
		t.Errorf("must name the unreadable path, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), sbxKeysConveniencePrompt) {
		t.Errorf("a read error must never be masked as \"no refs\" and offer the convenience prompt, got:\n%s", out.String())
	}
}

// --- probeProviderKeyRefs unit coverage: none / configured / error --------

func TestProbeProviderKeyRefs_NoneWhenBothFilesAbsent(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	// stepEnv seeds op-refs.env with empty content (not absent); simulate a
	// genuinely absent file via ErrNotExist for both paths.
	env.readFile = func(string) (string, error) { return "", os.ErrNotExist }
	state, _, err := probeProviderKeyRefs(env)
	if state != providerKeyRefsProbeNone || err != nil {
		t.Errorf("state=%v err=%v, want None/nil", state, err)
	}
}

func TestProbeProviderKeyRefs_SomeWhenARefIsConfigured(t *testing.T) {
	env, _ := stepEnv(t, "ANTHROPIC_API_KEY=op://v/a/k\n", "", "sk-val")
	state, _, err := probeProviderKeyRefs(env)
	if state != providerKeyRefsProbeSome || err != nil {
		t.Errorf("state=%v err=%v, want Some/nil", state, err)
	}
}

// A ref configured ONLY in hostmode.env (not op-refs.env) must still count as
// "some" — the tri-state helper checks BOTH files, mirroring currentOpRef.
func TestProbeProviderKeyRefs_SomeWhenOnlyHostModeHasIt(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	if err := env.writeFile(hostModeRefsPath(env), []byte("ANTHROPIC_API_KEY=op://v/a/k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, _, err := probeProviderKeyRefs(env)
	if state != providerKeyRefsProbeSome || err != nil {
		t.Errorf("state=%v err=%v, want Some/nil", state, err)
	}
}

// A read error on hostmode.env (op-refs.env absent/empty) is also an ERROR,
// not None — the second file matters exactly as much as the first.
func TestProbeProviderKeyRefs_ErrorOnHostModeReadFailure(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	env.readFile = func(p string) (string, error) {
		if strings.HasSuffix(p, "hostmode.env") {
			return "", os.ErrPermission
		}
		return "", os.ErrNotExist
	}
	state, path, err := probeProviderKeyRefs(env)
	if state != providerKeyRefsProbeError || err == nil {
		t.Errorf("state=%v err=%v, want Error/non-nil", state, err)
	}
	if !strings.HasSuffix(path, "hostmode.env") {
		t.Errorf("path = %q, want hostmode.env", path)
	}
}

// A symlink-loop-style read error (ELOOP) surfaces identically to any other
// non-ENOENT error: Error, never None.
func TestProbeProviderKeyRefs_SymlinkLoopIsErrorNotNone(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	env.readFile = func(string) (string, error) {
		return "", &os.PathError{Op: "open", Path: "op-refs.env", Err: os.ErrInvalid}
	}
	state, _, err := probeProviderKeyRefs(env)
	if state != providerKeyRefsProbeError || err == nil {
		t.Error("a symlink-loop-shaped read error must classify as Error, never None")
	}
}

// --- mutual exclusion: --use-sbx-keys + --use-1password ---------------------

func TestParseOnboardArgs_MutuallyExclusiveKeyFlags(t *testing.T) {
	_, err := parseOnboardArgs([]string{"--use-sbx-keys", "--use-1password"})
	if err == nil {
		t.Fatal("--use-sbx-keys + --use-1password together must be rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error must explain mutual exclusion, got: %v", err)
	}
}

func TestParseOnboardArgs_Use1Password(t *testing.T) {
	o, err := parseOnboardArgs([]string{"--use-1password"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.use1Password {
		t.Error("parsed use1Password must be true")
	}
	if o.useSbxKeys {
		t.Error("use1Password alone must not set useSbxKeys")
	}
}

// onboard rejects BOTH provider-key-source flags (it never provisions keys).
func TestOnboardOpts_RejectsUse1PasswordViaUsageDoc(t *testing.T) {
	if !strings.Contains(onboardUsage, "--use-1password") {
		t.Error("onboardUsage must mention --use-1password is rejected (setup-only)")
	}
}

// --- item 3 integration: the sbx-overwrite confirmation treats EOF as a
// hard failure too, never as the default-yes answer -------------------------

func TestReconcile_OverwritePromptEOF_FailsNotConsent(t *testing.T) {
	refs := allRefs("op://v/anthropic/key-NEW", "", "")
	env, calls := stepEnv(t, refs, "anthropic openai google", "sk-val")
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/anthropic/key-OLD"); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRef("OPENAI_API_KEY", "op://v/openai/key"); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRef("GEMINI_API_KEY", "op://v/gemini/key"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Interactive, but NO input at all: the reconcile overwrite prompt hits EOF.
	// Before the (answer, ok) API this silently applied the default (YES,
	// "1Password is the source of truth") and overwrote sbx with no consent.
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, false, false) {
		t.Fatal("EOF on the overwrite confirmation must fail setup, never silently apply the default")
	}
	if !strings.Contains(out.String(), "no answer read (EOF)") {
		t.Errorf("must explain the EOF clearly, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "secret set -f -g anthropic") {
		t.Errorf("an EOF'd overwrite confirmation must never touch sbx: %s", joined)
	}
}

// --- setupHostPhase persists the resolved mode, honestly ------------------

// A successful setup run via --use-sbx-keys persists provider_key_mode=sbx.
func TestSetupHostPhase_PersistsProviderKeyModeSbxOnSuccess(t *testing.T) {
	stubHostProvisioning(t)
	dir := t.TempDir()
	env, _ := stepEnv(t, "", "anthropic openai google", "sk-val")
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))

	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--use-sbx-keys", "--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderKeyMode != config.ProviderKeyModeSbx {
		t.Errorf("ProviderKeyMode = %q, want sbx", cfg.ProviderKeyMode)
	}
}

// A repeat setup run with NO flags, given a persisted sbx mode, succeeds with
// NO prompt at all (even with EOF stdin) — the whole point of persisting it.
func TestSetupHostPhase_RepeatRunWithPersistedSbxMode_NoPrompt(t *testing.T) {
	stubHostProvisioning(t)
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProviderKeyMode = config.ProviderKeyModeSbx
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	env, _ := stepEnv(t, "", "anthropic openai google", "sk-val")
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))

	var out bytes.Buffer
	// No --use-sbx-keys flag this time, no input at all (EOF) — must still
	// succeed with zero prompts, purely from the persisted mode.
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, true); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), sbxKeysConveniencePrompt) {
		t.Errorf("a persisted sbx mode must never prompt on repeat, got:\n%s", out.String())
	}
}

// A mode-save failure (cfg.Save errors after keys were successfully resolved)
// fails setup HONESTLY: it must not report success while silently failing to
// persist the mode it just claims to have used.
func TestSetupHostPhase_ModeSaveFailureFailsHonestly(t *testing.T) {
	stubHostProvisioning(t)
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgPath)
	// Config must already exist on disk so the atomic temp-file create inside
	// cfg.Save (not MkdirAll) is what fails.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	env, _ := stepEnv(t, "", "anthropic openai google", "sk-val")
	t.Setenv("PI_STACK_CONFIG", cfgPath)

	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	var out bytes.Buffer
	err = setupHostPhase(env, []string{"--use-sbx-keys", "--yes"}, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected setupHostPhase to fail when persisting provider_key_mode fails")
	}
	if !strings.Contains(err.Error(), "provider_key_mode") {
		t.Errorf("error must name what failed to persist, got: %v", err)
	}
}
