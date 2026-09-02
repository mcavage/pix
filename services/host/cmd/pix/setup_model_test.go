package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/pixhome"
	"pix/host/sys"
)

// modelSetupHome creates a temp PIX_HOME with the exact scaffolded default
// environment EnsureDefaultEnvironment would have written, and returns its
// resolved pixhome.Paths plus the pix.toml path.
func modelSetupHome(t *testing.T, refs string) (home pixhome.Paths, sidecarPath string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_HOME", dir)
	home, err := pixhome.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if refs != "" {
		if err := os.WriteFile(filepath.Join(dir, "secrets.env"), []byte(refs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := home.EnvironmentDir("default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sidecarPath = filepath.Join(root, "pix.toml")
	body := `schema = 1

[models]
# main = "anthropic/claude-sonnet-5"  # a model NAME (provider/id); empty
# means the shipped default for whichever provider this home configures
# (never Pi's own native default). Run 'pix env show' to see the model
# that will answer and the rule that chose it.

[memory]
scope = "shared"
`
	if err := os.WriteFile(sidecarPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, sidecarPath
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSetupModelSelection_NonInteractiveNeverPromptsOrWrites: a script (no
// TTY) gets no prompt and secrets.env/pix.toml stay untouched, even with a
// configured provider and a scaffolded environment.
func TestSetupModelSelection_NonInteractiveNeverPromptsOrWrites(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("2\n"), Interactive: false}

	setupModelSelection(d, home, defaultShellEnv(), true)

	if out.Len() != 0 {
		t.Errorf("non-interactive setup must print nothing new: %s", out.String())
	}
	if got := readFile(t, sidecar); got != before {
		t.Errorf("non-interactive setup must never write pix.toml:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestSetupModelSelection_NoProviderConfiguredSkipsSilently: with no
// provider ref configured, the picker has nothing to list and prints
// nothing (setupCredentials already covers that story).
func TestSetupModelSelection_NoProviderConfiguredSkipsSilently(t *testing.T) {
	home, sidecar := modelSetupHome(t, "")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("1\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	if out.Len() != 0 {
		t.Errorf("no configured provider must print nothing from the picker: %s", out.String())
	}
	if got := readFile(t, sidecar); got != before {
		t.Errorf("no configured provider must never write pix.toml")
	}
}

// TestSetupModelSelection_EmptyInputKeepsFallbackNoWrite: the default
// answer is "keep the shown fallback" — a bare Enter changes nothing.
func TestSetupModelSelection_EmptyInputKeepsFallbackNoWrite(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	if got := readFile(t, sidecar); got != before {
		t.Errorf("empty input must never write pix.toml:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if !strings.Contains(out.String(), "keeping the fallback: anthropic/claude-opus-5") {
		t.Errorf("want the fallback named on empty input:\n%s", out.String())
	}
}

// TestSetupModelSelection_InvalidChoiceKeepsFallbackNoWrite: a choice
// outside the listed range degrades to the same default, never an error.
func TestSetupModelSelection_InvalidChoiceKeepsFallbackNoWrite(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("not-a-number\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	if got := readFile(t, sidecar); got != before {
		t.Errorf("an invalid choice must never write pix.toml")
	}
	if !strings.Contains(out.String(), "keeping the fallback") {
		t.Errorf("want the fallback named on an invalid choice:\n%s", out.String())
	}
}

// TestSetupModelSelection_ChoosingListedModelWritesFullyQualifiedMain proves
// the whole write path: choosing an entry from the printed list records
// EXACTLY that fully-qualified catalog id under [models].main, and nothing
// else in the file changes.
func TestSetupModelSelection_ChoosingListedModelWritesFullyQualifiedMain(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	var out bytes.Buffer
	// Catalog order for anthropic: 1) claude-opus-5 (default/fallback)
	// 2) claude-fable-5 3) claude-sonnet-5 4) claude-haiku-4-5.
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("3\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	got := readFile(t, sidecar)
	if !strings.Contains(got, `main = "anthropic/claude-sonnet-5"`) {
		t.Errorf("want [models].main recorded with the fully qualified id:\n%s", got)
	}
	if !strings.Contains(out.String(), `recorded [models].main = "anthropic/claude-sonnet-5"`) {
		t.Errorf("want setup to confirm what it recorded:\n%s", out.String())
	}
	// Nothing else in the file was touched: the memory section and the
	// original commented guidance both survive verbatim.
	if !strings.Contains(got, "[memory]\nscope = \"shared\"") {
		t.Errorf("unrelated sidecar content must survive untouched:\n%s", got)
	}
	activeMainLines := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "main = ") {
			activeMainLines++
		}
	}
	if activeMainLines != 1 {
		t.Errorf("want exactly one active main= line:\n%s", got)
	}
}

// TestSetupModelSelection_ExistingEnvironmentNeverRewritten proves W1: when
// the default environment was NOT scaffolded this run, the picker never
// prompts and never writes — it only displays.
func TestSetupModelSelection_ExistingEnvironmentNeverRewritten(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	// A reader that WOULD answer "3" if ever read from — proving the
	// existing-environment path never even asks.
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("3\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), false)

	if got := readFile(t, sidecar); got != before {
		t.Errorf("an existing default environment must never be rewritten:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if strings.Contains(out.String(), "current models for the providers") {
		t.Errorf("an existing default environment must not be offered the picker:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "already exists; it will use anthropic/claude-opus-5") {
		t.Errorf("want the display-only fallback line naming the model and file:\n%s", out.String())
	}
	if !strings.Contains(out.String(), sidecar) {
		t.Errorf("want the exact pix.toml path named for a hand edit:\n%s", out.String())
	}
}

// TestSetupModelSelection_RefusesToOverwriteAnAlreadySetMain: a second run
// against a pix.toml that already declares [models].main (e.g. a hand edit
// mid-run) refuses rather than clobbering it.
func TestSetupModelSelection_RefusesToOverwriteAnAlreadySetMain(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	existing := readFile(t, sidecar)
	existing = strings.Replace(existing, `# main = "anthropic/claude-sonnet-5"`, `main = "anthropic/claude-haiku-4-5"`, 1)
	if err := os.WriteFile(sidecar, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("3\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	got := readFile(t, sidecar)
	if got != existing {
		t.Errorf("an already-declared main must never be overwritten:\nbefore:\n%s\nafter:\n%s", existing, got)
	}
	if !strings.Contains(out.String(), "could not record the model choice") {
		t.Errorf("want the refusal reported:\n%s", out.String())
	}
}
