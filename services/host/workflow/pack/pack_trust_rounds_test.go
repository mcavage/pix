package pack

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// --- from pack_v2_trust_round2_test.go ---
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
// able to claim the user's own MCP entries as the pack's contribution:
// reversibility reads come from the host-state activation record only, so
// both a same-pack reactivation and a later switch-away leave the user's
// entries untouched. (This test also covered a forged `knowledge` lock
// claim before the [[knowledge]] facet was retired, W2 U03A.)
func TestPackUse_SamePackLockForgeryCannotDeleteUserConfig(t *testing.T) {
	dir := isolatePackHost(t)

	// The user's OWN entry, added independently of any pack.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP(config.GWServerName)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "p", Schema: 1}) // Tier-0
	other := filepath.Join(dir, "other")
	mustWritePack(t, other, Manifest{Name: "other", Schema: 1})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK) // activate (legit)

	// Simulate the pull-forgery: the ACTIVE pack's lock now claims the user's
	// own entry as this pack's contribution.
	forged := "mcp = [\"gog\"]\n"
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
	if !slices.Contains(cfg2.MCP, config.GWServerName) {
		t.Fatalf("CRITICAL: same-pack reactivation honored a forged pack.lock; mcp=%v", cfg2.MCP)
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
	if !slices.Contains(cfg3.MCP, config.GWServerName) {
		t.Fatalf("CRITICAL: switch-away honored a forged pack.lock; mcp=%v", cfg3.MCP)
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
	prior.setActivationStack([]packActivationRecord{prior.newActivationRecord(root, packLock{MCP: []string{"old"}})})
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
	dir := isolatePackHost(t)
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
	dir := isolatePackHost(t)
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

// --- E: forged Remote cannot evict another pack's acceptance -------------------

// TestPackUse_ForgedRemoteCannotEvictOtherPacksAcceptance: acceptance
// provenance is HOST-recorded only. A local pack shipping a pack.lock that
// forges another pack's Remote must not get that Remote into its own
// acceptance record — recordAcceptance's same-remote hygiene sweep would
// otherwise DELETE the legit pack's acceptance.
func TestPackUse_ForgedRemoteCannotEvictOtherPacksAcceptance(t *testing.T) {
	dir := isolatePackHost(t)
	const legitRemote = "https://example.com/legit.git"

	// Legit pack B: host-recorded clone provenance + accepted Tier-1 surface.
	rootB := hostExecPack(t, dir, "b", "bin", "b-tool")
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
	rootE := hostExecPack(t, dir, "e", "bin", "e-tool")
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

// --- from pack_v2_trust_round3_test.go ---
// --- #1: cross-process lock, fresh-load mutations ------------------------------

// TestMutatePackTrustStore_InterleavedMutationsLoseNothing: two writers whose
// in-memory views were loaded independently (the `pack use` commit vs the host
// launch's wrapper refresh) both mutate through the serialized helper — each
// re-loads FRESH under the lock, so neither can clobber the other's committed
// record (the old last-writer-wins bug saved whichever stale object came last).
func TestMutatePackTrustStore_InterleavedMutationsLoseNothing(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "pack")

	// Writer 1 (a `pack use` commit): records an activation + an acceptance.
	if _, err := mutatePackTrustStore(func(s *PackTrustStore) error {
		s.setActivationStack([]packActivationRecord{s.newActivationRecord(root, packLock{MCP: []string{"a-mcp"}})})
		s.RecordAcceptance("path:"+root, PackTrustRecord{Path: root, Fingerprint: "fp1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Writer 2 (a concurrent host launch): its logical view predates writer 1,
	// but the helper re-loads fresh, so its Installed write lands BESIDE the
	// activation instead of over it.
	if _, err := mutatePackTrustStore(func(s *PackTrustStore) error {
		s.Installed = &packInstalledSet{Owner: "path:" + root, Wrappers: []string{"tool"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	after, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.activationFor(root); len(got.MCP) != 1 || got.MCP[0] != "a-mcp" {
		t.Errorf("the committed activation was clobbered by a later mutation, got %+v", got)
	}
	if fp, ok := after.acceptedFingerprint("path:" + root); !ok || fp != "fp1" {
		t.Errorf("the committed acceptance was clobbered, got (%q,%v)", fp, ok)
	}
	if after.Installed == nil || !slices.Contains(after.Installed.Wrappers, "tool") {
		t.Errorf("the second writer's own mutation must land too, got %+v", after.Installed)
	}
}

// TestMutatePackTrustStore_ConcurrentWritersSerialized: N goroutines each
// commit one acceptance record through the lock-serialized helper; every
// record survives (no lost update). Run with -race, this also exercises the
// flock across distinct file descriptors.
func TestMutatePackTrustStore_ConcurrentWritersSerialized(t *testing.T) {
	isolatePackHost(t)

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("path:/pack-%d", i)
			if _, err := mutatePackTrustStore(func(s *PackTrustStore) error {
				s.RecordAcceptance(key, PackTrustRecord{Path: fmt.Sprintf("/pack-%d", i), Fingerprint: "fp"})
				return nil
			}); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	after, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < writers; i++ {
		if _, ok := after.acceptedFingerprint(fmt.Sprintf("path:/pack-%d", i)); !ok {
			t.Errorf("writer %d's record was lost (last-writer-wins clobber); store=%+v", i, after.Accepted)
		}
	}
}

// --- #2: the pack payload lock is never a reversibility source -----------------

// TestPackUse_PayloadLockIsNeverAReversibilitySource: activation attribution
// lives ONLY in the launcher-owned trust store. A pack — local OR adopted —
// with no activation record therefore reverts NOTHING when you switch away
// (safe over-retention), and a pack.lock claiming the user's own MCP as this
// pack's contribution can never make that switch delete it.
func TestPackUse_PayloadLockIsNeverAReversibilitySource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adopted bool
	}{{"local", false}, {"adopted", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolatePackHost(t)
			pinLocalMCP(t)

			rootA := filepath.Join(dir, "a")
			mustWritePack(t, rootA, Manifest{Name: "a", Schema: 1})
			rootB := filepath.Join(dir, "b")
			mustWritePack(t, rootB, Manifest{Name: "b", Schema: 1})

			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			cfg.AddMCP("usermcp")
			cfg.Pack = rootA
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
			if tc.adopted {
				if err := recordPackAdoptionInTrustStore(rootA, "https://example.com/a.git", "c1"); err != nil {
					t.Fatal(err)
				}
			}
			if err := writePackLock(rootA, packLock{MCP: []string{"usermcp"}}); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			RunPackUse(fakeGitEnv(nil), &out, []string{rootB}, registerOK)
			cfg2, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(cfg2.MCP, "usermcp") {
				t.Errorf("CRITICAL: the pack payload lock was trusted and deleted the user's own MCP, cfg.MCP=%v", cfg2.MCP)
			}
		})
	}
}

// --- #3: unknown MCP classification fails closed --------------------------------

// TestLocalMCPClassifier_UnknownFailsClosed: when the local set cannot be
// established (no probe at all, or a failing probe), a non-gog name classifies
// as HOST-EXEC — the pack is Tier-1 and the gate fails closed on a non-TTY
// without --yes. gog stays reference-only Tier-0.
func TestLocalMCPClassifier_UnknownFailsClosed(t *testing.T) {
	// No probe available at all.
	unknown := LocalMCPClassifier(hostenv.Env{System: &systest.Fake{}}, nil)
	if !unknown("fastmail") {
		t.Error("unknown classification must treat a non-gog name as host-exec (fail closed)")
	}
	if unknown(config.GWServerName) {
		t.Error("gog stays the reference-only Tier-0 special case even when the partition is unknown")
	}
	// Probe resolves but errors.
	failEnv := hostenv.Env{System: &systest.Fake{RunFn: func(string, ...string) (string, error) { return "", fmt.Errorf("probe failed") }}}
	resolver := func() (string, error) { return "pix-host", nil }
	unknown2 := LocalMCPClassifier(failEnv, resolver)
	if !unknown2("notion") {
		t.Error("a failing `mcp --list` probe must classify a non-gog name as host-exec")
	}

	p := &Info{Root: "/p", Manifest: Manifest{Name: "p",
		Integrations: []Integration{{Name: "N", MCP: "notion"}}}}
	b := ComputeHostBoM(p, "", unknown2)
	if !b.Tier1() {
		t.Fatalf("a pack whose MCP cannot be classified must be Tier-1 (gated), got %+v", b)
	}
	// The gate itself fails closed non-interactively without --yes.
	if err := packTrustGate(strings.NewReader(""), io.Discard, false, false, "p", b); err == nil {
		t.Error("non-TTY without --yes must fail closed for an unclassifiable MCP")
	}
	// A nil classifier (no partition available) fails closed the same way.
	if bn := ComputeHostBoM(p, "", nil); !bn.Tier1() {
		t.Errorf("a nil classifier must fail closed too, got %+v", bn)
	}
	// gog-only packs stay Tier-0 under an unknown partition.
	pg := &Info{Root: "/p", Manifest: Manifest{Name: "g",
		Integrations: []Integration{{Name: "gog", MCP: config.GWServerName}}}}
	if ComputeHostBoM(pg, "", unknown2).Tier1() {
		t.Error("a gog-only reference must stay Tier-0 even when the partition is unknown")
	}
}

// --- #4: a clear failure is an honest failure ------------------------------------

// TestPackRm_ClearFailureExitsNonZero: when the installed host wrappers cannot
// be removed (symlinked host bin dir), `pack rm` must exit non-zero and must
// NOT claim "detached" — and nothing detaches, so a plain re-run retries.
// Subprocess: the failure path os.Exits.
// --- #5: acceptance identity is commit-stable ------------------------------------

// TestTrustKey_StableAcrossCommits: the trust key is the remote URL without
// the commit; a provenance update (new commit after a pull) does not move the
// identity, so an acceptance over an unchanged fingerprint still matches (no
// re-prompt) while a CHANGED fingerprint still mismatches (re-gates). A
// legacy commit-suffixed record is honored via the one-time fallback.
func TestTrustKey_StableAcrossCommits(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "clone")
	mustWritePack(t, root, Manifest{Name: "c", Schema: 1})
	const url = "https://example.com/x.git"

	if err := recordPackAdoptionInTrustStore(root, url, "c1"); err != nil {
		t.Fatal(err)
	}
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	key := store.TrustKey(root)
	if strings.Contains(key, "#") {
		t.Fatalf("the trust key must not embed the commit, got %q", key)
	}
	store.RecordAcceptance(key, PackTrustRecord{Path: CanonicalizePackRoot(root), Remote: url, Commit: "c1", Fingerprint: "fp1"})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	// A new commit lands (git pull): provenance updates, identity must not move.
	if err := recordPackAdoptionInTrustStore(root, url, "c2"); err != nil {
		t.Fatal(err)
	}
	store2, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := store2.TrustKey(root); got != key {
		t.Errorf("a commit bump moved the identity (%q -> %q); acceptance would spuriously re-gate", key, got)
	}
	got, ok := store2.acceptedFingerprint(store2.TrustKey(root))
	if !ok || got != "fp1" {
		t.Errorf("same fingerprint across commits must stay accepted (no re-prompt), got (%q,%v)", got, ok)
	}
	if got == "fp2-changed-surface" {
		t.Error("sanity: a changed fingerprint must mismatch and re-gate")
	}

}

// TestPackUse_NewCommitSameFingerprintDoesNotRegate (end-to-end): an accepted
// adopted Tier-1 pack whose provenance commit changes — but whose host-exec
// fingerprint is byte-identical — re-activates non-interactively with NO
// --yes. In-process: a misfiring gate would os.Exit(1) and fail the test
// binary, exactly like the non-interactive pack trust-gate tests.
func TestPackUse_NewCommitSameFingerprintDoesNotRegate(t *testing.T) {
	dir := isolatePackHost(t)
	root := hostExecPack(t, dir, "work", "bin", "platformio")
	const url = "https://example.com/work.git"
	if err := recordPackAdoptionInTrustStore(root, url, "c1"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK) // accept once

	// A README-only pull: new commit, identical host-exec surface.
	if err := recordPackAdoptionInTrustStore(root, url, "c2"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK) // no --yes, non-TTY
	if strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("a commit bump with an unchanged fingerprint must not re-prompt:\n%s", out.String())
	}

	// A CHANGED surface (mutated host-exec bin) still re-gates: a subsequent
	// non-interactive `pack use` (no --yes) now REFUSES until re-accepted,
	// rather than silently reusing the stale acceptance.
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\necho evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recordPackAdoptionInTrustStore(root, url, "c3"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if os.Getenv("PIX_TEST_TRUST") == "changed-surface-regates" {
		RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK) // no --yes, non-TTY
		return                                                        // exit 0 == a mutated surface slipped through unre-gated
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_NewCommitSameFingerprintDoesNotRegate$")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_TRUST=changed-surface-regates",
		"PIX_CONFIG="+filepath.Join(dir, "config.toml"),
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	if cmdOut, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("a changed host-exec fingerprint must still fail closed (re-gate/refuse); output:\n%s", cmdOut)
	}
}

// round3HostExecPack writes a pack with one host-exec [[bin]] facet (an
// external, sha-pinned binary — the retained Tier-1 fitness that phase2HostPack
// used to cover with a host=true proxy wrapper before the dormant host-mode
// wrapper installer was deleted) and returns its root. XDG_STATE_HOME must
// already be pointed at a temp dir by the caller.

// --- from pack_v2_trust_host_state2_test.go ---
func TestPackUse_ForgedDirectorySymlinkLockScrubbedNotFollowed(t *testing.T) {
	dir := isolatePackHost(t)
	victim := filepath.Join(dir, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "evil")
	mustWritePack(t, root, Manifest{Name: "evil", Schema: 1})
	if err := os.Symlink(victim, PackLockPath(root)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK)
	if fi, err := os.Lstat(PackLockPath(root)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("pack.lock must be a fresh regular file after adoption, got %v (err=%v)", fi, err)
	}
}
