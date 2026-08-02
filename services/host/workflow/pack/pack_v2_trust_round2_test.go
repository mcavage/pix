// pack_v2_trust_round2_test.go — round-2 security+review fixes for the
// packs-v2 Phase 2 trust model:
//
//	A — activation provenance (the reversible-switch attribution) lives in the
//	    LAUNCHER-OWNED trust store, never the pack-payload pack.lock: a
//	    same-pack `git pull` lock forgery can no longer delete the user's own
//	    config entries on reactivation/switch-away.
//	B — the acceptance fingerprint uses a CANONICAL, INJECTIVE encoding
//	    (structured JSON, then hash) — the reviewer's egress delimiter
//	    collision now produces distinct fingerprints — and only host=true
//	    [[bin]] entries ever enter the accepted surface (a Host flip re-gates
//	    instead of silently installing).
//	C — reference-only MCP integrations (remote gateway-catalog names, the
//	    host-provided gog registration) are Tier-0 again (no prompt, non-TTY
//	    OK); only an integration.mcp resolving to a LOCAL stdio host command
//	    is Tier-1.
//	D — wrapper install + attribution is a fail-closed transaction: a failed
//	    trust-store write means NO install (and a strict launch refusal), and
//	    an absent active pack still clears its previously-attributed wrappers
//	    or refuses the launch.
//	E — acceptance provenance (Remote/Commit) is host-recorded only: a forged
//	    pack.lock Remote cannot evict another pack's acceptance record.
//	F — the trust store is Lstat-refused on READ when symlinked (write
//	    already was).
package pack

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

// localMCPEnv returns a hostenv.Env whose `pix-host mcp --list` reports the
// given names as LOCAL stdio servers (every other command pretends to
// succeed, like fakeGitEnv).
func localMCPEnv(names ...string) hostenv.Env {
	list := strings.Join(names, "\n")
	// HostBinary is wired here rather than by reassigning a package var: the env
	// is the seam, and LocalMCPClassifier reads it off the env now.
	return hostenv.Env{System: &systest.Fake{RunFn: func(name string, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "mcp" && args[1] == "--list" {
			return list, nil
		}
		return "", nil
	}}, HostBinary: func() (string, error) { return "pix-host", nil }}
}

// pinLocalMCP pins the local-vs-gateway partition for the duration of a test:
// launcher.FindHostBinary resolves, and PackLocalMCP classifies exactly the given
// names as local. Restored on cleanup.
func pinLocalMCP(t *testing.T, names ...string) {
	t.Helper()
	prevClassifier := PackLocalMCP
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	PackLocalMCP = func() func(string) bool {
		return func(n string) bool { return set[n] }
	}
	t.Cleanup(func() { PackLocalMCP = prevClassifier })
}

// --- A: activation provenance in HOST state ----------------------------------

// TestPackUse_SamePackLockForgeryCannotDeleteUserConfig (CRITICAL): a local
// `git pull` (or zip update) rewriting pack.lock under the ALREADY-ACTIVE
// pack — the one case the old scrub skipped (prevRoot == root) — must not be
// able to claim the user's own MCP/knowledge entries as the pack's
// contribution: reversibility reads come from the host-state activation
// record only, so both a same-pack reactivation and a later switch-away leave
// the user's entries untouched.
func TestPackUse_SamePackLockForgeryCannotDeleteUserConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	// The user's OWN entries, added independently of any pack.
	userBundle := filepath.Join(dir, "user-bundle")
	if err := os.MkdirAll(userBundle, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP(config.GWServerName)
	cfg.AddKnowledgeBundle(userBundle)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	userBundleID := knowledge.CanonicalizeKnowledgeBundle(userBundle)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "p", Schema: 1}) // Tier-0
	other := filepath.Join(dir, "other")
	mustWritePack(t, other, Manifest{Name: "other", Schema: 1})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK) // activate (legit)

	// Simulate the pull-forgery: the ACTIVE pack's lock now claims the user's
	// own entries as this pack's contribution.
	forged := "mcp = [\"gog\"]\nknowledge = [\"" + userBundleID + "\"]\n"
	if err := os.WriteFile(PackLockPath(root), []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same-pack REACTIVATION (the previously-skipped scrub path).
	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK)
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg2.MCP, config.GWServerName) || !slices.Contains(cfg2.KnowledgeBundles, userBundleID) {
		t.Fatalf("CRITICAL: same-pack reactivation honored a forged pack.lock; mcp=%v knowledge=%v", cfg2.MCP, cfg2.KnowledgeBundles)
	}

	// Forge again and SWITCH AWAY — the other trusted-lock read path.
	if err := os.WriteFile(PackLockPath(root), []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{other}, registerOK)
	cfg3, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg3.MCP, config.GWServerName) || !slices.Contains(cfg3.KnowledgeBundles, userBundleID) {
		t.Fatalf("CRITICAL: switch-away honored a forged pack.lock; mcp=%v knowledge=%v", cfg3.MCP, cfg3.KnowledgeBundles)
	}
}

