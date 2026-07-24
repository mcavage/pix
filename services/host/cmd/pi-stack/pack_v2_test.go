package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// --- §2 schema ---------------------------------------------------------------

// TestLoadPack_ParsesV2Facets round-trips every v2 facet through pack.toml.
func TestLoadPack_ParsesV2Facets(t *testing.T) {
	root := t.TempDir()
	toml := `name = "work"
schema = 1
ollama_bridge_model = "qwen3.5:9b"
gog_account = "me@company.com"
memory_scope = "work"

[routing]
policy = "routing/policy.json"
scorecard = "routing/scorecard.json"

[[integrations]]
name = "Fastmail"
mcp  = "fastmail"
env  = "FASTMAIL_TOKEN"

[[proxy]]
name = "snowflake"

[[proxy]]
name = "platformio"
host = true

[[bin]]
name = "fastmail-mcp"
path = "bin/fastmail-mcp"
sha  = "9f2c"
host = true

[[knowledge]]
name = "team-runbooks"
source = "https://github.com/acme/runbooks.git"
shared = true

[[knowledge]]
name = "my-notes"
source = "~/notes/okf"
shared = false
`
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	// bin/fastmail-mcp must exist (repo-relative, must not escape/symlink).
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "fastmail-mcp"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	m := p.Manifest
	if m.GogAccount != "me@company.com" || m.MemoryScope != "work" {
		t.Errorf("gog/memory_scope not parsed: %+v", m)
	}
	if m.Routing == nil || m.Routing.Policy != "routing/policy.json" || m.Routing.Scorecard != "routing/scorecard.json" {
		t.Errorf("routing not parsed: %+v", m.Routing)
	}
	if len(m.Proxies) != 2 || m.Proxies[0].Name != "snowflake" || m.Proxies[0].Host {
		t.Errorf("sandbox proxy not parsed: %+v", m.Proxies)
	}
	if !m.Proxies[1].Host || m.Proxies[1].Name != "platformio" {
		t.Errorf("host proxy not parsed: %+v", m.Proxies)
	}
	if len(m.Bins) != 1 || m.Bins[0].SHA != "9f2c" || m.Bins[0].Path != "bin/fastmail-mcp" {
		t.Errorf("bin not parsed: %+v", m.Bins)
	}
	if len(m.Knowledge) != 2 || !m.Knowledge[0].Shared || m.Knowledge[1].Shared {
		t.Errorf("knowledge refs not parsed: %+v", m.Knowledge)
	}
}

// TestLoadPack_RejectsEmptyBinSHA: fail-closed — an unpinned external binary
// never reaches an exec path because it never even loads.
func TestLoadPack_RejectsEmptyBinSHA(t *testing.T) {
	root := t.TempDir()
	toml := "name=\"p\"\nschema=1\n[[bin]]\nname=\"tool\"\npath=\"bin/tool\"\n"
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPack(root); err == nil {
		t.Error("loadPack must reject a [[bin]] with no sha")
	}
}

// TestLoadPack_RejectsInvalidProxyName.
func TestLoadPack_RejectsInvalidProxyName(t *testing.T) {
	root := t.TempDir()
	toml := "name=\"p\"\nschema=1\n[[proxy]]\nname=\"../escape\"\n"
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPack(root); err == nil {
		t.Error("loadPack must reject an unsafe [[proxy]] name")
	}
}

// TestLoadPack_RejectsBinPathEscape: a [[bin]].path that walks out of the pack
// root must be refused at load time.
func TestLoadPack_RejectsBinPathEscape(t *testing.T) {
	root := t.TempDir()
	toml := "name=\"p\"\nschema=1\n[[bin]]\nname=\"tool\"\npath=\"../../etc/passwd\"\nsha=\"deadbeef\"\n"
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPack(root); err == nil {
		t.Error("loadPack must reject a [[bin]].path that escapes the pack root")
	}
}

// --- F2: sandbox bin/ wrappers -------------------------------------------------

