package main

// env_cmd_test.go proves `pix env` end to end through the SAME kong entry
// point (dispatch) production uses, against scratch $PIX_CONFIG/
// $XDG_STATE_HOME so a run never touches a real user's config or trust
// store (workflow/env's review/ls/show all read the environment trust
// store beside config.toml and lock in the state dir — review_test.go's
// tempConfigAndState documents the same two-var isolation this file needs).

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/sys/systest"
	"pix/host/workflow/env"
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

// ── env add: E1.10, dispatched through the SAME kong entry point ──────────

// tier0Source/tier1Source build the same two authored-file shapes
// registerTier0Env/registerHostExecEnv register directly, but as a bare
// SOURCE DIRECTORY `env add` itself must register — `add` has no verb yet
// to bypass in these tests, unlike every earlier env_cmd_test.go helper.
func tier0Source(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

func tier1Source(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	fixture := filepath.Join("..", "..", "workflow", "env", "testdata", "hostexec-fixture")
	entries, err := os.ReadDir(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(fixture, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func TestEnvAdd_RegisterTier0(t *testing.T) {
	d, out, errb := envDeps(t)
	root := tier0Source(t)
	if code := dispatch([]string{"env", "add", "work", root}, d); code != 0 {
		t.Fatalf("pix env add work %s = %d, want 0 (stderr: %s)", root, code, errb.String())
	}
	if !strings.Contains(out.String(), "pix env use work") {
		t.Errorf("stdout = %q, want it to name `pix env use work`", out.String())
	}

	d2, out2, errb2 := freshDeps()
	if code := dispatch([]string{"env", "show", "work", "--path"}, d2); code != 0 {
		t.Fatalf("pix env show work --path = %d, want 0 (stderr: %s)", code, errb2.String())
	}
	if strings.TrimSpace(out2.String()) == "" {
		t.Error("env show work --path after add is empty; add must have registered and saved it")
	}
}

func TestEnvAdd_RegisterTier1_NonTTYFailsClosedTransactionally(t *testing.T) {
	d, out, errb := envDeps(t)
	d.Interactive = false
	root := tier1Source(t)

	code := dispatch([]string{"env", "add", "work", root}, d)
	if code != 2 {
		t.Fatalf("pix env add work %s (non-TTY) = %d, want 2 (stdout: %s, stderr: %s)", root, code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Accept this host-execution footprint?") {
		t.Errorf("stdout = %q, want the review bill", out.String())
	}

	// Nothing was registered: a fresh `env show work` still reports unknown.
	d2, _, errb2 := freshDeps()
	if code := dispatch([]string{"env", "show", "work"}, d2); code != 2 {
		t.Fatalf("pix env show work after a refused add = %d, want 2", code)
	}
	if !strings.Contains(errb2.String(), `no environment named "work"`) {
		t.Errorf("stderr = %q, want the unknown-name form (nothing was registered)", errb2.String())
	}
}

func TestEnvAdd_RegisterTier1_YesAccepts(t *testing.T) {
	d, out, errb := envDeps(t)
	root := tier1Source(t)
	if code := dispatch([]string{"env", "add", "work", root, "--yes"}, d); code != 0 {
		t.Fatalf("pix env add work %s --yes = %d, want 0 (stderr: %s)", root, code, errb.String())
	}
	if !strings.Contains(out.String(), `recorded acceptance for environment "work"`) {
		t.Errorf("stdout = %q, want the acceptance line", out.String())
	}
	if !strings.Contains(out.String(), "pix env use work") {
		t.Errorf("stdout = %q, want it to name `pix env use work`", out.String())
	}
}

// TestEnvAdd_ZeroPathScaffoldsUnderDataDir proves the zero-path form end to
// end through dispatch: cwd genuinely has no `.sbxenv.yaml` (a scratch temp
// dir this test os.Chdir's into and restores afterward — the ONE test in
// this file that does, since envAddCmd.Run wires no Getwd override:
// production always resolves the real cwd).
func TestEnvAdd_ZeroPathScaffoldsUnderDataDir(t *testing.T) {
	d, out, errb := envDeps(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	if code := dispatch([]string{"env", "add", "home"}, d); code != 0 {
		t.Fatalf("pix env add home (scaffold) = %d, want 0 (stderr: %s)", code, errb.String())
	}
	firstLine, _, _ := strings.Cut(out.String(), "\n")
	if !filepath.IsAbs(firstLine) {
		t.Errorf("first output line = %q, want an absolute created root", firstLine)
	}
	if !strings.Contains(firstLine, filepath.Join("pix", "envs", "home")) {
		t.Errorf("first output line = %q, want it under <data-dir>/envs/home", firstLine)
	}
	if _, err := os.Stat(filepath.Join(firstLine, ".sbxenv.yaml")); err != nil {
		t.Errorf("scaffolded .sbxenv.yaml missing at %s: %v", firstLine, err)
	}
	if !strings.Contains(out.String(), "pix env use home") {
		t.Errorf("stdout = %q, want it to name `pix env use home`", out.String())
	}
}

// TestEnvAdd_ZeroPathCwdAmbiguityRefusesNamingBothForms proves D10 through
// dispatch: a real cwd that already holds `.sbxenv.yaml` refuses a
// zero-path add outright.
func TestEnvAdd_ZeroPathCwdAmbiguityRefusesNamingBothForms(t *testing.T) {
	d, _, errb := envDeps(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	code := dispatch([]string{"env", "add", "home"}, d)
	if code != 2 {
		t.Fatalf("pix env add home (cwd has .sbxenv.yaml) = %d, want 2", code)
	}
	got := errb.String()
	if !strings.Contains(got, "pix env add home "+cwd) {
		t.Errorf("stderr = %q, want it to name the register form", got)
	}
	if !strings.Contains(got, "pix env add home") {
		t.Errorf("stderr = %q, want it to name the bare scaffold form", got)
	}
}

// ── env use ───────────────────────────────────────────────────────────────

func TestEnvUse_Tier0Succeeds(t *testing.T) {
	d, out, errb := envDeps(t)
	registerTier0Env(t, "home")

	if code := dispatch([]string{"env", "use", "home"}, d); code != 0 {
		t.Fatalf("pix env use home = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), `"home" is now the default`) {
		t.Errorf("stdout = %q, want the default-set success line", out.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != "home" {
		t.Errorf("config.Environment = %q, want %q", cfg.Environment, "home")
	}
}

func TestEnvUse_UnreviewedTier1Refuses(t *testing.T) {
	d, _, errb := envDeps(t)
	registerHostExecEnv(t, "work")

	code := dispatch([]string{"env", "use", "work"}, d)
	if code != 2 {
		t.Fatalf("pix env use work (unreviewed) = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "pix env review work") {
		t.Errorf("stderr = %q, want it to name `pix env review work`", errb.String())
	}
	if strings.Contains(errb.String(), "pix: pix:") {
		t.Errorf("stderr = %q, must never double-prefix \"pix: \"", errb.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != "" {
		t.Errorf("a refused `env use` must never set the default; config.Environment = %q", cfg.Environment)
	}
}

func TestEnvUse_ReviewedThenChangedRefuses(t *testing.T) {
	d, _, errb := envDeps(t)
	registerHostExecEnv(t, "work")
	if code := dispatch([]string{"env", "review", "work", "--yes"}, d); code != 0 {
		t.Fatalf("pix env review work --yes = %d, want 0 (stderr: %s)", code, errb.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	root, ok := env.Root(cfg, "work")
	if !ok {
		t.Fatal("work must be registered")
	}
	sbxenvPath := filepath.Join(root, ".sbxenv.yaml")
	data, err := os.ReadFile(sbxenvPath)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(data),
		"    - name: warehouse-mcp\n      command: warehouse-mcp-server\n",
		"    - name: warehouse-mcp\n      command: warehouse-mcp-server\n    - name: extra-mcp\n      command: extra-mcp-server\n",
		1)
	if rewritten == string(data) {
		t.Fatal("test setup error: fixture .sbxenv.yaml did not match the expected replace target")
	}
	if err := os.WriteFile(sbxenvPath, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	d2, _, errb2 := freshDeps()
	code := dispatch([]string{"env", "use", "work"}, d2)
	if code != 2 {
		t.Fatalf("pix env use work (changed since review) = %d, want 2 (stderr: %s)", code, errb2.String())
	}
	if !strings.Contains(errb2.String(), "changed") || !strings.Contains(errb2.String(), "pix env review work") {
		t.Errorf("stderr = %q, want it to say changed and name `pix env review work`", errb2.String())
	}
}

func TestEnvUse_ConfigMutationIsOnlyTheEnvironmentKey(t *testing.T) {
	d, _, errb := envDeps(t)
	registerTier0Env(t, "home")
	before, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	if code := dispatch([]string{"env", "use", "home"}, d); code != 0 {
		t.Fatalf("pix env use home = %d, want 0 (stderr: %s)", code, errb.String())
	}
	after, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	inBefore := map[string]bool{}
	for _, l := range strings.Split(string(before), "\n") {
		inBefore[l] = true
	}
	var added []string
	for _, l := range strings.Split(string(after), "\n") {
		if strings.TrimSpace(l) == "" || inBefore[l] {
			continue
		}
		added = append(added, l)
	}
	if len(added) != 1 || !strings.HasPrefix(added[0], "environment") {
		t.Fatalf("config diff lines = %v, want exactly one `environment = ...` line added", added)
	}
}

// ── env forget ────────────────────────────────────────────────────────────

func TestEnvForget_SucceedsAndLeavesSourceByteIdentical(t *testing.T) {
	d, out, errb := envDeps(t)
	root := registerTier0Env(t, "home")
	sentinel := filepath.Join(root, ".sbxenv.yaml")
	before, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}

	if code := dispatch([]string{"env", "forget", "home"}, d); code != 0 {
		t.Fatalf("pix env forget home = %d, want 0 (stderr: %s)", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, `"home"`) || !strings.Contains(got, root) {
		t.Errorf("stdout = %q, want it to name the environment and the surviving root %s", got, root)
	}
	if strings.Contains(got, "removed") || strings.Contains(got, "deleted") {
		t.Errorf("stdout = %q, must never say removed/deleted", got)
	}

	after, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("source file must survive forget: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("source bytes changed: before %q, after %q", before, after)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Environments["home"]; ok {
		t.Error("home must no longer be registered after forget")
	}
}

func TestEnvForget_CurrentDefaultRefuses(t *testing.T) {
	d, _, errb := envDeps(t)
	registerTier0Env(t, "home")
	if code := dispatch([]string{"env", "use", "home"}, d); code != 0 {
		t.Fatalf("pix env use home = %d, want 0 (stderr: %s)", code, errb.String())
	}

	d2, _, errb2 := freshDeps()
	code := dispatch([]string{"env", "forget", "home"}, d2)
	if code != 2 {
		t.Fatalf("pix env forget home (current default) = %d, want 2 (stderr: %s)", code, errb2.String())
	}
	if !strings.Contains(errb2.String(), "current default") {
		t.Errorf("stderr = %q, want it to name the current-default refusal", errb2.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Environments["home"]; !ok {
		t.Error("a refused forget must leave the registration in place")
	}
	if cfg.Environment != "home" {
		t.Errorf("config.Environment = %q, want unchanged %q", cfg.Environment, "home")
	}
}

func TestEnvForget_UnknownNameRefuses(t *testing.T) {
	d, _, errb := envDeps(t)
	code := dispatch([]string{"env", "forget", "hoem"}, d)
	if code != 2 {
		t.Fatalf("pix env forget hoem = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), `no environment named "hoem"`) {
		t.Errorf("stderr = %q, want the unknown-name form", errb.String())
	}
}

// ── pix env rm: pointer error, not a working alias ───────────────────────

func TestEnvRm_IsAPointerErrorWithZeroMutation(t *testing.T) {
	envDeps(t)
	root := registerTier0Env(t, "home")
	beforeConfig, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, ".sbxenv.yaml")
	beforeSource, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}

	const want = "pix: `pix env rm` does not exist. Registering a name is not owning the files.\n" +
		"     pix env forget home     unregister the name (deletes no files)\n" +
		"     pix rm pix-repo-home    remove the sandbox\n" +
		"     rm -rf <path>           delete the source yourself; pix will not\n"

	for _, argv := range [][]string{
		{"env", "rm"},
		{"env", "rm", "home"},
		{"env", "rm", "--force", "home"},
	} {
		d2, out2, errb2 := freshDeps()
		code := dispatch(argv, d2)
		if code != 2 {
			t.Errorf("dispatch(%v) = %d, want 2", argv, code)
		}
		if out2.String() != "" {
			t.Errorf("dispatch(%v) stdout = %q, want nothing", argv, out2.String())
		}
		if errb2.String() != want {
			t.Errorf("dispatch(%v) stderr = %q, want the exact pointer error %q", argv, errb2.String(), want)
		}
	}

	afterConfig, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(afterConfig) != string(beforeConfig) {
		t.Errorf("config.toml changed after `pix env rm`; before %q, after %q", beforeConfig, afterConfig)
	}
	afterSource, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSource) != string(beforeSource) {
		t.Errorf("environment source changed after `pix env rm`")
	}

	ts, err := os.ReadDir(filepath.Dir(config.Path()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ts {
		if e.Name() == "environment-trust.json" {
			t.Errorf("`pix env rm` must never create the environment trust store")
		}
	}
}

// TestEnvRm_AbsentFromHelp checks only the generated "Commands:" block (the
// actual verb LISTING, one dispatchable node per line) rather than the
// whole help text: envCmd's own Help() prose deliberately NAMES `pix env
// rm` to explain why it does not exist (this file's `envRmPointerError`
// wording), exactly as the design doc's own help text does, so asserting
// "the substring `rm` never appears anywhere" would fail on that correct,
// intentional sentence. `hidden:""` (envCmd's Rm field) is what this test
// actually proves: rm dispatches (TestEnvRm_IsAPointerErrorWithZeroMutation)
// but never appears as a listed command.
func TestEnvRm_AbsentFromHelp(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	if err := cli.RunRoot[envCmd]("pix env", "", "", []string{"--help"}, d); err != nil {
		t.Fatalf("env --help: %v", err)
	}
	idx := strings.Index(out.String(), "Commands:")
	if idx < 0 {
		t.Fatalf("pix env --help has no \"Commands:\" block:\n%s", out.String())
	}
	commands := out.String()[idx:]
	for _, line := range strings.Split(commands, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "rm" || strings.HasPrefix(trimmed, "rm ") || strings.HasPrefix(trimmed, "rm<") || strings.HasPrefix(trimmed, "rm[") {
			t.Errorf("pix env --help lists `rm` as a command:\n%s", commands)
		}
	}
}

func TestEnvRm_HelpListsExactlySevenWorkingVerbs(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	if err := cli.RunRoot[envCmd]("pix env", "", "", []string{"--help"}, d); err != nil {
		t.Fatalf("env --help: %v", err)
	}
	got := out.String()
	for _, verb := range []string{"ls", "add", "use", "show", "edit", "review", "forget"} {
		if !strings.Contains(got, " "+verb+" ") && !strings.Contains(got, " "+verb+"\n") {
			t.Errorf("pix env --help missing wired verb %q:\n%s", verb, got)
		}
	}
	if strings.Contains(got, " rm ") || strings.Contains(got, " rm\n") {
		t.Errorf("pix env --help must never list `rm` as a verb:\n%s", got)
	}
}

// ── env edit: E1.12, through the SAME kong dispatch production uses ──────

// envEditDeps is envDeps plus a systest.Fake wired as d.Sys, so `env edit`
// dispatch tests can control $VISUAL/$EDITOR and the editor invocation
// without ever spawning a real process.
func envEditDeps(t *testing.T, fake *systest.Fake) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	d, out, errb := envDeps(t)
	d.Sys = fake
	return d, out, errb
}

func noEditorEditFake() *systest.Fake {
	return &systest.Fake{GetenvFn: func(string) string { return "" }}
}

// TestEnvEdit_TargetTokenTable is the dispatch-level analog of
// workflow/env/edit_test.go's TestEdit_TargetTokenTable: the exact
// positional enum, through kong argv parsing.
func TestEnvEdit_TargetTokenTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string // "" means success
	}{
		{name: "pix", argv: []string{"env", "edit", "work", "pix"}, want: ""},
		{name: "sbxenv", argv: []string{"env", "edit", "work", "sbxenv"}, want: ""},
		{name: "unrecognized", argv: []string{"env", "edit", "work", "yaml"}, want: `unknown target "yaml"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, out, errb := envEditDeps(t, noEditorEditFake())
			d.Interactive = false
			registerTier0Env(t, "work")
			code := dispatch(tc.argv, d)
			if tc.want == "" {
				if code != 0 {
					t.Fatalf("dispatch(%v) = %d, want 0 (stderr: %s)", tc.argv, code, errb.String())
				}
				return
			}
			if code != 2 {
				t.Fatalf("dispatch(%v) = %d, want 2 (stdout: %s)", tc.argv, code, out.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", errb.String(), tc.want)
			}
			if !strings.Contains(errb.String(), "pix env edit work pix") || !strings.Contains(errb.String(), "pix env edit work sbxenv") {
				t.Errorf("stderr = %q, want both explicit forms named", errb.String())
			}
		})
	}
}

// TestEnvEdit_NonTTYNoTargetExitsTwo: no token, no TTY -> exit 2 naming both
// explicit forms (AC-50), through the real kong-parsed positional (which
// leaves Target == "" since it is `optional:""`).
func TestEnvEdit_NonTTYNoTargetExitsTwo(t *testing.T) {
	d, _, errb := envEditDeps(t, noEditorEditFake())
	d.Interactive = false
	registerTier0Env(t, "work")
	code := dispatch([]string{"env", "edit", "work"}, d)
	if code != 2 {
		t.Fatalf("dispatch = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(errb.String(), "pix env edit work pix") || !strings.Contains(errb.String(), "pix env edit work sbxenv") {
		t.Errorf("stderr = %q, want both explicit forms named", errb.String())
	}
}

// TestEnvEdit_TTYNoTargetPromptsAndReadsChoice: same, but a TTY reads one
// bounded choice off d.In instead of refusing.
func TestEnvEdit_TTYNoTargetPromptsAndReadsChoice(t *testing.T) {
	d, out, errb := envEditDeps(t, noEditorEditFake())
	d.Interactive = true
	d.In = strings.NewReader("sbxenv\n")
	root := registerTier0Env(t, "work")
	code := dispatch([]string{"env", "edit", "work"}, d)
	if code != 0 {
		t.Fatalf("dispatch = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "which file? [pix/sbxenv]: ") {
		t.Errorf("stdout = %q, want the selection prompt", out.String())
	}
	if want := filepath.Join(root, ".sbxenv.yaml") + "\n"; !strings.HasSuffix(out.String(), want) {
		t.Errorf("stdout = %q, want it to end with %q (no editor configured)", out.String(), want)
	}
}

// TestEnvEdit_BothUnsetPrintsPathAndExitsZero exercises §8.1's exit-code
// scheme's own example verbatim: "0 only for a completed operation,
// including printing a path because $EDITOR was unset".
func TestEnvEdit_BothUnsetPrintsPathAndExitsZero(t *testing.T) {
	d, out, errb := envEditDeps(t, noEditorEditFake())
	d.Interactive = false
	root := registerTier0Env(t, "work")
	code := dispatch([]string{"env", "edit", "work", "sbxenv"}, d)
	if code != 0 {
		t.Fatalf("dispatch = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if want := filepath.Join(root, ".sbxenv.yaml") + "\n"; out.String() != want {
		t.Errorf("stdout = %q, want ONLY %q", out.String(), want)
	}
}

// TestEnvEdit_ArgvSpacesNoShell: a multi-word $EDITOR is split into a real
// argv and invoked directly (RunInteractive, never a shell).
func TestEnvEdit_ArgvSpacesNoShell(t *testing.T) {
	fake := &systest.Fake{
		GetenvFn:         func(string) string { return "myeditor --wait --flag" },
		RunInteractiveFn: func(string, ...string) error { return nil },
	}
	d, _, errb := envEditDeps(t, fake)
	d.Interactive = false
	root := registerTier0Env(t, "work")
	code := dispatch([]string{"env", "edit", "work", "pix"}, d)
	if code != 0 {
		t.Fatalf("dispatch = %d, want 0 (stderr: %s)", code, errb.String())
	}
	want := "myeditor --wait --flag " + filepath.Join(root, "pix.toml")
	if len(fake.Calls) != 1 || fake.Calls[0] != want {
		t.Errorf("fake.Calls = %v, want exactly [%q]", fake.Calls, want)
	}
}

// TestEnvEdit_EditorFailureIsOperationalNonTwo: through dispatch, the exit
// code an editor failure surfaces as is non-zero and NOT 2 (a refusal), the
// same distinction env_cmd.go's envRun preserves via cli.ExitCode.
func TestEnvEdit_EditorFailureIsOperationalNonTwo(t *testing.T) {
	fake := &systest.Fake{
		GetenvFn:         func(string) string { return "brokeneditor" },
		RunInteractiveFn: func(string, ...string) error { return errors.New("exit status 127") },
	}
	d, _, errb := envEditDeps(t, fake)
	d.Interactive = false
	registerTier0Env(t, "work")
	code := dispatch([]string{"env", "edit", "work", "sbxenv"}, d)
	if code == 0 || code == 2 {
		t.Fatalf("dispatch = %d, want a non-zero, non-2 operational code", code)
	}
	if !strings.Contains(errb.String(), "brokeneditor") {
		t.Errorf("stderr = %q, want it to name the editor", errb.String())
	}
}

// TestEnvEdit_VerdictOkReviewInvalid runs all three PRD §5.4 verdicts
// through dispatch, over a no-op editor that either leaves the file
// untouched (ok/review) or corrupts it (invalid).
func TestEnvEdit_VerdictOkReviewInvalid(t *testing.T) {
	t.Run("ok: accepted and unchanged", func(t *testing.T) {
		fake := &systest.Fake{
			GetenvFn:         func(string) string { return "true" },
			RunInteractiveFn: func(string, ...string) error { return nil },
		}
		d, _, errb := envEditDeps(t, fake)
		d.Interactive = false
		// A Tier0 fixture's `env review` never writes a record at all (there
		// is nothing to review), so a real accepted-and-unchanged verdict
		// needs the Tier1 host-exec fixture the review tests already use.
		registerHostExecEnv(t, "work")
		// Accept the environment as-is, over the SAME scratch config/trust
		// store the subsequent edit dispatch reads.
		if code := dispatch([]string{"env", "review", "work", "--yes"}, d); code != 0 {
			t.Fatalf("env review work --yes = %d, want 0 (stderr: %s)", code, errb.String())
		}
		d2, out2, errb2 := freshDeps()
		d2.Sys = fake
		d2.Interactive = false
		code := dispatch([]string{"env", "edit", "work", "sbxenv"}, d2)
		if code != 0 {
			t.Fatalf("dispatch = %d, want 0 (stderr: %s)", code, errb2.String())
		}
		if !strings.Contains(out2.String(), "pix env use work") {
			t.Errorf("stdout = %q, want the ok verdict", out2.String())
		}
	})

	t.Run("review: never accepted", func(t *testing.T) {
		fake := &systest.Fake{
			GetenvFn:         func(string) string { return "true" },
			RunInteractiveFn: func(string, ...string) error { return nil },
		}
		d, out, errb := envEditDeps(t, fake)
		d.Interactive = false
		// A Tier0 fixture can never sit unaccepted (review.go's own Review
		// writes no record for it at all, and postEditVerdict's Tier0 branch
		// says "ok" unconditionally): the never-accepted "review" verdict is
		// only real for a Tier1 host-exec fixture nobody has run `env review`
		// against yet.
		registerHostExecEnv(t, "work")
		code := dispatch([]string{"env", "edit", "work", "sbxenv"}, d)
		if code != 0 {
			t.Fatalf("dispatch = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(out.String(), "pix env review work") {
			t.Errorf("stdout = %q, want the review verdict", out.String())
		}
	})

	t.Run("ok: Tier0 with no acceptance record at all", func(t *testing.T) {
		// E1.12 pre-merge BLOCK fix (finding 3): a Tier0 environment must
		// verdict "ok" even though it was never run through `env review` —
		// there is nothing for review to accept in the first place.
		fake := &systest.Fake{
			GetenvFn:         func(string) string { return "true" },
			RunInteractiveFn: func(string, ...string) error { return nil },
		}
		d, out, errb := envEditDeps(t, fake)
		d.Interactive = false
		registerTier0Env(t, "work")
		code := dispatch([]string{"env", "edit", "work", "sbxenv"}, d)
		if code != 0 {
			t.Fatalf("dispatch = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(out.String(), "pix env use work") {
			t.Errorf("stdout = %q, want the ok verdict", out.String())
		}
		if strings.Contains(out.String(), "pix env review") {
			t.Errorf("stdout = %q, must never point a Tier0 environment at review", out.String())
		}
	})

	t.Run("invalid: corrupted by the editor", func(t *testing.T) {
		var root string
		fake := &systest.Fake{
			GetenvFn: func(string) string { return "true" },
			RunInteractiveFn: func(string, ...string) error {
				return os.WriteFile(filepath.Join(root, ".sbxenv.yaml"),
					[]byte("schemaVersion: \"1\"\nagent: pix\nnot_a_real_field: true\n"), 0o644)
			},
		}
		d, out, errb := envEditDeps(t, fake)
		d.Interactive = false
		root = registerTier0Env(t, "work")
		code := dispatch([]string{"env", "edit", "work", "sbxenv"}, d)
		if code != 0 {
			t.Fatalf("dispatch = %d, want 0 (stderr: %s)", code, errb.String())
		}
		if !strings.Contains(out.String(), "next: pix env edit work sbxenv") {
			t.Errorf("stdout = %q, want the invalid verdict's re-edit command", out.String())
		}
	})
}

// TestEnvEdit_NoSbxenvFlagInHelp: the ONLY spelling of the native-file
// target anywhere in `pix env edit --help`'s live usage text is the
// positional "sbxenv", never a "--sbxenv" flag.
func TestEnvEdit_NoSbxenvFlagInHelp(t *testing.T) {
	d, out, errb := envDeps(t)
	if code := dispatch([]string{"env", "edit", "--help"}, d); code != 0 {
		t.Fatalf("pix env edit --help = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if strings.Contains(out.String(), "--sbxenv") {
		t.Errorf("help text = %q, must never advertise a --sbxenv flag", out.String())
	}
}
