package pack

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"pix/host/config"
)

// localMCPEnv and pinLocalMCP are GONE with the local-vs-gateway probe they
// wired. A pack's MCP servers are classified by the TRANSPORT its manifest
// declares (command / image / manifest / url), so there is no ambient host
// question left to fake: every test below states the transport in the pack it
// writes, and fakeGitEnv is the only env any of them needs.

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
	cfg.AddMCP(usersOwnMCP)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "p", Schema: 1}) // Tier-0
	other := filepath.Join(dir, "other")
	mustWritePack(t, other, packinfo.Manifest{Name: "other", Schema: 1})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK) // activate (legit)

	// Simulate the pull-forgery: the ACTIVE pack's lock now claims the user's
	// own entry as this pack's contribution.
	forged := "mcp = [\"" + usersOwnMCP + "\"]\n"
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
	if !slices.Contains(cfg2.MCP, usersOwnMCP) {
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
	if !slices.Contains(cfg3.MCP, usersOwnMCP) {
		t.Fatalf("CRITICAL: switch-away honored a forged pack.lock; mcp=%v", cfg3.MCP)
	}
}

// TestPackTxnCommit_CfgSaveFailureRollsBackActivationRecord: an
// ordinary cfg.Save failure (here: config.toml is a non-empty directory, so
// the atomic rename fails while everything else stays writable) rolls the
// HOST-STATE activation record back to its prior value AND restores the prior
// pack.lock bytes — on-disk state stays mutually consistent.
func TestPackTxnCommit_CfgSaveFailureRollsBackActivationRecord(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "p", Schema: 1})

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
	cerr := commitOnePack(cfg, store, root, packLock{MCP: []string{"new"}})
	if cerr == nil {
		t.Fatal("expected packTxn.commit to fail when cfg.Save cannot write")
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
	inert := packinfo.Manifest{Name: "w", Schema: 1,
		Proxies: []packinfo.PackProxy{{Name: "w", Host: true}},
		Bins:    []packinfo.Bin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: false}}}
	b := ComputeHostBoM(&packinfo.Info{Root: root, Manifest: inert})
	if len(b.Bins) != 0 {
		t.Fatalf("a host=false [[bin]] must never enter the BoM, got %+v", b.Bins)
	}
	fpInert, _, err := ComputeHostExecFingerprint(root, b)
	if err != nil {
		t.Fatal(err)
	}
	flipped := inert
	flipped.Bins = []packinfo.Bin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: true}}
	b2 := ComputeHostBoM(&packinfo.Info{Root: root, Manifest: flipped})
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
	onlyInert := packinfo.Manifest{Name: "w", Schema: 1,
		Bins: []packinfo.Bin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: false}}}
	if ComputeHostBoM(&packinfo.Info{Root: root, Manifest: onlyInert}).Tier1() {
		t.Error("an inert (host=false) [[bin]] alone must not raise the tier")
	}
}

// --- C: MCP integration trust classification ----------------------------------

// TestComputeHostBoM_TransportDecidesHostExecClassification: the DECLARED
// TRANSPORT decides which host-exec surface an MCP integration contributes, and
// every one of the four is Tier-1 — there is no reference-only MCP left to be
// wrong about. `command` is a host binary spawned over stdio, so the reviewed
// argv is the bare command plus the pack's LITERAL args (never a PATH-resolved
// path, which is a property of this machine and not of what you consent to);
// `image`/`manifest` are containers the gateway runs; `url` still requires
// consent because tools can send conversation data to a pack-selected third
// party.
//
// This replaces the old local-vs-gateway probe, whose answer could change
// without the pack changing — so every caller had to carry a fail-closed guess
// for the case where the probe could not answer at all.
func TestComputeHostBoM_TransportDecidesHostExecClassification(t *testing.T) {
	remote := &packinfo.Info{Root: "/p", Manifest: packinfo.Manifest{
		Name:         "personal",
		Integrations: []packinfo.Integration{{Name: "Docs", MCP: "docs", URL: "https://docs.example.test/mcp"}},
	}}
	if b := ComputeHostBoM(remote); !b.Tier1() || len(b.RemoteMCP) != 1 || len(b.MCP) != 0 || len(b.Containers) != 0 {
		t.Errorf("an explicit remote endpoint must require consent as a remote, got %+v", b)
	}
	command := &packinfo.Info{Root: "/p", Manifest: packinfo.Manifest{
		Name: "personal", Integrations: []packinfo.Integration{
			{Name: "Notes", MCP: "notes", Command: "notes-mcp", Args: []string{"--readonly", "mcp"}},
		},
	}}
	if b := ComputeHostBoM(command); !b.Tier1() || len(b.MCP) != 1 || len(b.RemoteMCP) != 0 {
		t.Errorf("a host command MCP must be Tier-1 host-exec, got %+v", b)
	} else if got := strings.Join(b.MCP[0].Argv, " "); got != "notes-mcp --readonly mcp" {
		t.Errorf("reviewed argv = %q, want the bare command plus its declared literal args", got)
	}
	container := &packinfo.Info{Root: "/p", Manifest: packinfo.Manifest{
		Name: "personal", Integrations: []packinfo.Integration{
			{Name: "HR", MCP: "hr", Image: "hr-mcp:1"},
			{Name: "Meet", MCP: "meet", Manifest: "https://example.test/server.json"},
		},
	}}
	if b := ComputeHostBoM(container); !b.Tier1() || len(b.Containers) != 2 || len(b.MCP) != 0 {
		t.Errorf("image/manifest servers must disclose as containers, got %+v", b)
	}
}

