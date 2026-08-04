// pack_services_test.go — U08a: the trusted pack [[services]] UnitSpec
// (PRD AC-PACK-02, AC-SUP-05, Story 08).
//
// [[services]] is the SOLE way a pack may declare a long-running external
// service unit. This build is declaration-only (no supervisor consumption):
// the spec normalizes, validates fail-closed, enters the Tier-1
// bill-of-materials/consent screen, and every field is part of the host-exec
// fingerprint so ANY change re-gates.
//
// All tests use REAL temp packs on disk (LoadPack over a written pack.toml)
// and the real fingerprint/trust-store code. No mocks.
package pack

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeServicePack writes a real pack with the given [[services]] TOML body
// and returns its root. body is appended after a minimal identity header.
func writeServicePack(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "name = \"svc-pack\"\nschema = 2\n" + body
	if err := os.WriteFile(filepath.Join(dir, PackManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validGoPluginService = `
[[services]]
name = "telemetry"
runtime = "go-plugin"
activation = "on-demand"
path = "bin/telemetry"
sha = "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"
argv = ["--dir", "data"]
env = ["TELEMETRY_TOKEN"]
port = 12000
listen = "127.0.0.1"
health = "/healthz"
mounts = ["data"]
network = ["api.example.com"]
license = "MIT"
source = "https://github.com/example/telemetry"

[services.resources]
memory_mb = 256
cpu_percent = 50
`

const validContainerService = `
[[services]]
name = "indexer"
runtime = "container"
activation = "always"
image = "ghcr.io/example/indexer@sha256:aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"
port = 12100
health = "tcp"
license = "Apache-2.0"
source = "https://github.com/example/indexer"
`

// --- parse + normalize --------------------------------------------------------

func TestPackServices_ValidManifestLoads(t *testing.T) {
	root := writeServicePack(t, validGoPluginService+validContainerService)
	p, err := LoadPack(root)
	if err != nil {
		t.Fatalf("LoadPack: %v", err)
	}
	if len(p.Manifest.Services) != 2 {
		t.Fatalf("want 2 services, got %d", len(p.Manifest.Services))
	}
	s := p.Manifest.Services[0]
	if s.Name != "telemetry" || s.Runtime != "go-plugin" || s.Activation != "on-demand" {
		t.Fatalf("bad first service: %+v", s)
	}
	if s.Resources == nil || s.Resources.MemoryMB != 256 || s.Resources.CPUPercent != 50 {
		t.Fatalf("resources not parsed: %+v", s.Resources)
	}
	c := p.Manifest.Services[1]
	if c.Runtime != serviceRuntimeContainer || c.Image == "" {
		t.Fatalf("bad container service: %+v", c)
	}
}

// requireLoadError asserts LoadPack fails and the error mentions want.
func requireLoadError(t *testing.T, body, want string) {
	t.Helper()
	root := writeServicePack(t, body)
	_, err := LoadPack(root)
	if err == nil {
		t.Fatalf("LoadPack accepted invalid [[services]] (want error containing %q):\n%s", want, body)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not mention %q", err.Error(), want)
	}
}

// mutateValid returns validGoPluginService with one `key = value` line replaced.
func mutateValid(t *testing.T, key, newLine string) string {
	t.Helper()
	lines := strings.Split(validGoPluginService, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), key+" =") || strings.HasPrefix(strings.TrimSpace(l), key+" ") {
			lines[i] = newLine
			return strings.Join(lines, "\n")
		}
	}
	t.Fatalf("key %q not found in fixture", key)
	return ""
}

// --- rejections ----------------------------------------------------------------

func TestPackServices_RejectsValueShapedEnv(t *testing.T) {
	for _, bad := range []string{
		`env = ["TELEMETRY_TOKEN=xoxb-1234"]`,    // literal assignment
		`env = ["op://vault/item/credential"]`,   // op ref is a VALUE, not a name
		`env = ["xoxb-289234908usldkjfsdlkjfx"]`, // pasted secret
		`env = ["MY TOKEN"]`,                     // whitespace: not a var name
		`env = [""]`,                             // empty
	} {
		requireLoadError(t, mutateValid(t, "env", bad), "env")
	}
}

func TestPackServices_RejectsReservedPorts(t *testing.T) {
	for _, port := range []int{11435, 11437} {
		requireLoadError(t, mutateValid(t, "port", fmt.Sprintf("port = %d", port)), "reserved")
	}
	requireLoadError(t, mutateValid(t, "port", "port = 70000"), "port")
	requireLoadError(t, mutateValid(t, "port", "port = -1"), "port")
}

func TestPackServices_RejectsNonLoopbackListen(t *testing.T) {
	for _, listen := range []string{"0.0.0.0", "10.0.0.5", "example.com", "::"} {
		requireLoadError(t, mutateValid(t, "listen", fmt.Sprintf("listen = %q", listen)), "loopback")
	}
	// loopback spellings all pass
	for _, ok := range []string{"127.0.0.1", "::1", "localhost", "127.1.2.3"} {
		root := writeServicePack(t, mutateValid(t, "listen", fmt.Sprintf("listen = %q", ok)))
		if _, err := LoadPack(root); err != nil {
			t.Fatalf("loopback listen %q rejected: %v", ok, err)
		}
	}
}

func TestPackServices_RejectsInvalidRuntimeAndActivation(t *testing.T) {
	requireLoadError(t, mutateValid(t, "runtime", `runtime = "python"`), "runtime")
	requireLoadError(t, mutateValid(t, "runtime", `runtime = ""`), "runtime")
	requireLoadError(t, mutateValid(t, "activation", `activation = "sometimes"`), "activation")
	requireLoadError(t, mutateValid(t, "activation", `activation = ""`), "activation")
}

func TestPackServices_ExecutableIdentityFailClosed(t *testing.T) {
	// go-plugin without a sha pin
	requireLoadError(t, mutateValid(t, "sha", `sha = ""`), "sha")
	// sha must be a full sha256 hex
	requireLoadError(t, mutateValid(t, "sha", `sha = "abc123"`), "sha")
	// go-plugin without a path
	requireLoadError(t, mutateValid(t, "path", `path = ""`), "path")
	// path escaping the pack root
	requireLoadError(t, mutateValid(t, "path", `path = "../../bin/sh"`), "escapes")
	// absolute path
	requireLoadError(t, mutateValid(t, "path", `path = "/usr/bin/env"`), "repo-relative")
	// go-plugin may not carry an image (inserted inside the service table)
	requireLoadError(t, strings.Replace(validGoPluginService, `path = "bin/telemetry"`,
		"path = \"bin/telemetry\"\nimage = \"ghcr.io/x/y@sha256:aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66\"", 1), "image")
	// container image must be digest-pinned
	requireLoadError(t, strings.Replace(validContainerService,
		`image = "ghcr.io/example/indexer@sha256:aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"`,
		`image = "ghcr.io/example/indexer:latest"`, 1), "digest")
	// container may not carry a host path
	requireLoadError(t, validContainerService+"\n"+`path = "bin/indexer"`, "path")
}

func TestPackServices_NameHygiene(t *testing.T) {
	requireLoadError(t, mutateValid(t, "name", `name = "../evil"`), "name")
	requireLoadError(t, mutateValid(t, "name", `name = ""`), "name")
	// duplicate names
	requireLoadError(t, validGoPluginService+validGoPluginService, "duplicate")
	// reserved built-in unit names can never be shadowed by a pack
	for _, reserved := range []string{"memory", "knowledge", "broker", "monitor"} {
		requireLoadError(t, mutateValid(t, "name", fmt.Sprintf("name = %q", reserved)), "reserved")
	}
}

func TestPackServices_LicenseAndSourceRequired(t *testing.T) {
	requireLoadError(t, mutateValid(t, "license", `license = ""`), "license")
	requireLoadError(t, mutateValid(t, "license", `license = "MIT; rm -rf /"`), "license")
	requireLoadError(t, mutateValid(t, "source", `source = ""`), "source")
	requireLoadError(t, mutateValid(t, "source", `source = "http://example.com/x"`), "source")
	requireLoadError(t, mutateValid(t, "source", `source = "https://user:pw@example.com/x"`), "source")
}

func TestPackServices_MountAndNetworkHygiene(t *testing.T) {
	requireLoadError(t, mutateValid(t, "mounts", `mounts = ["/etc"]`), "repo-relative")
	requireLoadError(t, mutateValid(t, "mounts", `mounts = ["../up"]`), "escapes")
	requireLoadError(t, mutateValid(t, "network", `network = ["https://api.example.com/path"]`), "network")
	requireLoadError(t, mutateValid(t, "network", `network = [""]`), "network")
}

// --- Tier-1 gate ---------------------------------------------------------------

func TestPackServices_ServiceAloneIsTier1AndFailsClosedNonTTY(t *testing.T) {
	root := writeServicePack(t, validGoPluginService)
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := ComputeHostBoM(p, "", func(string) bool { return false })
	if len(bom.Services) != 1 {
		t.Fatalf("service missing from BoM: %+v", bom)
	}
	if !bom.Tier1() {
		t.Fatal("a [[services]] declaration must be Tier-1 (host-exec surface)")
	}
	var out bytes.Buffer
	if err := packTrustGate(nil, &out, false, false, p.Manifest.Name, bom); err == nil {
		t.Fatal("non-TTY adoption of a pack service must fail closed")
	}
}

func TestPackServices_ConsentRendersEveryField(t *testing.T) {
	root := writeServicePack(t, validGoPluginService+validContainerService)
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := ComputeHostBoM(p, "", func(string) bool { return false })
	var out bytes.Buffer
	renderHostBoM(&out, bom)
	screen := out.String()
	for _, want := range []string{
		"telemetry", "go-plugin", "on-demand",
		"bin/telemetry", "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66",
		"--dir", "TELEMETRY_TOKEN", "12000", "127.0.0.1", "/healthz",
		"data", "api.example.com", "256", "50", "MIT", "https://github.com/example/telemetry",
		"indexer", "container", "always",
		"ghcr.io/example/indexer@sha256:", "12100", "tcp", "Apache-2.0",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("consent screen missing %q:\n%s", want, screen)
		}
	}
}

