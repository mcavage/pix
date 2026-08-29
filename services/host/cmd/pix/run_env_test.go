package main

// run_env_test.go — the L4 half of E2.5's proofs: what `pix run --env`
// resolves, what the launch composes from it, and the two properties a
// cutover is most likely to break silently — the primary workspace it
// mounts, and a create/attach pair that must fingerprint IDENTICALLY or
// every second `pix run` refuses.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/workflow/launch"
)

const runEnvTestVersion = "0.0.99"

// runEnvCfg is a config with one configured MCP server and one configured
// skill tree — the two facts the pre-cutover argv carried as flags.
func runEnvCfg(t *testing.T, skillDir string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.MCP = []string{"github"}
	cfg.Skills.Paths = []string{skillDir}
	cfg.Kits.Stack = []string{"/kits/stacked"}
	return cfg
}

// The PRIMARY workspace is the run's own project directory: an explicit
// `pix run DIR`, or the default ".", canonicalized. It is NEVER the
// selected environment's source root.
func TestRunEffectiveInput_PrimaryWorkspaceIsTheRunDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	skills := t.TempDir()
	cfg := runEnvCfg(t, skills)
	envRoot := t.TempDir()
	sel := launch.EnvSelection{
		Name: "work", Root: envRoot, Reviewed: true,
		Document: &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1},
	}

	// (a) An explicit DIR.
	explicit := t.TempDir()
	in, err := runEffectiveInput(cfg, launch.RunOpts{Workspace: explicit, Name: "pix-a"}, sel, runEnvTestVersion)
	if err != nil {
		t.Fatalf("runEffectiveInput: %v", err)
	}
	if in.PrimaryWorkspace.Path != explicit {
		t.Errorf("explicit DIR: primary workspace = %q, want %q", in.PrimaryWorkspace.Path, explicit)
	}

	// (b) The default ".", which must canonicalize rather than travel as a
	// relative string into a document sbx resolves elsewhere.
	cwd, _ := os.Getwd()
	in, err = runEffectiveInput(cfg, launch.RunOpts{Workspace: ".", Name: "pix-b"}, sel, runEnvTestVersion)
	if err != nil {
		t.Fatalf("runEffectiveInput: %v", err)
	}
	if in.PrimaryWorkspace.Path != cwd {
		t.Errorf("default \".\": primary workspace = %q, want %q", in.PrimaryWorkspace.Path, cwd)
	}

	// (c) The environment root is a SEPARATE thing and is never mounted.
	for _, ws := range append([]envinfo.WorkspaceFact{in.PrimaryWorkspace, in.PersonalContext}, in.AdditionalWorkspaces...) {
		if ws.Path == envRoot {
			t.Errorf("the environment root %q was composed as a workspace", envRoot)
		}
	}

	// (d) The environment's OTHER workspaces are all there: the personal
	// context unconditionally, plus every configured/flag skill tree.
	if in.PersonalContext.Path != config.ContextDir() {
		t.Errorf("personal context = %q, want %q (unconditional)", in.PersonalContext.Path, config.ContextDir())
	}
	if !hasWorkspace(in.AdditionalWorkspaces, skills) {
		t.Errorf("configured skill tree %q is not mounted: %+v", skills, in.AdditionalWorkspaces)
	}

	// (e) An empty workspace is refused, never resolved by accident.
	if _, err := runEffectiveInput(cfg, launch.RunOpts{Workspace: "", Name: "pix-c"}, sel, runEnvTestVersion); err == nil {
		t.Error("an empty workspace must be refused, not composed from the environment root")
	}
}

// PARITY with the pre-cutover `sbx run` argv: `sbx env create` reads only
// the effective document, so every mount, kit and preloaded MCP server the
// old argv carried as a flag must appear in that document — an active
// pack's contribution included.
func TestRunEffectiveInput_ParityWithLegacySbxArgs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	skills := t.TempDir()
	cfg := runEnvCfg(t, skills)
	packSkills := t.TempDir()
	o := launch.RunOpts{
		Workspace: t.TempDir(),
		Name:      "pix-parity",
		// What ApplyPackContribution folds in for an ACTIVE pack: a skill
		// tree, a generated mixin kit, a preloaded MCP server.
		Skills:    []string{packSkills},
		PackKits:  []string{"/tmp/generated-pi-mixin", "/tmp/pack-mixin"},
		StaticMCP: []string{"github", "pack-warehouse"},
		LocalKit:  "/repo/pi-kit",
	}
	sel := launch.EnvSelection{Document: &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1}}
	in, err := runEffectiveInput(cfg, o, sel, runEnvTestVersion)
	if err != nil {
		t.Fatalf("runEffectiveInput: %v", err)
	}
	legacy := launch.BuildSbxArgs(cfg, o, runEnvTestVersion)

	// Every legacy --static-mcp name is a declared server.
	names := map[string]bool{}
	for _, s := range in.MCPServers {
		names[s.Name] = true
	}
	for i, a := range legacy {
		if a == "--static-mcp" && !names[legacy[i+1]] {
			t.Errorf("legacy --static-mcp %q has no server in the effective document (%v)", legacy[i+1], names)
		}
	}
	// Every legacy --kit is a declared kit (the generated Pi mixin travels
	// as MixinKit and stacks last).
	kits := map[string]bool{in.MixinKit: true}
	for _, k := range in.ExtraKits {
		kits[k] = true
	}
	for i, a := range legacy {
		if a == "--kit" && !kits[legacy[i+1]] {
			t.Errorf("legacy --kit %q is missing from the effective document (%v)", legacy[i+1], sortedKeys(kits))
		}
	}
	// Every legacy extra mount is a declared workspace.
	for _, m := range launch.MountDirs(cfg, o) {
		if m == config.ContextDir() {
			continue // the unconditional personal-context workspace
		}
		if !hasWorkspace(in.AdditionalWorkspaces, m) {
			t.Errorf("legacy mount %q is missing from the effective document", m)
		}
	}
}

