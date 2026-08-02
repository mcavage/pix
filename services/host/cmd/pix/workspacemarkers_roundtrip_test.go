// workspacemarkers_roundtrip_test.go — U-W0b.05: cross-boundary workspace
// marker round-trip coverage, landed BEFORE the pix rename (U-W3.11 flips
// ".pix" -> ".pix" and this file must stay green with only the literal
// dir-name constant changed, per FF10).
//
// Every OTHER *_test.go file in this package already unit-tests one marker's
// writer or reader in isolation (pack_v2_test.go for profile,
// run_knowledge_test.go for knowledge/knowledge.scope, workspacestate_test.go
// for the symlink-safety seam, onboard_test.go for onboarding.json,
// hoststate_test.go for host-state.json's absence). What none of them do is
// enumerate the FULL current marker set in one place and pin the EXACT bytes
// each production writer emits — the contract the TS-side readers
// (extensions/memory-recall.ts, extensions/memory-capture.ts,
// extensions/knowledge-recall.ts, extensions/ollama-bridge.ts, exercised from
// tests/workspace-markers.test.mjs) depend on byte-for-byte. This file is that
// single inventory, so a change to trailing-newline/whitespace/format on
// either side of the boundary is caught here instead of silently drifting.
//
// THE MEASURED SET (shards.md U-W0b.05's row), and where each one actually
// lives:
//
//	.pix/profile               — Go writes (pack.go writeMemoryScope),
//	                                   TS reads (memory-recall.ts,
//	                                   memory-capture.ts)
//	.pix/knowledge.scope        — Go writes (run.go writeKnowledgeScope),
//	                                   TS reads (knowledge-recall.ts)
//	.pix/ollama-bridge.model    — Go writes (run.go writeOllamaBridgeFile),
//	                                   TS reads (ollama-bridge.ts)
//	.pix/sandbox.pack           — Go writes AND reads
//	                                   (writeSandboxPackMarker /
//	                                   readSandboxPackMarker); no TS reader
//	.pix/knowledge               — Go writes AND reads
//	                                   (knowledge.KnowledgeUseProject / knowledge.ReadProjectPointer
//	                                   / projectBundle); no TS reader
//	.pix/onboarding.json         — the IN-SANDBOX AGENT writes it (not Go);
//	                                   Go reads + removes it (reconcileOnboarding)
//	.pix/host-state.json         — NEVER a file on EITHER side, by design
//	                                   (hoststate.go); this file's job is to
//	                                   prove that stays true even after every
//	                                   OTHER marker above has been written into
//	                                   the same workspace
//	routing, artifacts, custom-memory.db — NOT workspace ".pix" markers at
//	                                   all: routing.Dir()/taskArtifactRoot()
//	                                   resolve under the HOST's
//	                                   XDG_DATA_HOME/pix tree, never under
//	                                   any workspace's .pix; TestRoutingAnd
//	                                   ArtifactsAreHostDataRootNotWorkspaceMarker
//	                                   below pins that boundary so a future edit
//	                                   can't accidentally nest them under a
//	                                   workspace without this test noticing.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/knowledge"
	"pix/host/routing"
)

// workspaceMarkerFiles is the literal, enumerated set of REAL files a
// workspace's .pix dir can hold today. A future marker must be added
// here (and given its own round-trip below) or this inventory silently goes
// stale — that is the whole point of a named list instead of a comment.
var workspaceMarkerFiles = []string{
	"profile",
	"knowledge.scope",
	"ollama-bridge.model",
	"sandbox.pack",
	"knowledge",
	"onboarding.json",
}

func TestWorkspaceMarkerInventory_MatchesEnumeratedSet(t *testing.T) {
	want := map[string]bool{
		"profile":             true,
		"knowledge.scope":     true,
		"ollama-bridge.model": true,
		"sandbox.pack":        true,
		"knowledge":           true,
		"onboarding.json":     true,
	}
	if len(workspaceMarkerFiles) != len(want) {
		t.Fatalf("workspaceMarkerFiles has %d entries, want %d — update BOTH this list and the round-trip coverage below", len(workspaceMarkerFiles), len(want))
	}
	for _, name := range workspaceMarkerFiles {
		if !want[name] {
			t.Errorf("unexpected marker %q in workspaceMarkerFiles — is it covered by a round-trip test below?", name)
		}
	}
}

