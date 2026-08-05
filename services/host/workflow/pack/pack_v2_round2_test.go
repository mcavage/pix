package pack

// Round-2 review tests for packs-v2 Phase 1 (docs/design/packs-v2-impl.md):
// one (or more) test per finding A–G of the second security + correctness
// review. See the matching fix comments in pack.go / run.go.
//
// Finding A [CRITICAL] (knowledge-ref host-file disclosure + RCE) was retired
// along with the [[knowledge]] facet itself (W2 U03A) — its coverage went
// with it.

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

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
			if err := WriteManifest(dest, Manifest{Name: "adopted", Schema: 1}); err != nil {
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
	// The marker is on disk NOW — before any RunPackUse post-clone step ran
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

// TestPackUse_EmptyLockSwitchRemovesNothing (finding C): with the previous
// pack's activation attribution LOST (the pack.lock is only a hint),
// switching packs removes NOTHING it can't attribute — accumulation is
// accepted over deleting a user's own entry. (The knowledge-bundle half of
// this coverage was retired with the [[knowledge]] facet, W2 U03A; the mcp
// half is what remains.)
func TestPackUse_EmptyLockSwitchRemovesNothing(t *testing.T) {
	dir := isolatePackHost(t)

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, Manifest{Name: "a", Schema: 1, Integrations: []Integration{{Name: "A", MCP: "a-mcp"}}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, Manifest{Name: "b", Schema: 1})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	RunPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"}, registerOK)
	cfg, _ := config.Load()
	if !slices.Contains(cfg.MCP, "a-mcp") {
		t.Fatalf("setup: pack use A did not attach: %+v", cfg)
	}

	// Simulate lost/never-written attribution: drop BOTH the host-state
	// activation record (the authoritative source) and the pack.lock hint.
	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	store.Activations = nil
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(PackLockPath(rootA)); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{rootB}, registerOK)
	cfg2, _ := config.Load()
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
	dir := isolatePackHost(t)

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, Manifest{Name: "a", Schema: 1, Integrations: []Integration{{Name: "A", MCP: "a-mcp"}}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, Manifest{Name: "b", Schema: 1})

	env := fakeGitEnv(nil)
	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	// The reactivation needs no --yes: the first use recorded the acceptance.
	RunPackUse(env, &out, []string{rootA, "--yes"}, registerOK)
	out.Reset()
	RunPackUse(env, &out, []string{rootA}, registerOK) // same-pack reactivation

	lock := readPackLock(rootA)
	if !slices.Contains(lock.MCP, "a-mcp") {
		t.Fatalf("same-pack reactivation erased the lock attribution, lock.MCP = %v", lock.MCP)
	}

	out.Reset()
	RunPackUse(env, &out, []string{rootB}, registerOK)
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
	dir := isolatePackHost(t)

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
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		GogAccount: "work@company.com", OllamaBridgeModel: "work-model"})

	env := fakeGitEnv(nil)
	var out bytes.Buffer
	RunPackUse(env, &out, []string{root}, registerOK)
	cfg1, _ := config.Load()
	if cfg1.GogAccount != "work@company.com" || cfg1.OllamaBridgeModel != "work-model" {
		t.Fatalf("setup: pack did not layer config: %+v", cfg1)
	}

	// The author drops both fields from the manifest, then re-uses the pack.
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1})
	out.Reset()
	RunPackUse(env, &out, []string{root}, registerOK)
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
// pack redeclares) is still handed to mcp.RegisterServers — observable here as the
// registration note from the fake (gateway-less) env, which the old
// only-newly-added gate never produced.
func TestPackUse_RegistersMcpAlreadyPresentInConfig(t *testing.T) {
	dir := isolatePackHost(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("fastmail") // already present BEFORE pack use
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Integrations: []Integration{{Name: "Fastmail", MCP: "fastmail"}}})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	// mcp.RegisterServers must be INVOKED for an already-present pack MCP (retry
	// recovery), not skipped by an only-newly-added gate — observable as its own
	// per-server line for fastmail (here classified remote in the fake env).
	if !strings.Contains(out.String(), "fastmail") {
		t.Errorf("mcp.RegisterServers must run for an already-present pack MCP (retry recovery), got:\n%s", out.String())
	}
}

// TestPackAdd_Mcp_RetryReregisters: `pack add mcp <name>` on the active pack
// re-registers even when the name is already in cfg.MCP, so a retry after a
// failed gateway registration actually recovers instead of silently no-oping.
func TestPackAdd_Mcp_RetryReregisters(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1})
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
	RunPackAdd(fakeGitEnv(nil), &out, []string{"mcp", "fastmail", root, "--yes"}, registerOK)
	// Re-invocation is observable as mcp.RegisterServers' own per-server line for
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
	p := &Info{Root: root, Manifest: Manifest{Name: "p", Proxies: []PackProxy{{Name: "a"}, {Name: "b"}}}}

	kit1, err := SynthesizePackKit(p)
	if err != nil || kit1 == "" {
		t.Fatalf("first synth failed: %q, err=%v", kit1, err)
	}
	kit2, err := SynthesizePackKit(p) // second launch, same pack
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
		if strings.Contains(e.Name(), ".tmp-") || strings.Contains(e.Name(), ".old-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover synth debris: %s", e.Name())
		}
	}
}

// --- finding G [CONCERN]: create-time pack marker round-trip ------------------
