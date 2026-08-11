package pack

import (
	"bytes"
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"slices"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// --- from pack_v2_review_test.go ---
// --- finding #2 [BLOCK]: pack.lock must record only what THIS activation
// actually added -------------------------------------------------------------

// TestPackUse_LockOnlyRecordsWhatThisActivationAdded: when a pack merely
// RE-DECLARES an mcp/knowledge entry the user already had, pack.lock must NOT
// claim it as this activation's contribution — otherwise switching away would
// remove the user's own pre-existing entry.
func TestPackUse_LockOnlyRecordsWhatThisActivationAdded(t *testing.T) {
	dir := isolatePackHost(t)

	// The user already has usersOwnMCP configured, BEFORE any pack use.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP(usersOwnMCP)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, packinfo.Manifest{Name: "a", Schema: 1, Integrations: []packinfo.Integration{
		// The overlapping name the pack merely re-declares, transport and all.
		{Name: "Notes", MCP: usersOwnMCP, Command: "notes-mcp"},
	}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packinfo.Manifest{Name: "b", Schema: 1})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	RunPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"}, registerOK)

	lockA := readPackLock(rootA)
	if slices.Contains(lockA.MCP, usersOwnMCP) {
		t.Fatalf("pack.lock must not claim a pre-existing mcp as its own contribution, lock.MCP = %v", lockA.MCP)
	}

	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{rootB}, registerOK)

	cfgAfterB, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfgAfterB.MCP, usersOwnMCP) {
		t.Errorf("switching away from A must NOT remove the user's pre-existing mcp, cfg.MCP = %v", cfgAfterB.MCP)
	}
}

// --- finding #3 [BLOCK]: reversibility under missing/corrupt/unwritable
// pack.lock -------------------------------------------------------------------

// TestReadPackLock_CorruptFileReturnsSafeDefault: a corrupt pack.lock must not
// yield a partially-decoded, unpredictable lock — it must degrade to the safe
// (empty) default, same as an absent file.
func TestReadPackLock_CorruptFileReturnsSafeDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(PackLockPath(root), []byte("mcp = [\"a\", not valid toml!!! ["), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readPackLock(root)
	want := packLock{}
	if len(got.MCP) != 0 || got.Remote != "" {
		t.Errorf("readPackLock(corrupt) = %+v, want the zero value %+v", got, want)
	}
}

// (finding #3's warn-on-lock-failure behavior was superseded by round-4 F1:
// a lock-write failure now ABORTS `pack use` before cfg.Save — see
// TestPackUse_LockWriteFailureAbortsWithoutCommit in pack_v2_round4_test.go.)

// --- finding #5 [BLOCK]: F4 must switch ALL config, and pack rm must undo
// contributions ---------------------------------------------------------------

// TestPackUse_RestoresScalarToPriorValueOnSwitchAway: a pack that declares a
// config scalar overwrites cfg; switching to a pack that does NOT declare it
// must restore whatever cfg held before (not leak the value across packs, and
// not just leave it stuck). This used to be written against gog_account, which
// went with the built-in Google Workspace surface; ollama_bridge_model is the
// same mechanism and the scalar that remains.
func TestPackUse_RestoresScalarToPriorValueOnSwitchAway(t *testing.T) {
	dir := isolatePackHost(t)

	// A manual value set before any pack was ever active.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OllamaBridgeModel = "manual-model"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	rootWork := filepath.Join(dir, "work")
	mustWritePack(t, rootWork, packinfo.Manifest{Name: "work", Schema: 1, OllamaBridgeModel: "work-model"})
	rootPersonal := filepath.Join(dir, "personal")
	mustWritePack(t, rootPersonal, packinfo.Manifest{Name: "personal", Schema: 1})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{rootWork}, registerOK)
	cfgWork, _ := config.Load()
	if cfgWork.OllamaBridgeModel != "work-model" {
		t.Fatalf("pack use work should set ollama_bridge_model, got %q", cfgWork.OllamaBridgeModel)
	}

	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{rootPersonal}, registerOK)
	cfgPersonal, _ := config.Load()
	if cfgPersonal.OllamaBridgeModel != "manual-model" {
		t.Errorf("switching to a pack with no ollama_bridge_model should restore the PRIOR value, got %q, want %q", cfgPersonal.OllamaBridgeModel, "manual-model")
	}
}