// TestPackAdd_Proxy_ScaffoldsWrapperAndManifest: `pack add proxy` writes the
// bin/<name> shim (0755) + a [[proxy]] manifest entry, and prints the recreate
// line (F2/ADR-3) for a non-host proxy.
func TestPackAdd_Proxy_ScaffoldsWrapperAndManifest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "pack")
	env := fakeGitEnv(nil)
	var out bytes.Buffer
	runPackAdd(env, &out, []string{"proxy", "snowflake", root})

	binFile := filepath.Join(root, "bin", "snowflake")
	fi, err := os.Stat(binFile)
	if err != nil {
		t.Fatalf("bin/snowflake not scaffolded: %v", err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/snowflake is not executable: %v", fi.Mode())
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	if len(p.Manifest.Proxies) != 1 || p.Manifest.Proxies[0].Name != "snowflake" || p.Manifest.Proxies[0].Host {
		t.Errorf("proxy manifest entry missing/wrong: %+v", p.Manifest.Proxies)
	}
	if !strings.Contains(out.String(), "pi-stack run --replace") {
		t.Errorf("expected the recreate line, got:\n%s", out.String())
	}
}

// TestPackAdd_Proxy_Host_NoRecreateLine: a host=true proxy is a Phase-2 facet —
// nothing sandbox-related changed, so no recreate line is printed.
func TestPackAdd_Proxy_Host_NoRecreateLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "pack")
	var out bytes.Buffer
	runPackAdd(fakeGitEnv(nil), &out, []string{"proxy", "platformio", root, "--host"})
	if strings.Contains(out.String(), "pi-stack run --replace") {
		t.Errorf("host proxy should not print the sandbox recreate line, got:\n%s", out.String())
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	if len(p.Manifest.Proxies) != 1 || !p.Manifest.Proxies[0].Host {
		t.Errorf("host proxy not recorded: %+v", p.Manifest.Proxies)
	}
}

// TestSynthesizePackKit_SandboxOnly: the ephemeral mixin kit carries only the
// NON-host proxy wrappers, copied (not symlinked) into files/home/.local/bin/.
func TestSynthesizePackKit_SandboxOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "snowflake"), []byte("#!/usr/bin/env bash\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &packInfo{Root: root, Manifest: packManifest{
		Name: "work",
		Proxies: []packProxy{
			{Name: "snowflake"},
			{Name: "platformio", Host: true},
		},
	}}
	kit, err := synthesizePackKit(p)
	if err != nil || kit == "" {
		t.Fatalf("expected a synthesized kit dir, got %q, err=%v", kit, err)
	}
	if _, err := os.Stat(filepath.Join(kit, "spec.yaml")); err != nil {
		t.Errorf("spec.yaml missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kit, "files", "home", ".local", "bin", "snowflake")); err != nil {
		t.Errorf("snowflake wrapper not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kit, "files", "home", ".local", "bin", "platformio")); err == nil {
		t.Error("a host=true proxy must NOT be copied into the sandbox kit")
	}
}

// TestSynthesizePackKit_NoProxiesReturnsEmpty: a pack with no sandbox proxies
// synthesizes nothing (the caller must not stack an empty kit).
func TestSynthesizePackKit_NoProxiesReturnsEmpty(t *testing.T) {
	p := &packInfo{Root: t.TempDir(), Manifest: packManifest{Name: "p"}}
	if kit, err := synthesizePackKit(p); err != nil || kit != "" {
		t.Errorf("expected no kit and no error, got %q, err=%v", kit, err)
	}
}

// --- F1: mcp attach ------------------------------------------------------------

// TestPackAdd_Mcp_NotActive_NoAttachNoRecreate: adding an mcp integration to a
// pack that is NOT the active pack only writes the manifest — nothing in the
// sandbox facet set changed, so no recreate line and no cfg.MCP mutation.
func TestPackAdd_Mcp_NotActive_NoAttachNoRecreate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "pack")
	var out bytes.Buffer
	runPackAdd(fakeGitEnv(nil), &out, []string{"mcp", "fastmail", root, "--env", "FASTMAIL_TOKEN"})

	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	if len(p.Manifest.Integrations) != 1 || p.Manifest.Integrations[0].MCP != "fastmail" || p.Manifest.Integrations[0].Env != "FASTMAIL_TOKEN" {
		t.Errorf("mcp integration not recorded: %+v", p.Manifest.Integrations)
	}
	if strings.Contains(out.String(), "pi-stack run --replace") {
		t.Errorf("inactive pack must not print the recreate line, got:\n%s", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(cfg.MCP, "fastmail") {
		t.Error("cfg.MCP must not gain the mcp until the pack is activated")
	}
}

// TestPackAdd_Mcp_Active_AttachesAndPrintsRecreate: adding an mcp integration to
// the CURRENTLY ACTIVE pack attaches it into cfg.MCP immediately and prints the
// recreate line (F1 + ADR-3).
func TestPackAdd_Mcp_Active_AttachesAndPrintsRecreate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePackManifest(root, packManifest{Name: "work", Schema: 1}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = root
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// F5 (Phase 2): attaching an MCP to the active pack is Tier-1 — the host
	// BoM gate fires; --yes accepts it non-interactively (tests have no TTY).
	runPackAdd(fakeGitEnv(nil), &out, []string{"mcp", "fastmail", root, "--yes"})

	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg2.MCP, "fastmail") {
		t.Errorf("cfg.MCP should gain fastmail on the active pack, got %v", cfg2.MCP)
	}
	if !strings.Contains(out.String(), "pi-stack run --replace") {
		t.Errorf("expected the recreate line, got:\n%s", out.String())
	}
	lock := readPackLock(root)
	if !containsStr(lock.MCP, "fastmail") {
		t.Errorf("pack.lock should record the attached mcp, got %+v", lock)
	}
}

