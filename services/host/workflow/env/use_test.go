package env

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

// ── Tier0: use succeeds with no review at all ────────────────────────────

func TestUse_Tier0NeedsNoReview(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	if err := Use(cfg, "home", noBareLookPath); err != nil {
		t.Fatalf("Use on a Tier0 environment: %v", err)
	}
	if cfg.Environment != "home" {
		t.Errorf("cfg.Environment = %q, want %q", cfg.Environment, "home")
	}
}

// ── Tier1 unreviewed: refuses exit 2, names `pix env review NAME` ───────

func TestUse_Tier1UnreviewedRefuses(t *testing.T) {
	root, cfg := reviewFixture(t, "work")
	_ = root

	err := Use(cfg, "work", noBareLookPath)
	if err == nil {
		t.Fatal("Use on an unreviewed Tier1 environment must refuse")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), "pix env review work") {
		t.Errorf("error = %q, want it to name `pix env review work`", err.Error())
	}
	if cfg.Environment != "" {
		t.Errorf("cfg.Environment = %q, want unchanged (empty) on refusal", cfg.Environment)
	}
}

// ── Tier1 reviewed + unchanged: succeeds ─────────────────────────────────

func TestUse_Tier1ReviewedAndUnchangedSucceeds(t *testing.T) {
	_, cfg := reviewFixture(t, "work")
	if _, err := Review(cfg, "work", nil, noBareLookPath, ReviewOptions{
		Out: &bytes.Buffer{}, Yes: true,
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	if err := Use(cfg, "work", noBareLookPath); err != nil {
		t.Fatalf("Use after review: %v", err)
	}
	if cfg.Environment != "work" {
		t.Errorf("cfg.Environment = %q, want %q", cfg.Environment, "work")
	}
}

// ── Tier1 reviewed then changed: refuses, still names `pix env review NAME` ──

func TestUse_Tier1ChangedSinceReviewRefuses(t *testing.T) {
	newRoot, cfg := reviewFixture(t, "work")
	if _, err := Review(cfg, "work", nil, noBareLookPath, ReviewOptions{
		Out: &bytes.Buffer{}, Yes: true,
	}); err != nil {
		t.Fatalf("Review: %v", err)
	}

	sbxenvPath := filepath.Join(newRoot, ".sbxenv.yaml")
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

	err = Use(cfg, "work", noBareLookPath)
	if err == nil {
		t.Fatal("Use after the reviewed surface changed must refuse")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), "changed") || !strings.Contains(err.Error(), "pix env review work") {
		t.Errorf("error = %q, want it to say changed and name `pix env review work`", err.Error())
	}
	if cfg.Environment != "" {
		t.Errorf("cfg.Environment = %q, want unchanged (empty) on refusal", cfg.Environment)
	}
}

// ── unknown name: the same typed refusal every other verb gives ─────────

func TestUse_UnknownNameRefuses(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	err := Use(cfg, "hoem", noBareLookPath)
	if err == nil {
		t.Fatal("Use of an unregistered name must refuse")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), `no environment named "hoem"`) {
		t.Errorf("error = %q, want the unknown-name form", err.Error())
	}
}

// ── config mutation is exactly one key ───────────────────────────────────

func TestUse_ConfigMutationIsOnlyTheEnvironmentKey(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	if err := Use(cfg, "home", noBareLookPath); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	beforeLines := diffLines(string(before), string(after))
	if len(beforeLines) != 1 || !strings.HasPrefix(beforeLines[0], "environment") {
		t.Fatalf("config diff lines = %v, want exactly one `environment = ...` line changed", beforeLines)
	}
}

// diffLines returns the lines present in b but not in a — a crude
// line-set diff sufficient for asserting Save() only ever ADDS or CHANGES
// the single `environment` key line, never anything else.
func diffLines(a, b string) []string {
	inA := map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		inA[l] = true
	}
	var out []string
	for _, l := range strings.Split(b, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !inA[l] {
			out = append(out, l)
		}
	}
	return out
}
