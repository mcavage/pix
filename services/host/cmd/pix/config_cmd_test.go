package main

// config_cmd_test.go proves configCmd (config_cmd.go) end to end through the
// SAME kong entry point production uses (cli.RunRoot), against a scratch
// $PIX_CONFIG so a run never touches a real user's config. configCmd is not
// wired into rootCmd yet (see config_cmd.go's integration note), so these
// tests parse a small root local to this file rather than rootCmd — exactly
// the shape rootCmd itself will take once the field is swapped in.

import (
	"bytes"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

// runConfigParse drives the SHIPPED root, so every case below exercises the
// wired verb (`Config configCmd` in rootCmd) rather than a stand-in struct.
func runConfigParse(argv []string, d *cli.Deps) error {
	return cli.RunRoot[rootCmd]("pix", "", "", argv, d)
}

func configDeps(t *testing.T) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("PIX_CONFIG", t.TempDir()+"/config.toml")
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("")}, &out, &errb
}

// ── show ─────────────────────────────────────────────────────────────────

func TestConfigCmd_Show(t *testing.T) {
	d, out, _ := configDeps(t)
	if err := runConfigParse([]string{"config", "show"}, d); err != nil {
		t.Fatalf("config show: %v", err)
	}
	got := out.String()
	for _, want := range []string{"# path: ", "config.toml", "run_intent = "} {
		if !strings.Contains(got, want) {
			t.Errorf("config show missing %q, got:\n%s", want, got)
		}
	}
}

// TestConfigCmd_ShowIsDefault: bare `pix config` is `show`, matching the
// legacy dispatcher's `sub := "show"` fallback.
func TestConfigCmd_ShowIsDefault(t *testing.T) {
	d, out, _ := configDeps(t)
	if err := runConfigParse([]string{"config"}, d); err != nil {
		t.Fatalf("bare config: %v", err)
	}
	if !strings.Contains(out.String(), "# path: ") {
		t.Errorf("bare config should behave like show, got:\n%s", out.String())
	}
}

// ── path ─────────────────────────────────────────────────────────────────

func TestConfigCmd_Path(t *testing.T) {
	d, out, _ := configDeps(t)
	if err := runConfigParse([]string{"config", "path"}, d); err != nil {
		t.Fatalf("config path: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != config.Path() {
		t.Errorf("config path = %q, want %q", got, config.Path())
	}
}

func TestConfigCmd_PathOpRefs(t *testing.T) {
	d, out, _ := configDeps(t)
	if err := runConfigParse([]string{"config", "path", "op-refs"}, d); err != nil {
		t.Fatalf("config path op-refs: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != config.OpRefsPath() {
		t.Errorf("config path op-refs = %q, want %q", got, config.OpRefsPath())
	}
}

// TestConfigCmd_PathBadArgument: any other trailing token is a typo, not a
// silently-ignored arg — exit 2 (usage), matching the legacy dispatcher.
func TestConfigCmd_PathBadArgument(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "path", "bogus"}, d)
	if err == nil {
		t.Fatal("expected an error for `config path bogus`")
	}
	if cli.ExitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2 (usage)", cli.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "op-refs") {
		t.Errorf("error should mention op-refs, got %v", err)
	}
}

// ── get ──────────────────────────────────────────────────────────────────

// TestConfigCmd_GetStdoutIsClean is the contract the Makefile's operational
// targets depend on ($(shell pix config get mcp)): stdout carries ONLY the
// resolved value, no decoration, no trailing blank line beyond the one
// newline a shell command substitution strips anyway.
func TestConfigCmd_GetStdoutIsClean(t *testing.T) {
	d, out, errb := configDeps(t)
	if err := runConfigParse([]string{"config", "get", "run_intent"}, d); err != nil {
		t.Fatalf("config get run_intent: %v", err)
	}
	if got := out.String(); got != "overlord\n" {
		t.Errorf("stdout = %q, want exactly \"overlord\\n\" (script-clean)", got)
	}
	if errb.String() != "" {
		t.Errorf("stderr = %q, want empty", errb.String())
	}
}

func TestConfigCmd_GetUnknownKey(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "get", "nope"}, d)
	if err == nil || cli.ExitCode(err) != 2 {
		t.Fatalf("config get nope: err=%v, want a usage (exit 2) error", err)
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should say unknown key, got %v", err)
	}
}