// TestPackRm_RemovesActivePackContributions (finding #5): `pack rm` must undo
// the active pack's mcp + config-scalar contributions, not just clear cfg.Pack —
// otherwise "detached" is a lie about what happened.
func TestPackRm_RemovesActivePackContributions(t *testing.T) {
	dir := isolatePackHost(t)

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, packinfo.Manifest{
		Name:              "work",
		Schema:            1,
		OllamaBridgeModel: "work-model",
		Integrations:      []packinfo.Integration{{Name: "Fastmail", MCP: "fastmail", Command: "fastmail-mcp"}},
	})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	cfgActive, _ := config.Load()
	if !slices.Contains(cfgActive.MCP, "fastmail") || cfgActive.OllamaBridgeModel != "work-model" {
		t.Fatalf("setup: pack use did not attach as expected: %+v", cfgActive)
	}

	out.Reset()
	RunPackRm(&out, nil)

	cfgAfter, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfgAfter.Pack != "" {
		t.Errorf("pack rm should clear the active pack, got %q", cfgAfter.Pack)
	}
	if slices.Contains(cfgAfter.MCP, "fastmail") {
		t.Errorf("pack rm should remove the active pack's mcp contribution, cfg.MCP = %v", cfgAfter.MCP)
	}
	if cfgAfter.OllamaBridgeModel != config.DefaultOllamaBridgeModel {
		t.Errorf("pack rm should revert ollama_bridge_model to its prior (default) value %q, got %q",
			config.DefaultOllamaBridgeModel, cfgAfter.OllamaBridgeModel)
	}
}

// TestPackActivation_ReportsRegisteredDeregisteredNeverAttachedDetached: pack
// activation only ever touches HOST registration (cfg.MCP + the gateway),
// never a live sandbox, so its report must say `registered`/`deregistered`,
// never `attached`/`detached` (those words belong to a claim about a running
// session pix cannot make from here — see mcpLsAttachmentNote).
func TestPackActivation_ReportsRegisteredDeregisteredNeverAttachedDetached(t *testing.T) {
	dir := isolatePackHost(t)

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, packinfo.Manifest{
		Name:         "work",
		Schema:       1,
		Integrations: []packinfo.Integration{{Name: "Fastmail", MCP: "fastmail", Command: "fastmail-mcp"}},
	})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	if !strings.Contains(out.String(), "added to your mcp list: fastmail") {
		t.Errorf("pack use report = %q, want a %q line", out.String(), "added to your mcp list: fastmail")
	}
	for _, bad := range []string{"attached mcp", "detached mcp"} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("pack use report contains %q, a claim about a live sandbox this command never touched:\n%s", bad, out.String())
		}
	}

	out.Reset()
	RunPackRm(&out, nil)
	if !strings.Contains(out.String(), "deregistered mcp: fastmail") {
		t.Errorf("pack rm report = %q, want a %q line", out.String(), "deregistered mcp: fastmail")
	}
	for _, bad := range []string{"attached mcp", "detached mcp:"} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("pack rm report contains %q, a claim about a live sandbox this command never touched:\n%s", bad, out.String())
		}
	}
}

// --- finding #6 [BLOCK]: SynthesizePackKit must rebuild from scratch and fail
// closed -----------------------------------------------------------------------

// TestSynthesizePackKit_RebuildRemovesStaleWrapper: a proxy removed from
// pack.toml since the last synth must not leave its wrapper on the sandbox
// PATH — the kit dir is rebuilt from scratch, not merged into.
func TestSynthesizePackKit_RebuildRemovesStaleWrapper(t *testing.T) {
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

	p1 := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{Name: "p", Proxies: []packinfo.PackProxy{{Name: "a"}, {Name: "b"}}}}
	kit1, err := SynthesizePackKit(p1)
	if err != nil || kit1 == "" {
		t.Fatalf("expected a kit dir, got %q, err=%v", kit1, err)
	}
	if _, err := os.Stat(filepath.Join(kit1, "files", "home", ".local", "bin", "b")); err != nil {
		t.Fatalf("wrapper b should exist after the first synth: %v", err)
	}

	// pack.toml no longer declares "b" (e.g. the author removed it).
	p2 := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{Name: "p", Proxies: []packinfo.PackProxy{{Name: "a"}}}}
	kit2, err := SynthesizePackKit(p2)
	if err != nil || kit2 == "" {
		t.Fatalf("expected a kit dir from the second synth, got %q, err=%v", kit2, err)
	}
	if kit2 == kit1 {
		t.Fatalf("round-3 R2: each launch must synthesize into its OWN unique dir, got %q twice", kit1)
	}
	if _, err := os.Stat(filepath.Join(kit2, "files", "home", ".local", "bin", "b")); err == nil {
		t.Error("finding #6: a removed proxy's wrapper must NOT survive a rebuild")
	}
	if _, err := os.Stat(filepath.Join(kit2, "files", "home", ".local", "bin", "a")); err != nil {
		t.Errorf("wrapper a should still exist: %v", err)
	}
}

