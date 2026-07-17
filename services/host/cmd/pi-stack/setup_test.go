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
// (the XDG DATA base, ~/.local/share/pi-stack/knowledge) and wires it.
func TestSetup_KnowledgeDefaultNonInteractiveScaffolds(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(cfgDir, "config.toml"))
	// The default bundle now lives under the DATA base (XDG storage reconciliation),
	// not beside config.toml — point XDG_DATA_HOME at a temp dir so it is hermetic.
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

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

	defDir := filepath.Join(dataHome, "pi-stack", "knowledge")
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

// TestSetupSecretsSection_SeedsWhenNeeded: when an integration that needs a
// credential was added, the secrets section seeds op-refs.env (idempotently) and
// explains 1Password/op in plain terms.
func TestSetupSecretsSection_SeedsWhenNeeded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	f := fakeEnv{present: map[string]bool{}} // op not installed (non-blocking)
	var out bytes.Buffer
	var todos []string
	setupSecretsSection(f.env(), &out, credNeeds, true, func(c string) { todos = append(todos, c) })

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
	setupSecretsSection(f.env(), &bytes.Buffer{}, credNeeds, true, func(string) {})
	after, _ := os.ReadFile(refs)
	if string(after) != string(edited) {
		t.Error("re-running setupSecretsSection clobbered an existing op-refs.env")
	}
}

// TestSetupSecretsSection_SkipsWhenNotNeeded: with no credential-needing
// integration, the section creates NO op-refs.env and prints the skip note.
func TestSetupSecretsSection_SkipsWhenNotNeeded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	f := fakeEnv{present: map[string]bool{}}
	var out bytes.Buffer
	setupSecretsSection(f.env(), &out, credNone, false, func(string) {})

	refs := filepath.Join(dir, "op-refs.env")
	if _, err := os.Stat(refs); err == nil {
		t.Errorf("op-refs.env must NOT be created when no credential is needed: %s", refs)
	}
	if !strings.Contains(out.String(), "Skipping") {
		t.Errorf("expected the skip note, got:\n%s", out.String())
	}
}

// TestSetup_ReadySummary: a present provider key + sbx on PATH -> "You are ready".
func TestSetup_ReadySummary(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output:  map[string]string{"sbx secret ls": "anthropic\n"},
		ports:   map[int]bool{},
	}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	runSetup(defaultCfg(), f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	if !strings.Contains(buf.String(), "You are ready") {
		t.Errorf("expected a ready summary, got:\n%s", buf.String())
	}
}

// TestSetup_GithubOnlyNotReady: github is NOT a model provider, so a github-only
// secret set does NOT satisfy readiness — setup reports "NOT fully ready".
func TestSetup_GithubOnlyNotReady(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output:  map[string]string{"sbx secret ls": "github\n"},
		ports:   map[int]bool{},
	}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	runSetup(defaultCfg(), f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	s := buf.String()
	// github is shown as set in Step 1...
	if !strings.Contains(s, "✓ github set") {
		t.Errorf("expected github shown as set in Step 1, got:\n%s", s)
	}
	// ...but it does NOT satisfy readiness.
	if !strings.Contains(s, "NOT fully ready") {
		t.Errorf("github-only must be NOT fully ready, got:\n%s", s)
	}
}

