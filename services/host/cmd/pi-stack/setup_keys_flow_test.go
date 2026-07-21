// setup_keys_flow_test.go — the setupProvisionKeys rework: a 1Password ref
// is required per provider (STEP 1, setup.go), but sbx reconciliation
// (STEP 2, secret_sync.go) never re-pastes or re-syncs a key that's already
// in the known-good state, tracked via the launcher-owned synced-ref record
// (syncedrefs.go) since sbx secret values are write-only.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stepEnv sets up a temp config+state dir (so syncedrefs.go's real os-backed
// store works) and returns a shellEnv whose op-refs.env content, `sbx secret
// ls` output, and `op read` result are all controllable.
func stepEnv(t *testing.T, refsContent, sbxLsOut string, opReadVal string) (shellEnv, *[]string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	files := map[string]string{"/cfg/pi-stack/op-refs.env": refsContent}
	calls := &[]string{}
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
				return sbxLsOut, nil
			case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "set":
				return "", nil
			}
			return "", nil
		},
	}
	return env, calls
}

// --- STEP 1: op-refs.env -----------------------------------------------

// A ref that already exists is CONFIRMED (Enter keeps it) rather than
// blindly re-pasted, and since sbx already has the synced value, STEP 2 must
// not touch op/sbx either.
func TestSetupProvisionKeys_RefPresent_ConfirmKeepsNoResync(t *testing.T) {
	env, calls := stepEnv(t, "ANTHROPIC_API_KEY=op://v/anthropic/key\n", "anthropic\n", "sk-val\n")
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/anthropic/key"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	setupProvisionKeys(env, strings.NewReader("\n\n\n"), &out, true, false)

	if !strings.Contains(out.String(), "op://v/anthropic/key") {
		t.Errorf("must show the stored ref for confirmation, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "op read") || strings.Contains(joined, "secret set") {
		t.Errorf("unchanged synced ref must not trigger op read or sbx secret set:\n%s", joined)
	}
}

// No ref yet: interactive prompts for one.
func TestSetupProvisionKeys_RefAbsent_Prompted(t *testing.T) {
	env, _ := stepEnv(t, "", "\n", "sk-val\n")
	var out bytes.Buffer
	setupProvisionKeys(env, strings.NewReader("op://v/anthropic/key\nop://v/openai/key\nop://v/google/key\n"), &out, true, false)
	if !strings.Contains(out.String(), "anthropic:") {
		t.Errorf("must prompt for a ref when none exists, got:\n%s", out.String())
	}
}

// --- STEP 2: sbx reconciliation -----------------------------------------

// sbx missing the key: set + record, no ask.
func TestReconcile_SbxMissing_SetsAndRecords(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\n"
	env, calls := stepEnv(t, refs, "\n", "sk-val\n") // sbx has nothing
	var out bytes.Buffer
	setupProvisionKeys(env, strings.NewReader(""), &out, false, false)

	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "op read op://v/anthropic/key") {
		t.Errorf("missing key must be resolved via op read:\n%s", joined)
	}
	if !strings.Contains(joined, "sbx secret set -f -g anthropic -t sk-val") {
		t.Errorf("missing key must be set in sbx:\n%s", joined)
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/anthropic/key" {
		t.Errorf("synced ref not recorded: %q, %v", ref, ok)
	}
}

// sbx has the key AND the recorded ref is unchanged: no op read, no sbx set.
func TestReconcile_SbxPresentSameRef_NoOp(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\n"
	env, calls := stepEnv(t, refs, "anthropic\n", "sk-val\n")
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/anthropic/key"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	setupProvisionKeys(env, strings.NewReader(""), &out, false, false)

	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "op read") {
		t.Errorf("unchanged ref must not trigger op read:\n%s", joined)
	}
	if strings.Contains(joined, "secret set") {
		t.Errorf("unchanged ref must not trigger sbx secret set:\n%s", joined)
	}
}