// TestSynthesizePackKit_FailsClosedOnUnreadableWrapper: a declared proxy whose
// bin/<name> can't be read must refuse the WHOLE kit ("", no kit) rather than
// silently building a partial one, and must leave a previously-good kit
// untouched (never a half-written swap).
func TestSynthesizePackKit_FailsClosedOnUnreadableWrapper(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "a"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	pGood := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{Name: "p", Proxies: []packinfo.PackProxy{{Name: "a"}}}}
	kitGood, err := SynthesizePackKit(pGood)
	if err != nil || kitGood == "" {
		t.Fatalf("expected the first (good) synth to succeed, got %q, err=%v", kitGood, err)
	}

	// "missing" has no bin/missing file on disk.
	pBad := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{Name: "p", Proxies: []packinfo.PackProxy{{Name: "a"}, {Name: "missing"}}}}
	kitBad, badErr := SynthesizePackKit(pBad)
	if kitBad != "" || badErr == nil {
		t.Errorf("expected (\"\", error) (fail closed) when a declared wrapper is unreadable, got %q, err=%v", kitBad, badErr)
	}
	if badErr != nil && !strings.Contains(badErr.Error(), "refusing") {
		t.Errorf("expected a refusal message, got: %v", badErr)
	}
	// The previously-good kit must be untouched (still has "a").
	if _, err := os.Stat(filepath.Join(kitGood, "files", "home", ".local", "bin", "a")); err != nil {
		t.Errorf("a failed synth must not corrupt the existing kit: %v", err)
	}
}

// --- finding #7 [BLOCK]: pack add mcp must canonicalize the active-pack
// comparison -------------------------------------------------------------------

func TestCanonicalizePackRoot_NormalizesEquivalentPaths(t *testing.T) {
	a := packinfo.CanonicalizePackRoot("/tmp/x/y/../y/work")
	b := packinfo.CanonicalizePackRoot("/tmp/x/y/work")
	if a != b {
		t.Errorf("CanonicalizePackRoot(%q) = %q, want it to equal packinfo.CanonicalizePackRoot(%q) = %q", "/tmp/x/y/../y/work", a, "/tmp/x/y/work", b)
	}
}

// --- finding #8 [CONCERN]: honest live-vs-recreate messaging -----------------

func TestPrintPackRecreateLine_MentionsSkills(t *testing.T) {
	var out bytes.Buffer
	printPackRecreateLine(&out)
	if !strings.Contains(out.String(), "skills") {
		t.Errorf("recreate line should mention skills (also create-only), got:\n%s", out.String())
	}
}