// TestSetup_NotReadySummary: no key and no sbx -> the "NOT fully ready" block
// leads with the sbx-install guidance (the real blocker is that sbx is not
// installed, not just a missing key), and still mentions the missing key.
func TestSetup_NotReadySummary(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	runSetup(defaultCfg(), f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	s := buf.String()
	if !strings.Contains(s, "NOT fully ready") {
		t.Errorf("expected the not-ready summary, got:\n%s", s)
	}
	// The real blocker is a missing sbx, so the install guidance must lead.
	if !strings.Contains(s, "Docker Sandboxes (sbx) is not installed") ||
		!strings.Contains(s, "https://docs.docker.com/sandboxes/") {
		t.Errorf("expected the sbx-install guidance for the no-sbx case, got:\n%s", s)
	}
	// The misleading "no provider key" verdict must NOT be the sole message.
	if strings.Contains(s, "no provider key is set") {
		t.Errorf("the old misleading no-provider-key verdict must not appear for a no-sbx host, got:\n%s", s)
	}
}

// TestSetup_ProbeFailedNotReady: sbx IS on PATH but `sbx secret ls` errors. Setup
// must NOT assert the keys are missing: Step 1 says they could not be verified
// (no per-key ✗ / `sbx secret set`), and the not-ready verdict uses the distinct
// "could not be verified" message, NOT the confirmed "no provider key" one.
func TestSetup_ProbeFailedNotReady(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{
		present: map[string]bool{"sbx": true}, // on PATH, but no `sbx secret ls` output -> run errors
		output:  map[string]string{},
		ports:   map[int]bool{},
	}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	steps := runSetup(defaultCfg(), f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	s := buf.String()
	if !strings.Contains(s, "NOT fully ready") {
		t.Errorf("expected a not-ready summary, got:\n%s", s)
	}
	if !strings.Contains(s, "could not be verified") {
		t.Errorf("expected the 'could not be verified' verdict for a failed probe, got:\n%s", s)
	}
	// It must NOT claim keys are missing.
	if strings.Contains(s, "no model provider key is set") {
		t.Errorf("probe-failed must NOT use the confirmed no-key verdict, got:\n%s", s)
	}
	// Step 1 must not print per-key `sbx secret set` TODOs (that would assert absence).
	for _, step := range steps {
		if strings.HasPrefix(step, "sbx secret set -g") {
			t.Errorf("probe-failed must NOT emit per-key `sbx secret set` steps, got %v", steps)
		}
	}
}

// setHostResolver overrides the package-level pi-stack-host resolver for a test
// so setup's local-vs-remote MCP partition reads a fake `mcp --list`. It
// restores the original on cleanup.
func setHostResolver(t *testing.T, path string) {
	t.Helper()
	old := hostBinaryResolver
	hostBinaryResolver = func() (string, error) { return path, nil }
	t.Cleanup(func() { hostBinaryResolver = old })
}

// TestSetup_NeedsCredsFromLocalMembership: needsCreds (and thus the Step 4
// op-refs seeding) is driven by LOCAL-non-gog membership, NOT `cfg.MCP - gog`.
// A local stdio server (slack, per `pi-stack-host mcp --list`) sets needsCreds
// and seeds op-refs.env; a remote-only cfg (notion, NOT in the local set) does
// NOT seed op-refs and does NOT set needsCreds.
func TestSetup_NeedsCredsFromLocalMembership(t *testing.T) {
	t.Run("local server needs creds", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "config.toml")
		t.Setenv("PI_STACK_CONFIG", cfgFile)
		setHostResolver(t, "/usr/bin/pi-stack-host")
		f := fakeEnv{
			present: map[string]bool{},
			output:  map[string]string{"/usr/bin/pi-stack-host mcp --list": "slack\n"},
			envVars: map[string]string{"PI_STACK_CONFIG": cfgFile},
			ports:   map[int]bool{},
		}
		cfg := defaultCfg()
		cfg.MCP = []string{"slack"}
		var buf bytes.Buffer
		sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
		runSetup(cfg, f.env(), sio, setupOpts{assumeYes: true},
			func(*config.Config) error { return nil })
		s := buf.String()
		if strings.Contains(s, "Step 4 of 4 — Integration credentials   (skipped)") {
			t.Errorf("slack is a local server: Step 4 must NOT be skipped, got:\n%s", s)
		}
		refs := filepath.Join(filepath.Dir(cfgFile), "op-refs.env")
		if _, err := os.Stat(refs); err != nil {
			t.Errorf("expected op-refs.env seeded for a local credential server at %s: %v", refs, err)
		}
	})

	t.Run("remote-only does not need creds", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "config.toml")
		t.Setenv("PI_STACK_CONFIG", cfgFile)
		setHostResolver(t, "/usr/bin/pi-stack-host")
		f := fakeEnv{
			present: map[string]bool{},
			output:  map[string]string{"/usr/bin/pi-stack-host mcp --list": "slack\n"}, // notion NOT local
			envVars: map[string]string{"PI_STACK_CONFIG": cfgFile},
			ports:   map[int]bool{},
		}
		cfg := defaultCfg()
		cfg.MCP = []string{"notion"}
		var buf bytes.Buffer
		sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
		runSetup(cfg, f.env(), sio, setupOpts{assumeYes: true},
			func(*config.Config) error { return nil })
		s := buf.String()
		if !strings.Contains(s, "Step 4 of 4 — Integration credentials   (skipped)") {
			t.Errorf("notion is a remote catalog server: Step 4 must be skipped, got:\n%s", s)
		}
		refs := filepath.Join(filepath.Dir(cfgFile), "op-refs.env")
		if _, err := os.Stat(refs); err == nil {
			t.Errorf("op-refs.env must NOT be seeded for a remote-only cfg: %s", refs)
		}
	})
}

