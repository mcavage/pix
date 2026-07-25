package main

// Tests for the packs-v2 Phase 1 review + security findings
// (docs/design/packs-v2-impl.md). One (or more) test per numbered finding in
// the review; see the fix comments in pack.go/run.go/config/config.go for the
// finding each test guards.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

// --- finding #1 [CRITICAL SECURITY]: adopted-pack private knowledge refs must
// never read host files -----------------------------------------------------

// TestResolvePackKnowledgeRef_RejectsAdoptedPrivate: a shared=false local-path
// reference is NEVER honored when the pack is adopted (cloned from a remote) —
// pack.toml there is attacker-controlled, so this is the CRITICAL guard against
// AddKnowledgeBundle indexing an arbitrary host directory (e.g. ~/.ssh).
func TestResolvePackKnowledgeRef_RejectsAdoptedPrivate(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir() // stands in for e.g. ~/.ssh
	if err := os.WriteFile(filepath.Join(target, "id_rsa"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := packKnowledge{Name: "attacker-ref", Source: target, Shared: false}
	_, err := resolvePackKnowledgeRef(&bytes.Buffer{}, root, true /* adopted */, k)
	if err == nil {
		t.Fatal("expected an error resolving a private ref on an adopted pack")
	}
	if err != errPrivateRefSkippedAdopted {
		t.Errorf("expected errPrivateRefSkippedAdopted, got %v", err)
	}
}

// TestResolvePackKnowledgeRef_AllowsAuthoredPrivate: the SAME reference is fine
// for a pack the user authored locally (adopted=false) — the whole point of a
// private reference.
func TestResolvePackKnowledgeRef_AllowsAuthoredPrivate(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	k := packKnowledge{Name: "my-notes", Source: target, Shared: false}
	resolved, err := resolvePackKnowledgeRef(&bytes.Buffer{}, root, false, k)
	if err != nil {
		t.Fatalf("resolvePackKnowledgeRef: %v", err)
	}
	want := canonicalizeKnowledgeBundle(target)
	if resolved != want {
		t.Errorf("resolved = %q, want %q", resolved, want)
	}
}

// TestResolvePackKnowledgeRef_RejectsPathInsidePackTree (finding #1, sub (b)):
// a private reference that resolves INSIDE the pack's own tree must be
// rejected — the author should embed it under knowledge/ instead.
func TestResolvePackKnowledgeRef_RejectsPathInsidePackTree(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "private-notes")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	k := packKnowledge{Name: "oops", Source: inside, Shared: false}
	_, err := resolvePackKnowledgeRef(&bytes.Buffer{}, root, false, k)
	if err == nil {
		t.Fatal("expected an error for a private ref resolving inside the pack root")
	}
	if !strings.Contains(err.Error(), "embed") {
		t.Errorf("expected the error to suggest embedding, got: %v", err)
	}
}

// TestResolvePackKnowledgeRef_SkipsNonexistentLocalDir (finding #1, sub (a)):
// a private reference to a path that doesn't exist (or isn't a directory) must
// be refused, never handed to AddKnowledgeBundle (no knowledge-service
// poisoning with a dangling entry).
func TestResolvePackKnowledgeRef_SkipsNonexistentLocalDir(t *testing.T) {
	root := t.TempDir()
	k := packKnowledge{Name: "typo", Source: filepath.Join(root, "..", "does-not-exist-xyz"), Shared: false}
	if _, err := resolvePackKnowledgeRef(&bytes.Buffer{}, root, false, k); err == nil {
		t.Fatal("expected an error for a nonexistent private knowledge dir")
	}

	// A file (not a directory) is also refused.
	notADir := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	k2 := packKnowledge{Name: "file-not-dir", Source: notADir, Shared: false}
	if _, err := resolvePackKnowledgeRef(&bytes.Buffer{}, root, false, k2); err == nil {
		t.Fatal("expected an error for a private knowledge ref that is a file, not a dir")
	}
}

// TestPackUse_AdoptedPackSkipsPrivateKnowledgeRef is the CRITICAL end-to-end
// regression test: `pack use` of an adopted pack (pack.lock already carries a
// Remote from a prior clone — the same state a real `pack use <git-url>`
// leaves behind) with a shared=false [[knowledge]] entry pointing at a
// sensitive host directory must NOT index it, and must tell the user it
// skipped it.
func TestPackUse_AdoptedPackSkipsPrivateKnowledgeRef(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	root := filepath.Join(dir, "adopted-pack")
	sensitive := filepath.Join(dir, "ssh-stand-in")
	if err := os.MkdirAll(sensitive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sensitive, "id_rsa"), []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, root, packManifest{Name: "adopted", Schema: 1, Knowledge: []packKnowledge{
		{Name: "attacker-ref", Source: sensitive, Shared: false},
	}})
	// Simulate this pack having been cloned via `pack use <git-url>` at some
	// earlier point: its pack.lock already carries adoption provenance.
	if err := writePackLock(root, packLock{Remote: "https://example.com/attacker/pack.git"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	sensitiveID := canonicalizeKnowledgeBundle(sensitive)
	if containsStr(cfg.KnowledgeBundles, sensitiveID) {
		t.Fatalf("CRITICAL: adopted pack's private knowledge ref was indexed! cfg.KnowledgeBundles = %v", cfg.KnowledgeBundles)
	}
	if !strings.Contains(out.String(), "skipped 1 private knowledge ref") {
		t.Errorf("expected a skip notice, got:\n%s", out.String())
	}
	// Adoption marker must survive the rewrite.
	if !isAdoptedPack(root) {
		t.Error("pack.lock should still carry the adoption marker after `pack use`")
	}
}

// --- finding #2 [BLOCK]: pack.lock must record only what THIS activation
// actually added -------------------------------------------------------------

// TestPackUse_LockOnlyRecordsWhatThisActivationAdded: when a pack merely
// RE-DECLARES an mcp/knowledge entry the user already had, pack.lock must NOT
// claim it as this activation's contribution — otherwise switching away would
// remove the user's own pre-existing entry.
func TestPackUse_LockOnlyRecordsWhatThisActivationAdded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	// The user already has gwServerName configured, BEFORE any pack use.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP(gwServerName)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, packManifest{Name: "a", Schema: 1, Integrations: []packIntegration{
		{Name: "Gog", MCP: gwServerName}, // overlapping name the pack merely re-declares
	}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packManifest{Name: "b", Schema: 1})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	runPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"})

	lockA := readPackLock(rootA)
	if containsStr(lockA.MCP, gwServerName) {
		t.Fatalf("pack.lock must not claim a pre-existing mcp as its own contribution, lock.MCP = %v", lockA.MCP)
	}

	out.Reset()
	runPackUse(fakeGitEnv(nil), &out, []string{rootB})

	cfgAfterB, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfgAfterB.MCP, gwServerName) {
		t.Errorf("switching away from A must NOT remove the user's pre-existing gog mcp, cfg.MCP = %v", cfgAfterB.MCP)
	}
}