// --- from pack_v2_round2_test.go ---
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
			if err := WriteManifest(dest, packinfo.Manifest{Name: "adopted", Schema: 1}); err != nil {
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
	mustWritePack(t, rootA, packinfo.Manifest{Name: "a", Schema: 1, Integrations: []packinfo.Integration{{Name: "A", MCP: "a-mcp", Command: "a-mcp-bin"}}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packinfo.Manifest{Name: "b", Schema: 1})

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
	mustWritePack(t, rootA, packinfo.Manifest{Name: "a", Schema: 1, Integrations: []packinfo.Integration{{Name: "A", MCP: "a-mcp", Command: "a-mcp-bin"}}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packinfo.Manifest{Name: "b", Schema: 1})

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
// (ollama_bridge_model) REMOVED from the manifest between activations reverts to
// its prior value on the next `pack use` of the same pack, instead of staying
// live forever. Its MCP contribution reconciles the same way.
func TestPackUse_SamePackReactivationReconcilesRemovedFields(t *testing.T) {
	dir := isolatePackHost(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OllamaBridgeModel = "manual-model"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, packinfo.Manifest{Name: "work", Schema: 1,
		OllamaBridgeModel: "work-model",
		Integrations:      []packinfo.Integration{{Name: "Notes", MCP: "notes", Command: "notes-mcp"}}})

	env := fakeGitEnv(nil)
	var out bytes.Buffer
	RunPackUse(env, &out, []string{root, "--yes"}, registerOK)
	cfg1, _ := config.Load()
	if cfg1.OllamaBridgeModel != "work-model" || !slices.Contains(cfg1.MCP, "notes") {
		t.Fatalf("setup: pack did not layer config: %+v", cfg1)
	}

	// The author drops both facets from the manifest, then re-uses the pack.
	mustWritePack(t, root, packinfo.Manifest{Name: "work", Schema: 1})
	out.Reset()
	RunPackUse(env, &out, []string{root}, registerOK)
	cfg2, _ := config.Load()
	if cfg2.OllamaBridgeModel != "manual-model" {
		t.Errorf("removed ollama_bridge_model must revert to prior on re-use, got %q", cfg2.OllamaBridgeModel)
	}
	if slices.Contains(cfg2.MCP, "notes") {
		t.Errorf("a removed integration must stop being registered on re-use, got %v", cfg2.MCP)
	}
}

// --- finding E [BLOCK]: post-Save registration is idempotent over ALL pack
// MCPs, not just newly-added ---------------------------------------------------

// TestPackUse_RegistersMcpAlreadyPresentInConfig: a pack MCP already in cfg.MCP
// (a retry after a failed gateway registration, or a user-preexisting name the
// pack redeclares) is still handed to the registrar, not skipped by an
// only-newly-added gate. The registrar IS the seam now (RegisterFn is injected),
// so the witness is what it was asked to register — and it must arrive with the
// pack's declared server spec, or registration could not resolve the transport.
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
	mustWritePack(t, root, packinfo.Manifest{Name: "work", Schema: 1,
		Integrations: []packinfo.Integration{{Name: "Fastmail", MCP: "fastmail", Command: "fastmail-mcp", Args: []string{"mcp"}}}})

	var out bytes.Buffer
	var registered []string
	var servers map[string]config.MCPServer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, recordRegistrations(&registered, &servers))
	if !slices.Contains(registered, "fastmail") {
		t.Errorf("registration must run for an already-present pack MCP (retry recovery), asked for %v", registered)
	}
	spec, ok := servers["fastmail"]
	if !ok || spec.Command != "fastmail-mcp" || !slices.Equal(spec.Args, []string{"mcp"}) {
		t.Errorf("the registrar must receive the pack's declared spec, got %+v", servers)
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
	p := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{Name: "p", Proxies: []packinfo.PackProxy{{Name: "a"}, {Name: "b"}}}}

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

// --- from pack_v2_round3_test.go ---
// --- S1 [CRITICAL]: pack.lock symlink bypass + arbitrary overwrite ------------

// TestWritePackLock_RefusesSymlinkDest: os.WriteFile FOLLOWS a symlink, so a
// pack.lock symlink planted at the destination must be Lstat-refused — never
// written through — and the symlink's target must stay untouched.
func TestWritePackLock_RefusesSymlinkDest(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious host file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, PackLockPath(root)); err != nil {
		t.Fatal(err)
	}

	err := writePackLock(root, packLock{MCP: []string{"evil"}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writePackLock through a symlink must refuse, got err=%v", err)
	}
	b, rerr := os.ReadFile(victim)
	if rerr != nil || string(b) != "precious host file\n" {
		t.Errorf("symlink target must be untouched, got %q (err=%v)", b, rerr)
	}
}

// TestWritePackLock_AtomicRoundTripNoDebris: the lock is written via a same-dir
// temp file + rename — an overwrite of an existing lock round-trips correctly
// and leaves no temp file behind (the temp-then-rename is what guarantees an
// interrupted write can never truncate the previous lock in place).
func TestWritePackLock_AtomicRoundTripNoDebris(t *testing.T) {
	root := t.TempDir()
	if err := writePackLock(root, packLock{MCP: []string{"first"}}); err != nil {
		t.Fatal(err)
	}
	if err := writePackLock(root, packLock{MCP: []string{"second"}, Remote: "https://example.com/p.git"}); err != nil {
		t.Fatal(err)
	}
	got := readPackLock(root)
	if !slices.Contains(got.MCP, "second") || got.Remote != "https://example.com/p.git" {
		t.Errorf("lock round-trip = %+v", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover lock temp file: %s", e.Name())
		}
	}
	if fi, err := os.Lstat(PackLockPath(root)); err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Errorf("pack.lock must be a regular file, got %v (err=%v)", fi, err)
	}
}

// TestClonePack_ScrubsSymlinkPackLock: the S1 end-to-end. A malicious remote
// pack commits pack.lock as a SYMLINK (-> a host file). clonePack must scrub it
// BEFORE markPackAdopted, so (a) the host file is never written through, and
// (b) a fresh REAL lock carrying the adoption marker lands on disk (the clone
// stays ADOPTED). (The private-local-knowledge-ref half of this end-to-end
// case was retired with the [[knowledge]] facet, W2 U03A.)
func TestClonePack_ScrubsSymlinkPackLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("host secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const url = "https://example.com/attacker/pack.git"
	env := hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "clone" {
			dest := args[len(args)-1]
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", err
			}
			// The attacker's tree: a manifest, plus pack.lock checked in as a
			// symlink at a host file.
			if err := WriteManifest(dest, packinfo.Manifest{Name: "evil", Schema: 1}); err != nil {
				return "", err
			}
			if err := os.Symlink(victim, PackLockPath(dest)); err != nil {
				return "", err
			}
		}
		if len(args) >= 3 && args[2] == "rev-parse" {
			return "def456\n", nil
		}
		return "", nil
	}}}

	dest, err := clonePack(env, &bytes.Buffer{}, url)
	if err != nil {
		t.Fatalf("clonePack: %v", err)
	}
	// The symlink is GONE — replaced by a real lock file carrying adoption.
	fi, lerr := os.Lstat(PackLockPath(dest))
	if lerr != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Fatalf("pack.lock must be a fresh regular file after adoption, got %v (err=%v)", fi, lerr)
	}
	if !isAdoptedPack(dest) {
		t.Error("S1: the clone must read as ADOPTED (fresh real lock, marker intact)")
	}
	if lock := readPackLock(dest); lock.Remote != url || lock.Commit != "def456" {
		t.Errorf("adoption provenance = %+v, want Remote=%s Commit=def456", lock, url)
	}
	// The symlink's target was never written through.
	if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "host secret\n" {
		t.Errorf("symlink target must be untouched, got %q (err=%v)", b, rerr)
	}
	if _, perr := packinfo.LoadPack(dest); perr != nil {
		t.Fatalf("LoadPack: %v", perr)
	}
}