// TestCommitPackActivation_CfgSaveFailureRollsBackActivationRecord: an
// ordinary cfg.Save failure (here: config.toml is a non-empty directory, so
// the atomic rename fails while everything else stays writable) rolls the
// HOST-STATE activation record back to its prior value AND restores the prior
// pack.lock bytes — on-disk state stays mutually consistent.
func TestCommitPackActivation_CfgSaveFailureRollsBackActivationRecord(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "p", Schema: 1})

	// Prior on-disk state: activation record + lock attribute "old".
	prior, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	prior.setActivation(root, packLock{MCP: []string{"old"}})
	if err := prior.Save(); err != nil {
		t.Fatal(err)
	}
	if err := writePackLock(root, packLock{MCP: []string{"old"}}); err != nil {
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
	cfg.AddMCP("new")
	cfg.Pack = root

	// Break cfg.Save only: config.toml becomes a non-empty directory.
	if err := os.MkdirAll(filepath.Join(cfgPath, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	cerr := commitPackActivation(cfg, store, root, packLock{MCP: []string{"new"}})
	if cerr == nil {
		t.Fatal("expected commitPackActivation to fail when cfg.Save cannot write")
	}
	if !strings.Contains(cerr.Error(), "activation record rolled back") {
		t.Errorf("error should say the activation record was rolled back, got: %v", cerr)
	}
	after, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.activationFor(root); len(got.MCP) != 1 || got.MCP[0] != "old" {
		t.Errorf("on-disk activation record must be rolled back to the prior value, got %+v", got)
	}
	lockAfter, err := os.ReadFile(PackLockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockAfter, lockBefore) {
		t.Errorf("prior pack.lock must be restored.\nbefore: %q\nafter:  %q", lockBefore, lockAfter)
	}
}

// --- B: canonical fingerprint + host-only bins --------------------------------

// TestHostExecFingerprint_CanonicalEncodingResistsCollisions: the reviewer's
// collision — under the old NUL/newline concatenation, egress ["a","b"] and
// egress ["a\negress\x00b"] hashed IDENTICALLY (one crafted string encoding
// two entries), as did MCP argv ["a","b"] vs ["a\x1fb"]. The canonical JSON
// encoding must keep every distinct surface distinct.
func TestHostExecFingerprint_CanonicalEncodingResistsCollisions(t *testing.T) {
	root := t.TempDir()
	fpOf := func(b hostBoM) string {
		t.Helper()
		fp, _, err := ComputeHostExecFingerprint(root, b)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return fp
	}
	if fpOf(hostBoM{Egress: []string{"a", "b"}}) == fpOf(hostBoM{Egress: []string{"a\negress\x00b"}}) {
		t.Error("COLLISION: a crafted egress string encodes two entries with an identical fingerprint")
	}
	if fpOf(hostBoM{MCP: []hostBoMMCP{{Name: "m", Argv: []string{"a", "b"}}}}) ==
		fpOf(hostBoM{MCP: []hostBoMMCP{{Name: "m", Argv: []string{"a\x1fb"}}}}) {
		t.Error("COLLISION: a crafted argv element merges two args with an identical fingerprint")
	}
	if fpOf(hostBoM{Creds: []string{"A"}, Egress: []string{"x"}}) ==
		fpOf(hostBoM{Creds: []string{"A"}, Egress: []string{"x\ncred\x00B"}}) {
		t.Error("COLLISION: an egress string must not be able to smuggle a cred entry")
	}
	// Determinism: entry order never changes the fingerprint.
	if fpOf(hostBoM{Egress: []string{"a", "b"}, Creds: []string{"X", "Y"}}) !=
		fpOf(hostBoM{Egress: []string{"b", "a"}, Creds: []string{"Y", "X"}}) {
		t.Error("a pure reorder must not change the fingerprint")
	}
}

// TestComputeHostBoM_InertBinNeverInSurface: a host=false [[bin]] is inert —
// it never enters the BoM (so it is never part of an accepted surface), never
// raises the tier, and flipping it to host=true CHANGES the fingerprint (the
// old model fingerprinted it identically and then silently installed it).
func TestComputeHostBoM_InertBinNeverInSurface(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "w"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inert := Manifest{Name: "w", Schema: 1,
		Proxies: []PackProxy{{Name: "w", Host: true}},
		Bins:    []packBin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: false}}}
	b := ComputeHostBoM(&Info{Root: root, Manifest: inert}, "", nil)
	if len(b.Bins) != 0 {
		t.Fatalf("a host=false [[bin]] must never enter the BoM, got %+v", b.Bins)
	}
	fpInert, _, err := ComputeHostExecFingerprint(root, b)
	if err != nil {
		t.Fatal(err)
	}
	flipped := inert
	flipped.Bins = []packBin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: true}}
	b2 := ComputeHostBoM(&Info{Root: root, Manifest: flipped}, "", nil)
	if len(b2.Bins) != 1 {
		t.Fatalf("a host=true [[bin]] must enter the BoM, got %+v", b2.Bins)
	}
	fpFlipped, _, err := ComputeHostExecFingerprint(root, b2)
	if err != nil {
		t.Fatal(err)
	}
	if fpInert == fpFlipped {
		t.Error("SILENT INSTALL: flipping [[bin]] host=false→true must change the fingerprint (re-gate)")
	}
	// A pack whose ONLY facet is an inert bin is Tier-0 outright.
	onlyInert := Manifest{Name: "w", Schema: 1,
		Bins: []packBin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: false}}}
	if ComputeHostBoM(&Info{Root: root, Manifest: onlyInert}, "", nil).Tier1() {
		t.Error("an inert (host=false) [[bin]] alone must not raise the tier")
	}
}

