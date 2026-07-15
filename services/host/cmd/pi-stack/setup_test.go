package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// captureSave returns a save func that records the last config it was handed,
// so tests can assert on what setup decided to persist.
func captureSave(dst **config.Config) func(*config.Config) error {
	return func(c *config.Config) error { *dst = c; return nil }
}

// TestSetup_NonInteractiveWritesConfig: `setup --account … --non-interactive`
// writes the account, enables gog, ensures the memory service, and NEVER tells
// the user to hand-edit the toml.
func TestSetup_NonInteractiveWritesConfig(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}

	var saved *config.Config
	runSetup(defaultCfg(), f.env(), sio,
		setupOpts{account: "me@x.com", assumeYes: true}, captureSave(&saved))

	if saved == nil {
		t.Fatal("setup must save the config")
	}
	if saved.GogAccount != "me@x.com" {
		t.Errorf("GogAccount = %q, want me@x.com", saved.GogAccount)
	}
	if !containsStr(saved.MCP, "gog") {
		t.Errorf("MCP = %v, want it to contain gog", saved.MCP)
	}
	if !containsStr(saved.Services, "memory") {
		t.Errorf("Services = %v, want it to contain memory", saved.Services)
	}

	out := buf.String()
	// The whole point: it never instructs hand-editing the toml.
	for _, forbidden := range []string{"edit the toml", "edit config.toml", "edit the file", "edit the config"} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("setup must not instruct hand-editing, found %q in:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "pi-stack serve") || !strings.Contains(out, "pi-stack run") {
		t.Errorf("expected the next-steps summary, got:\n%s", out)
	}
}

// TestSetup_NonTTYNoAccountGuides: non-TTY with no --account -> setup does what
// it can and guides the account via `pi-stack config set` commands (never file
// editing), and does not prompt/hang.
func TestSetup_NonTTYNoAccountGuides(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}

	var saved *config.Config
	steps := runSetup(defaultCfg(), f.env(), sio, setupOpts{}, captureSave(&saved))
	out := buf.String()

	if saved == nil || saved.GogAccount != "" {
		t.Errorf("no account should be written, got %+v", saved)
	}
	joined := strings.Join(steps, "\n")
	if !strings.Contains(joined, "pi-stack config set gog_account") {
		t.Errorf("expected a config-set gog_account step, got %v", steps)
	}
	if !strings.Contains(out, "pi-stack config set gog_account") {
		t.Errorf("expected the config-set guidance in output, got:\n%s", out)
	}
}

// TestSetup_ProviderSecrets: missing provider keys surface the exact
// `sbx secret set -g <key>` command; present ones don't.
func TestSetup_ProviderSecrets(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output:  map[string]string{"sbx secret ls": "anthropic openai\n"},
		ports:   map[int]bool{},
	}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	steps := runSetup(defaultCfg(), f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	joined := strings.Join(steps, "\n")
	if strings.Contains(joined, "sbx secret set -g anthropic") {
		t.Errorf("anthropic is set, should not be a step: %v", steps)
	}
	for _, want := range []string{"sbx secret set -g google", "sbx secret set -g github"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected missing-secret step %q, got %v", want, steps)
		}
	}
}