// TestClonePack_ScrubsCheckedInRegularPackLock: a checked-in REGULAR pack.lock
// is hostile too — markPackAdopted MERGES into the existing lock, so remote-
// authored attribution (e.g. MCP=["gog"]) would later cause a switch-away to
// remove the USER'S own MCP entries. A fresh clone's pack.lock always came
// from the remote and must be scrubbed, never merged.
func TestClonePack_ScrubsCheckedInRegularPackLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))

	const url = "https://example.com/attacker/pack2.git"
	env := hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "clone" {
			dest := args[len(args)-1]
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", err
			}
			if err := WriteManifest(dest, packinfo.Manifest{Name: "evil2", Schema: 1}); err != nil {
				return "", err
			}
			// Poisoned attribution: claims the user's own MCP as this pack's.
			if err := os.WriteFile(PackLockPath(dest), []byte("mcp = [\"gog\", \"slack\"]\n"), 0o644); err != nil {
				return "", err
			}
		}
		return "", nil
	}}}

	dest, err := clonePack(env, &bytes.Buffer{}, url)
	if err != nil {
		t.Fatalf("clonePack: %v", err)
	}
	lock := readPackLock(dest)
	if len(lock.MCP) != 0 {
		t.Errorf("remote-authored attribution must be scrubbed, not merged; got MCP=%v", lock.MCP)
	}
	if lock.Remote != url {
		t.Errorf("adoption marker must still be written, got Remote=%q", lock.Remote)
	}
}

// --- R1 [BLOCK]: two-file commit ordering + over-claim tolerance --------------