// sbx has the key but the ref changed: ask before overwriting. Yes -> sets +
// records; No -> leaves sbx (and the record) alone.
func TestReconcile_SbxPresentChangedRef_OverwritePrompt(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key-NEW\n"

	// declines -> left alone; the record stays at its OLD (stale) value rather
	// than being updated to the declined NEW ref, so the mismatch persists and
	// a real change keeps re-prompting on the next run.
	// (STEP 1 prompts once per provider in providerKeyRefOrder — anthropic has
	// a ref (blank keeps it), openai/google don't (blank skips them) — so the
	// STEP 2 overwrite answer for anthropic is the 4th line.)
	env, calls := stepEnv(t, refs, "anthropic\n", "sk-val\n")
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/anthropic/key-OLD"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	setupProvisionKeys(env, strings.NewReader("\n\n\nn\n"), &out, true, false)
	if !strings.Contains(out.String(), "overwrite it with the 1Password ref?") {
		t.Errorf("must ask before overwriting a changed ref, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if strings.Contains(joined, "op read") || strings.Contains(joined, "secret set") {
		t.Errorf("declined overwrite must not touch op/sbx:\n%s", joined)
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/anthropic/key-OLD" {
		t.Errorf("declined overwrite must leave the stale record as-is (still mismatched, so it re-prompts): %q, %v", ref, ok)
	}

	// accepts -> resolves + sets + records the NEW ref
	env2, calls2 := stepEnv(t, refs, "anthropic\n", "sk-new-val\n")
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/anthropic/key-OLD"); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	setupProvisionKeys(env2, strings.NewReader("\n\n\ny\n"), &out2, true, false)
	joined2 := strings.Join(*calls2, "\n")
	if !strings.Contains(joined2, "op read op://v/anthropic/key-NEW") {
		t.Errorf("accepted overwrite must op-read the new ref:\n%s", joined2)
	}
	if !strings.Contains(joined2, "sbx secret set -f -g anthropic -t sk-new-val") {
		t.Errorf("accepted overwrite must set sbx:\n%s", joined2)
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/anthropic/key-NEW" {
		t.Errorf("accepted overwrite must record the new ref: %q, %v", ref, ok)
	}
}

// --- non-interactive (non-TTY / CI) --------------------------------------

// Non-interactive: no prompts anywhere, missing keys still get set, and a
// changed ref is NEVER overwritten without --yes.
func TestSetupProvisionKeys_NonInteractive_NoPromptsSetsOnlyMissingNoOverwrite(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\nOPENAI_API_KEY=op://v/openai/key-NEW\n"
	// anthropic: missing from sbx -> must be set.
	// openai: present in sbx, ref CHANGED (recorded key-OLD) -> must NOT be
	// overwritten without --yes.
	env, calls := stepEnv(t, refs, "openai\n", "resolved\n")
	if err := recordSyncedRef("OPENAI_API_KEY", "op://v/openai/key-OLD"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	setupProvisionKeys(env, strings.NewReader(""), &out, false, false)

	if strings.Contains(out.String(), "?") {
		t.Errorf("non-interactive must never prompt, got:\n%s", out.String())
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "sbx secret set -f -g anthropic -t resolved") {
		t.Errorf("genuinely missing key must still be set:\n%s", joined)
	}
	if strings.Contains(joined, "op://v/openai/key-NEW") {
		t.Errorf("changed ref for an sbx-present key must NOT be resolved/overwritten without --yes:\n%s", joined)
	}
	if ref, ok := syncedRef("OPENAI_API_KEY"); !ok || ref != "op://v/openai/key-OLD" {
		t.Errorf("openai record must be untouched: %q, %v", ref, ok)
	}
}

// Non-interactive WITH --yes (assumeYes): a changed ref for an sbx-present
// key IS overwritten (no prompt, since there's no TTY to ask).
func TestSetupProvisionKeys_NonInteractiveAssumeYes_Overwrites(t *testing.T) {
	refs := "OPENAI_API_KEY=op://v/openai/key-NEW\n"
	env, calls := stepEnv(t, refs, "openai\n", "resolved\n")
	if err := recordSyncedRef("OPENAI_API_KEY", "op://v/openai/key-OLD"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	setupProvisionKeys(env, strings.NewReader(""), &out, false, true)

	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "sbx secret set -f -g openai -t resolved") {
		t.Errorf("--yes must overwrite a changed ref even non-interactively:\n%s", joined)
	}
	if ref, ok := syncedRef("OPENAI_API_KEY"); !ok || ref != "op://v/openai/key-NEW" {
		t.Errorf("record must be updated to the new ref: %q, %v", ref, ok)
	}
}

// --- synced-refs.json symlink safety --------------------------------------

func TestSyncedRefsStore_SymlinkSafe(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfgDir, "synced-refs.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	if _, err := loadSyncedRefsStore(); err == nil {
		t.Error("load must refuse a symlinked store")
	}
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/a/k"); err == nil {
		t.Error("save must refuse a symlinked store")
	}
	// The symlink target must be untouched (never written through).
	b, err := os.ReadFile(target)
	if err != nil || string(b) != `{"version":1}` {
		t.Errorf("symlink target must not be modified: %q, err=%v", string(b), err)
	}
}
