package provision

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
)

// The onboarding half of provision, tested against REAL boundaries like the rest
// of this package: a real config file on disk and the package's own
// composition (Register/VerifyCatalogMCPReady) rather than a Deps struct built
// for the test. Nothing here stubs an answer.
//
// The allowlist has exactly one member now that packs are gone (Pix v2
// cutover, AC-16): a curated catalog endpoint pix knows the URL for
// (mcp.McpCatalogNames). An onboarding proposal naming anything else is
// refused.

// writeProposal writes <ws>/.pix/onboarding.json against a temp config file.
func writeProposal(t *testing.T, ws, body string) string {
	t.Helper()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	if err := os.MkdirAll(filepath.Join(ws, ".pix"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, ".pix", OnboardingFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateOnboarding_Allowlist(t *testing.T) {
	declared := map[string]config.MCPServer{"notes": {Command: "notes-mcp"}}
	for i, r := range []*OnboardingResult{
		{Version: 1, MCP: []string{"notes"}},
		{Version: 1, MCP: []string{"notion", "atlassian", "granola"}},
	} {
		if err := validateOnboarding(r, declared); err != nil {
			t.Errorf("ok[%d] rejected: %v", i, err)
		}
	}
	bad := map[string]*OnboardingResult{
		"bad version": {Version: 2},
		"unknown mcp": {Version: 1, MCP: []string{"evil-server"}},
		// "linear" is the drift reading mcp.McpCatalogNames directly removes: it
		// looks like a plausible catalog name, but pix knows no endpoint for it,
		// so accepting it would persist a server that never comes up.
		"unshipped catalog-looking mcp": {Version: 1, MCP: []string{"linear"}},
		"model whitespace":              {Version: 1, OllamaBridgeModel: "bad model"},
	}
	for name, r := range bad {
		if err := validateOnboarding(r, declared); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

// The shipped catalog IS the allowlist now that packs are gone: a name
// mcp.McpCatalogNames recognizes is accepted, and one it does not is refused
// with a message naming the fix.
func TestValidateOnboarding_CatalogIsTheAllowlist(t *testing.T) {
	ws := t.TempDir()
	name := "notion"
	writeProposal(t, ws, `{"version":1,"mcp":["`+name+`"]}`)

	var out bytes.Buffer
	ReconcileOnboarding(ws, realEnv(), strings.NewReader(""), &out, true, false)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.MCP, name) {
		t.Errorf("a shipped catalog server must be accepted; mcp=%v out:\n%s", cfg.MCP, out.String())
	}

	// The same host, a name the catalog does NOT know: refused, nothing applied.
	ws2 := t.TempDir()
	path := writeProposal(t, ws2, `{"version":1,"mcp":["warehouse"]}`)
	out.Reset()
	ReconcileOnboarding(ws2, realEnv(), strings.NewReader(""), &out, true, false)
	if !strings.Contains(out.String(), "not a known catalog server") {
		t.Errorf("refusal must say the catalog does not know it, got:\n%s", out.String())
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a refused proposal must be left for review: %v", serr)
	}
	after, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(after.MCP, "warehouse") {
		t.Errorf("an undeclared name must never be persisted: %v", after.MCP)
	}
}

// validateOnboardingShape is the PRE-ADOPTION half, and the reason it exists at
// all: `pix setup --pack X --mcp Y` must validate its own flags before X is
// adopted, and at that moment no pack has declared anything. Checking
// admissibility there would reject exactly the case setup is for. So the shape
// check accepts a syntactically valid undeclared name, and the admissibility
// check (validateOnboarding, run once a pack is present) is what refuses it.
func TestValidateOnboardingShape_PreAdoptionAcceptsUndeclaredNames(t *testing.T) {
	r := &OnboardingResult{Version: 1, MCP: []string{"not-yet-declared"}}
	if err := validateOnboardingShape(r); err != nil {
		t.Errorf("shape validation must not require a declaration: %v", err)
	}
	if err := ValidateSetupSemantics(Opts{Packs: []string{"/some/pack"}, Mcp: []string{"not-yet-declared"}}); err != nil {
		t.Errorf("`pix setup --pack ... --mcp <undeclared>` must pass pre-adoption validation: %v", err)
	}
	// But it is still SHAPE: version and syntax are refused here, not later.
	for name, bad := range map[string]*OnboardingResult{
		"bad version": {Version: 2},
		"empty name":  {Version: 1, MCP: []string{"  "}},
		"whitespace":  {Version: 1, MCP: []string{"two names"}},
	} {
		if err := validateOnboardingShape(bad); err == nil {
			t.Errorf("%s: expected a shape rejection", name)
		}
	}
	// And admissibility still bites once a pack is present to answer.
	if err := validateOnboarding(r, map[string]config.MCPServer{"other": {Command: "x"}}); err == nil {
		t.Error("post-adoption validation must refuse a name no pack declares")
	}
	// --with without --pack is a usage error, not a shape one.
	if err := ValidateSetupSemantics(Opts{WithSetup: []string{"account"}}); err == nil {
		t.Error("--with without --pack must be refused")
	}
}

// applyOnboarding writes exactly the proposal's fields, and re-applying it
// changes nothing (the idempotence fitness function).
func TestApplyOnboarding_FieldsThenIdempotent(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	r := &OnboardingResult{
		Version: 1, MCP: []string{"notes", "notion"},
		OllamaBridgeModel: "qwen3.5:9b", MemoryWatcherModel: "qwen3.5:9b",
	}
	if first := applyOnboarding(r, cfg); len(first) == 0 {
		t.Fatal("first apply made no changes")
	}
	if !slices.Contains(cfg.MCP, "notes") || !slices.Contains(cfg.MCP, "notion") {
		t.Errorf("mcp = %v", cfg.MCP)
	}
	if !slices.Contains(cfg.Services, "memory") {
		t.Errorf("memory service should be ensured: %v", cfg.Services)
	}
	if cfg.OllamaBridgeModel != "qwen3.5:9b" || cfg.MemoryWatcherModel != "qwen3.5:9b" {
		t.Errorf("models not applied: bridge=%q watcher=%q", cfg.OllamaBridgeModel, cfg.MemoryWatcherModel)
	}
	if second := applyOnboarding(r, cfg); len(second) != 0 {
		t.Errorf("second apply not idempotent, changed: %v", second)
	}
}

// The CI path: assumeYes applies the file, persists it, clears the marker — and
// says so about the registration it did NOT perform (Register is composition the
// command layer supplies, unwired here).
func TestReconcileOnboarding_AppliesFromFile(t *testing.T) {
	ws := t.TempDir()
	path := writeProposal(t, ws, `{"version":1,"mcp":["notion"]}`)

	var out bytes.Buffer
	ReconcileOnboarding(ws, realEnv(), strings.NewReader(""), &out, true, false)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("onboarding.json should be removed after apply, err=%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.MCP, "notion") {
		t.Errorf("config not applied: mcp=%v", cfg.MCP)
	}
	if !strings.Contains(out.String(), "mcp add skipped") {
		t.Errorf("an unperformed registration must say so, got:\n%s", out.String())
	}
}

// Two ways a proposal does NOT get applied, and both leave the file for a human
// plus a config with nothing in it: no consent (non-TTY without --yes), and a
// name outside the allowlist.
func TestReconcileOnboarding_LeavesFileWhenNotApplied(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		assumeYes  bool
		declare    bool
		want       string
	}{
		{"no consent", `{"version":1,"mcp":["notion"]}`, false, true, "Not a terminal"},
		{"refused name", `{"version":1,"mcp":["evil-server"]}`, true, false, "refusing onboarding proposal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			path := writeProposal(t, ws, tc.body)
			var out bytes.Buffer
			ReconcileOnboarding(ws, realEnv(), strings.NewReader(""), &out, tc.assumeYes, false)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the file must be left for review, err=%v", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output must explain itself (%q), got:\n%s", tc.want, out.String())
			}
			if cfg, err := config.Load(); err != nil || len(cfg.MCP) != 0 {
				t.Errorf("nothing may be persisted: mcp=%v err=%v", cfg.MCP, err)
			}
		})
	}
}

