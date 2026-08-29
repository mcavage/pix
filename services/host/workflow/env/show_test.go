package env

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

// tier0Fixture registers a minimal, non-host-executing `.sbxenv.yaml`
// (schemaVersion + agent only — no mcp/secrets/host.services) under name,
// returning its canonical root. A Tier0 environment needs no review at all
// (bom.go's Tier1()), so this is what most `show` tests want: the default
// screen's "review: unaccepted" line without pulling in the review/gate
// machinery hostexec-fixture exercises.
func tier0Fixture(t *testing.T, cfg *config.Config, name string) string {
	t.Helper()
	root := t.TempDir()
	doc := "schemaVersion: \"1\"\nagent: pix\n"
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(cfg, name, root); err != nil {
		t.Fatal(err)
	}
	canon, err := config.CanonicalEnvironmentPath(root)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// ── AC-46: no environment selected -> exit 0, name "none", both renderings ──

func TestComputeShow_NoneSelected(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	r, err := ComputeShow(cfg, "")
	if err != nil {
		t.Fatalf("ComputeShow with nothing registered/selected must not error, got %v", err)
	}
	if r.Selected {
		t.Errorf("ShowResult = %+v, want Selected == false", r)
	}
}

func TestRenderShowDefault_NoneSelectedNamesNone(t *testing.T) {
	var out bytes.Buffer
	RenderShowDefault(&out, ShowResult{})
	got := out.String()
	if !strings.Contains(got, "none") {
		t.Errorf("default show with no selection = %q, want it to name `none`", got)
	}
	if !strings.Contains(got, "built-in defaults") {
		t.Errorf("default show with no selection = %q, want the D17 built-in-defaults prose", got)
	}
}

func TestRenderShowJSON_NoneSelectedEmitsEnvironmentNone(t *testing.T) {
	var out bytes.Buffer
	if err := RenderShowJSON(&out, ShowResult{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"environment": "none"`) {
		t.Errorf("show --json with no selection = %s, want \"environment\":\"none\" (AC-46)", got)
	}
	if !strings.Contains(got, `"schema_version": 1`) {
		t.Errorf("show --json = %s, want schema_version (AC-64)", got)
	}
}

// ── omitted NAME uses the machine default ────────────────────────────────

func TestComputeShow_OmittedNameUsesMachineDefault(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := tier0Fixture(t, cfg, "work")
	if err := cfg.UseEnvironment("work"); err != nil {
		t.Fatal(err)
	}

	r, err := ComputeShow(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Selected || r.Name != "work" || r.Root != root {
		t.Errorf("ComputeShow(\"\") with a machine default = %+v, want the selected environment", r)
	}
}

// ── unknown exact name: the typed error, never a fuzzy match ────────────

func TestComputeShow_UnknownNameExact(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "work", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Registration alone (no .sbxenv.yaml written) is enough to prove the
	// unknown-name path never reaches Load's file parsing for a NAME that
	// simply is not registered at all.
	_, err := ComputeShow(cfg, "hoem")
	if err == nil {
		t.Fatal("ComputeShow(unregistered name) must error")
	}
	var unk *config.UnknownEnvironmentError
	if !errors.As(err, &unk) {
		t.Fatalf("err = %v (%T), want *config.UnknownEnvironmentError", err, err)
	}
	if unk.Name != "hoem" {
		t.Errorf("UnknownEnvironmentError.Name = %q, want %q", unk.Name, "hoem")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2 (usage refusal)", got)
	}
}

// ── --path: byte-exact canonical path + newline, nothing else (AC-55) ───

func TestRenderShowPath_ByteExact(t *testing.T) {
	var out bytes.Buffer
	RenderShowPath(&out, ShowResult{Selected: true, Root: "/Users/alice/dev/work-pix-env"})
	if got, want := out.String(), "/Users/alice/dev/work-pix-env\n"; got != want {
		t.Errorf("RenderShowPath = %q, want %q byte-exact", got, want)
	}
}

func TestNoSelectionForPathError(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	err := NoSelectionForPathError(cfg)
	if cli.ExitCode(err) != 2 {
		t.Errorf("cli.ExitCode(NoSelectionForPathError) = %d, want 2", cli.ExitCode(err))
	}
	if !strings.Contains(err.Error(), "select one: pix env show <name> --path") {
		t.Errorf("NoSelectionForPathError = %q, want the runnable-command line", err.Error())
	}
}

// ── golden default view: one screen, ends with the --effective pointer ──

func TestRenderShowDefault_GoldenLineCountAndEffectivePointer(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	tier0Fixture(t, cfg, "work")

	r, err := ComputeShow(cfg, "work")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RenderShowDefault(&out, r)
	got := out.String()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	const oneScreenBound = 10
	if len(lines) > oneScreenBound {
		t.Errorf("default show is %d lines, want <= %d (one screen, AC-53):\n%s", len(lines), oneScreenBound, got)
	}
	last := lines[len(lines)-1]
	if want := "full rendered environment: pix env show work --effective"; last != want {
		t.Errorf("last line = %q, want %q", last, want)
	}
	for _, want := range []string{"work", "root:", ".sbxenv.yaml", "review:", "unaccepted", "sandbox:"} {
		if !strings.Contains(got, want) {
			t.Errorf("default show missing %q:\n%s", want, got)
		}
	}
}

func TestRenderShowDefault_SidecarPresentIsListed(t *testing.T) {
	var out bytes.Buffer
	RenderShowDefault(&out, ShowResult{Selected: true, Name: "work", Root: "/w", SidecarPresent: true})
	if !strings.Contains(out.String(), "pix.toml") {
		t.Errorf("default show with a sidecar present must list pix.toml, got:\n%s", out.String())
	}
}

func TestRenderShowDefault_AcceptedShowsFingerprint(t *testing.T) {
	var out bytes.Buffer
	RenderShowDefault(&out, ShowResult{
		Selected: true, Name: "work", Root: "/w", Accepted: true,
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	got := out.String()
	if !strings.Contains(got, "accepted") || strings.Contains(got, "unaccepted") {
		t.Errorf("accepted show must say accepted (not unaccepted), got:\n%s", got)
	}
	if !strings.Contains(got, "0123456789ab") {
		t.Errorf("accepted show must name the (short) fingerprint, got:\n%s", got)
	}
}

// ── --effective: declared, but not yet available (D8) ───────────────────

func TestErrEffectiveNotAvailable_IsOperationalNotUsage(t *testing.T) {
	if got := cli.ExitCode(ErrEffectiveNotAvailable); got == 2 || got == 0 {
		t.Errorf("cli.ExitCode(ErrEffectiveNotAvailable) = %d, want a non-zero, non-2 operational code (D19)", got)
	}
	if !strings.Contains(ErrEffectiveNotAvailable.Error(), "not yet available") {
		t.Errorf("ErrEffectiveNotAvailable = %q, want it to say not yet available", ErrEffectiveNotAvailable.Error())
	}
}

// TestErrEffectiveNotAvailable_NamesNoInternalUnitID is finding C12: the
// message a user actually sees must never leak an internal unit/ticket
// label ("E2.1", the renderer's own tracking ID) — that is planning
// metadata for this repo, not information that helps whoever typed
// `--effective`.
func TestErrEffectiveNotAvailable_NamesNoInternalUnitID(t *testing.T) {
	if msg := ErrEffectiveNotAvailable.Error(); strings.Contains(msg, "E2.1") {
		t.Errorf("ErrEffectiveNotAvailable = %q, must not name the internal unit E2.1", msg)
	}
}

// ── --json carries schema_version and every structured fact (AC-64) ─────

func TestRenderShowJSON_SelectedCarriesFacts(t *testing.T) {
	var out bytes.Buffer
	err := RenderShowJSON(&out, ShowResult{
		Selected: true, Name: "work", Root: "/w", SbxenvPresent: true,
		SidecarPresent: true, Accepted: true, Fingerprint: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`"environment": "work"`, `"root": "/w"`, `"sbxenv_present": true`,
		`"sidecar_present": true`, `"accepted": true`, `"fingerprint": "abc123"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("show --json missing %q:\n%s", want, got)
		}
	}
}