// The CREATE fingerprint and the fingerprint an ATTACH recomputes must be
// EQUAL for the same environment: an attach resolves no create-only flag,
// so any create-only fact inside the fingerprint would refuse every second
// `pix run`.
func TestCreationFingerprint_CreateThenAttachDoesNotDrift(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	skills := t.TempDir()
	cfg := runEnvCfg(t, skills)
	ws := t.TempDir()
	sel := launch.EnvSelection{
		Name: "work", Root: t.TempDir(), Reviewed: true,
		Document: &envinfo.Document{
			SchemaVersion: envinfo.SchemaVersionV1,
			Env:           map[string]string{"PIX_MEMORY_SCOPE": "${PIX_MEMORY_SCOPE:-personal}"},
		},
	}
	// A CREATE resolves kits, a pinned local image, a generated mixin kit
	// and the full static-MCP set.
	createOpts := launch.RunOpts{
		Workspace: ws, Name: "pix-drift", LocalKit: "/repo/pi-kit", LocalImageTag: "local-42",
		PackKits: []string{"/tmp/mixin-abc123"}, StaticMCP: []string{"github", "pix-uat-9f"},
	}
	createIn, err := runEffectiveInput(cfg, createOpts, sel, runEnvTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := launch.CreateHMACResolver(filepath.Dir(config.Path()), nil)
	if err != nil {
		t.Fatalf("CreateHMACResolver: %v", err)
	}
	created, reset, err := launch.CreationFingerprint(launch.CreationFactsFor(createIn), resolver)
	if err != nil || reset {
		t.Fatalf("create fingerprint = (reset=%v, %v)", reset, err)
	}

	// The ATTACH: the SAME environment, none of the create-only facts.
	attachOpts := launch.RunOpts{Workspace: ws, Name: "pix-drift"}
	current, reset, err := currentCreationFingerprint(cfg, attachOpts, sel, runEnvTestVersion)
	if err != nil || reset {
		t.Fatalf("attach fingerprint = (reset=%v, %v)", reset, err)
	}
	if drifts := envinfo.Attribute(nil, envinfo.Fingerprint(created), envinfo.Fingerprint(current)); len(drifts) > 0 {
		t.Fatalf("create-then-attach drifted on %d facet(s): %v", len(drifts), drifts)
	}
}

// An unknown `--env` is EXACT-name refused: non-zero, the registry's own
// unknown-environment message, and NOTHING written to config.
func TestResolveRunEnvironment_UnknownNameRefusesAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	registerTier0Env(t, "home")
	before := configHash(t)

	if _, err := resolveRunEnvironment("hoem"); err == nil {
		t.Fatal("an unknown --env must refuse")
	} else if !strings.Contains(err.Error(), "hoem") || !strings.Contains(err.Error(), "home") {
		t.Errorf("refusal must name the typo and the known names, got: %v", err)
	}
	if after := configHash(t); after != before {
		t.Error("an unknown --env wrote to config.toml; --env selects for one run only (AC-22)")
	}

	// The EXACT name resolves; a prefix of it never does.
	sel, err := resolveRunEnvironment("home")
	if err != nil || sel.Name != "home" {
		t.Fatalf("exact name: (%+v, %v)", sel, err)
	}
	if _, err := resolveRunEnvironment("ho"); err == nil {
		t.Error("a prefix must never resolve an environment")
	}
	if after := configHash(t); after != before {
		t.Error("resolving an environment wrote to config.toml")
	}
}

// Two repositories under ONE environment get two distinct pix-* names, and
// therefore two distinct effective files: one environment never collides
// with itself (PRD §5.1).
func TestTwoRepositoriesOneEnvironment_DistinctNames(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	nameA, nameB := resolveSandboxName("", a), resolveSandboxName("", b)
	if nameA == nameB {
		t.Fatalf("two workspaces collided on one sandbox name: %q", nameA)
	}
	for _, n := range []string{nameA, nameB} {
		if !strings.HasPrefix(n, "pix-") {
			t.Errorf("sandbox name %q is outside the pix-* namespace", n)
		}
	}
}

func hasWorkspace(list []envinfo.WorkspaceFact, path string) bool {
	for _, w := range list {
		if w.Path == path {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func configHash(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(config.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return "absent"
		}
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