// --- F4: atomic switch, reversibility -----------------------------------------

func mustWritePack(t *testing.T, root string, m packManifest) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePackManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

// TestPackUse_ReversibleSwitch: pack-use(A) -> pack-use(B) -> pack-use(A) yields
// the SAME cfg.MCP as the first pack-use(A) (no accumulation), and a
// user-added MCP present before any pack use survives every switch (§7 fitness
// function #6).
func TestPackUse_ReversibleSwitch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootA, packManifest{Name: "a", Schema: 1, Integrations: []packIntegration{{Name: "A", MCP: "a-mcp"}}})
	mustWritePack(t, rootB, packManifest{Name: "b", Schema: 1, Integrations: []packIntegration{{Name: "B", MCP: "b-mcp"}}})

	// A pre-existing, user-added MCP that no pack ever declared.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("usermcp")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	env := fakeGitEnv(nil)
	var out bytes.Buffer

	// F5 (Phase 2): packs declaring an integration.mcp are Tier-1 — --yes
	// accepts the host BoM non-interactively (tests have no TTY).
	runPackUse(env, &out, []string{rootA, "--yes"})
	cfgAfterA1, _ := config.Load()
	wantAfterA := append([]string(nil), cfgAfterA1.MCP...)

	out.Reset()
	runPackUse(env, &out, []string{rootB, "--yes"})
	cfgAfterB, _ := config.Load()
	if !containsStr(cfgAfterB.MCP, "usermcp") {
		t.Errorf("user-added mcp must survive a switch, cfg.MCP = %v", cfgAfterB.MCP)
	}
	if containsStr(cfgAfterB.MCP, "a-mcp") {
		t.Errorf("switching away from A must remove a-mcp, cfg.MCP = %v", cfgAfterB.MCP)
	}
	if !containsStr(cfgAfterB.MCP, "b-mcp") {
		t.Errorf("switching to B must add b-mcp, cfg.MCP = %v", cfgAfterB.MCP)
	}

	out.Reset()
	runPackUse(env, &out, []string{rootA, "--yes"})
	cfgAfterA2, _ := config.Load()

	if !stringSlicesEqualUnordered(cfgAfterA2.MCP, wantAfterA) {
		t.Errorf("reversible switch: cfg.MCP after A->B->A = %v, want %v (same as first A)", cfgAfterA2.MCP, wantAfterA)
	}
	if !containsStr(cfgAfterA2.MCP, "usermcp") {
		t.Errorf("user-added mcp must survive every switch, final cfg.MCP = %v", cfgAfterA2.MCP)
	}
}

func stringSlicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// TestPackUse_KnowledgeReversible: switching packs swaps knowledge_bundles the
// same way MCP swaps — the previous pack's embedded bundle is removed, the new
// pack's is added, and switching back restores the original set.
func TestPackUse_KnowledgeReversible(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")
	if err := os.MkdirAll(filepath.Join(rootA, "knowledge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "knowledge", "x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootB, "knowledge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "knowledge", "y.md"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, rootA, packManifest{Name: "a", Schema: 1})
	mustWritePack(t, rootB, packManifest{Name: "b", Schema: 1})
	// re-adding knowledge/ dirs after writePackManifest overwrote nothing (manifest
	// write doesn't touch knowledge/), but ensure they still exist for loadPack.

	env := fakeGitEnv(nil)
	var out bytes.Buffer
	runPackUse(env, &out, []string{rootA})
	cfgA, _ := config.Load()
	wantA := append([]string(nil), cfgA.KnowledgeBundles...)

	out.Reset()
	runPackUse(env, &out, []string{rootB})
	cfgB, _ := config.Load()
	aID := canonicalizeKnowledgeBundle(filepath.Join(rootA, "knowledge"))
	bID := canonicalizeKnowledgeBundle(filepath.Join(rootB, "knowledge"))
	if containsStr(cfgB.KnowledgeBundles, aID) {
		t.Errorf("switching away from A should remove its bundle, got %v", cfgB.KnowledgeBundles)
	}
	if !containsStr(cfgB.KnowledgeBundles, bID) {
		t.Errorf("switching to B should add its bundle, got %v", cfgB.KnowledgeBundles)
	}

	out.Reset()
	runPackUse(env, &out, []string{rootA})
	cfgA2, _ := config.Load()
	if !stringSlicesEqualUnordered(cfgA2.KnowledgeBundles, wantA) {
		t.Errorf("reversible switch: knowledge_bundles after A->B->A = %v, want %v", cfgA2.KnowledgeBundles, wantA)
	}
}

// --- F6: knowledge shared vs private -------------------------------------------

// TestPackUse_PrivateKnowledgeNeverTravels: a shared=false [[knowledge]] entry
// resolves to a path OUTSIDE the pack root — it is never copied into the pack's
// own tree, so sharing the pack repo can never leak it (§7 fitness function #2,
// scoped down to a pure assertion: the resolved bundle path is not inside root).
func TestPackUse_PrivateKnowledgeNeverTravels(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	root := filepath.Join(dir, "work")
	privateDir := filepath.Join(dir, "owner-private-notes")
	if err := os.MkdirAll(privateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(privateDir, "secret-plan.md"), []byte("do not share"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1, Knowledge: []packKnowledge{
		{Name: "my-notes", Source: privateDir, Shared: false},
	}})

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	wantID := canonicalizeKnowledgeBundle(privateDir)
	if !containsStr(cfg.KnowledgeBundles, wantID) {
		t.Errorf("private bundle should still be indexed locally, got %v", cfg.KnowledgeBundles)
	}
	// The resolved bundle must NOT live inside the pack's own tree — i.e. it
	// travels with the pack repo only if it is UNDER root, which it must not be.
	if strings.HasPrefix(wantID, canonicalizeKnowledgeBundle(root)+string(filepath.Separator)) {
		t.Errorf("private knowledge %q must not live inside the pack root %q", wantID, root)
	}
	// And the pack.toml reference line is a LOCAL PATH (inert for an adopter who
	// doesn't have that path), never the bundle's content.
	raw, err := os.ReadFile(filepath.Join(root, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "do not share") {
		t.Error("pack.toml must never contain private bundle content")
	}
}

// TestPackAddKnowledge_RefAndPrivateFlags exercises `pack add knowledge --ref
// --private`.
func TestPackAddKnowledge_RefAndPrivateFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "pack")
	var out bytes.Buffer
	runPackAdd(fakeGitEnv(nil), &out, []string{"knowledge", "my-notes", root, "--ref", "/tmp/notes", "--private"})

	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	if len(p.Manifest.Knowledge) != 1 {
		t.Fatalf("expected one knowledge ref, got %+v", p.Manifest.Knowledge)
	}
	k := p.Manifest.Knowledge[0]
	if k.Source != "/tmp/notes" || k.Shared {
		t.Errorf("knowledge ref = %+v, want source=/tmp/notes shared=false", k)
	}
	if !strings.Contains(out.String(), "NOT travel") {
		t.Errorf("expected a private-does-not-travel note, got:\n%s", out.String())
	}
}

// --- F1/ADR-3: recreate line -----------------------------------------------

// TestPackUse_AlwaysPrintsRecreateLine covers §7 fitness function #7: `pack
// use` always emits the recreate instruction.
func TestPackUse_AlwaysPrintsRecreateLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "p")
	mustWritePack(t, root, packManifest{Name: "p", Schema: 1})

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root})
	if !strings.Contains(out.String(), "pi-stack run --replace") {
		t.Errorf("pack use must always print the recreate line, got:\n%s", out.String())
	}
}