// --- C: MCP integration trust classification ----------------------------------

// TestComputeHostBoM_RemoteMCPReferenceRequiresConsent: the local-vs-gateway partition
// decides the tier — a name NOT in the local set (a remote gateway-catalog
// server, or gog, which the bridge never lists) is reference-only. An explicit
// pack-selected URL still requires consent because tools can send conversation
// data there; a name the host serves locally is Tier-1. An UNKNOWN partition
// (pix-host unresolved / probe failed) now FAILS CLOSED (round-3 #3):
// every non-gog name classifies as host-exec so the gate fires — the name
// still lands in cfg.MCP and attaches via --mcp, so an already-registered
// local server would otherwise run its host command ungated.
func TestComputeHostBoM_RemoteMCPReferenceRequiresConsent(t *testing.T) {
	p := &Info{Root: "/p", Manifest: Manifest{
		Name:         "personal",
		Integrations: []Integration{{Name: "Docs", MCP: "docs", URL: "https://docs.example.test/mcp"}},
	}}
	resolver := func() (string, error) { return "pix-host", nil }
	remoteOnly := LocalMCPClassifier(localMCPEnv("fastmail"), resolver)
	if b := ComputeHostBoM(p, "", remoteOnly); !b.Tier1() || len(b.RemoteMCP) != 1 || len(b.MCP) != 0 {
		t.Errorf("an explicit remote endpoint must require consent, got %+v", b)
	}
	localRef := &Info{Root: "/p", Manifest: Manifest{
		Name: "personal", Integrations: []Integration{{Name: "Notion", MCP: "notion"}},
	}}
	unknown := LocalMCPClassifier(hostenv.Env{System: &systest.Fake{}}, nil)
	if b := ComputeHostBoM(localRef, "", unknown); !b.Tier1() {
		t.Errorf("an unknown local partition must FAIL CLOSED as host-exec (round-3 #3), got %+v", b)
	}
	local := LocalMCPClassifier(localMCPEnv("notion"), resolver)
	if b := ComputeHostBoM(localRef, "", local); !b.Tier1() || len(b.MCP) != 1 {
		t.Errorf("a LOCAL stdio MCP command must be Tier-1, got %+v", b)
	} else if got := strings.Join(b.MCP[0].Argv, " "); got != "pix-host mcp notion" {
		t.Errorf("local argv = %q", got)
	}
}