// --- finding #3 [BLOCK]: reversibility under missing/corrupt/unwritable
// pack.lock -------------------------------------------------------------------

// TestReadPackLock_CorruptFileReturnsSafeDefault: a corrupt pack.lock must not
// yield a partially-decoded, unpredictable lock — it must degrade to the safe
// (empty) default, same as an absent file.
func TestReadPackLock_CorruptFileReturnsSafeDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(packLockPath(root), []byte("mcp = [\"a\", not valid toml!!! ["), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readPackLock(root)
	want := packLock{}
	if len(got.MCP) != 0 || len(got.Knowledge) != 0 || got.Remote != "" {
		t.Errorf("readPackLock(corrupt) = %+v, want the zero value %+v", got, want)
	}
}

// (finding #3's warn-on-lock-failure behavior was superseded by round-4 F1:
// a lock-write failure now ABORTS `pack use` before cfg.Save — see
// TestPackUse_LockWriteFailureAbortsWithoutCommit in pack_v2_round4_test.go.)

// --- finding #5 [BLOCK]: F4 must switch ALL config, and pack rm must undo
// contributions ---------------------------------------------------------------

// TestPackUse_RestoresGogAccountToPriorValueOnSwitchAway: a pack that declares
// gog_account overwrites cfg.GogAccount; switching to a pack that does NOT
// declare it must restore whatever cfg held before (not leak the value across
// packs, and not just leave it stuck).
func TestPackUse_RestoresGogAccountToPriorValueOnSwitchAway(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	// A manual value set before any pack was ever active.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("manual@example.com")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	rootWork := filepath.Join(dir, "work")
	mustWritePack(t, rootWork, packManifest{Name: "work", Schema: 1, GogAccount: "work@company.com"})
	rootPersonal := filepath.Join(dir, "personal")
	mustWritePack(t, rootPersonal, packManifest{Name: "personal", Schema: 1})

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{rootWork})
	cfgWork, _ := config.Load()
	if cfgWork.GogAccount != "work@company.com" {
		t.Fatalf("pack use work should set gog_account, got %q", cfgWork.GogAccount)
	}

	out.Reset()
	runPackUse(fakeGitEnv(nil), &out, []string{rootPersonal})
	cfgPersonal, _ := config.Load()
	if cfgPersonal.GogAccount != "manual@example.com" {
		t.Errorf("switching to a pack with no gog_account should restore the PRIOR value, got %q, want %q", cfgPersonal.GogAccount, "manual@example.com")
	}
}