// TestComputeHostBoM_IsAPureFunctionOfTheManifest is the invariant that
// REPLACED the fail-closed local-MCP guess: the BoM asks the host nothing, so
// there is no "unknown classification" state to fail closed on. The same
// manifest must produce the same bill of materials — and the same fingerprint —
// on every machine and at every moment, which is what makes an acceptance
// recorded on one day still valid on the next.
func TestComputeHostBoM_IsAPureFunctionOfTheManifest(t *testing.T) {
	root := t.TempDir()
	p := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{Name: "p", Integrations: []packinfo.Integration{
		{Name: "N", MCP: "notes", Command: "notes-mcp", Args: []string{"mcp"}, Env: "NOTES_TOKEN"},
		{Name: "D", MCP: "docs", URL: "https://docs.example.test/mcp"},
	}}}
	first, second := ComputeHostBoM(p), ComputeHostBoM(p)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("ComputeHostBoM is not deterministic:\n%+v\n%+v", first, second)
	}
	fp1, _, err := ComputeHostExecFingerprint(root, first)
	if err != nil {
		t.Fatal(err)
	}
	fp2, _, err := ComputeHostExecFingerprint(root, second)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Errorf("the same manifest must fingerprint identically, got %s and %s", fp1, fp2)
	}
	// A declared MCP server can never be Tier-0, so the gate cannot be skipped
	// for one: the non-interactive gate still fails closed without --yes.
	if !first.Tier1() {
		t.Fatal("a pack declaring MCP servers must be Tier-1")
	}
	if err := packTrustGate(strings.NewReader(""), io.Discard, false, false, "p", first); err == nil {
		t.Error("non-TTY without --yes must fail closed for a pack with host-exec MCP")
	}
}

// TestPackUse_RemoteMCPReferenceRequiresYes: a non-interactive caller must
// explicitly accept a pack-selected remote endpoint. --yes is used here so
// the in-process test can cross the same gate without an os.Exit.
func TestPackUse_RemoteMCPReferenceRequiresYes(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "personal", Schema: 1,
		Integrations: []packinfo.Integration{{Name: "Docs", MCP: "docs", URL: "https://docs.example.test/mcp"}}})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{"--yes", root}, registerOK)
	// The ENDPOINT, not the label that used to precede it. This test read
	// `strings.Contains(out, "Remote MCP:")` while its own message said the screen
	// must disclose the endpoint — so it would have passed on a screen that printed
	// the heading and no URL. Assert the thing that matters: a remote MCP is where
	// conversation content goes, and the URL is fingerprinted, so a pack that
	// repointed it must show the new destination on the screen it re-gates with.
	if !strings.Contains(out.String(), "https://docs.example.test/mcp") {
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

// TestPackUse_ReferenceOnlyIntegrationStaysTier0 is what remains of the old
// "a gog reference stays Tier-0" case: the vendor special case is gone (a pack
// that wants that server declares a transport like any other, and every
// transport is Tier-1), but the Tier-0 half of the gate still needs an
// end-to-end witness. An integration that names NO mcp server contributes only
// a credential NAME, which is solicited rather than executed — so it adopts
// silently on a non-TTY with no --yes.
func TestPackUse_ReferenceOnlyIntegrationStaysTier0(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packinfo.Manifest{Name: "personal", Schema: 1,
		Integrations: []packinfo.Integration{{Name: "Vendor CLI", Env: "VENDOR_TOKEN"}}})

	var out bytes.Buffer
	if err := RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK); err != nil {
		t.Fatalf("a credential-only integration must adopt silently: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("a credential-only integration must not render the Tier-1 screen, got:\n%s", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pack != root {
		t.Errorf("the pack must still activate, cfg.Pack=%q", cfg.Pack)
	}
	if len(cfg.MCP) != 0 {
		t.Errorf("an integration with no mcp name must register no server, mcp=%v", cfg.MCP)
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

			rootA := filepath.Join(dir, "a")
			mustWritePack(t, rootA, packinfo.Manifest{Name: "a", Schema: 1})
			rootB := filepath.Join(dir, "b")
			mustWritePack(t, rootB, packinfo.Manifest{Name: "b", Schema: 1})

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

// --- #3: classification has no unknown state left to fail closed on -------------
//
// TestLocalMCPClassifier_UnknownFailsClosed is GONE, and the reason is the whole
// point of the change it guarded: the classifier it tested asked a host binary
// at runtime which servers were "local", so the answer had an UNKNOWN state and
// every caller had to carry a fail-closed guess for it. Classification is now a
// pure map lookup over the manifest's declared transports, so the unknown state
// does not exist and cannot be mis-handled. What that test actually protected —
// "a pack contributing a host-exec MCP server is always Tier-1, and the gate
// fails closed on a non-TTY without --yes" — is asserted directly by
// TestComputeHostBoM_IsAPureFunctionOfTheManifest and
// TestComputeHostBoM_TransportDecidesHostExecClassification above.

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
	mustWritePack(t, root, packinfo.Manifest{Name: "c", Schema: 1})
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
	store.RecordAcceptance(key, PackTrustRecord{Path: packinfo.CanonicalizePackRoot(root), Remote: url, Commit: "c1", Fingerprint: "fp1"})
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
// --yes, while a CHANGED surface still refuses.
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
	if err := RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK); err == nil {
		t.Errorf("a changed host-exec fingerprint must still fail closed (re-gate/refuse); output:\n%s", out.String())
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
	mustWritePack(t, root, packinfo.Manifest{Name: "evil", Schema: 1})
	if err := os.Symlink(victim, PackLockPath(root)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK)
	if fi, err := os.Lstat(PackLockPath(root)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("pack.lock must be a fresh regular file after adoption, got %v (err=%v)", fi, err)
	}
}