// --- secret hygiene ----------------------------------------------------------

// TestSolicitPackCredentials_OnlyWritesOpRefs: op-refs.env only ever gains an
// op:// ref, never a pasted literal, and a non-ref input is skipped (§7 fitness
// function #3).
func TestSolicitPackCredentials_OnlyWritesOpRefs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	env := defaultShellEnv()
	env.lookPath = func(name string) (string, error) {
		if name == "op" {
			return "/usr/bin/op", nil
		}
		return "", os.ErrNotExist
	}
	p := &packInfo{Manifest: packManifest{Integrations: []packIntegration{
		{Name: "Fastmail", MCP: "fastmail", Env: "FASTMAIL_TOKEN"},
	}}}
	in := strings.NewReader("op://Private/Fastmail/token\n")
	var out bytes.Buffer
	solicitPackCredentials(env, in, &out, true, p)

	content, err := os.ReadFile(config.OpRefsPath())
	if err != nil {
		t.Fatalf("op-refs.env not written: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "FASTMAIL_TOKEN=op://Private/Fastmail/token") {
		t.Errorf("expected the op:// ref written, got:\n%s", s)
	}
	// Never a literal secret value anywhere in the file.
	if strings.Contains(s, "xoxb-") || strings.Contains(s, "sk-") {
		t.Errorf("op-refs.env must never carry a literal secret:\n%s", s)
	}
}

// TestSolicitPackCredentials_RejectsPastedLiteral: a non-op:// paste is skipped,
// never written.
func TestSolicitPackCredentials_RejectsPastedLiteral(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	env := defaultShellEnv()
	env.lookPath = func(name string) (string, error) {
		if name == "op" {
			return "/usr/bin/op", nil
		}
		return "", os.ErrNotExist
	}
	p := &packInfo{Manifest: packManifest{Integrations: []packIntegration{
		{Name: "Fastmail", MCP: "fastmail", Env: "FASTMAIL_TOKEN"},
	}}}
	in := strings.NewReader("pasted-literal-secret-123\n")
	var out bytes.Buffer
	solicitPackCredentials(env, in, &out, true, p)

	if _, err := os.Stat(config.OpRefsPath()); err == nil {
		content, _ := os.ReadFile(config.OpRefsPath())
		if strings.Contains(string(content), "pasted-literal-secret-123") {
			t.Error("a pasted non-op:// literal must never be written to op-refs.env")
		}
	}
}

// --- F4: memory scope tag ------------------------------------------------------

func TestWriteMemoryScope_NoExplicitScopeIsShared(t *testing.T) {
	// A pack with NO explicit memory_scope must NOT scope memory to its name —
	// memory stays the single shared store (no profile file => "default"), else
	// conversational captures get hidden from the default recall view.
	ws := t.TempDir()
	p := &packInfo{Manifest: packManifest{Name: "work"}}
	writeMemoryScope(ws, p)
	if _, err := os.Stat(filepath.Join(ws, ".pi-stack", "profile")); err == nil {
		t.Error("a pack without explicit memory_scope must NOT write a scope file (memory stays shared)")
	}
}