// TestRevertPackPriorContribution_ToleratesOverclaimingLock: the crash residue
// the R1 ordering deliberately chooses is a lock that OVER-claims (it names
// contributions cfg.Save never committed). Reverting such a lock must be a
// clean no-op: nothing removed, nothing clobbered, no error.
func TestRevertPackPriorContribution_ToleratesOverclaimingLock(t *testing.T) {
	cfg := &config.Config{MCP: []string{"users-own"}, OllamaBridgeModel: "users-own-model"}
	lock := packLock{
		MCP:               []string{"never-committed"},
		OllamaBridgeModel: "pack-model", PriorOllamaBridgeModel: "stale-model",
	}
	removedMCP := revertPackPriorContribution(cfg, lock)
	if len(removedMCP) != 0 {
		t.Errorf("over-claimed entries must remove nothing, got mcp=%v", removedMCP)
	}
	if !slices.Contains(cfg.MCP, "users-own") {
		t.Errorf("a user's own MCP must survive, got %v", cfg.MCP)
	}
	// cfg.OllamaBridgeModel != lock.OllamaBridgeModel => the guarded revert must
	// not fire.
	if cfg.OllamaBridgeModel != "users-own-model" {
		t.Errorf("a scalar must not be clobbered by an over-claiming lock, got %q", cfg.OllamaBridgeModel)
	}
}

// --- R2 [BLOCK]: per-launch unique kit dirs + age-gated sweep ------------------

// TestSweepStaleKitTemps_AgeGatedLaunchDirs: an old (>1h) per-launch kit dir is
// swept on the next synth; a fresh one — possibly a concurrent launch mid-create
// — is left alone.
func TestSweepStaleKitTemps_AgeGatedLaunchDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "a"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{Name: "p", Proxies: []packinfo.PackProxy{{Name: "a"}}}}

	kit1, err := SynthesizePackKit(p)
	if err != nil || kit1 == "" {
		t.Fatalf("first synth failed: %q, err=%v", kit1, err)
	}
	// Plant an OLD launch dir and an old legacy stable dir beside it.
	base := packKitDir(root)
	old := base + kitLaunchInfix + "stale"
	for _, d := range []string{old, base} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(d, past, past); err != nil {
			t.Fatal(err)
		}
	}

	kit2, err := SynthesizePackKit(p)
	if err != nil || kit2 == "" {
		t.Fatalf("second synth failed: %q, err=%v", kit2, err)
	}
	for _, gone := range []string{old, base} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("stale kit dir %s must be swept, stat err=%v", gone, err)
		}
	}
	// kit1 is fresh (same test run) — the age gate must protect it.
	if _, err := os.Stat(filepath.Join(kit1, "spec.yaml")); err != nil {
		t.Errorf("a fresh launch dir must never be swept: %v", err)
	}
}

// --- R3 [BLOCK]: marker only on a DEFINITE create ------------------------------

// --- from pack_v2_round4_test.go ---
// brokenPackLock makes writePackLock(root, ...) fail deterministically: the
// destination pack.lock is a non-empty DIRECTORY, so the atomic tmp+rename in
// writePackLock fails (rename onto a directory), while everything else in the
// pack root (pack.toml, bin/) stays perfectly readable/writable.
func brokenPackLock(t *testing.T, root string) {
	t.Helper()
	lockDir := PackLockPath(root)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- F1: abort-on-lock-failure, unit level -------------------------------------

// commitOnePack is a single-pack commitPackActivation-shaped call, inlined
// here now that the CLI-only wrapper (only `pack add mcp` used it) is gone:
// packTxn.commit is still the ONE commit point `pack use` shares, and these
// tests pin its rollback contract directly.
func commitOnePack(cfg *config.Config, store *PackTrustStore, root string, lock packLock) error {
	return packTxn{
		records:  []packActivationRecord{store.newActivationRecord(root, lock)},
		lockRoot: root,
		lock:     lock,
	}.commit(cfg)
}

// TestPackTxnCommit_LockFailureAbortsBeforeSave: when the lock can't be
// written, packTxn.commit returns an error WITHOUT calling cfg.Save — the
// on-disk config is untouched (here: never even created).
func TestPackTxnCommit_LockFailureAbortsBeforeSave(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "p", Schema: 1})
	brokenPackLock(t, root)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("fastmail")
	cfg.Pack = root

	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	if err := commitOnePack(cfg, store, root, packLock{MCP: []string{"fastmail"}}); err == nil {
		t.Fatal("expected an error when the lock cannot be written")
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("F1: cfg.Save must not run after a lock-write failure; config file exists (stat err=%v)", err)
	}

	// Sanity: with a writable lock the same commit succeeds and saves.
	if err := os.RemoveAll(PackLockPath(root)); err != nil {
		t.Fatal(err)
	}
	if err := commitOnePack(cfg, store, root, packLock{MCP: []string{"fastmail"}}); err != nil {
		t.Fatalf("commit with a writable lock should succeed: %v", err)
	}
	if readPackLock(root).MCP[0] != "fastmail" {
		t.Error("lock not written on the success path")
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(saved.MCP, "fastmail") {
		t.Error("config not saved on the success path")
	}
}