// TestPackUse_RemoteMCPReferenceRequiresYes: a non-interactive caller must
// explicitly accept a pack-selected remote endpoint. --yes is used here so
// the in-process test can cross the same gate without an os.Exit.
func TestPackUse_RemoteMCPReferenceRequiresYes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t, "fastmail") // notion is NOT local
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "personal", Schema: 1,
		Integrations: []Integration{{Name: "Docs", MCP: "docs", URL: "https://docs.example.test/mcp"}}})

	var out bytes.Buffer
	RunPackUse(localMCPEnv("fastmail"), &out, []string{"--yes", root}, registerOK)
	if !strings.Contains(out.String(), "Remote MCP:") {
		t.Errorf("the consent screen must disclose the endpoint, got:\n%s", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pack != root || !slices.Contains(cfg.MCP, "docs") {
		t.Errorf("the reference must still attach: pack=%q mcp=%v", cfg.Pack, cfg.MCP)
	}
}

// TestPackUse_GogReferenceStaysTier0: gog is host-provided (the launcher
// builds its hardened argv; the pack contributes only the name) and is never
// in the local stdio list — a gog reference is Tier-0 (packs.md §9 names it
// as the canonical reference-only case).
func TestPackUse_GogReferenceStaysTier0(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t) // empty local set — gog is never listed
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "personal", Schema: 1,
		Integrations: []Integration{{Name: "gog", MCP: config.GWServerName, Env: "GOG_KEYRING"}}})

	var out bytes.Buffer
	RunPackUse(localMCPEnv(), &out, []string{root}, registerOK) // no --yes, non-TTY
	if strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("a gog reference must adopt silently (Tier-0), got:\n%s", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.MCP, config.GWServerName) {
		t.Errorf("gog must still attach, mcp=%v", cfg.MCP)
	}
}

// --- D: fail-closed install/clear transactions --------------------------------

// TestRefreshHostPackWrappers_StoreWriteFailureFailsClosed: when the intended
// attribution cannot be recorded (read-only config dir), NOTHING installs and
// refresh returns an error in BOTH modes — a strict host launch refuses, and
// there is never a live wrapper the store attributes to nobody.
func TestRefreshHostPackWrappers_StoreWriteFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only dir cannot force the store write to fail")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := phase2HostPack(t, dir, "work", "platformio")
	acceptPackSurface(t, root, "")
	cfg := &config.Config{Pack: root}

	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	var out bytes.Buffer
	if _, err := RefreshHostPackWrappers(&out, cfg, true); err == nil {
		t.Error("strict refresh must REFUSE when the attribution cannot be recorded")
	}
	if _, err := os.Stat(filepath.Join(HostPackBinDir(), "platformio")); err == nil {
		t.Error("nothing may install when the attribution write failed (orphan wrapper)")
	}
	out.Reset()
	if _, err := RefreshHostPackWrappers(&out, cfg, false); err == nil {
		t.Error("lenient refresh must also surface the failed transaction as an error")
	}
	if _, err := os.Stat(filepath.Join(HostPackBinDir(), "platformio")); err == nil {
		t.Error("nothing may install in lenient mode either")
	}
}

