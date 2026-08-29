package pack

import (
	"bytes"
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/sys/systest"
)

// --- §2 schema ---------------------------------------------------------------

// TestLoadPack_ParsesV2Facets round-trips every v2 facet through pack.toml.
func TestLoadPack_ParsesV2Facets(t *testing.T) {
	root := t.TempDir()
	toml := `name = "work"
schema = 1
ollama_bridge_model = "qwen3.5:9b"
memory_scope = "work"

# An unknown/legacy facet must never fail the load. gog_account was a real key
# once; a pack still carrying it must load, with the value simply never read.
gog_account = "me@company.com"

[unknown_facet]
key = "value"

[[integrations]]
name    = "Fastmail"
mcp     = "fastmail"
env     = "FASTMAIL_TOKEN"
command = "fastmail-mcp"
args    = ["--readonly", "mcp"]
probe   = ["fastmail-mcp", "--version"]

[[proxy]]
name = "warehouse"

[[proxy]]
name = "platformio"
host = true

[[bin]]
name = "fastmail-mcp"
path = "bin/fastmail-mcp"
sha  = "9f2c"
host = true
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

	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	m := p.Manifest
	if m.MemoryScope != "work" || m.OllamaBridgeModel != "qwen3.5:9b" {
		t.Errorf("scalars not parsed: %+v", m)
	}
	// The transport triple is what registration and the trust screen both read,
	// so all three round-trip or neither does.
	if len(m.Integrations) != 1 {
		t.Fatalf("integrations = %+v", m.Integrations)
	}
	ig := m.Integrations[0]
	if ig.Command != "fastmail-mcp" || strings.Join(ig.Args, " ") != "--readonly mcp" {
		t.Errorf("command transport not parsed: %+v", ig)
	}
	if strings.Join(ig.Probe, " ") != "fastmail-mcp --version" {
		t.Errorf("probe not parsed: %+v", ig)
	}
	if len(m.Proxies) != 2 || m.Proxies[0].Name != "warehouse" || m.Proxies[0].Host {
		t.Errorf("sandbox proxy not parsed: %+v", m.Proxies)
	}
	if !m.Proxies[1].Host || m.Proxies[1].Name != "platformio" {
		t.Errorf("host proxy not parsed: %+v", m.Proxies)
	}
	if len(m.Bins) != 1 || m.Bins[0].SHA != "9f2c" || m.Bins[0].Path != "bin/fastmail-mcp" {
		t.Errorf("bin not parsed: %+v", m.Bins)
	}
}

// TestLoadPack_RejectsUnsafeV2Facets table-drives fail-closed load-time
// rejections across the v2 facets: an unpinned external binary never reaches
// an exec path because it never even loads (empty sha), a [[bin]].path that
// walks out of the pack root is refused, and an unsafe [[proxy]] name is
// refused too.
func TestLoadPack_RejectsUnsafeV2Facets(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{"empty bin sha", "name=\"p\"\nschema=1\n[[bin]]\nname=\"tool\"\npath=\"bin/tool\"\n"},
		{"invalid proxy name", "name=\"p\"\nschema=1\n[[proxy]]\nname=\"../escape\"\n"},
		{"bin path escape", "name=\"p\"\nschema=1\n[[bin]]\nname=\"tool\"\npath=\"../../etc/passwd\"\nsha=\"deadbeef\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte(tc.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := packinfo.LoadPack(root); err == nil {
				t.Errorf("LoadPack must reject: %s", tc.name)
			}
		})
	}
}

// --- F2: sandbox bin/ wrappers -------------------------------------------------

// TestSynthesizePackKit_SandboxOnly: the ephemeral mixin kit carries only the
// NON-host proxy wrappers, copied (not symlinked) into files/home/.local/bin/.
func TestSynthesizePackKit_SandboxOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "warehouse"), []byte("#!/usr/bin/env bash\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{
		Name: "work",
		Proxies: []packinfo.PackProxy{
			{Name: "warehouse"},
			{Name: "platformio", Host: true},
		},
	}}
	kit, err := SynthesizePackKit(p)
	if err != nil || kit == "" {
		t.Fatalf("expected a synthesized kit dir, got %q, err=%v", kit, err)
	}
	if _, err := os.Stat(filepath.Join(kit, "spec.yaml")); err != nil {
		t.Errorf("spec.yaml missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kit, "files", "home", ".local", "bin", "warehouse")); err != nil {
		t.Errorf("warehouse wrapper not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kit, "files", "home", ".local", "bin", "platformio")); err == nil {
		t.Error("a host=true proxy must NOT be copied into the sandbox kit")
	}
}

// TestSynthesizePackKit_NoProxiesReturnsEmpty: a pack with no sandbox proxies
// synthesizes nothing (the caller must not stack an empty kit).
func TestSynthesizePackKit_NoProxiesReturnsEmpty(t *testing.T) {
	p := &packinfo.Info{Root: t.TempDir(), Manifest: packinfo.Manifest{Name: "p"}}
	if kit, err := SynthesizePackKit(p); err != nil || kit != "" {
		t.Errorf("expected no kit and no error, got %q, err=%v", kit, err)
	}
}

// --- F4: atomic switch, reversibility -----------------------------------------

func mustWritePack(t *testing.T, root string, m packinfo.Manifest) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

// TestPackUse_ReversibleSwitch: pack-use(A) -> pack-use(B) -> pack-use(A) yields
// the SAME cfg.MCP as the first pack-use(A) (no accumulation), and a
// user-added MCP present before any pack use survives every switch (§7 fitness
// function #6).
func TestPackUse_ReversibleSwitch(t *testing.T) {
	dir := isolatePackHost(t)

	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootA, packinfo.Manifest{Name: "a", Schema: 1, Integrations: []packinfo.Integration{{Name: "A", MCP: "a-mcp", Command: "a-mcp-bin"}}})
	mustWritePack(t, rootB, packinfo.Manifest{Name: "b", Schema: 1, Integrations: []packinfo.Integration{{Name: "B", MCP: "b-mcp", URL: "https://b.example.test/mcp"}}})

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
	RunPackUse(env, &out, []string{rootA, "--yes"}, registerOK)
	cfgAfterA1, _ := config.Load()
	wantAfterA := append([]string(nil), cfgAfterA1.MCP...)

	out.Reset()
	RunPackUse(env, &out, []string{rootB, "--yes"}, registerOK)
	cfgAfterB, _ := config.Load()
	if !slices.Contains(cfgAfterB.MCP, "usermcp") {
		t.Errorf("user-added mcp must survive a switch, cfg.MCP = %v", cfgAfterB.MCP)
	}
	if slices.Contains(cfgAfterB.MCP, "a-mcp") {
		t.Errorf("switching away from A must remove a-mcp, cfg.MCP = %v", cfgAfterB.MCP)
	}
	if !slices.Contains(cfgAfterB.MCP, "b-mcp") {
		t.Errorf("switching to B must add b-mcp, cfg.MCP = %v", cfgAfterB.MCP)
	}

	out.Reset()
	RunPackUse(env, &out, []string{rootA, "--yes"}, registerOK)
	cfgAfterA2, _ := config.Load()

	if !stringSlicesEqualUnordered(cfgAfterA2.MCP, wantAfterA) {
		t.Errorf("reversible switch: cfg.MCP after A->B->A = %v, want %v (same as first A)", cfgAfterA2.MCP, wantAfterA)
	}
	if !slices.Contains(cfgAfterA2.MCP, "usermcp") {
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

// The [[knowledge]] ref facet's reversible-swap and private-never-travels
// coverage was retired along with the facet itself (W2 U03A). The embedded
// knowledge/ dir stays (packinfo.LoadPack's KnowledgeDir; a plain markdown file placed
// there by hand), just inert.

// --- F1/ADR-3: recreate line -----------------------------------------------

// TestPackUse_AlwaysPrintsRecreateLine covers §7 fitness function #7: `pack
// use` always emits the recreate instruction.
func TestPackUse_AlwaysPrintsRecreateLine(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "p")
	mustWritePack(t, root, packinfo.Manifest{Name: "p", Schema: 1})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK)
	if !strings.Contains(out.String(), "pix rm <box> && pix run") {
		t.Errorf("pack use must always print the recreate line, got:\n%s", out.String())
	}
}