// ── profile ──────────────────────────────────────────────────────────────

func TestMarkerRoundTrip_Profile(t *testing.T) {
	ws := t.TempDir()
	writeMemoryScope(ws, &packInfo{Manifest: packManifest{MemoryScope: "work"}})

	got := readFile(t, filepath.Join(ws, ".pix", "profile"))
	if got != "work\n" {
		t.Errorf("profile file = %q, want %q (memory-recall.ts/memory-capture.ts .trim() this, so a trailing newline must be exactly one)", got, "work\n")
	}

	// nil pack (or a default/unscoped one) removes the marker entirely — the TS
	// side's readFileSync then throws ENOENT and both readers fall back to
	// "default", which is the documented un-scoped case.
	writeMemoryScope(ws, nil)
	if _, err := os.Stat(filepath.Join(ws, ".pix", "profile")); !os.IsNotExist(err) {
		t.Errorf("profile marker should be removed for a nil pack, stat err=%v", err)
	}
}

// ── knowledge.scope ──────────────────────────────────────────────────────

func TestMarkerRoundTrip_KnowledgeScope(t *testing.T) {
	ws := t.TempDir()
	if err := writeKnowledgeScope(ws, []string{"/global/bundle", "/project/bundle"}); err != nil {
		t.Fatalf("writeKnowledgeScope: %v", err)
	}
	got := readFile(t, filepath.Join(ws, ".pix", "knowledge.scope"))
	want := "/global/bundle\n/project/bundle\n"
	if got != want {
		t.Errorf("knowledge.scope = %q, want %q (knowledge-recall.ts splits on [\\n,] and trims each line)", got, want)
	}
}

// ── ollama-bridge.model ──────────────────────────────────────────────────

func TestMarkerRoundTrip_OllamaBridgeModel(t *testing.T) {
	ws := t.TempDir()
	writeOllamaBridgeFile(ws, "qwen3.5:9b")
	got := readFile(t, filepath.Join(ws, ".pix", "ollama-bridge.model"))
	if got != "qwen3.5:9b\n" {
		t.Errorf("ollama-bridge.model = %q, want %q (ollama-bridge.ts .trim()s this)", got, "qwen3.5:9b\n")
	}

	// An empty model falls back to the shipped default, never an empty file —
	// ollama-bridge.ts treats an empty trimmed read as "absent" and uses its
	// own "qwen3.5:9b" default, so the two defaults must actually agree.
	ws2 := t.TempDir()
	writeOllamaBridgeFile(ws2, "  ")
	got2 := readFile(t, filepath.Join(ws2, ".pix", "ollama-bridge.model"))
	if strings.TrimSpace(got2) != "qwen3.5:9b" {
		t.Errorf("blank model wrote %q, want the default qwen3.5:9b (must match ollama-bridge.ts's own hardcoded default)", got2)
	}
}

// ── sandbox.pack ─────────────────────────────────────────────────────────