// --- F1: abort-on-lock-failure, end-to-end through RunPackUse ------------------

// TestPackUse_LockWriteFailureAbortsWithoutCommit asserts a forced
// writePackLock failure leaves the config UNCOMMITTED: no MCP added, pack not
// switched — the config file is never written at all.
func TestPackUse_LockWriteFailureAbortsWithoutCommit(t *testing.T) {
	dir := isolatePackHost(t)
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "work", Schema: 1, Integrations: []packinfo.Integration{
		{Name: "fastmail", MCP: "fastmail", Command: "fastmail-mcp"},
	}})
	brokenPackLock(t, root)

	// --yes accepts the Phase-2 Tier-1 gate (the pack declares an mcp) so the
	// run reaches the commit point instead of failing closed at the gate.
	var buf bytes.Buffer
	err := RunPackUse(fakeGitEnv(nil), &buf, []string{root, "--yes"}, registerOK)
	if err == nil {
		t.Fatalf("expected `pack use` to fail on a lock-write failure; output:\n%s", buf.String())
	}
	if out := buf.String() + err.Error(); !strings.Contains(out, "aborting without saving config") {
		t.Errorf("expected the abort message, got:\n%s", out)
	}
	// Nothing committed: the config file must not exist (Save never ran), so
	// the pack was not switched and no MCP was added.
	if _, serr := os.Stat(cfgPath); !os.IsNotExist(serr) {
		b, _ := os.ReadFile(cfgPath)
		t.Errorf("F1: config must stay uncommitted after a lock failure; found:\n%s", b)
	}
}

// --- from pack_v2_phase1_fixups_test.go ---
// --- FIX A: Save failure restores the prior lock -------------------------------

