package pack

// Tests for the packs-v2 Phase 1 review + security findings
// (docs/design/packs-v2-impl.md). One (or more) test per numbered finding in
// the review; see the fix comments in pack.go/run.go/config/config.go for the
// finding each test guards.
//
// finding #1 [CRITICAL SECURITY] (the adopted-pack private [[knowledge]] ref
// guard) was retired along with the [[knowledge]] facet itself (W2 U03A) — its
// coverage went with it.

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
)

// --- finding #2 [BLOCK]: pack.lock must record only what THIS activation
// actually added -------------------------------------------------------------

// TestPackUse_LockOnlyRecordsWhatThisActivationAdded: when a pack merely
// RE-DECLARES an mcp/knowledge entry the user already had, pack.lock must NOT
// claim it as this activation's contribution — otherwise switching away would
// remove the user's own pre-existing entry.
func TestPackUse_LockOnlyRecordsWhatThisActivationAdded(t *testing.T) {
	dir := isolatePackHost(t)

	// The user already has config.GWServerName configured, BEFORE any pack use.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP(config.GWServerName)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, Manifest{Name: "a", Schema: 1, Integrations: []Integration{
		{Name: "gog", MCP: config.GWServerName}, // overlapping name the pack merely re-declares
	}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, Manifest{Name: "b", Schema: 1})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	RunPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"}, registerOK)

	lockA := readPackLock(rootA)
	if slices.Contains(lockA.MCP, config.GWServerName) {
		t.Fatalf("pack.lock must not claim a pre-existing mcp as its own contribution, lock.MCP = %v", lockA.MCP)
	}

	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{rootB}, registerOK)

	cfgAfterB, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfgAfterB.MCP, config.GWServerName) {
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

// TestPackUse_RestoresGogAccountToPriorValueOnSwitchAway: a pack that declares
// gog_account overwrites cfg.GogAccount; switching to a pack that does NOT
// declare it must restore whatever cfg held before (not leak the value across
// packs, and not just leave it stuck).
func TestPackUse_RestoresGogAccountToPriorValueOnSwitchAway(t *testing.T) {
	dir := isolatePackHost(t)

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
	mustWritePack(t, rootWork, Manifest{Name: "work", Schema: 1, GogAccount: "work@company.com"})
	rootPersonal := filepath.Join(dir, "personal")
	mustWritePack(t, rootPersonal, Manifest{Name: "personal", Schema: 1})

	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{rootWork}, registerOK)
	cfgWork, _ := config.Load()
	if cfgWork.GogAccount != "work@company.com" {
		t.Fatalf("pack use work should set gog_account, got %q", cfgWork.GogAccount)
	}

	out.Reset()
	RunPackUse(fakeGitEnv(nil), &out, []string{rootPersonal}, registerOK)
	cfgPersonal, _ := config.Load()
	if cfgPersonal.GogAccount != "manual@example.com" {
		t.Errorf("switching to a pack with no gog_account should restore the PRIOR value, got %q, want %q", cfgPersonal.GogAccount, "manual@example.com")
	}
}

// TestPackRm_RemovesActivePackContributions (finding #5): `pack rm` must undo
// the active pack's mcp + gog_account contributions, not just clear cfg.Pack —
// otherwise "detached" is a lie about what happened.
func TestPackRm_RemovesActivePackContributions(t *testing.T) {
	dir := isolatePackHost(t)

	root := filepath.Join(dir, "work")
	mustWritePack(t, root, Manifest{
		Name:         "work",
		Schema:       1,
		GogAccount:   "work@company.com",
		Integrations: []Integration{{Name: "Fastmail", MCP: "fastmail"}},
	})

	var out bytes.Buffer
	// --yes: Tier-1 pack (declares an mcp); tests have no TTY (Phase-2 gate).
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK)
	cfgActive, _ := config.Load()
	if !slices.Contains(cfgActive.MCP, "fastmail") || cfgActive.GogAccount != "work@company.com" {
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
	if cfgAfter.GogAccount != "" {
		t.Errorf("pack rm should revert gog_account to its prior (empty) value, got %q", cfgAfter.GogAccount)
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

	p1 := &Info{Root: root, Manifest: Manifest{Name: "p", Proxies: []PackProxy{{Name: "a"}, {Name: "b"}}}}
	kit1, err := SynthesizePackKit(p1)
	if err != nil || kit1 == "" {
		t.Fatalf("expected a kit dir, got %q, err=%v", kit1, err)
	}
	if _, err := os.Stat(filepath.Join(kit1, "files", "home", ".local", "bin", "b")); err != nil {
		t.Fatalf("wrapper b should exist after the first synth: %v", err)
	}

	// pack.toml no longer declares "b" (e.g. the author removed it).
	p2 := &Info{Root: root, Manifest: Manifest{Name: "p", Proxies: []PackProxy{{Name: "a"}}}}
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

	pGood := &Info{Root: root, Manifest: Manifest{Name: "p", Proxies: []PackProxy{{Name: "a"}}}}
	kitGood, err := SynthesizePackKit(pGood)
	if err != nil || kitGood == "" {
		t.Fatalf("expected the first (good) synth to succeed, got %q, err=%v", kitGood, err)
	}

	// "missing" has no bin/missing file on disk.
	pBad := &Info{Root: root, Manifest: Manifest{Name: "p", Proxies: []PackProxy{{Name: "a"}, {Name: "missing"}}}}
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
	a := CanonicalizePackRoot("/tmp/x/y/../y/work")
	b := CanonicalizePackRoot("/tmp/x/y/work")
	if a != b {
		t.Errorf("CanonicalizePackRoot(%q) = %q, want it to equal CanonicalizePackRoot(%q) = %q", "/tmp/x/y/../y/work", a, "/tmp/x/y/work", b)
	}
}

// TestPackAdd_Mcp_CanonicalizesActivePackComparison: `pack add mcp fastmail
// ./work` (a RELATIVE path) must still recognize the active pack even though
// cfg.Pack is stored as an absolute path.
func TestPackAdd_Mcp_CanonicalizesActivePackComparison(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	root := filepath.Join(dir, "work")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1})

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
	RunPackAdd(fakeGitEnv(nil), &out, []string{"mcp", "fastmail", "./work", "--yes"}, registerOK)

	if strings.Contains(out.String(), "activate the pack to attach it") {
		t.Errorf("finding #7: relative path should have matched the active pack, got:\n%s", out.String())
	}
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg2.MCP, "fastmail") {
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