// TestSetup_CredentialFreeLocalServerNotCategorical: a local server we cannot
// prove needs a credential (pio, in the `mcp --list` set) triggers Step 4, but
// the copy MUST stay conditional. It must NOT categorically assert the
// integration "needs 1Password creds"; conditional wording ("MIGHT need") is the
// honest form, since there is no per-server credential metadata.
func TestSetup_CredentialFreeLocalServerNotCategorical(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)
	setHostResolver(t, "/usr/bin/pi-stack-host")
	f := fakeEnv{
		present: map[string]bool{},
		output:  map[string]string{"/usr/bin/pi-stack-host mcp --list": "pio\n"},
		envVars: map[string]string{"PI_STACK_CONFIG": cfgFile},
		ports:   map[int]bool{},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"pio"}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	runSetup(cfg, f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	s := buf.String()
	// The recovery note must be conditional, never a categorical requirement.
	if strings.Contains(s, "an integration needs 1Password creds") {
		t.Errorf("recovery note must not assert a categorical requirement for pio:\n%s", s)
	}
	if !strings.Contains(s, "MIGHT need a password") {
		t.Errorf("expected the conditional 'MIGHT need a password' recovery note, got:\n%s", s)
	}
}

// TestSetup_CredIntrospectionFailureIsUnknown: cfg has a non-gog server (slack)
// but the local-set probe fails (`pi-stack-host mcp --list` errors), so the
// credential state is UNKNOWN, not none. Step 4 must NOT print the confident
// "no integration needs a password. Skipping" copy; it must print the honest
// "could not determine" copy and add a pi-stack mcp register recovery TODO.
func TestSetup_CredIntrospectionFailureIsUnknown(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)
	setHostResolver(t, "/usr/bin/pi-stack-host")
	f := fakeEnv{
		present: map[string]bool{},
		// No `mcp --list` output registered -> env.run returns an error -> probe fails.
		output:  map[string]string{},
		envVars: map[string]string{"PI_STACK_CONFIG": cfgFile},
		ports:   map[int]bool{},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	steps := runSetup(cfg, f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	s := buf.String()
	if strings.Contains(s, "You added no integrations that need a password. Skipping.") {
		t.Errorf("probe failed with slack configured: must NOT print the confident skip, got:\n%s", s)
	}
	if !strings.Contains(s, "Could not determine whether your integrations need credentials") {
		t.Errorf("expected the honest 'could not determine' copy, got:\n%s", s)
	}
	if !containsStr(steps, "pi-stack mcp register") {
		t.Errorf("expected a 'pi-stack mcp register' recovery TODO, got %v", steps)
	}
}

// TestSetup_TolerantRegistration: setup drives the tolerant registerServers path
// (SBX_MCP_URL set, so registration actually runs). A local stdio server (slack)
// gets the exact `sbx mcp add slack` call; a remote gateway-catalog name (notion)
// is SKIPPED, never registered as local.
func TestSetup_TolerantRegistration(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)
	setHostResolver(t, "/usr/bin/pi-stack-host")
	f := fakeEnv{
		present: map[string]bool{"sbx": true}, // no op -> bare registration
		output: map[string]string{
			"/usr/bin/pi-stack-host mcp --list": "slack\n", // notion NOT local
			// the exact bare `sbx mcp add slack` call the registrar builds:
			"sbx mcp add slack --command /usr/bin/pi-stack-host --args mcp --args slack": "ok",
		},
		envVars: map[string]string{
			"PI_STACK_CONFIG": cfgFile,
			"SBX_MCP_URL":     "https://gateway.docker.com",
		},
		ports: map[int]bool{},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack", "notion"}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	runSetup(cfg, f.env(), sio, setupOpts{assumeYes: true},
		func(*config.Config) error { return nil })
	s := buf.String()
	// slack (local) is registered via the exact sbx mcp add call.
	if !strings.Contains(s, "registered: slack") {
		t.Errorf("expected slack (local) registered, got:\n%s", s)
	}
	// notion (remote catalog) is skipped, never registered as local.
	if !strings.Contains(s, "notion: gateway-catalog server, not locally registered") {
		t.Errorf("expected notion skipped as a gateway-catalog server, got:\n%s", s)
	}
	if strings.Contains(s, "registered: notion") {
		t.Errorf("notion (remote) must NOT be registered as local, got:\n%s", s)
	}
}

// TestSetup_GogNeedsAuth: gog absent / auth status errors -> setup labels gog
// "needs auth" (never "configured"), adds the gog-auth-login step, and still
// writes gog into cfg.MCP.
func TestSetup_GogNeedsAuth(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}
	var saved *config.Config
	runSetup(defaultCfg(), f.env(), sio,
		setupOpts{account: "me@x.com", assumeYes: true}, captureSave(&saved))
	s := buf.String()
	if !strings.Contains(s, "needs auth") || !strings.Contains(s, "gog auth login") {
		t.Errorf("expected gog 'needs auth' + 'gog auth login', got:\n%s", s)
	}
	if strings.Contains(s, "gog configured") {
		t.Errorf("gog must not be reported configured before auth:\n%s", s)
	}
	if saved == nil || !containsStr(saved.MCP, "gog") {
		t.Errorf("gog must still be written to cfg.MCP, got %+v", saved)
	}
}