// TestConfigCmd_GetRemovedKey: a key that no longer exists must still REFUSE
// with exit 2 and a clean stdout. It used to carry a bespoke "retired" notice
// naming what replaced it; that courtesy is gone (pix has no released users to
// keep recovery paths for) and these now fall through to the generic
// unknown-key error. The refusal and the clean stdout are the parts that
// mattered -- `pix config get` is what the Makefile shells out to, so a
// refusal that printed to stdout would be read as a value.
func TestConfigCmd_GetRemovedKey(t *testing.T) {
	d, out, _ := configDeps(t)
	err := runConfigParse([]string{"config", "get", "knowledge_bundles"}, d)
	if err == nil {
		t.Fatal("expected config get knowledge_bundles to refuse: the key does not exist")
	}
	if cli.ExitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2", cli.ExitCode(err))
	}
	if out.String() != "" {
		t.Errorf("stdout must stay clean on a refusal, got %q", out.String())
	}
}

// ── set / unset ──────────────────────────────────────────────────────────

func TestConfigCmd_SetAndUnset(t *testing.T) {
	d, out, _ := configDeps(t)
	if err := runConfigParse([]string{"config", "set", "run_intent", "strategy"}, d); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if !strings.Contains(out.String(), `run_intent = "strategy"`) {
		t.Errorf("set output missing new value, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "# saved to "+config.Path()) {
		t.Errorf("set output missing save path, got:\n%s", out.String())
	}

	// The write round-trips through disk: a fresh load sees it.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunIntent != "strategy" {
		t.Errorf("RunIntent after set = %q, want strategy", cfg.RunIntent)
	}

	out.Reset()
	if err := runConfigParse([]string{"config", "unset", "run_intent"}, d); err != nil {
		t.Fatalf("config unset: %v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunIntent != config.DefaultRunIntent {
		t.Errorf("RunIntent after unset = %q, want the default %q", cfg.RunIntent, config.DefaultRunIntent)
	}
}

// TestConfigCmd_SetArityError proves the arity contract stays
// provision.ApplyConfigChange's: the CLI layer does not re-validate it, so
// mapping its error to a usage (exit 2) failure is the whole job.
func TestConfigCmd_SetArityError(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "set", "run_intent"}, d)
	if err == nil {
		t.Fatal("expected an arity error for set with no value")
	}
	if cli.ExitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2 (usage)", cli.ExitCode(err))
	}
}

// TestConfigCmd_SetRemovedKey: host.enabled (the deleted `pix host` escape
// hatch) must REFUSE rather than silently no-op. Silently accepting a set for a
// key nothing reads is the failure mode worth guarding: the user walks away
// believing they configured something. The wording no longer matters, the
// non-zero exit does.
func TestConfigCmd_SetRemovedKey(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "set", "host.enabled", "true"}, d)
	if err == nil || cli.ExitCode(err) != 2 {
		t.Errorf("config set host.enabled: err=%v, want a refusal (exit 2), never a silent no-op", err)
	}
}

func TestConfigCmd_SetUnknownKey(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "set", "nope", "x"}, d)
	if err == nil || cli.ExitCode(err) != 2 {
		t.Errorf("config set nope: err=%v, want a usage (exit 2) error", err)
	}
}

// ── help / bad flag (the usage/exit corpus contract) ────────────────────

func TestConfigCmd_Help(t *testing.T) {
	d, out, _ := configDeps(t)
	if err := runConfigParse([]string{"config", "--help"}, d); err != nil {
		t.Fatalf("config --help: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Usage: pix config", "show", "path", "get", "set", "unset"} {
		if !strings.Contains(got, want) {
			t.Errorf("config --help missing %q, got:\n%s", want, got)
		}
	}
}

func TestConfigCmd_BadFlag(t *testing.T) {
	d, _, _ := configDeps(t)
	err := runConfigParse([]string{"config", "--this-is-not-a-real-flag-9x7z"}, d)
	if err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if cli.ExitCode(err) != 2 {
		t.Errorf("exit code = %d, want 2", cli.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error should say unknown flag, got %v", err)
	}
}