// TestPackRm_RemovesActivePackContributions (finding #5): `pack rm` must undo
// the active pack's mcp + gog_account contributions, not just clear cfg.Pack —
// otherwise "detached" is a lie about what happened.
func TestPackRm_RemovesActivePackContributions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, packManifest{
		Name:         "work",
		Schema:       1,
		GogAccount:   "work@company.com",
		Integrations: []packIntegration{{Name: "Fastmail", MCP: "fastmail"}},
	})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	runPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"})
	cfgActive, _ := config.Load()
	if !containsStr(cfgActive.MCP, "fastmail") || cfgActive.GogAccount != "work@company.com" {
		t.Fatalf("setup: pack use did not attach as expected: %+v", cfgActive)
	}

	out.Reset()
	runPackRm(&out, nil)

	cfgAfter, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfgAfter.Pack != "" {
		t.Errorf("pack rm should clear the active pack, got %q", cfgAfter.Pack)
	}
	if containsStr(cfgAfter.MCP, "fastmail") {
		t.Errorf("pack rm should remove the active pack's mcp contribution, cfg.MCP = %v", cfgAfter.MCP)
	}
	if cfgAfter.GogAccount != "" {
		t.Errorf("pack rm should revert gog_account to its prior (empty) value, got %q", cfgAfter.GogAccount)
	}
}

// --- finding #6 [BLOCK]: synthesizePackKit must rebuild from scratch and fail
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

	p1 := &packInfo{Root: root, Manifest: packManifest{Name: "p", Proxies: []packProxy{{Name: "a"}, {Name: "b"}}}}
	kit1, err := synthesizePackKit(p1)
	if err != nil || kit1 == "" {
		t.Fatalf("expected a kit dir, got %q, err=%v", kit1, err)
	}
	if _, err := os.Stat(filepath.Join(kit1, "files", "home", ".local", "bin", "b")); err != nil {
		t.Fatalf("wrapper b should exist after the first synth: %v", err)
	}

	// pack.toml no longer declares "b" (e.g. the author removed it).
	p2 := &packInfo{Root: root, Manifest: packManifest{Name: "p", Proxies: []packProxy{{Name: "a"}}}}
	kit2, err := synthesizePackKit(p2)
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

	pGood := &packInfo{Root: root, Manifest: packManifest{Name: "p", Proxies: []packProxy{{Name: "a"}}}}
	kitGood, err := synthesizePackKit(pGood)
	if err != nil || kitGood == "" {
		t.Fatalf("expected the first (good) synth to succeed, got %q, err=%v", kitGood, err)
	}

	// "missing" has no bin/missing file on disk.
	pBad := &packInfo{Root: root, Manifest: packManifest{Name: "p", Proxies: []packProxy{{Name: "a"}, {Name: "missing"}}}}
	kitBad, badErr := synthesizePackKit(pBad)
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
	a := canonicalizePackRoot("/tmp/x/y/../y/work")
	b := canonicalizePackRoot("/tmp/x/y/work")
	if a != b {
		t.Errorf("canonicalizePackRoot(%q) = %q, want it to equal canonicalizePackRoot(%q) = %q", "/tmp/x/y/../y/work", a, "/tmp/x/y/work", b)
	}
}