// TestPackTxnCommit_SaveFailureRestoresPriorLock: a same-pack
// reactivation that DROPS an MCP, with the commit forced to fail (read-only
// config dir — under the round-2 A model this now trips the HOST-STATE
// activation write, which shares the config dir and aborts BEFORE cfg.Save),
// must (1) restore the prior pack.lock byte-for-byte, (2) leave the on-disk
// config unchanged, (3) leave the on-disk PRIOR activation record intact, and
// (4) let a subsequent successful `pack rm` remove everything cleanly — no
// orphaned, unattributed MCP.
func TestPackTxnCommit_SaveFailureRestoresPriorLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only dir cannot force cfg.Save to fail")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "work", Schema: 1})

	// Prior activation state: the config carries the pack's MCP contribution
	// and the lock attributes it.
	cfgBefore := "pack = \"" + root + "\"\nmcp = [\"fastmail\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePackLock(root, packLock{MCP: []string{"fastmail"}}); err != nil {
		t.Fatal(err)
	}
	// Round-2 A: the AUTHORITATIVE attribution is the host-state activation
	// record — seed it exactly as the prior activation's commit would have.
	priorStore, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	priorStore.setActivationStack([]packActivationRecord{priorStore.newActivationRecord(root, packLock{MCP: []string{"fastmail"}})})
	if err := priorStore.Save(); err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.ReadFile(PackLockPath(root))
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Same-pack reactivation whose manifest no longer declares the MCP: the
	// contribution is dropped from cfg and the NEW lock is empty.
	if !cfg.RemoveMCP("fastmail") {
		t.Fatal("test setup: fastmail should have been in cfg.MCP")
	}

	// Force cfg.Save to fail: writeFileAtomic's CreateTemp needs a writable
	// config dir.
	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	cerr := commitOnePack(cfg, store, root, packLock{})
	if cerr == nil {
		t.Fatal("expected packTxn.commit to fail when the commit cannot write")
	}
	if !strings.Contains(cerr.Error(), "nothing was committed") {
		t.Errorf("error should say nothing was committed, got: %v", cerr)
	}

	// (1) The prior lock is back, byte-for-byte — NOT the new empty lock.
	lockAfter, err := os.ReadFile(PackLockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockAfter, lockBefore) {
		t.Errorf("FIX A: prior pack.lock must be restored on a Save failure.\nbefore: %q\nafter:  %q", lockBefore, lockAfter)
	}
	// (2) The on-disk config is untouched.
	cfgAfter, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgAfter) != cfgBefore {
		t.Errorf("on-disk config must be unchanged after a Save failure.\nbefore: %q\nafter:  %q", cfgBefore, cfgAfter)
	}

	// (3) The on-disk PRIOR activation record is intact (the failed commit
	// never overwrote it).
	if err := os.Chmod(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	afterStore, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := afterStore.activationFor(root); len(got.MCP) != 1 || got.MCP[0] != "fastmail" {
		t.Errorf("prior activation record must survive a failed commit, got %+v", got)
	}

	// (4) Config writable again: `pack rm` removes everything cleanly — the
	// intact activation record still attributes fastmail, so nothing is
	// orphaned.
	var out bytes.Buffer
	RunPackRm(&out, nil)
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(saved.MCP, "fastmail") {
		t.Errorf("FIX A: pack rm must remove the lock-attributed MCP; config still has it:\n%s", out.String())
	}
	if saved.Pack != "" {
		t.Errorf("pack rm must detach the active pack, still: %q", saved.Pack)
	}
}

// TestPackTxnCommit_SaveFailureRemovesFirstLock: when there was NO
// prior lock (first activation), a Save failure must remove the just-written
// lock — an over-claiming lock beside an uncommitted config is the exact
// divergence FIX A closes.
func TestPackTxnCommit_SaveFailureRemovesFirstLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only dir cannot force cfg.Save to fail")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o555); err != nil { // read-only from the start
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "work", Schema: 1})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("fastmail")
	cfg.Pack = root

	if cerr := commitOnePack(cfg, &PackTrustStore{}, root, packLock{MCP: []string{"fastmail"}}); cerr == nil {
		t.Fatal("expected packTxn.commit to fail when cfg.Save cannot write")
	}
	if _, serr := os.Stat(PackLockPath(root)); !os.IsNotExist(serr) {
		t.Errorf("FIX A: with no prior lock, a Save failure must remove the new lock (stat err=%v)", serr)
	}
}

// --- FIX B: broken active pack fails the launch closed -------------------------

// TestSolicitPackCredentials_AsksForSetupLinkedIntegrations pins the deletion of
// a skip that guaranteed a failure.
//
// The solicitor used to `continue` on any integration with `setup != ""`,
// reasoning that the setup hook would handle the credential. Nothing pix runs
// can put a secret into someone's 1Password vault, so the hook could only ever
// fail on it — and since every integration that needs a credential also has a
// setup step, nothing was ever asked for. `pix setup` then died on the first
// missing reference having offered no way to supply it, which is exactly what
// happened on the first real run.
func TestSolicitPackCredentials_AsksForSetupLinkedIntegrations(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, packinfo.PackManifestName), []byte(`name = "p"
schema = 1

[[integrations]]
  name    = "Acme"
  mcp     = "acme"
  command = "acme-mcp"
  env     = "ACME_TOKEN"
  setup   = "acme"

[[setup]]
  id = "acme"
  required = true
  [[setup.require]]
    kind = "op-ref"
    env  = "ACME_TOKEN"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var buf bytes.Buffer
	// Empty answer declines, so this exercises only whether it ASKS.
	solicitPackCredentials(hostenv.Env{System: &systest.Fake{
		LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		RunFn:      func(string, ...string) (string, error) { return "acct", nil },
	}}, strings.NewReader("\n"), &buf, true, p)
	if !strings.Contains(buf.String(), "ACME_TOKEN") {
		t.Errorf("an integration with a setup hook still needs its credential asked for; got:\n%s", buf.String())
	}
}