func TestWriteMemoryScope_ExplicitOverridesName(t *testing.T) {
	ws := t.TempDir()
	p := &packInfo{Manifest: packManifest{Name: "work", MemoryScope: "shared-team"}}
	writeMemoryScope(ws, p)
	got := strings.TrimSpace(readFile(t, filepath.Join(ws, ".pi-stack", "profile")))
	if got != "shared-team" {
		t.Errorf("profile = %q, want %q", got, "shared-team")
	}
}

func TestWriteMemoryScope_NoPackRemovesStaleFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".pi-stack"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ws, ".pi-stack", "profile")
	if err := os.WriteFile(stale, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMemoryScope(ws, nil)
	if _, err := os.Stat(stale); err == nil {
		t.Error("no active pack should remove the stale profile file")
	}
}

func TestWriteMemoryScope_DefaultNameUnscoped(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".pi-stack"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ws, ".pi-stack", "profile")
	if err := os.WriteFile(stale, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &packInfo{Manifest: packManifest{Name: "default"}}
	writeMemoryScope(ws, p)
	if _, err := os.Stat(stale); err == nil {
		t.Error("a pack named/scoped \"default\" should be unscoped (no profile file)")
	}
}

// --- buildSbxArgs: PackKits stacking -------------------------------------------

// TestBuildSbxArgs_PackKits_NeverSuppressesBaseKit: PackKits must stack
// alongside the base git/local kit pin, unlike o.Kits (the escape hatch) which
// replaces it. This guards the ADR-2 deviation (see docs deviation note).
func TestBuildSbxArgs_PackKits_NeverSuppressesBaseKit(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", PackKits: []string{"/pack/kit"}}, "0.0.99")
	if pinnedGitKit(args) == "" {
		t.Errorf("PackKits must not suppress the base git kit pin, got %v", args)
	}
	if !contains(args, []string{"--kit", "/pack/kit"}) {
		t.Errorf("pack kit missing from %v", args)
	}
}

// TestPackCapabilitiesJSON_LoadedAndMounted: a pack's capabilities.json is
// discovered by loadPack and mounted by synthesizePackKit into the sandbox at
// files/home/.pi/agent/capabilities.json — even with no [[proxy]] entries. This
// is what lets a pack carry its own capability routing (killing the overlay kit).
func TestPackCapabilitiesJSON_LoadedAndMounted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte("name = \"work\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caps := `{"profile":"work","capabilities":{}}`
	if err := os.WriteFile(filepath.Join(root, "capabilities.json"), []byte(caps), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}
	if p.CapabilitiesFile == "" {
		t.Fatal("loadPack did not set CapabilitiesFile for a pack with capabilities.json")
	}
	kit, err := synthesizePackKit(p)
	if err != nil || kit == "" {
		t.Fatalf("expected a kit for a capabilities-only pack, got %q err=%v", kit, err)
	}
	// The synthesized kit's manifest MUST carry schemaVersion — the sbx kit loader
	// rejects a manifest without it ("schemaVersion is required").
	spec, err := os.ReadFile(filepath.Join(kit, "spec.yaml"))
	if err != nil {
		t.Fatalf("kit spec.yaml missing: %v", err)
	}
	if !strings.Contains(string(spec), "schemaVersion:") || !strings.Contains(string(spec), "kind: mixin") {
		t.Fatalf("synthesized kit spec.yaml must declare schemaVersion + kind: mixin, got:\n%s", spec)
	}
	got, err := os.ReadFile(filepath.Join(kit, "files", "home", ".pi", "agent", "capabilities.json"))
	if err != nil {
		t.Fatalf("capabilities.json not mounted into the kit: %v", err)
	}
	if string(got) != caps {
		t.Errorf("mounted capabilities.json mismatch:\n got: %s\nwant: %s", got, caps)
	}
}
