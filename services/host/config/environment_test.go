package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// environment_test.go is the config-schema half of E1.5 (Story 1, native
// sandbox environments, docs/design/environments.md §5.3): `environment =
// "NAME"` (the machine default selection) and `[environments]` (the name ->
// canonical absolute local path registry). Wave C owns the `pix env` verbs
// that call these helpers; this file only proves the schema, canonicalization,
// and sparse-Save contract.

// ── canonicalization ─────────────────────────────────────────────────────

func TestAddEnvironmentCanonicalizesTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no resolvable home dir")
	}
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.AddEnvironment("home", "~/envs/mine")
	if err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	want := filepath.Join(home, "envs", "mine")
	if got != want {
		t.Errorf("AddEnvironment canonical path = %q, want %q", got, want)
	}
	if cfg.Environments["home"] != want {
		t.Errorf("Environments[home] = %q, want %q", cfg.Environments["home"], want)
	}
}

func TestAddEnvironmentCanonicalizesRelativePath(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.AddEnvironment("rel", "envs/rel")
	if err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("AddEnvironment canonical path = %q, want absolute", got)
	}
	if strings.Contains(got, "~") {
		t.Errorf("AddEnvironment canonical path = %q, must not retain ~", got)
	}
}

func TestAddEnvironmentAlreadyAbsoluteStaysClean(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.AddEnvironment("home", "/abs/envs/home")
	if err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if got != "/abs/envs/home" {
		t.Errorf("AddEnvironment canonical path = %q, want unchanged /abs/envs/home", got)
	}
}

// ── registry + selection round trip ─────────────────────────────────────

func TestEnvironmentsMapRoundTrips(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	homePath, err := cfg.AddEnvironment("home", "/abs/envs/home")
	if err != nil {
		t.Fatal(err)
	}
	workPath, err := cfg.AddEnvironment("work", "/abs/envs/work")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("work"); err != nil {
		t.Fatalf("UseEnvironment: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Environment != "work" {
		t.Errorf("Environment = %q, want work", got.Environment)
	}
	if len(got.Environments) != 2 || got.Environments["home"] != homePath || got.Environments["work"] != workPath {
		t.Errorf("Environments = %v, want home=%q work=%q", got.Environments, homePath, workPath)
	}
}

func TestUseEnvironmentRefusesUnregistered(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("nope"); err == nil {
		t.Fatal("expected an error selecting an unregistered environment")
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q, want unchanged empty after a refused selection", cfg.Environment)
	}
}

func TestUseEnvironmentEmptyClearsDefault(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment(""); err != nil {
		t.Fatalf("UseEnvironment(\"\"): %v", err)
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q, want cleared", cfg.Environment)
	}
}

func TestRemoveEnvironmentClearsMatchingDefault(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("home"); err != nil {
		t.Fatal(err)
	}
	if !cfg.RemoveEnvironment("home") {
		t.Fatal("RemoveEnvironment(home) reported no change")
	}
	if _, ok := cfg.Environments["home"]; ok {
		t.Errorf("Environments still has home after RemoveEnvironment")
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q, want cleared (its registration was removed)", cfg.Environment)
	}
}

func TestRemoveEnvironmentLeavesUnrelatedDefaultAlone(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("work", "/abs/work"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("work"); err != nil {
		t.Fatal(err)
	}
	if !cfg.RemoveEnvironment("home") {
		t.Fatal("RemoveEnvironment(home) reported no change")
	}
	if cfg.Environment != "work" {
		t.Errorf("Environment = %q, want unchanged work", cfg.Environment)
	}
}

// ── sparse Save: no default noise, exactly one added key ────────────────

func TestSaveWithNoEnvironmentAddsNoKeys(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = "acme" // force a write, unrelated to environments
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	if strings.Contains(raw, "environment") {
		t.Errorf("raw file petrified environment/environments with nothing set:\n%s", raw)
	}
}

// TestSelectingEnvironmentAddsExactlyOneKey is the byte-diff proof: choosing a
// default among an ALREADY-registered environment changes the file by exactly
// one line, `environment = "home"`. Registration itself is a separate, prior
// write (AddEnvironment/`pix env add`), so this isolates what selection alone
// costs.
func TestSelectingEnvironmentAddsExactlyOneKey(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	before := rawFile(t, path)

	if err := cfg.UseEnvironment("home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	after := rawFile(t, path)

	beforeLines := map[string]bool{}
	for _, l := range strings.Split(strings.TrimRight(before, "\n"), "\n") {
		beforeLines[l] = true
	}
	var added []string
	for _, l := range strings.Split(strings.TrimRight(after, "\n"), "\n") {
		if !beforeLines[l] {
			added = append(added, l)
		}
	}
	if len(added) != 1 || added[0] != `environment = "home"` {
		t.Fatalf("selection diff = %v, want exactly [environment = \"home\"]\nbefore:\n%s\nafter:\n%s", added, before, after)
	}
}

// ── malformed/noncanonical persisted paths fail closed ──────────────────

// TestLoadDropsNoncanonicalEnvironmentPaths: a hand-edited config.toml is the
// only way a `~`-bearing or relative environments path reaches disk (Save()
// only ever writes AddEnvironment's already-canonical output). Loading one
// must not trust it as a local root: it is dropped, surfaced as an unknown
// key (the same "tell them" contract as a retired [plugins.*] slot), and a
// default naming the dropped entry resolves to no default rather than a
// dangling selection.
func TestLoadDropsNoncanonicalEnvironmentPaths(t *testing.T) {
	path := tempConfig(t)
	const toml = "environment = \"home\"\n\n[environments]\n" +
		"home = \"~/envs/home\"\n" +
		"work = \"relative/work\"\n" +
		"good = \"/abs/canonical/good\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Environments["home"]; ok {
		t.Errorf("Environments kept a ~-bearing path: %v", got.Environments)
	}
	if _, ok := got.Environments["work"]; ok {
		t.Errorf("Environments kept a relative path: %v", got.Environments)
	}
	if p, ok := got.Environments["good"]; !ok || p != "/abs/canonical/good" {
		t.Errorf("Environments[good] = %q, ok=%v, want the untouched canonical entry", p, ok)
	}
	if got.Environment != "" {
		t.Errorf("Environment = %q, want cleared (its registration was dropped as noncanonical)", got.Environment)
	}
	unknown := got.UnknownKeys()
	for _, want := range []string{"environments.home", "environments.work"} {
		if !slices.Contains(unknown, want) {
			t.Errorf("UnknownKeys() = %v, want it to include %q", unknown, want)
		}
	}
	if slices.Contains(unknown, "environments.good") {
		t.Errorf("UnknownKeys() wrongly flagged the canonical entry: %v", unknown)
	}
}

func TestLoadDropsDanglingEnvironmentDefault(t *testing.T) {
	path := tempConfig(t)
	const toml = "environment = \"ghost\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Environment != "" {
		t.Errorf("Environment = %q, want cleared (names no registered environment)", got.Environment)
	}
}
