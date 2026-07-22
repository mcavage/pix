// setup_keys_flow_test.go — the mandatory-1Password-provider-key invariant:
// pi-stack ALWAYS sources model provider keys (Anthropic/OpenAI/Google) from
// 1Password, never merely from whatever sbx already has. setupProvisionKeys
// (setup.go) collects + validates a ref for every provider (op installed +
// signed in are hard preconditions; an existing ref is confirmed, not
// re-pasted, but must still resolve; a missing ref is prompted for once on a
// TTY, or reported as an exact `pi-stack secret set` command otherwise), then
// reconcileProviderKeysWithSbx (secret_sync.go) brings sbx into line using the
// launcher-owned synced-ref record (syncedrefs.go) since sbx secret values are
// write-only.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// stepEnv sets up a temp config+state dir (so syncedrefs.go's real os-backed
// store works) and returns a shellEnv whose op-refs.env content is
// controllable and whose sbx secret store is a STATEFUL in-memory set seeded
// from sbxLsOut: a `secret set -g <name>` call actually adds <name>, so a
// later `secret ls` (including setupProvisionKeys' own final probe) reflects
// it — matching real sbx behavior instead of a frozen fixture. `op read`
// returns opReadVal for every ref (tests that need to distinguish which ref
// was read assert on the "op read <ref>" call text, not the resolved value).
func stepEnv(t *testing.T, refsContent, sbxLsOut string, opReadVal string) (shellEnv, *[]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	files := map[string]string{"/cfg/pi-stack/op-refs.env": refsContent}
	calls := &[]string{}
	sbxNames := map[string]bool{}
	for _, w := range strings.Fields(sbxLsOut) {
		sbxNames[w] = true
	}
	env := shellEnv{
		getenv: func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return "/cfg"
			}
			return ""
		},
		readFile: func(p string) (string, error) {
			if v, ok := files[p]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		},
		writeFile: func(p string, d []byte, _ os.FileMode) error {
			files[p] = string(d)
			return nil
		},
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			*calls = append(*calls, name+" "+strings.Join(args, " "))
			switch {
			case name == "op" && len(args) >= 1 && args[0] == "--version":
				return "2.0", nil
			case name == "op" && len(args) >= 1 && args[0] == "account":
				return "acct", nil
			case name == "op" && len(args) >= 1 && args[0] == "read":
				return opReadVal, nil
			case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls":
				names := make([]string, 0, len(sbxNames))
				for n := range sbxNames {
					names = append(names, n)
				}
				sort.Strings(names)
				return strings.Join(names, "\n") + "\n", nil
			case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "set":
				for i, a := range args {
					if a == "-g" && i+1 < len(args) {
						sbxNames[args[i+1]] = true
					}
				}
				return "", nil
			}
			return "", nil
		},
	}
	return env, calls
}

// allRefs builds op-refs.env content declaring all three provider refs, with
// per-provider overrides (empty override = the given default ref).
func allRefs(anthropic, openai, gemini string) string {
	pick := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	return "ANTHROPIC_API_KEY=" + pick(anthropic, "op://v/anthropic/key") + "\n" +
		"OPENAI_API_KEY=" + pick(openai, "op://v/openai/key") + "\n" +
		"GEMINI_API_KEY=" + pick(gemini, "op://v/gemini/key") + "\n"
}

func countOccurrences(haystack, needle string) int {
	return strings.Count(haystack, needle)
}

// --- hard preconditions ---------------------------------------------------

