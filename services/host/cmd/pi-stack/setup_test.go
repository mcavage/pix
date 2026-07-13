package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// TestSetup_NonTTYChecklist: with a non-TTY stdin, setup prints the ordered
// checklist and returns without blocking. Nothing installed -> every step is a
// TODO in dependency order, and the closing two-command block is present.
func TestSetup_NonTTYChecklist(t *testing.T) {
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	var buf bytes.Buffer
	sio := setupIO{in: strings.NewReader(""), out: &buf, isTTY: false}

	seeded := false
	seed := func(path string) (bool, error) { seeded = true; return true, nil }
	tokened := false
	ensureToken := func() (string, error) { tokened = true; return "tok", nil }

	steps := runSetup(defaultCfg(), f.env(), sio, seed, ensureToken)
	out := buf.String()

	if !seeded || !tokened {
		t.Fatalf("setup must call seed + ensureToken (seeded=%v tokened=%v)", seeded, tokened)
	}
	if len(steps) == 0 {
		t.Fatal("expected TODO steps when nothing is installed")
	}
	// Non-TTY note present, so it never hangs waiting on input.
	if !strings.Contains(out, "non-interactive") {
		t.Errorf("expected non-interactive note, got:\n%s", out)
	}
	// The two commands that matter + verifier.
	for _, want := range []string{"pi-stack serve", "pi-stack doctor"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected closing command %q, got:\n%s", want, out)
		}
	}
	// Dependency order: prerequisites before provider keys before models.
	iSbx := strings.Index(out, "Prerequisites:")
	iKeys := strings.Index(out, "Provider keys:")
	iModels := strings.Index(out, "Local models")
	if !(iSbx >= 0 && iSbx < iKeys && iKeys < iModels) {
		t.Errorf("sections out of dependency order: sbx=%d keys=%d models=%d", iSbx, iKeys, iModels)
	}
}

// TestSetup_SeedIdempotent: real config.Seed against a temp dir. First run
// writes; second run leaves it as-is (never clobbers). Uses PI_STACK_CONFIG to
// keep it hermetic.
func TestSetup_SeedIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgPath)

	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	tokenCalls := 0
	ensureToken := func() (string, error) { tokenCalls++; return "tok", nil }

	// First run: config absent -> Seed writes it.
	var buf1 bytes.Buffer
	sio1 := setupIO{in: strings.NewReader(""), out: &buf1, isTTY: false}
	runSetup(defaultCfg(), f.env(), sio1, config.Seed, ensureToken)
	if !strings.Contains(buf1.String(), "wrote default config") {
		t.Errorf("first run should write the config, got:\n%s", buf1.String())
	}
	first, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}

	// Second run: config present -> left as-is, byte-identical.
	var buf2 bytes.Buffer
	sio2 := setupIO{in: strings.NewReader(""), out: &buf2, isTTY: false}
	runSetup(defaultCfg(), f.env(), sio2, config.Seed, ensureToken)
	if !strings.Contains(buf2.String(), "already present") {
		t.Errorf("second run should not clobber, got:\n%s", buf2.String())
	}
	second, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("config.Seed clobbered an existing file")
	}
}

// TestSetup_MCPCreds: op section only appears when MCP is configured.
func TestSetup_MCPCreds(t *testing.T) {
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	seed := func(string) (bool, error) { return false, nil }
	ensureToken := func() (string, error) { return "tok", nil }

	// No MCP -> no credentials section.
	var noMCP bytes.Buffer
	runSetup(defaultCfg(), f.env(), setupIO{in: strings.NewReader(""), out: &noMCP}, seed, ensureToken)
	if strings.Contains(noMCP.String(), "MCP credentials:") {
		t.Error("MCP credentials section should be absent when no MCP configured")
	}

	// With MCP -> section present.
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	var withMCP bytes.Buffer
	runSetup(cfg, f.env(), setupIO{in: strings.NewReader(""), out: &withMCP}, seed, ensureToken)
	if !strings.Contains(withMCP.String(), "MCP credentials:") {
		t.Error("MCP credentials section should be present when MCP configured")
	}
}