// TestRefreshHostPackWrappers_AbsentPackClearsAttributedWrappersOrRefuses: a
// missing active pack (deleted dir) and a detached pack (no active pack at
// all) both still clear whatever host state attributes to the bin dir — and
// when the clear FAILS, a strict (launch) refresh refuses instead of leaving
// orphan host executables on PATH.
func TestRefreshHostPackWrappers_AbsentPackClearsAttributedWrappersOrRefuses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	seed := func() {
		t.Helper()
		if err := os.MkdirAll(HostPackBinDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(HostPackBinDir(), "stale"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		store, err := loadPackTrustStore()
		if err != nil {
			t.Fatal(err)
		}
		store.Installed = &packInstalledSet{Owner: "path:/gone", Wrappers: []string{"stale"}}
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
	}

	// 1) Active pack dir deleted: degrade, but the stale wrapper is cleared.
	seed()
	var out bytes.Buffer
	if p, err := RefreshHostPackWrappers(&out, &config.Config{Pack: filepath.Join(dir, "gone")}, true); err != nil || p != nil {
		t.Fatalf("absent pack must degrade after clearing, got (%v,%v)", p, err)
	}
	if _, err := os.Stat(filepath.Join(HostPackBinDir(), "stale")); err == nil {
		t.Error("an absent active pack must still clear its attributed wrappers")
	}
	if store, err := loadPackTrustStore(); err != nil || store.Installed != nil {
		t.Errorf("attribution must be discarded after a confirmed clear (store=%+v err=%v)", store, err)
	}

	// 2) No active pack at all: same clearing contract.
	seed()
	out.Reset()
	if _, err := RefreshHostPackWrappers(&out, &config.Config{}, true); err != nil {
		t.Fatalf("no active pack must clear cleanly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(HostPackBinDir(), "stale")); err == nil {
		t.Error("detached state must still clear attributed wrappers")
	}

	// 3) The clear FAILS (symlinked bin dir): a strict launch must refuse.
	if err := os.RemoveAll(HostPackBinDir()); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, HostPackBinDir()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	store.Installed = &packInstalledSet{Owner: "path:/gone", Wrappers: []string{"stale"}}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, rerr := RefreshHostPackWrappers(&out, &config.Config{}, true); rerr == nil {
		t.Error("a strict refresh must REFUSE when stale attributed wrappers cannot be cleared")
	}
	out.Reset()
	if _, rerr := RefreshHostPackWrappers(&out, &config.Config{Pack: filepath.Join(dir, "gone")}, true); rerr == nil {
		t.Error("an absent pack whose stale wrappers cannot be cleared must refuse a strict refresh")
	}
}

// --- E: forged Remote cannot evict another pack's acceptance -------------------

// TestPackUse_ForgedRemoteCannotEvictOtherPacksAcceptance: acceptance
// provenance is HOST-recorded only. A local pack shipping a pack.lock that
// forges another pack's Remote must not get that Remote into its own
// acceptance record — recordAcceptance's same-remote hygiene sweep would
// otherwise DELETE the legit pack's acceptance.
func TestPackUse_ForgedRemoteCannotEvictOtherPacksAcceptance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	const legitRemote = "https://example.com/legit.git"

	// Legit pack B: host-recorded clone provenance + accepted Tier-1 surface.
	rootB := phase2HostPack(t, dir, "b", "b-tool")
	if err := recordPackAdoptionInTrustStore(rootB, legitRemote, "c1"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{rootB, "--yes"}, registerOK)
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	keyB := store.TrustKey(rootB)
	if _, ok := store.acceptedFingerprint(keyB); !ok {
		t.Fatalf("setup: B's acceptance not recorded (store=%+v)", store)
	}

	// Evil local pack E ships a forged pack.lock claiming B's Remote.
	rootE := phase2HostPack(t, dir, "e", "e-tool")
	if err := os.WriteFile(PackLockPath(rootE), []byte("remote = \""+legitRemote+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{rootE, "--yes"}, registerOK)

	after, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.acceptedFingerprint(keyB); !ok {
		t.Errorf("a forged pack.lock Remote evicted the legit pack's acceptance (store=%+v)", after)
	}
	recE, ok := after.Accepted[after.TrustKey(rootE)]
	if !ok {
		t.Fatalf("E's own acceptance should exist (store=%+v)", after)
	}
	if recE.Remote != "" {
		t.Errorf("E's acceptance must not carry the forged Remote, got %q", recE.Remote)
	}
}

// --- F: symlinked trust store refused on read ----------------------------------

// TestLoadPackTrustStore_RefusesSymlinkedStoreOnRead: a pack-trust.json that
// is a symlink (at an attacker-influenced file carrying crafted acceptance)
// is refused at READ, not just at write.
func TestLoadPackTrustStore_RefusesSymlinkedStoreOnRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	crafted := filepath.Join(dir, "crafted.json")
	if err := os.WriteFile(crafted, []byte(`{"version":1,"accepted":{"path:/evil":{"fingerprint":"aaaa"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(crafted, packTrustStorePath()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := loadPackTrustStore(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlinked trust store must be refused on read, got err=%v", err)
	}
}