// --- fingerprint ---------------------------------------------------------------

// TestPackServices_ServicelessFingerprintUnchanged pins the fingerprint of a
// pre-U08a surface, computed with the code BEFORE [[services]] existed. Any
// drift means every already-accepted pack re-gates on upgrade — compatibility
// broken.
func TestPackServices_ServicelessFingerprintUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "wrap"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := hostBoM{
		Proxies: []string{"wrap"},
		Bins:    []packBin{{Name: "tool", Path: "bin/tool", SHA: "AB12", Host: true}},
		Egress:  []string{"api.example.com"},
		Creds:   []string{"MY_TOKEN"},
	}
	fp, _, err := ComputeHostExecFingerprint(dir, b)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "b15ba6c81f409cc6d06c3fd1235a87712a10298ff02e877e1321a508f6e0ad53"
	if fp != golden {
		t.Fatalf("serviceless fingerprint drifted (breaks every accepted pack):\n got %s\nwant %s", fp, golden)
	}
}

func serviceFingerprint(t *testing.T, body string) string {
	t.Helper()
	root := writeServicePack(t, body)
	p, err := LoadPack(root)
	if err != nil {
		t.Fatalf("LoadPack: %v\n%s", err, body)
	}
	bom := ComputeHostBoM(p, "", func(string) bool { return false })
	fp, _, err := ComputeHostExecFingerprint(root, bom)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestPackServices_FingerprintStable(t *testing.T) {
	a := serviceFingerprint(t, validGoPluginService+validContainerService)
	b := serviceFingerprint(t, validContainerService+validGoPluginService) // manifest reorder
	if a != b {
		t.Fatal("pure manifest reorder must not change the fingerprint")
	}
	if a2 := serviceFingerprint(t, validGoPluginService+validContainerService); a2 != a {
		t.Fatal("recompute must be deterministic")
	}
}

func TestPackServices_EveryFieldChangesFingerprint(t *testing.T) {
	base := serviceFingerprint(t, validGoPluginService)
	mutations := map[string]string{
		"name":        `name = "telemetry2"`,
		"activation":  `activation = "always"`,
		"path":        `path = "bin/telemetry2"`,
		"sha":         `sha = "bb11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"`,
		"argv":        `argv = ["--dir", "other"]`,
		"env":         `env = ["OTHER_TOKEN"]`,
		"port":        `port = 12001`,
		"listen":      `listen = "::1"`,
		"health":      `health = "/livez"`,
		"mounts":      `mounts = ["data2"]`,
		"network":     `network = ["api2.example.com"]`,
		"license":     `license = "BSD-3-Clause"`,
		"source":      `source = "https://github.com/example/other"`,
		"memory_mb":   `memory_mb = 512`,
		"cpu_percent": `cpu_percent = 75`,
	}
	for key, line := range mutations {
		fp := serviceFingerprint(t, mutateValid(t, key, line))
		if fp == base {
			t.Fatalf("changing service field %q did not change the fingerprint", key)
		}
	}
	// runtime change (go-plugin -> container swaps identity fields)
	if fp := serviceFingerprint(t, strings.Replace(validContainerService, `name = "indexer"`, `name = "telemetry"`, 1)); fp == base {
		t.Fatal("changing service runtime did not change the fingerprint")
	}
	// a service present at all differs from none
	if fp := serviceFingerprint(t, ""); fp == base {
		t.Fatal("removing the service did not change the fingerprint")
	}
}

// --- re-gate through the real trust store ---------------------------------------

func TestPackServices_ChangeReGatesAgainstTrustStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	root := writeServicePack(t, validGoPluginService)
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := ComputeHostBoM(p, "", func(string) bool { return false })
	fp, _, err := ComputeHostExecFingerprint(root, bom)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutatePackTrustStore(func(s *PackTrustStore) error {
		s.RecordAcceptance(s.TrustKey(root), PackTrustRecord{Fingerprint: fp, Path: CanonicalizePackRoot(root)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := store.acceptedFingerprint(store.TrustKey(root)); !ok || got != fp {
		t.Fatal("acceptance not recorded")
	}

	// Mutate ONE service field on disk (the sha pin) — the accepted fingerprint
	// must no longer match, so adoption re-gates.
	mutated := "name = \"svc-pack\"\nschema = 2\n" + mutateValid(t, "sha",
		`sha = "cc11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"`)
	if err := os.WriteFile(filepath.Join(root, PackManifestName), []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom2 := ComputeHostBoM(p2, "", func(string) bool { return false })
	fp2, _, err := ComputeHostExecFingerprint(root, bom2)
	if err != nil {
		t.Fatal(err)
	}
	if fp2 == fp {
		t.Fatal("service sha change did not change the fingerprint")
	}
	if got, _ := store.acceptedFingerprint(store.TrustKey(root)); got == fp2 {
		t.Fatal("mutated surface must NOT match the accepted fingerprint (re-gate)")
	}
}