func TestMarkerRoundTrip_SandboxPack(t *testing.T) {
	ws := t.TempDir()
	packRoot := filepath.Join(t.TempDir(), "mypack")
	if err := os.MkdirAll(packRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSandboxPackMarker(ws, packRoot)

	want := canonicalizePackRoot(packRoot)
	got := readSandboxPackMarker(ws)
	if got != want {
		t.Errorf("readSandboxPackMarker = %q, want %q", got, want)
	}
	raw := readFile(t, sandboxPackMarkerPath(ws))
	if raw != want+"\n" {
		t.Errorf("sandbox.pack file = %q, want %q", raw, want+"\n")
	}

	// Pack-less creation removes any prior marker.
	writeSandboxPackMarker(ws, "")
	if got := readSandboxPackMarker(ws); got != "" {
		t.Errorf("readSandboxPackMarker after pack-less write = %q, want empty", got)
	}
}

// ── knowledge (project pointer) ──────────────────────────────────────────

func TestMarkerRoundTrip_KnowledgeProjectPointer(t *testing.T) {
	repo := t.TempDir()
	bundle := t.TempDir()

	var out bytes.Buffer
	if err := knowledge.KnowledgeUseProject(bundle, repo, &out); err != nil {
		t.Fatalf("knowledge.KnowledgeUseProject: %v", err)
	}

	// knowledge.ReadProjectPointer sees the portable (repo-relative-or-absolute) ref...
	pointer := knowledge.ReadProjectPointer(repo)
	if pointer == "" {
		t.Fatal("knowledge.ReadProjectPointer returned empty after knowledge.KnowledgeUseProject wrote the pointer")
	}
	// ...and projectBundle re-resolves that ref back to the SAME canonical id
	// the store keys its `bundle` column on — the round trip
	// wireKnowledgeScope actually depends on every real run.
	want := knowledge.CanonicalizeKnowledgeBundle(bundle)
	got := projectBundle(repo)
	if got != want {
		t.Errorf("projectBundle round-trip = %q, want %q (pointer was %q)", got, want, pointer)
	}
}

// ── onboarding.json ──────────────────────────────────────────────────────
//
// Unlike every marker above, this file is written by the IN-SANDBOX AGENT,
// not by Go — Go is the READER here. The round trip under test is therefore
// "the exact JSON shape the agent is documented to write" -> reconcileOnboarding
// consumes it -> the marker is gone and config reflects it exactly once.

func TestMarkerRoundTrip_OnboardingJSON(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")

	dir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	proposal := onboardingResult{Version: 1, MCP: []string{gwServerName}}
	data, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, onboardingFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	env := fakeEnv{present: map[string]bool{}}.env()
	var out bytes.Buffer
	reconcileOnboarding(ws, env, strings.NewReader(""), &out, true, false)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("onboarding.json must be removed once applied, stat err=%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// onboarding has deliberately NO account writer (Google Workspace
	// authorization needs a browser); the marker's mcp entry is what should
	// round-trip into config.
	if !containsStr(cfg.MCP, gwServerName) {
		t.Errorf("cfg.MCP = %v, want %s round-tripped from the marker", cfg.MCP, gwServerName)
	}
}

// ── host-state.json: the negative control, exercised WITH every other
// marker present in the same workspace ─────────────────────────────────────

func TestMarkerRoundTrip_HostStateNeverBecomesAWorkspaceFile(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")

	// Write every OTHER real marker into the same workspace...
	writeMemoryScope(ws, &packInfo{Manifest: packManifest{MemoryScope: "work"}})
	if err := writeKnowledgeScope(ws, []string{"/a"}); err != nil {
		t.Fatal(err)
	}
	writeOllamaBridgeFile(ws, "qwen3.5:9b")
	writeSandboxPackMarker(ws, filepath.Join(ws, "pack"))
	var out bytes.Buffer
	_ = knowledge.KnowledgeUseProject(t.TempDir(), ws, &out)
	if err := os.WriteFile(filepath.Join(ws, ".pix", onboardingFileName), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv{present: map[string]bool{}}.env()
	reconcileOnboarding(ws, env, strings.NewReader(""), &out, true, false)

	// ...and confirm host-state.json never appeared. See hoststate.go's own
	// package comment + hoststate_test.go's
	// TestInjectTrustedHostState_IgnoresStaleWorkspaceFile for the full
	// rationale (trusted facts travel only inside the generated prompt, never
	// a workspace file a hostile clone could also write).
	if _, err := os.Stat(filepath.Join(ws, ".pix", "host-state.json")); !os.IsNotExist(err) {
		t.Errorf("host-state.json must never exist in a ws, stat err=%v", err)
	}
}

// ── routing / artifacts / custom-memory.db: host data-root, not a workspace
// marker ───────────────────────────────────────────────────────────────────

func TestRoutingAndArtifactsAreHostDataRootNotWorkspaceMarkers(t *testing.T) {
	ws := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("ROUTING_DIR", "")

	routingDir := routing.Dir()
	artifactsDir := taskArtifactRoot()

	for _, p := range []struct{ name, dir string }{
		{"routing", routingDir},
		{"artifacts", artifactsDir},
	} {
		if !strings.HasPrefix(p.dir, dataHome) {
			t.Errorf("%s dir %q must resolve under XDG_DATA_HOME (%q), not somewhere workspace-relative", p.name, p.dir, dataHome)
		}
		if strings.Contains(p.dir, ".pix") {
			t.Errorf("%s dir %q must never contain a .pix path component — it is host data, not a ws marker", p.name, p.dir)
		}
		if strings.HasPrefix(p.dir, ws) {
			t.Errorf("%s dir %q must never live inside the ws %q", p.name, p.dir, ws)
		}
	}
}