func TestSetupProvisionKeys_OpNotInstalled_FailsWithExactFix(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", os.ErrNotExist },
		readFile: func(string) (string, error) { return "", os.ErrNotExist },
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, false) {
		t.Fatal("must fail when op is not installed")
	}
	if !strings.Contains(out.String(), "op` CLI isn't installed") {
		t.Errorf("must explain op is missing, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "developer.1password.com/docs/cli") {
		t.Errorf("must print the exact fix (install link), got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "?") {
		t.Errorf("must never prompt when op is missing, got:\n%s", out.String())
	}
}

func TestSetupProvisionKeys_OpNotSignedIn_FailsWithExactFix(t *testing.T) {
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		readFile: func(string) (string, error) { return "", os.ErrNotExist },
		run: func(name string, args ...string) (string, error) {
			if name == "op" && len(args) >= 1 && args[0] == "account" {
				return "", nil // no account configured
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, false) {
		t.Fatal("must fail when op has no account configured")
	}
	if !strings.Contains(out.String(), "op signin") {
		t.Errorf("must print the exact fix (op signin), got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "?") {
		t.Errorf("must never prompt when op isn't signed in, got:\n%s", out.String())
	}
}

// --- STEP 1: existing refs are confirmed, not re-pasted, but validated ----

// A ref that already exists, resolves, and is already synced is confirmed
// (not re-pasted) — but validation still resolves it via `op read`, and
// reconcile still finds sbx already in the recorded-same state so it adds no
// further op read / sbx secret set of its own.
func TestSetupProvisionKeys_RefsPresent_ConfirmedNotRepastedNoResync(t *testing.T) {
	refs := allRefs("", "", "")
	env, calls := stepEnv(t, refs, "anthropic openai google", "sk-val")
	// Record ref + matching digest (of the resolved value stepEnv will return,
	// "sk-val") so this is a true known-same no-op, not merely a ref-string
	// match — exercising the digest-aware skip path, not the legacy one.
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
	if !setupProvisionKeys(env, strings.NewReader(""), &out, true, false) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "paste a 1Password ref") {
		t.Errorf("an already-configured ref must never be re-pasted, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "1Password ref configured") {
		t.Errorf("must confirm each existing ref, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if countOccurrences(joined, "op read") != 3 {
		t.Errorf("STEP 1 must validate every configured ref via op read exactly once each:\n%s", joined)
	}
	if strings.Contains(joined, "secret set") {
		t.Errorf("an unchanged, already-synced ref (ref AND digest match) must never trigger sbx secret set:\n%s", joined)
	}
}

// An existing ref that does NOT resolve (op read fails / resolves empty)
// fails setup outright — it is never silently skipped, and nothing gets
// persisted as a result.
func TestSetupProvisionKeys_ExistingRefBroken_FailsNoPersist(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "") // op read resolves EMPTY
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, false) {
		t.Fatal("a broken existing ref must fail setup")
	}
	if !strings.Contains(out.String(), "does not resolve") {
		t.Errorf("must explain the ref doesn't resolve, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "pi-stack secret set ANTHROPIC_API_KEY") {
		t.Errorf("must print the exact fix command, got:\n%s", out.String())
	}
	if _, ok := syncedRef("ANTHROPIC_API_KEY"); ok {
		t.Error("a broken ref must never be recorded as synced")
	}
}

// --- STEP 1: missing refs, interactive ------------------------------------

// Every provider missing a ref, interactive TTY: each is prompted for exactly
// once (not a summary line), the valid answers are validated + persisted to
// BOTH op-refs.env and hostmode.env, and reconcile sets all three (missing
// from sbx) without asking.
func TestSetupProvisionKeys_MissingRefs_InteractivePromptsCollectsAndPersistsBoth(t *testing.T) {
	env, calls := stepEnv(t, "", "", "sk-val")
	in := strings.NewReader("op://V/anthropic/key\nop://V/openai/key\nop://V/gemini/key\n")
	var out bytes.Buffer
	if !setupProvisionKeys(env, in, &out, true, false) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	if n := strings.Count(out.String(), "paste a 1Password ref"); n != 3 {
		t.Errorf("must prompt exactly once per missing provider (3 total), got %d:\n%s", n, out.String())
	}
	opRefs := ""
	hostMode := ""
	// re-read through the env's own readFile so this checks the ACTUAL paths
	// setup writes, not a hardcoded guess.
	if c, err := env.readFile(defaultOpRefsPath(env)); err == nil {
		opRefs = c
	}
	if c, err := env.readFile(hostModeRefsPath(env)); err == nil {
		hostMode = c
	}
	for _, want := range []string{"ANTHROPIC_API_KEY=op://V/anthropic/key", "OPENAI_API_KEY=op://V/openai/key", "GEMINI_API_KEY=op://V/gemini/key"} {
		if !strings.Contains(opRefs, want) {
			t.Errorf("op-refs.env missing %q, got:\n%s", want, opRefs)
		}
		if !strings.Contains(hostMode, want) {
			t.Errorf("hostmode.env missing %q, got:\n%s", want, hostMode)
		}
	}
	joined := strings.Join(*calls, "\n")
	if strings.Count(joined, "sbx secret set") != 3 {
		t.Errorf("all three keys were missing from sbx and must all be set:\n%s", joined)
	}
}

// An invalid ref (not op://) or one that doesn't resolve reprompts, capped at
// providerKeyPromptAttempts; the resolved value is never echoed.
func TestSetupProvisionKeys_InvalidThenValidRef_Reprompts(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	in := strings.NewReader("not-a-ref\nop://V/anthropic/key\nop://V/openai/key\nop://V/gemini/key\n")
	var out bytes.Buffer
	if !setupProvisionKeys(env, in, &out, true, false) {
		t.Fatalf("expected eventual success, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "not a valid op:// ref") {
		t.Errorf("must explain the invalid paste, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "sk-val") {
		t.Error("resolved secret value must never be echoed")
	}
}

// EOF while prompting (no input available) is a hard failure, NOT "skip a
// provider" — a key is mandatory.
func TestSetupProvisionKeys_EOFDuringPrompt_Fails(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, false) {
		t.Fatal("EOF must fail setup, not silently skip the provider")
	}
	if !strings.Contains(out.String(), "no input") || !strings.Contains(out.String(), "required") {
		t.Errorf("must explain a ref is required, got:\n%s", out.String())
	}
}

// Repeated invalid input exhausts the retry budget and fails cleanly.
func TestSetupProvisionKeys_TooManyInvalidAttempts_Fails(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	in := strings.NewReader("nope\nnope\nnope\nnope\n")
	var out bytes.Buffer
	if setupProvisionKeys(env, in, &out, true, false) {
		t.Fatal("must fail after too many invalid attempts")
	}
	if !strings.Contains(out.String(), "too many invalid attempts") {
		t.Errorf("must say why it gave up, got:\n%s", out.String())
	}
}

// --- STEP 1: missing refs, non-interactive --------------------------------

// Non-interactive with missing refs: no prompts anywhere, the exact
// `pi-stack secret set` command is printed per missing provider, and setup
// fails.
func TestSetupProvisionKeys_MissingRefs_NonInteractive_ExactCommandsNoPrompt(t *testing.T) {
	env, _ := stepEnv(t, "", "", "sk-val")
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Fatal("missing refs non-interactively must fail")
	}
	if strings.Contains(out.String(), "paste a 1Password ref") {
		t.Errorf("must never prompt non-interactively, got:\n%s", out.String())
	}
	for _, want := range []string{
		"pi-stack secret set ANTHROPIC_API_KEY op://Vault/Item/field",
		"pi-stack secret set OPENAI_API_KEY op://Vault/Item/field",
		"pi-stack secret set GEMINI_API_KEY op://Vault/Item/field",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("must print exact fix command %q, got:\n%s", want, out.String())
		}
	}
}

// --- STEP 2/3: sbx reconciliation -----------------------------------------

// sbx missing one key (others already present+recorded-same, so those are
// no-ops): the missing one is set + recorded, no ask, and setup succeeds.
func TestReconcile_SbxMissingOneKey_SetsAndRecords(t *testing.T) {
	refs := allRefs("", "", "")
	env, calls := stepEnv(t, refs, "openai google", "sk-val") // anthropic missing from sbx
	if err := recordSyncedRefWithDigest("OPENAI_API_KEY", "op://v/openai/key", secretDigestHex("sk-val")); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRefWithDigest("GEMINI_API_KEY", "op://v/gemini/key", secretDigestHex("sk-val")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if !setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "sbx secret set -f -g anthropic -t sk-val") {
		t.Errorf("missing key must be set in sbx:\n%s", joined)
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/anthropic/key" {
		t.Errorf("synced ref not recorded: %q, %v", ref, ok)
	}
}

// sbx has the key AND the recorded ref is unchanged: reconcile adds no op
// read / sbx set of its own (STEP 1 still validates every ref once).
func TestReconcile_SbxPresentSameRef_NoOp(t *testing.T) {
	refs := allRefs("", "", "")
	env, calls := stepEnv(t, refs, "anthropic openai google", "sk-val")
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
	if !setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "secret set") {
		t.Errorf("unchanged, already-present refs (ref AND digest match) must not trigger sbx secret set:\n%s", joined)
	}
	if countOccurrences(joined, "op read") != 3 {
		t.Errorf("STEP 1 validation must still read every ref exactly once:\n%s", joined)
	}
}

// sbx has the key but the ref changed: ask ONCE, in a single BATCHED prompt
// (never once per provider) — 1Password is the source of truth, so the
// default is YES (replace). Declining is a REAL FAILURE now: setup must never
// report success while sbx and host mode would source a provider's key from
// two different places. Accepting sets + records the new ref.
func TestReconcile_SbxPresentChangedRef_OverwritePrompt(t *testing.T) {
	refs := allRefs("op://v/anthropic/key-NEW", "", "")

	// declines -> FAILS. The record stays at its OLD (stale) value (never
	// updated to the declined NEW ref), so the mismatch persists and a real
	// change keeps re-prompting on the next run.
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
	if setupProvisionKeys(env, strings.NewReader("n\n"), &out, true, false) {
		t.Fatalf("a declined batch overwrite must fail setup (1Password is the source of truth), got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Replace these sbx values from 1Password so sandbox and host mode use the same source? [Y/n]:") {
		t.Errorf("must ask the exact batched prompt, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "setup incomplete; sbx and host mode would use different sources") {
		t.Errorf("must explain WHY it failed, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "secret set") {
		t.Errorf("declined overwrite must not touch sbx:\n%s", joined)
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/anthropic/key-OLD" {
		t.Errorf("declined overwrite must leave the stale record as-is (still mismatched, so it re-prompts): %q, %v", ref, ok)
	}

	// accepts (the default, bare Enter) -> resolves + sets + records the NEW
	// ref, reusing STEP 1's already-validated value (no second op read of the
	// same ref).
	env2, calls2 := stepEnv(t, refs, "anthropic openai google", "sk-new-val")
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/anthropic/key-OLD"); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRef("OPENAI_API_KEY", "op://v/openai/key"); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRef("GEMINI_API_KEY", "op://v/gemini/key"); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	if !setupProvisionKeys(env2, strings.NewReader("\n"), &out2, true, false) {
		t.Fatalf("expected success (default answer is YES), got:\n%s", out2.String())
	}
	joined2 := strings.Join(*calls2, "\n")
	if countOccurrences(joined2, "op read op://v/anthropic/key-NEW") != 1 {
		t.Errorf("accepted overwrite must resolve the new ref exactly once (STEP 1, reused by reconcile):\n%s", joined2)
	}
	if !strings.Contains(joined2, "sbx secret set -f -g anthropic -t sk-new-val") {
		t.Errorf("accepted overwrite must set sbx:\n%s", joined2)
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/anthropic/key-NEW" {
		t.Errorf("accepted overwrite must record the new ref: %q, %v", ref, ok)
	}
}

// Several providers changed at once: reconcile asks ONE batched prompt naming
// all of them, not one prompt per provider.
func TestReconcile_MultipleChangedRefs_OneBatchedPrompt(t *testing.T) {
	refs := allRefs("op://v/anthropic/key-NEW", "op://v/openai/key-NEW", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key-OLD",
		"OPENAI_API_KEY":    "op://v/openai/key-OLD",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := recordSyncedRef(envVar, ref); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if !setupProvisionKeys(env, strings.NewReader("y\n"), &out, true, false) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	if n := strings.Count(out.String(), "Replace these sbx values from 1Password"); n != 1 {
		t.Errorf("must ask exactly ONE batched prompt for both changed providers, got %d:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "anthropic") || !strings.Contains(out.String(), "openai") {
		t.Errorf("the batched prompt must name both changed providers, got:\n%s", out.String())
	}
}

// --- non-interactive (non-TTY / CI) reconciliation ------------------------

// Non-interactive without --yes: no prompts anywhere (there's no TTY to ask),
// a genuinely missing (from sbx) key is still set, and a changed ref for an
// sbx-present key is NEVER overwritten without --yes — but that now makes
// setup FAIL overall (1Password is the source of truth; leaving sbx on a
// stale value is not success), with the exact rerun command printed.
func TestSetupProvisionKeys_NonInteractive_MissingSyncsButChangedRefFailsWithRerunGuidance(t *testing.T) {
	refs := allRefs("", "op://v/openai/key-NEW", "")
	// anthropic: recorded+present -> no-op. openai: present in sbx, ref CHANGED
	// (recorded key-OLD) -> must NOT be silently overwritten without --yes, and
	// must fail setup. gemini: missing from sbx -> must still be set.
	env, calls := stepEnv(t, refs, "anthropic openai", "resolved")
	if err := recordSyncedRefWithDigest("ANTHROPIC_API_KEY", "op://v/anthropic/key", secretDigestHex("resolved")); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRef("OPENAI_API_KEY", "op://v/openai/key-OLD"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Fatalf("a changed ref sbx wasn't allowed to replace (no --yes) must fail setup, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "?") {
		t.Errorf("non-interactive must never prompt, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "re-run this command with --yes to replace them from 1Password") {
		t.Errorf("must print the exact rerun guidance, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "sbx secret set -f -g google -t resolved") {
		t.Errorf("genuinely missing key must still be set:\n%s", joined)
	}
	if strings.Contains(joined, "sbx secret set -f -g openai") {
		t.Errorf("changed ref for an sbx-present key must NOT be overwritten without --yes:\n%s", joined)
	}
	if ref, ok := syncedRef("OPENAI_API_KEY"); !ok || ref != "op://v/openai/key-OLD" {
		t.Errorf("openai record must be untouched: %q, %v", ref, ok)
	}
}

// Non-interactive WITH --yes (assumeYes): a changed ref for an sbx-present
// key IS overwritten (no prompt, since there's no TTY to ask).
func TestSetupProvisionKeys_NonInteractiveAssumeYes_Overwrites(t *testing.T) {
	refs := allRefs("", "op://v/openai/key-NEW", "")
	env, calls := stepEnv(t, refs, "anthropic openai google", "resolved")
	if err := recordSyncedRefWithDigest("ANTHROPIC_API_KEY", "op://v/anthropic/key", secretDigestHex("resolved")); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRef("OPENAI_API_KEY", "op://v/openai/key-OLD"); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRefWithDigest("GEMINI_API_KEY", "op://v/gemini/key", secretDigestHex("resolved")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if !setupProvisionKeys(env, strings.NewReader(""), &out, false, true) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "sbx secret set -f -g openai -t resolved") {
		t.Errorf("--yes must overwrite a changed ref even non-interactively:\n%s", joined)
	}
	if ref, ok := syncedRef("OPENAI_API_KEY"); !ok || ref != "op://v/openai/key-NEW" {
		t.Errorf("record must be updated to the new ref: %q, %v", ref, ok)
	}
}

// --- STEP 2: hostmode.env must carry all three ----------------------------

// If mirroring into hostmode.env doesn't actually land all three refs (e.g.
// the file isn't writable), setup fails rather than declaring success with an
// incomplete host mode.
func TestSetupProvisionKeys_HostModeMissingRef_Fails(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	// Sabotage the mirror step: writes to hostmode.env silently fail (as if the
	// file were unwritable), while op-refs.env itself stays readable/writable.
	realWrite := env.writeFile
	env.writeFile = func(p string, d []byte, m os.FileMode) error {
		if strings.HasSuffix(p, "hostmode.env") {
			return os.ErrPermission
		}
		return realWrite(p, d, m)
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, true, false) {
		t.Fatal("must fail when hostmode.env doesn't end up with all three refs")
	}
	if !strings.Contains(out.String(), "hostmode.env") {
		t.Errorf("must explain the hostmode.env shortfall, got:\n%s", out.String())
	}
}

// --- final probe: fail-open only when sbx is truly unavailable -----------

// sbx entirely unavailable (not on PATH): even with all three refs valid and
// wired, setup succeeds — fail-open only for a genuinely absent sbx.
func TestSetupProvisionKeys_SbxUnavailable_FailsOpen(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "", "sk-val")
	env.lookPath = func(name string) (string, error) {
		if name == "sbx" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	var out bytes.Buffer
	if !setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Error("must fail open (true) when sbx can't be probed, refs are still fully valid")
	}
}

// sbx reachable but missing one of the three keys after reconciliation
// (declined overwrite is fine; a genuinely missing key that fails to sync is
// not) must be a real failure, not "any one key is enough".
func TestSetupProvisionKeys_FinalProbeRequiresAllThree(t *testing.T) {
	refs := allRefs("", "", "")
	// sbx probes fine but only reports two of the three names even after the
	// reconcile pass — simulate a sync that silently didn't take by having
	// `sbx secret set` succeed yet never actually add the name (a broken sbx).
	env, _ := stepEnv(t, refs, "anthropic openai", "sk-val")
	env.run = func(name string, args ...string) (string, error) {
		switch {
		case name == "op" && len(args) >= 1 && args[0] == "--version":
			return "2.0", nil
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return "sk-val", nil
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls":
			return "anthropic\nopenai\n", nil // google never shows up, even after "set"
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "set":
			return "", nil
		}
		return "", nil
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Fatal("must fail when the final probe can't confirm all three keys")
	}
}

// --- item 6: final probe fails CLOSED on a real sbx error, not open --------

// The final probe distinguishes sbx genuinely absent (fail open — already
// covered by TestSetupProvisionKeys_SbxUnavailable_FailsOpen) from sbx present
// but `sbx secret ls` failing on that LAST call: that must fail CLOSED with a
// diagnostic, never silently pass a box whose completeness couldn't actually
// be verified.
func TestSetupProvisionKeys_FinalProbe_SbxCommandFails_FailsClosed(t *testing.T) {
	refs := allRefs("", "", "")
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	// Record every ref as already synced so reconcile's own probe (the FIRST
	// `sbx secret ls` call) finds nothing changed and is a clean no-op —
	// isolating the failure to the FINAL probe's call, which is what this test
	// is about.
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := recordSyncedRefWithDigest(envVar, ref, secretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	env.run = func(name string, args ...string) (string, error) {
		switch {
		case name == "op" && len(args) >= 1 && args[0] == "--version":
			return "2.0", nil
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return "sk-val", nil
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls":
			calls++
			if calls == 1 {
				return "anthropic\nopenai\ngoogle\n", nil // reconcile's own probe succeeds
			}
			return "", fmt.Errorf("control plane down") // the FINAL probe fails
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "set":
			return "", nil
		}
		return "", nil
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Fatal("a failing final `sbx secret ls` must fail setup, not fail open")
	}
	if !strings.Contains(out.String(), "could not verify sbx has all three provider keys") {
		t.Errorf("must print the diagnostic, got:\n%s", out.String())
	}
}

// --- item 7: no cached secret value ever reaches printed output -----------

// syncProviderKeyToSbx must redact the resolved value from BOTH sbx's raw
// output and the wrapping Go error text before printing either — an exec
// error can echo the full argv (including "-t <value>") back verbatim.
func TestSyncProviderKeyToSbx_RedactsValueFromOutputAndError(t *testing.T) {
	const secretVal = "sk-should-never-print"
	env := shellEnv{
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" {
				// Simulate sbx echoing the full failed command (including the
				// secret value) back in its own stdout/stderr AND the wrapping Go
				// error carrying the same argv.
				return "sbx: command failed: sbx secret set -f -g anthropic -t " + secretVal,
					fmt.Errorf("exit status 1: -t %s", secretVal)
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	p := struct{ envVar, name string }{"ANTHROPIC_API_KEY", "anthropic"}
	if syncProviderKeyToSbx(env, &out, p, "op://v/a/k", secretVal) {
		t.Fatal("expected failure (sbx secret set errored)")
	}
	if strings.Contains(out.String(), secretVal) {
		t.Errorf("resolved secret value must never appear in printed output, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "***") {
		t.Errorf("expected the redaction marker in place of the value, got:\n%s", out.String())
	}
}

// --- new-item: known-same requires BOTH ref AND digest --------------------

// Rotation: the recorded ref is unchanged, but the value it now resolves to
// (in 1Password) has changed since we last synced it. Ref-string equality
// alone would wrongly call this "no-op"; the digest must catch it, treat it
// as changed/unknown, and route it through the same batched confirm (or
// --yes) path as a genuinely new ref.
func TestReconcile_SameRefRotatedValue_TreatedAsChanged(t *testing.T) {
	refs := allRefs("", "", "")
	// op read now resolves to a NEW value at the SAME ref (rotation in place).
	env, calls := stepEnv(t, refs, "anthropic openai google", "sk-rotated")
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		// Recorded digest is for the OLD value ("sk-val"), not what op read
		// resolves to now ("sk-rotated").
		if err := recordSyncedRefWithDigest(envVar, ref, secretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	// Non-interactive with --yes: no prompt needed, but the changed (rotated)
	// value must still be re-synced to sbx.
	if !setupProvisionKeys(env, strings.NewReader(""), &out, false, true) {
		t.Fatalf("expected success, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	for _, name := range []string{"anthropic", "openai", "google"} {
		if !strings.Contains(joined, "sbx secret set -f -g "+name+" -t sk-rotated") {
			t.Errorf("rotated value at an unchanged ref must be re-synced for %s:\n%s", name, joined)
		}
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/anthropic/key" {
		t.Errorf("ref must still be recorded after rotation resync: %q, %v", ref, ok)
	}
}

// Rotation without --yes, interactive decline: a same-ref rotated value must
// prompt (batched) exactly like a changed ref, and declining fails setup —
// it must never be silently treated as "same" just because the ref string
// didn't change.
func TestReconcile_SameRefRotatedValue_PromptsAndDeclineFails(t *testing.T) {
	refs := allRefs("", "", "")
	env, calls := stepEnv(t, refs, "anthropic openai google", "sk-rotated")
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
	if setupProvisionKeys(env, strings.NewReader("n\n"), &out, true, false) {
		t.Fatalf("declining a rotated-value overwrite must fail setup, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Replace these sbx values from 1Password") {
		t.Errorf("a same-ref rotated value must still trigger the batched confirm prompt, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "secret set") {
		t.Errorf("declined rotation overwrite must not touch sbx:\n%s", joined)
	}
}

// Legacy record: a ref recorded before the digest feature existed (ref only,
// no digest) is UNKNOWN, not known-same, even though the ref string matches
// exactly — it must go through the same batched overwrite confirmation as a
// brand-new ref, never silently skipped.
func TestReconcile_LegacyRecordNoDigest_TreatedAsUnknownNotSame(t *testing.T) {
	refs := allRefs("", "", "")
	env, calls := stepEnv(t, refs, "anthropic openai google", "sk-val")
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		// recordSyncedRef (no digest) is exactly the legacy shape: a store
		// written before this feature existed.
		if err := recordSyncedRef(envVar, ref); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Fatalf("a legacy record without --yes must fail setup (not silently treated as same), got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "kept sbx's existing value for") {
		t.Errorf("a legacy record must be routed through the batched overwrite confirmation, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "secret set") {
		t.Errorf("a declined/unconfirmed legacy record must not be silently resynced:\n%s", joined)
	}

	// With --yes, the legacy record DOES get resynced (and upgraded to carry a
	// digest going forward).
	env2, calls2 := stepEnv(t, refs, "anthropic openai google", "sk-val")
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := recordSyncedRef(envVar, ref); err != nil {
			t.Fatal(err)
		}
	}
	var out2 bytes.Buffer
	if !setupProvisionKeys(env2, strings.NewReader(""), &out2, false, true) {
		t.Fatalf("expected success with --yes, got:\n%s", out2.String())
	}
	joined2 := strings.Join(*calls2, "\n")
	if !strings.Contains(joined2, "sbx secret set -f -g anthropic -t sk-val") {
		t.Errorf("--yes must resync a legacy record:\n%s", joined2)
	}
	if digest := syncedRefDigest("ANTHROPIC_API_KEY"); digest != secretDigestHex("sk-val") {
		t.Errorf("resyncing a legacy record must upgrade it to carry a digest, got %q", digest)
	}
}
