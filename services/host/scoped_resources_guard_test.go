package main

// scoped_resources_guard_test.go — the repo-level guard for the two
// coexistence properties that are invisible in any single package's own
// tests, because both are about what production code must NEVER contain:
//
//  1. Pix never writes a HOST-GLOBAL sbx secret. Every provider credential
//     is written `--sandbox <name>`-scoped at launch (secret/scoped.go's
//     setScopedSbxSecret, the ONE `sbx secret set` call site). A `-g`/
//     `--global` write would hand every sandbox on the host — including a
//     coexisting PIX_HOME's — this stack's keys, and would make a rotation
//     invisible until something re-pushed the global.
//  2. Pix never names a RUNTIME resource with the bare, unscoped
//     "pix-memory"/"pix-session" forms. Those two names stay RESERVED
//     (envinfo refuses an authored server that claims either), but the
//     memory container, the memory MCP registration and the session MCP
//     registration a launch actually creates are all stack-scoped
//     (stack.MemoryContainerName/MCPMemoryName/MCPSessionName). A bare
//     fallback is exactly the collision two PIX_HOMEs on one host produce.
//
// It is a source-level guard on purpose: these are "no code path reaches
// this" properties, and the only honest way to check a negative across a
// whole module is to read the module.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// prodGoFiles walks the launcher module and returns every PRODUCTION .go
// file (no _test.go, no testdata/). A test fixture is allowed to mention a
// global write or a bare name — proving one is refused requires naming it.
func prodGoFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) < 50 {
		t.Fatalf("only %d production files found — the walk is broken, and a broken walk passes every negative check", len(out))
	}
	return out
}

// globalSecretWrite matches an `sbx secret set` argv composition that also
// carries a global scope flag, in either spelling, on the same statement.
var globalSecretWrite = regexp.MustCompile(`"secret",\s*"set"[^\n]*"(-g|--global)"|"(-g|--global)"[^\n]*"secret",\s*"set"`)

// TestProductionNeverWritesAGlobalSbxSecret is invariant 1.
func TestProductionNeverWritesAGlobalSbxSecret(t *testing.T) {
	for _, path := range prodGoFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if globalSecretWrite.Match(data) {
			t.Errorf("%s composes a HOST-GLOBAL `sbx secret set`: Pix writes sandbox-scoped secrets only (secret/scoped.go)", path)
		}
	}
}

// TestExactlyOneScopedSecretWriteSite is the other half of invariant 1: a
// second `sbx secret set` call site anywhere is how a scoped write quietly
// grows an unscoped twin. There is ONE, and it is the scoped one.
func TestExactlyOneScopedSecretWriteSite(t *testing.T) {
	var sites []string
	for _, path := range prodGoFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), `"secret", "set"`) {
			sites = append(sites, path)
		}
	}
	want := []string{filepath.Join("secret", "scoped.go")}
	if strings.Join(sites, ",") != strings.Join(want, ",") {
		t.Errorf("`sbx secret set` call sites = %v, want exactly %v (setScopedSbxSecret)", sites, want)
	}
	data, err := os.ReadFile(filepath.Join("secret", "scoped.go"))
	if err != nil {
		t.Fatalf("read scoped.go: %v", err)
	}
	// The scope flag and its value are part of the SAME argv as the write:
	// an `sbx secret set` whose --sandbox came from somewhere else could be
	// composed with an empty scope, which sbx treats as global.
	if !strings.Contains(string(data), `"secret", "set", "-f", "--sandbox", sandbox, name, "-t", val`) {
		t.Errorf("secret/scoped.go no longer composes the exact scoped argv `sbx secret set -f --sandbox <name> <key> -t <value>`")
	}
	if !strings.Contains(string(data), "refusing to write sbx secrets with no sandbox scope") {
		t.Errorf("secret/scoped.go no longer refuses an EMPTY sandbox scope, which sbx would treat as a global write")
	}
}

// stripNonCode removes line comments and JSON struct tags before the bare-name
// scan. A prose mention of the reserved name (every one of these files
// EXPLAINS the reservation) and a `json:"pix-memory"` manifest key are not
// resource names, and a guard that cannot tell them apart is a guard nobody
// can keep passing honestly.
func stripNonCode(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = jsonTag.ReplaceAllString(line, "")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

var jsonTag = regexp.MustCompile("`[^`]*`")

// bareRuntimeName matches a bare "pix-memory"/"pix-session" STRING LITERAL.
// The scoped composers never contain one (they build "pix-memory-<id>" from
// a validated id), so any literal here is either a reservation constant or
// a fallback — and this test names the exact files allowed to hold the
// former.
var bareRuntimeName = regexp.MustCompile(`"pix-(memory|session)"`)

// reservationSites are the ONLY production files permitted a bare literal,
// and each must KEEP it (see the reverse check below). stack/names.go is
// deliberately absent: it composes every scoped name from a "pix-memory-" /
// "pix-session-" prefix and a validated id, so it holds no bare form to
// permit — which is the whole shape this guard wants everywhere else.
// each because the bare name is the thing being RESERVED or REFUSED there,
// never a name composed for a live resource.
var reservationSites = map[string]string{
	filepath.Join("envinfo", "builtins.go"):    "the legacy reserved names an authored .sbxenv.yaml is refused for claiming",
	filepath.Join("container", "container.go"): "container.Name, the legacy name reset refuses to guess and no composer emits",
	filepath.Join("session", "mcp.go"):         "session.ReservedMCPName, the reservation itself — the value a launch SCOPES, never registers bare",
	filepath.Join("health", "pixhome.go"):      "a health ROW LABEL and a documented default, not a name any resource is created under",
}

// TestNoBareRuntimeResourceNames is invariant 2.
func TestNoBareRuntimeResourceNames(t *testing.T) {
	for _, path := range prodGoFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bareRuntimeName.MatchString(stripNonCode(string(data))) {
			continue
		}
		if _, ok := reservationSites[path]; !ok {
			t.Errorf("%s carries a bare \"pix-memory\"/\"pix-session\" literal: a RUNTIME resource name must come from stack.MemoryContainerName/MCPMemoryName/MCPSessionName, which are stack-scoped", path)
		}
	}
	// And the reverse: a reservation site that stopped carrying the literal
	// means the reservation itself was deleted, which this guard must not
	// silently pass.
	for path := range reservationSites {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !bareRuntimeName.MatchString(stripNonCode(string(data))) {
			t.Errorf("%s no longer names the reserved bare form at all — the reservation was deleted, not scoped", path)
		}
	}
}