// --- secret hygiene ----------------------------------------------------------

// TestSolicitPackCredentials_OnlyWritesOpRefs: op-refs.env only ever gains an
// op:// ref, never a pasted literal, and a non-ref input is skipped (§7 fitness
// function #3).
func TestSolicitPackCredentials_OnlyWritesOpRefs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	// Real OS (this asserts on a file actually written to disk), with LookPath
	// faked so `op` resolves without one installed.
	env := hostenv.Env{System: &systest.Fake{
		Base: sys.Real{},
		LookPathFn: func(name string) (string, error) {
			if name == "op" {
				return "/usr/bin/op", nil
			}
			return "", os.ErrNotExist
		},
	}}
	p := &packinfo.Info{Manifest: packinfo.Manifest{Integrations: []packinfo.Integration{
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
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	// Real OS (this asserts on a file actually written to disk), with LookPath
	// faked so `op` resolves without one installed.
	env := hostenv.Env{System: &systest.Fake{
		Base: sys.Real{},
		LookPathFn: func(name string) (string, error) {
			if name == "op" {
				return "/usr/bin/op", nil
			}
			return "", os.ErrNotExist
		},
	}}
	p := &packinfo.Info{Manifest: packinfo.Manifest{Integrations: []packinfo.Integration{
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
	p := &packinfo.Info{Manifest: packinfo.Manifest{Name: "work"}}
	packinfo.WriteMemoryScope(ws, p)
	if _, err := os.Stat(filepath.Join(ws, ".pix", "profile")); err == nil {
		t.Error("a pack without explicit memory_scope must NOT write a scope file (memory stays shared)")
	}
}

func TestWriteMemoryScope_ExplicitOverridesName(t *testing.T) {
	ws := t.TempDir()
	p := &packinfo.Info{Manifest: packinfo.Manifest{Name: "work", MemoryScope: "shared-team"}}
	packinfo.WriteMemoryScope(ws, p)
	got := strings.TrimSpace(readFile(t, filepath.Join(ws, ".pix", "profile")))
	if got != "shared-team" {
		t.Errorf("profile = %q, want %q", got, "shared-team")
	}
}

func TestWriteMemoryScope_NoPackRemovesStaleFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".pix"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ws, ".pix", "profile")
	if err := os.WriteFile(stale, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	packinfo.WriteMemoryScope(ws, nil)
	if _, err := os.Stat(stale); err == nil {
		t.Error("no active pack should remove the stale profile file")
	}
}

func TestWriteMemoryScope_DefaultNameUnscoped(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".pix"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ws, ".pix", "profile")
	if err := os.WriteFile(stale, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &packinfo.Info{Manifest: packinfo.Manifest{Name: "default"}}
	packinfo.WriteMemoryScope(ws, p)
	if _, err := os.Stat(stale); err == nil {
		t.Error("a pack named/scoped \"default\" should be unscoped (no profile file)")
	}
}

// --- buildSbxArgs: PackKits stacking -------------------------------------------

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
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	if p.CapabilitiesFile == "" {
		t.Fatal("LoadPack did not set CapabilitiesFile for a pack with capabilities.json")
	}
	kit, err := SynthesizePackKit(p)
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

func TestPackWebSearchJSONLoadedAndMounted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte("name = \"work\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := `{"provider":"openai","openaiBaseUrl":"https://models.example.test/v1","openaiModel":"reasoner"}`
	if err := os.WriteFile(filepath.Join(root, "web-search.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	if p.WebSearchFile == "" {
		t.Fatal("LoadPack did not set WebSearchFile")
	}
	kit, err := SynthesizePackKit(p)
	if err != nil || kit == "" {
		t.Fatalf("expected web-search-only pack kit, got %q err=%v", kit, err)
	}
	got, err := os.ReadFile(filepath.Join(kit, "files", "home", ".pi", "web-search.json"))
	if err != nil {
		t.Fatalf("web-search.json not mounted: %v", err)
	}
	if string(got) != config {
		t.Fatalf("mounted web-search.json = %s, want %s", got, config)
	}
}

func TestPackWebSearchJSONRejectsMalformedAndSymlink(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(string) error
	}{
		{name: "malformed", make: func(root string) error {
			return os.WriteFile(filepath.Join(root, "web-search.json"), []byte(`{"provider":`), 0o644)
		}},
		{name: "non-object", make: func(root string) error {
			return os.WriteFile(filepath.Join(root, "web-search.json"), []byte(`[]`), 0o644)
		}},
		{name: "symlink", make: func(root string) error {
			target := filepath.Join(root, "target.json")
			if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(root, "web-search.json"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte("name = \"work\"\nschema = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tc.make(root); err != nil {
				t.Fatal(err)
			}
			if _, err := packinfo.LoadPack(root); err == nil || !strings.Contains(err.Error(), "web-search.json") {
				t.Fatalf("LoadPack error = %v, want web-search.json rejection", err)
			}
		})
	}
}

// TestSynthesizePackKit_EgressAllow: a sandbox [[proxy]] with egress emits
// permissions.network.allow into the synthesized mixin kit, so the wrapper can reach
// its host endpoint (else the sbx egress proxy 403s host.docker.internal).
func TestSynthesizePackKit_EgressAllow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "snow"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{
		Name:    "work",
		Proxies: []packinfo.PackProxy{{Name: "snow", Egress: []string{"host.docker.internal:11442"}}},
	}}
	kit, err := SynthesizePackKit(p)
	if err != nil || kit == "" {
		t.Fatalf("kit=%q err=%v", kit, err)
	}
	b, _ := os.ReadFile(filepath.Join(kit, "spec.yaml"))
	for _, want := range []string{"permissions:", "host.docker.internal:11442", "localhost:11442"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("proxy egress missing %q in permissions.network.allow:\n%s", want, b)
		}
	}
}