func TestParseSetupArgs(t *testing.T) {
	o, err := ParseSetupArgs([]string{"--mcp", "notes", "--mcp=notion", "--model", "m",
		"--pack", "one", "--pack=two", "--with", "optional", "--pull-models", "--yes",
		// accepted and discarded: kong still declares it, nothing reads it
		"--models=a,b"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.Model != "m" || !o.AssumeYes || !o.PullModels {
		t.Errorf("parsed = %+v", o)
	}
	if got := strings.Join(o.Mcp, ","); got != "notes,notion" {
		t.Errorf("mcp = %q", got)
	}
	if got := strings.Join(o.Packs, ","); got != "one,two" {
		t.Errorf("packs = %q (order is the invocation order)", got)
	}
	if got := strings.Join(o.WithSetup, ","); got != "optional" {
		t.Errorf("with = %q", got)
	}
	if _, err := ParseSetupArgs([]string{"--model"}); err == nil {
		t.Error("a value flag without its value should error")
	}
	// The retired vendor flags must now be REJECTED, not silently swallowed.
	// They named a Google Workspace transaction that no longer exists, and
	// quietly accepting them would let a stale script look like it worked.
	for _, retired := range []string{"--google-workspace", "--credentials"} {
		if _, err := ParseSetupArgs([]string{retired}); err == nil {
			t.Errorf("%s is retired and must be rejected, not accepted-and-discarded", retired)
		}
	}
	if _, err := ParseSetupArgs([]string{"--bogus"}); err == nil {
		t.Error("an unknown flag should error")
	}
}
