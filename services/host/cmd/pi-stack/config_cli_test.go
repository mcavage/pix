package main

import (
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// TestApplyConfigChange_GogAccount: set writes the value, unset clears it.
func TestApplyConfigChange_GogAccount(t *testing.T) {
	cfg := defaultCfg()
	sum, err := applyConfigChange(cfg, false, "gog_account", []string{"me@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GogAccount != "me@x.com" || !strings.Contains(sum, "me@x.com") {
		t.Errorf("set gog_account: cfg=%q summary=%q", cfg.GogAccount, sum)
	}
	if _, err := applyConfigChange(cfg, true, "gog_account", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.GogAccount != "" {
		t.Errorf("unset gog_account: cfg=%q, want empty", cfg.GogAccount)
	}
	// set with the wrong arity errors.
	if _, err := applyConfigChange(cfg, false, "gog_account", nil); err == nil {
		t.Error("expected an arity error for set gog_account with no value")
	}
}

// TestApplyConfigChange_MCP: set adds (idempotent), unset removes.
func TestApplyConfigChange_MCP(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyConfigChange(cfg, false, "mcp", []string{"gog"}); err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg.MCP, "gog") {
		t.Errorf("MCP = %v, want gog added", cfg.MCP)
	}
	// Adding again is a no-op (no duplicate).
	_, _ = applyConfigChange(cfg, false, "mcp", []string{"gog"})
	if n := countStr(cfg.MCP, "gog"); n != 1 {
		t.Errorf("MCP should contain gog exactly once, got %d in %v", n, cfg.MCP)
	}
	if _, err := applyConfigChange(cfg, true, "mcp", []string{"gog"}); err != nil {
		t.Fatal(err)
	}
	if containsStr(cfg.MCP, "gog") {
		t.Errorf("MCP = %v, want gog removed", cfg.MCP)
	}
	if _, err := applyConfigChange(cfg, false, "mcp", nil); err == nil {
		t.Error("expected an error for mcp with no server name")
	}
}

// TestApplyConfigChange_Services: set adds, unset removes.
func TestApplyConfigChange_Services(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyConfigChange(cfg, false, "services", []string{"knowledge"}); err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge added", cfg.Services)
	}
	if _, err := applyConfigChange(cfg, true, "services", []string{"knowledge"}); err != nil {
		t.Fatal(err)
	}
	if containsStr(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge removed", cfg.Services)
	}
}

// TestApplyConfigChange_KnowledgeBundles: set adds the abs bundle path AND
// enables the knowledge service; unset removes the bundle. Adds are deduped and
// canonicalized; the value round-trips through Save/Load into config.toml.
func TestApplyConfigChange_KnowledgeBundles(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", t.TempDir()+"/config.toml")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	abs, _ := filepath.Abs("bundles/okf")
	sum, err := applyConfigChange(cfg, false, "knowledge_bundles", []string{"bundles/okf"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg.KnowledgeBundles, abs) {
		t.Errorf("KnowledgeBundles = %v, want abs path %q added", cfg.KnowledgeBundles, abs)
	}
	// Setting a bundle must also ensure the knowledge service is enabled.
	if !containsStr(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge enabled", cfg.Services)
	}
	if !strings.Contains(sum, "knowledge") {
		t.Errorf("summary = %q, want it to mention knowledge", sum)
	}

	// Adding again is a no-op (dedupe on the canonical path).
	_, _ = applyConfigChange(cfg, false, "knowledge_bundles", []string{"bundles/okf"})
	if n := countStr(cfg.KnowledgeBundles, abs); n != 1 {
		t.Errorf("KnowledgeBundles should contain %q once, got %d in %v", abs, n, cfg.KnowledgeBundles)
	}

	// Save + reload: the config.toml carries the abs path and the service.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(got.KnowledgeBundles, abs) || !containsStr(got.Services, "knowledge") {
		t.Errorf("round-trip lost data: bundles=%v services=%v", got.KnowledgeBundles, got.Services)
	}

	// Unset removes the bundle (the knowledge service stays; unset targets the
	// bundle path only).
	if _, err := applyConfigChange(got, true, "knowledge_bundles", []string{"bundles/okf"}); err != nil {
		t.Fatal(err)
	}
	if containsStr(got.KnowledgeBundles, abs) {
		t.Errorf("KnowledgeBundles = %v, want bundle removed", got.KnowledgeBundles)
	}

	// Arity error: no path.
	if _, err := applyConfigChange(got, false, "knowledge_bundles", nil); err == nil {
		t.Error("expected an arity error for knowledge_bundles with no value")
	}
}

// TestApplyConfigChange_Models: set overrides, unset resets to the default.
func TestApplyConfigChange_Models(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyConfigChange(cfg, false, "memory_watcher_model", []string{"llama3"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryWatcherModel != "llama3" {
		t.Errorf("watcher = %q, want llama3", cfg.MemoryWatcherModel)
	}
	if _, err := applyConfigChange(cfg, true, "memory_watcher_model", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryWatcherModel != config.DefaultMemoryWatcherModel {
		t.Errorf("watcher after unset = %q, want default", cfg.MemoryWatcherModel)
	}
	if _, err := applyConfigChange(cfg, false, "memory_embed_model", []string{"embed-x"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryEmbedModel != "embed-x" {
		t.Errorf("embed = %q, want embed-x", cfg.MemoryEmbedModel)
	}
}

// TestApplyConfigChange_UnknownKey errors and lists the supported keys.
func TestApplyConfigChange_UnknownKey(t *testing.T) {
	_, err := applyConfigChange(defaultCfg(), false, "nope", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("expected unknown-key error, got %v", err)
	}
}

// TestConfigSaveRoundTrip proves the write half of the repo-less workflow: a
// set applied + Save()d + Load()ed back preserves the value (no hand-editing).
func TestConfigSaveRoundTrip(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", t.TempDir()+"/config.toml")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfigChange(cfg, false, "gog_account", []string{"round@trip.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfigChange(cfg, false, "mcp", []string{"gog"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.GogAccount != "round@trip.com" || !containsStr(got.MCP, "gog") {
		t.Errorf("round-trip lost data: %+v", got)
	}
}

// TestApplyProfileConfigChange_GogAccount: --profile targets the profile table,
// creating it if absent; unset clears it.
func TestApplyProfileConfigChange_GogAccount(t *testing.T) {
	cfg := defaultCfg()
	if cfg.Profiles != nil && len(cfg.Profiles) != 0 {
		t.Fatalf("expected no profiles to start, got %v", cfg.Profiles)
	}
	sum, err := applyProfileConfigChange(cfg, false, "work", "gog_account", []string{"me@work.com"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles["work"].GogAccount != "me@work.com" {
		t.Errorf("profiles.work.gog_account = %q, want me@work.com", cfg.Profiles["work"].GogAccount)
	}
	if !strings.Contains(sum, "profiles.work.gog_account") || !strings.Contains(sum, "me@work.com") {
		t.Errorf("summary = %q", sum)
	}
	// The base config must be untouched.
	if cfg.GogAccount != "" {
		t.Errorf("base gog_account leaked = %q, want empty", cfg.GogAccount)
	}
	if _, err := applyProfileConfigChange(cfg, true, "work", "gog_account", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles["work"].GogAccount != "" {
		t.Errorf("unset: profiles.work.gog_account = %q, want empty", cfg.Profiles["work"].GogAccount)
	}
}

// TestApplyProfileConfigChange_MCP: add/remove on a profile's mcp list, deduped.
func TestApplyProfileConfigChange_MCP(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyProfileConfigChange(cfg, false, "personal", "mcp", []string{"fastmail"}); err != nil {
		t.Fatal(err)
	}
	if !containsStr(pv(cfg.Profiles["personal"].MCP), "fastmail") {
		t.Errorf("profiles.personal.mcp = %v, want fastmail added", cfg.Profiles["personal"].MCP)
	}
	// Adding again is a no-op.
	_, _ = applyProfileConfigChange(cfg, false, "personal", "mcp", []string{"fastmail"})
	if n := countStr(pv(cfg.Profiles["personal"].MCP), "fastmail"); n != 1 {
		t.Errorf("mcp should hold fastmail once, got %d in %v", n, cfg.Profiles["personal"].MCP)
	}
	// The base mcp list is untouched.
	if containsStr(cfg.MCP, "fastmail") {
		t.Errorf("base mcp leaked = %v", cfg.MCP)
	}
	if _, err := applyProfileConfigChange(cfg, true, "personal", "mcp", []string{"fastmail"}); err != nil {
		t.Fatal(err)
	}
	if containsStr(pv(cfg.Profiles["personal"].MCP), "fastmail") {
		t.Errorf("unset: profiles.personal.mcp = %v, want fastmail removed", cfg.Profiles["personal"].MCP)
	}
}

// TestApplyProfileConfigChange_KnowledgeBundlesAndKit: bundles canonicalize; the
// kit key writes the per-profile kits.stack.
func TestApplyProfileConfigChange_KnowledgeBundlesAndKit(t *testing.T) {
	cfg := defaultCfg()
	abs, _ := filepath.Abs("bundles/work")
	if _, err := applyProfileConfigChange(cfg, false, "work", "knowledge_bundles", []string{"bundles/work"}); err != nil {
		t.Fatal(err)
	}
	if !containsStr(pv(cfg.Profiles["work"].KnowledgeBundles), abs) {
		t.Errorf("profiles.work.knowledge_bundles = %v, want abs %q", cfg.Profiles["work"].KnowledgeBundles, abs)
	}
	// Per-profile bundles must NOT flip on the global knowledge service.
	if containsStr(cfg.Services, "knowledge") {
		t.Errorf("per-profile bundle should not enable the global knowledge service, services=%v", cfg.Services)
	}
	sum, err := applyProfileConfigChange(cfg, false, "work", "kit", []string{"../work-overlay/kit"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(pv(cfg.Profiles["work"].Kits.Stack), "../work-overlay/kit") {
		t.Errorf("profiles.work.kits.stack = %v, want the kit added", cfg.Profiles["work"].Kits.Stack)
	}
	if !strings.Contains(sum, "kits.stack") {
		t.Errorf("summary = %q, want it to mention kits.stack", sum)
	}
}

// TestApplyProfileConfigChange_GlobalKeysRejected: services + memory_* are
// global; --profile on them is a clear error, not a silent write.
func TestApplyProfileConfigChange_GlobalKeysRejected(t *testing.T) {
	for _, key := range []string{"services", "memory_watcher_model", "memory_embed_model"} {
		cfg := defaultCfg()
		_, err := applyProfileConfigChange(cfg, false, "work", key, []string{"x"})
		if err == nil || !strings.Contains(err.Error(), "global") {
			t.Errorf("key %q with --profile: expected a 'global' error, got %v", key, err)
		}
		if len(cfg.Profiles) != 0 {
			t.Errorf("key %q: a rejected global key must not create a profile, got %v", key, cfg.Profiles)
		}
	}
}

// TestApplyProfileConfigChange_UnknownKey errors.
func TestApplyProfileConfigChange_UnknownKey(t *testing.T) {
	_, err := applyProfileConfigChange(defaultCfg(), false, "work", "nope", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "unknown per-profile key") {
		t.Errorf("expected an unknown-per-profile-key error, got %v", err)
	}
}

// TestProfileConfigRoundTrip: a --profile set + Save + Load preserves the
// profile table and leaves the base config untouched.
func TestProfileConfigRoundTrip(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", t.TempDir()+"/config.toml")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyProfileConfigChange(cfg, false, "work", "gog_account", []string{"me@work.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyProfileConfigChange(cfg, false, "work", "mcp", []string{"gog"}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got.Profiles["work"]
	if !ok {
		t.Fatalf("profile 'work' lost on round-trip: %+v", got.Profiles)
	}
	if p.GogAccount != "me@work.com" || p.MCP == nil || !containsStr(*p.MCP, "gog") {
		t.Errorf("round-trip lost profile data: %+v", p)
	}
	if got.GogAccount != "" || containsStr(got.MCP, "gog") {
		t.Errorf("base config contaminated by profile write: gog=%q mcp=%v", got.GogAccount, got.MCP)
	}
}

// TestSplitProfileArg: pulls --profile / --profile= out of a config-write argv.
func TestSplitProfileArg(t *testing.T) {
	name, rest := splitProfileArg([]string{"--profile", "work", "mcp", "gog"})
	if name != "work" || len(rest) != 2 || rest[0] != "mcp" || rest[1] != "gog" {
		t.Errorf("got name=%q rest=%v", name, rest)
	}
	name, rest = splitProfileArg([]string{"mcp", "--profile=personal", "slack"})
	if name != "personal" || len(rest) != 2 || rest[0] != "mcp" || rest[1] != "slack" {
		t.Errorf("got name=%q rest=%v", name, rest)
	}
	name, rest = splitProfileArg([]string{"gog_account", "me@x.com"})
	if name != "" || len(rest) != 2 {
		t.Errorf("no flag: got name=%q rest=%v", name, rest)
	}
}

func countStr(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}

// pv derefs a *[]string profile field (nil -> nil) for test assertions.
func pv(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