// TestPackAdd_Mcp_CanonicalizesActivePackComparison: `pack add mcp fastmail
// ./work` (a RELATIVE path) must still recognize the active pack even though
// cfg.Pack is stored as an absolute path.
func TestPackAdd_Mcp_CanonicalizesActivePackComparison(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "work")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = root // stored absolute, as `pack use` always leaves it
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	t.Chdir(dir)
	var out bytes.Buffer
	runPackAdd(fakeGitEnv(nil), &out, []string{"mcp", "fastmail", "./work", "--yes"})

	if strings.Contains(out.String(), "activate the pack to attach it") {
		t.Errorf("finding #7: relative path should have matched the active pack, got:\n%s", out.String())
	}
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg2.MCP, "fastmail") {
		t.Errorf("expected fastmail to attach to the active pack, cfg.MCP = %v", cfg2.MCP)
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

// TestStalePackReattachWarning_FiresWhenCreateTimePackDiffers (finding G): the
// warning keys on the CREATE-TIME marker vs the active pack — it fires when
// they differ (a pack switched since create, or removed via `pack rm`), and
// names both sides.
func TestStalePackReattachWarning_FiresWhenCreateTimePackDiffers(t *testing.T) {
	ws := t.TempDir()
	oldRoot := filepath.Join(t.TempDir(), "old-pack")
	newRoot := filepath.Join(t.TempDir(), "new-pack")
	writeSandboxPackMarker(ws, oldRoot)

	// Switched pack since create: marker != active -> warn.
	cfg := &config.Config{Pack: newRoot}
	msg := stalePackReattachWarning(cfg, runOpts{Workspace: ws}, true)
	if msg == "" {
		t.Fatal("expected a stale-pack warning when the create-time pack differs from the active pack")
	}
	if !strings.Contains(msg, canonicalizePackRoot(oldRoot)) || !strings.Contains(msg, canonicalizePackRoot(newRoot)) || !strings.Contains(msg, "--replace") {
		t.Errorf("warning should name both packs and the fix, got: %q", msg)
	}

	// `pack rm` case: marker set, active pack EMPTY -> still warn (the old
	// sandbox keeps the removed pack's create-time facets).
	msgRm := stalePackReattachWarning(&config.Config{}, runOpts{Workspace: ws}, true)
	if msgRm == "" {
		t.Fatal("expected a warning after pack rm (marker set, active pack empty)")
	}
	if !strings.Contains(msgRm, canonicalizePackRoot(oldRoot)) || !strings.Contains(msgRm, "--replace") {
		t.Errorf("rm-case warning should name the create-time pack and the fix, got: %q", msgRm)
	}
}

// TestStalePackReattachWarning_SilentWhenNoMarkerOrIdentical (finding G): no
// marker (a pre-marker or pack-less sandbox) or an identical marker (the
// sandbox already carries the active pack) must NOT warn — the identical case
// is the false positive the marker exists to kill.
func TestStalePackReattachWarning_SilentWhenNoMarkerOrIdentical(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	cfg := &config.Config{Pack: root}

	// No marker in the workspace: silent, even with an active pack.
	if msg := stalePackReattachWarning(cfg, runOpts{Workspace: t.TempDir()}, true); msg != "" {
		t.Errorf("no marker must not warn, got %q", msg)
	}

	// Marker matches the active pack: the sandbox already has it — silent.
	ws := t.TempDir()
	writeSandboxPackMarker(ws, root)
	if msg := stalePackReattachWarning(cfg, runOpts{Workspace: ws}, true); msg != "" {
		t.Errorf("marker == active pack must not warn (false positive), got %q", msg)
	}

	// Create / --replace paths never warn, marker or not.
	if msg := stalePackReattachWarning(cfg, runOpts{Workspace: ws}, false); msg != "" {
		t.Errorf("a create/first-launch (reattaching=false) must not warn, got %q", msg)
	}
	writeSandboxPackMarker(ws, filepath.Join(t.TempDir(), "other"))
	if msg := stalePackReattachWarning(cfg, runOpts{Workspace: ws, Replace: true}, true); msg != "" {
		t.Errorf("--replace recreates, so must not warn, got %q", msg)
	}

	// Marker removed when the sandbox is created pack-less: silent afterwards.
	writeSandboxPackMarker(ws, "")
	if msg := stalePackReattachWarning(&config.Config{}, runOpts{Workspace: ws}, true); msg != "" {
		t.Errorf("pack-less create removes the marker, so no warning, got %q", msg)
	}
}