// TestSetup_SaveIdempotent: running the real save (Config.Save) twice against a
// temp config yields byte-stable output and the same values (never clobbers to
// a different state).
func TestSetup_SaveIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgPath)

	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	save := func(c *config.Config) error { return c.Save() }

	// Use Load() (not defaultCfg) so both runs go through applyDefaults exactly
	// like the real CLI path — otherwise a nil vs empty Plugins map alone diffs.
	start, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf1 bytes.Buffer
	runSetup(start, f.env(), setupIO{in: strings.NewReader(""), out: &buf1},
		setupOpts{account: "me@x.com", assumeYes: true}, save)
	first, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}

	// Re-run loads the written config, re-applies, saves again — must be stable.
	reload, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf2 bytes.Buffer
	runSetup(reload, f.env(), setupIO{in: strings.NewReader(""), out: &buf2},
		setupOpts{account: "me@x.com", assumeYes: true}, save)
	second, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("setup save not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestParseSetupArgs covers the flag forms.
func TestParseSetupArgs(t *testing.T) {
	got, err := parseSetupArgs([]string{"--account", "a@b.com", "--yes"})
	if err != nil || got.account != "a@b.com" || !got.assumeYes {
		t.Errorf("parseSetupArgs = %+v, err %v", got, err)
	}
	got, err = parseSetupArgs([]string{"--account=c@d.com", "--non-interactive"})
	if err != nil || got.account != "c@d.com" || !got.assumeYes {
		t.Errorf("parseSetupArgs eq form = %+v, err %v", got, err)
	}
	if _, err := parseSetupArgs([]string{"--account"}); err == nil {
		t.Error("expected error for --account with no value")
	}
	if _, err := parseSetupArgs([]string{"--bogus"}); err == nil {
		t.Error("expected error for unknown flag")
	}
	// --knowledge, space + equals forms.
	got, err = parseSetupArgs([]string{"--knowledge", "/tmp/kb", "--non-interactive"})
	if err != nil || got.knowledge != "/tmp/kb" || !got.assumeYes {
		t.Errorf("parseSetupArgs --knowledge = %+v, err %v", got, err)
	}
	got, err = parseSetupArgs([]string{"--knowledge=/tmp/kb2"})
	if err != nil || got.knowledge != "/tmp/kb2" {
		t.Errorf("parseSetupArgs --knowledge= = %+v, err %v", got, err)
	}
	if _, err := parseSetupArgs([]string{"--knowledge"}); err == nil {
		t.Error("expected error for --knowledge with no value")
	}
}

// TestSetup_KnowledgeFlagScaffolds: non-interactive `setup --knowledge <dir>`
// scaffolds the global OKF bundle at that path and wires it into config (the
// knowledge service is enabled and the bundle is added to knowledge_bundles).
func TestSetup_KnowledgeFlagScaffolds(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	kb := filepath.Join(t.TempDir(), "skb")
	absKB, _ := filepath.Abs(kb)

	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var saved *config.Config
	runSetup(cfg, f.env(), sio,
		setupOpts{account: "me@x.com", knowledge: kb, assumeYes: true}, captureSave(&saved))

	if saved == nil {
		t.Fatal("setup must save the config")
	}
	if !containsStr(saved.Services, "knowledge") {
		t.Errorf("Services = %v, want it to contain knowledge", saved.Services)
	}
	if !containsStr(saved.KnowledgeBundles, absKB) {
		t.Errorf("KnowledgeBundles = %v, want it to contain %s", saved.KnowledgeBundles, absKB)
	}
	// The bundle was actually scaffolded on disk (reused U3 init logic).
	if _, err := os.Stat(filepath.Join(kb, "index.md")); err != nil {
		t.Errorf("expected scaffolded index.md at %s: %v", kb, err)
	}
}

// TestSetup_KnowledgeAlreadyConfiguredSkips: with a bundle already in config,
// setup reports it and does NOT clobber it or scaffold the default dir.
func TestSetup_KnowledgeAlreadyConfiguredSkips(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(cfgDir, "config.toml"))

	cfg := defaultCfg()
	cfg.KnowledgeBundles = []string{"/existing/bundle"}
	cfg.AddService("knowledge")

	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}

	var saved *config.Config
	runSetup(cfg, f.env(), sio,
		setupOpts{account: "me@x.com", assumeYes: true}, captureSave(&saved))

	if saved == nil {
		t.Fatal("setup must save the config")
	}
	// No clobber: the one configured bundle is untouched.
	if len(saved.KnowledgeBundles) != 1 || saved.KnowledgeBundles[0] != "/existing/bundle" {
		t.Errorf("KnowledgeBundles = %v, want [/existing/bundle] unchanged", saved.KnowledgeBundles)
	}
	// The default KB dir must NOT have been scaffolded.
	if _, err := os.Stat(filepath.Join(cfgDir, "knowledge", "index.md")); err == nil {
		t.Errorf("default KB scaffolded despite an already-configured bundle")
	}
	if !strings.Contains(buf.String(), "already configured") {
		t.Errorf("expected an 'already configured' report, got:\n%s", buf.String())
	}
}

// TestSetup_KnowledgeDefaultNonInteractiveScaffolds: non-interactive setup with
// no --knowledge flag and no configured bundle scaffolds the DEFAULT global KB
// (<config-dir>/knowledge) and wires it.
func TestSetup_KnowledgeDefaultNonInteractiveScaffolds(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(cfgDir, "config.toml"))

	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var saved *config.Config
	runSetup(cfg, f.env(), sio,
		setupOpts{account: "me@x.com", assumeYes: true}, captureSave(&saved))

	defDir := filepath.Join(cfgDir, "knowledge")
	if _, err := os.Stat(filepath.Join(defDir, "index.md")); err != nil {
		t.Errorf("expected default KB scaffolded at %s: %v", defDir, err)
	}
	if saved == nil || !containsStr(saved.Services, "knowledge") {
		t.Errorf("Services = %v, want it to contain knowledge", saved.Services)
	}
	if !containsStr(saved.KnowledgeBundles, defDir) {
		t.Errorf("KnowledgeBundles = %v, want it to contain %s", saved.KnowledgeBundles, defDir)
	}
}

// containsStr reports whether list contains s.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestSetupSecretsSection_Seeds is the gate for the "nothing set me up" fix:
// the secrets section ALWAYS seeds op-refs.env (idempotently) and explains
// 1Password/op in plain terms.
func TestSetupSecretsSection_Seeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	f := fakeEnv{present: map[string]bool{}} // op not installed (non-blocking)
	var out bytes.Buffer
	var todos []string
	setupSecretsSection(f.env(), &out, func(c string) { todos = append(todos, c) })

	refs := filepath.Join(dir, "op-refs.env")
	if _, err := os.Stat(refs); err != nil {
		t.Fatalf("op-refs.env was not seeded at %s: %v", refs, err)
	}
	s := out.String()
	for _, want := range []string{"1Password", "op-refs.env", "pi-stack secret edit"} {
		if !strings.Contains(s, want) {
			t.Errorf("secrets section missing %q:\n%s", want, s)
		}
	}
	// Re-running must not clobber (idempotent).
	before, _ := os.ReadFile(refs)
	_ = os.WriteFile(refs, append(before, []byte("\nSLACK_TOKEN=op://v/i/f\n")...), 0o600)
	edited, _ := os.ReadFile(refs)
	setupSecretsSection(f.env(), &bytes.Buffer{}, func(string) {})
	after, _ := os.ReadFile(refs)
	if string(after) != string(edited) {
		t.Error("re-running setupSecretsSection clobbered an existing op-refs.env")
	}
}
