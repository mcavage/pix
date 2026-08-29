package main

// env_cmd_test.go proves `pix env` end to end through the SAME kong entry
// point (dispatch) production uses, against scratch $PIX_CONFIG/
// $XDG_STATE_HOME so a run never touches a real user's config or trust
// store (workflow/env's review/ls/show all read the environment trust
// store beside config.toml and lock in the state dir — review_test.go's
// tempConfigAndState documents the same two-var isolation this file needs).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

func envDeps(t *testing.T) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	return freshDeps()
}

// freshDeps builds a new *cli.Deps with empty buffers over whatever
// $PIX_CONFIG/$XDG_STATE_HOME are ALREADY set to — for a second dispatch
// within a test that must keep reading the same scratch config/trust
// store envDeps just isolated, rather than rolling a brand new one.
func freshDeps() (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("")}, &out, &errb
}

// registerFixtureEnv copies src (a directory holding an authored
// `.sbxenv.yaml` and optional `pix.toml`) into a fresh root, registers it
// under name against the current scratch $PIX_CONFIG, and saves it —
// this file's equivalent of workflow/env's copyFixture/Register pair,
// reachable here only through config.Config directly, since registration
// has no verb yet (E1.10 owns `add`).
func registerFixtureEnv(t *testing.T, src, name string) string {
	t.Helper()
	root := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	canon, err := cfg.AddEnvironment(name, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return canon
}

// registerTier0Env registers name against a minimal, non-host-executing
// `.sbxenv.yaml` (no mcp/secrets/host.services) — the shape `env show`'s
// default screen exercises without pulling in review's gate machinery.
func registerTier0Env(t *testing.T, name string) string {
	t.Helper()
	src := t.TempDir()
	doc := "schemaVersion: \"1\"\nagent: pix\n"
	if err := os.WriteFile(filepath.Join(src, ".sbxenv.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return registerFixtureEnv(t, src, name)
}

// registerHostExecEnv registers name against the shared Tier1 fixture
// (workflow/env/testdata/hostexec-fixture), so `env review` has a real
// host-executing bill to render.
func registerHostExecEnv(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("..", "..", "workflow", "env", "testdata", "hostexec-fixture")
	return registerFixtureEnv(t, src, name)
}

// registerEnvWithLocalSkill registers name against a fresh root declaring a
// LOCAL `[pi].skills` entry at a relative path under root ("./skills") —
// the E1.9 BLOCK regression fixture: `env show`/`env review` supply no
// caller EffectiveMounts at all (env_cmd.go's own nil), so this only
// succeeds when Load itself validates the skill against its own implicit,
// read-only environment-root workspace.
func registerEnvWithLocalSkill(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pix.toml"), []byte("schema = 1\n\n[pi]\nskills = [\"./skills\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	canon, err := cfg.AddEnvironment(name, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return canon
}

// ── bare `pix env` is `env ls` ───────────────────────────────────────────

func TestEnvBareIsLs(t *testing.T) {
	d, out, errb := envDeps(t)
	if code := dispatch([]string{"env"}, d); code != 0 {
		t.Fatalf("pix env = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "built-in defaults") {
		t.Errorf("bare `pix env` on an empty registry = %q, want it to behave like `env ls`", out.String())
	}

	d2, out2, errb2 := envDeps(t)
	registerTier0Env(t, "work")
	if code := dispatch([]string{"env"}, d2); code != 0 {
		t.Fatalf("pix env = %d, want 0 (stderr: %s)", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "work") {
		t.Errorf("bare `pix env` after registering work = %q, want the ls listing", out2.String())
	}

	// The explicit spelling, over the SAME registered environment, renders
	// identically — proving `env` and `env ls` are the same dispatch, not
	// two renderers that happen to agree today.
	d3, out3, errb3 := freshDeps()
	if code := dispatch([]string{"env", "ls"}, d3); code != 0 {
		t.Fatalf("pix env ls = %d, want 0 (stderr: %s)", code, errb3.String())
	}
	if out3.String() != out2.String() {
		t.Errorf("`env` = %q, `env ls` = %q, want them identical", out2.String(), out3.String())
	}
}

// ── env show: no selection -> exit 0, names `none` ───────────────────────

func TestEnvShow_NoSelection(t *testing.T) {
	d, out, errb := envDeps(t)
	if code := dispatch([]string{"env", "show"}, d); code != 0 {
		t.Fatalf("pix env show (no selection) = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "none") {
		t.Errorf("env show with no selection = %q, want it to name `none`", out.String())
	}
}

func TestEnvShow_NoSelectionJSON(t *testing.T) {
	d, out, errb := envDeps(t)
	if code := dispatch([]string{"env", "show", "--json"}, d); code != 0 {
		t.Fatalf("pix env show --json (no selection) = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), `"environment": "none"`) {
		t.Errorf("env show --json with no selection = %q, want environment \"none\" (AC-46)", out.String())
	}
}

// ── env show --path: byte-exact canonical path + newline ────────────────

func TestEnvShow_PathByteExact(t *testing.T) {
	d, out, errb := envDeps(t)
	root := registerTier0Env(t, "work")
	if code := dispatch([]string{"env", "show", "work", "--path"}, d); code != 0 {
		t.Fatalf("pix env show work --path = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if got, want := out.String(), root+"\n"; got != want {
		t.Errorf("env show work --path = %q, want %q byte-exact", got, want)
	}
}

func TestEnvShow_PathWithNoSelectionRefuses(t *testing.T) {
	d, _, errb := envDeps(t)
	code := dispatch([]string{"env", "show", "--path"}, d)
	if code != 2 {
		t.Fatalf("pix env show --path (no selection) = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "select one: pix env show") {
		t.Errorf("stderr = %q, want the runnable-command line", errb.String())
	}
}

// ── env show: unknown exact name ──────────────────────────────────────────

func TestEnvShow_UnknownNameExact(t *testing.T) {
	d, _, errb := envDeps(t)
	code := dispatch([]string{"env", "show", "hoem"}, d)
	if code != 2 {
		t.Fatalf("pix env show hoem = %d, want 2", code)
	}
	got := errb.String()
	if !strings.Contains(got, `no environment named "hoem"`) {
		t.Errorf("stderr = %q, want the unknown-name form", got)
	}
	if strings.Contains(got, "pix: pix:") {
		t.Errorf("stderr = %q, must never double-prefix \"pix: \"", got)
	}
}

// ── env show --effective: declared, not yet available (D8) ──────────────

func TestEnvShow_EffectiveNotYetAvailable(t *testing.T) {
	d, _, errb := envDeps(t)
	registerTier0Env(t, "work")
	code := dispatch([]string{"env", "show", "work", "--effective"}, d)
	if code == 0 || code == 2 {
		t.Fatalf("pix env show work --effective = %d, want a non-zero, non-2 operational code", code)
	}
	if !strings.Contains(errb.String(), "not yet available") {
		t.Errorf("stderr = %q, want it to say not yet available", errb.String())
	}
	if strings.Contains(errb.String(), "pix: pix:") {
		t.Errorf("stderr = %q, must never double-prefix \"pix: \"", errb.String())
	}
}

// ── env show: --path/--json/--effective are mutually exclusive ──────────

func TestEnvShow_FlagsMutuallyExclusive(t *testing.T) {
	for _, argv := range [][]string{
		{"env", "show", "work", "--path", "--json"},
		{"env", "show", "work", "--path", "--effective"},
		{"env", "show", "work", "--json", "--effective"},
	} {
		d, _, errb := envDeps(t)
		if code := dispatch(argv, d); code != 2 {
			t.Errorf("dispatch(%v) = %d, want 2 (usage refusal)", argv, code)
		}
		if !strings.Contains(errb.String(), "mutually exclusive") {
			t.Errorf("dispatch(%v) stderr = %q, want a mutually-exclusive refusal", argv, errb.String())
		}
	}
}

// ── env review: dispatches E1.8 with real roots/TTY/PATH lookup ─────────

func TestEnvReview_NonTTYFailsClosed(t *testing.T) {
	d, out, errb := envDeps(t)
	d.Interactive = false
	registerHostExecEnv(t, "work")

	code := dispatch([]string{"env", "review", "work"}, d)
	if code != 2 {
		t.Fatalf("pix env review work (non-TTY) = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "Accept this host-execution footprint?") {
		t.Errorf("stdout = %q, want the review bill", out.String())
	}
	if !strings.Contains(out.String(), "pix env review work --yes") {
		t.Errorf("stdout = %q, want the --yes re-run command", out.String())
	}
}

func TestEnvReview_YesAccepts(t *testing.T) {
	d, out, errb := envDeps(t)
	registerHostExecEnv(t, "work")

	code := dispatch([]string{"env", "review", "work", "--yes"}, d)
	if code != 0 {
		t.Fatalf("pix env review work --yes = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), `recorded acceptance for environment "work"`) {
		t.Errorf("stdout = %q, want the acceptance line", out.String())
	}

	// `env show work` now reports it accepted, over the SAME scratch
	// config/trust store `env review --yes` just wrote to.
	d2, out2, errb2 := freshDeps()
	if code := dispatch([]string{"env", "show", "work"}, d2); code != 0 {
		t.Fatalf("pix env show work = %d, want 0 (stderr: %s)", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "accepted") {
		t.Errorf("env show after review --yes = %q, want it to report accepted", out2.String())
	}
}

// ── E1.9 BLOCK: full dispatch with a local sidecar skill under root ──────

// TestEnvShow_WithLocalSidecarSkillSucceeds proves `pix env show` — which
// supplies no writable workspace at all pre-E2 — still succeeds for an
// environment declaring a LOCAL skill under its own root, through the SAME
// kong dispatch production uses.
func TestEnvShow_WithLocalSidecarSkillSucceeds(t *testing.T) {
	d, out, errb := envDeps(t)
	registerEnvWithLocalSkill(t, "work")
	if code := dispatch([]string{"env", "show", "work"}, d); code != 0 {
		t.Fatalf("pix env show work (local sidecar skill under root) = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "pix.toml") {
		t.Errorf("env show work = %q, want it to list pix.toml as an authored file", out.String())
	}
}

// TestEnvReview_WithLocalSidecarSkillSucceeds proves the same for `pix env
// review`: this fixture is Tier0 (no host-exec facet), so it succeeds
// silently with no gate, but only once Load's own skill validation passes.
func TestEnvReview_WithLocalSidecarSkillSucceeds(t *testing.T) {
	d, out, errb := envDeps(t)
	d.Interactive = false
	registerEnvWithLocalSkill(t, "work")
	if code := dispatch([]string{"env", "review", "work"}, d); code != 0 {
		t.Fatalf("pix env review work (local sidecar skill under root) = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if out.String() != "" {
		t.Errorf("env review work (Tier0) = %q, want no output at all", out.String())
	}
}
