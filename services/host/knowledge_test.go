package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/plugin"
)

// Compile-time guarantee the adapter satisfies the plugin interface.
var _ plugin.KnowledgeStore = (*knowledgeStoreAdapter)(nil)

// writeKnowledgeBundle materializes a small OKF bundle on disk (two concepts,
// each with citations) and returns the root dir.
func writeKnowledgeBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"policies/refunds.md": `---
type: policy
title: Refund Policy
description: How customer refunds are processed and approved.
tags:
  - billing
  - support
---
Refunds are issued within 14 days of a return. A refund over $500 needs
manager approval before the refund is processed.

# Citations
- https://example.com/refund-policy
- Internal billing runbook
`,
		"tables/users.md": `---
type: table
title: Users
description: The users dimension table.
---
The users table stores one row per registered account holder.

# Citations
- https://example.com/schema/users
`,
	}

	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// newTestKnowledgeStore builds an FTS-only store (nil embedder -> no Ollama
// needed), mirroring memory_plugin_test.go's newTestAdapter.
func newTestKnowledgeStore(t *testing.T) *knowledgeStore {
	t.Helper()
	st, err := newKnowledgeStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestKnowledgeReindexAndQuery(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)

	n, bundles, err := st.reindex([]string{dir})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2", n)
	}
	canon := canonicalizeBundle(dir)
	if len(bundles) != 1 || bundles[0] != canon {
		t.Fatalf("bundles = %v, want [%s]", bundles, canon)
	}

	// Keyword match should rank the refund policy first and carry its citations
	// plus a non-empty snippet drawn from the body.
	hits := st.query("refund approval", nil, 8)
	if len(hits) == 0 {
		t.Fatal("query returned no hits")
	}
	top := hits[0]
	if top.ID != "policies/refunds" {
		t.Fatalf("top hit id = %q, want policies/refunds", top.ID)
	}
	if top.Title != "Refund Policy" {
		t.Errorf("top hit title = %q", top.Title)
	}
	if top.Bundle != canon {
		t.Errorf("top hit bundle = %q, want %q", top.Bundle, canon)
	}
	if top.Snippet == "" {
		t.Error("top hit snippet should be non-empty")
	}
	if top.Score <= 0 {
		t.Errorf("top hit score = %v, want > 0", top.Score)
	}
	wantCite := []string{"https://example.com/refund-policy", "Internal billing runbook"}
	if len(top.Citations) != len(wantCite) {
		t.Fatalf("citations = %v, want %v", top.Citations, wantCite)
	}
	for i, c := range wantCite {
		if top.Citations[i] != c {
			t.Errorf("citation[%d] = %q, want %q", i, top.Citations[i], c)
		}
	}
}

// TestKnowledgeReindexBadRoot is the F5 guard: reindex on a nonexistent or
// non-directory root returns a HARD error (not nil/0), so a typo'd bundle path
// never masquerades as a healthy empty index.
func TestKnowledgeReindexBadRoot(t *testing.T) {
	st := newTestKnowledgeStore(t)

	// A path that does not exist.
	missing := filepath.Join(t.TempDir(), "nope", "does-not-exist")
	if n, _, err := st.reindex([]string{missing}); err == nil {
		t.Fatalf("reindex(%q) = nil error (indexed %d), want a hard error", missing, n)
	}

	// A path that exists but is a regular file, not a directory.
	file := filepath.Join(t.TempDir(), "a-file.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.reindex([]string{file}); err == nil {
		t.Fatalf("reindex(%q) = nil error, want a not-a-directory error", file)
	}
}

// TestKnowledgeReindexUnreadableFileWarns is the F5 guard for the warning path:
// a bundle whose file can't be read makes ReadBundle record a "read ..." warning,
// which reindex must promote to a hard error instead of silently indexing 0.
func TestKnowledgeReindexUnreadableFileWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file mode 0 is still readable, can't simulate an unreadable file")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(bad, []byte("---\ntype: policy\ntitle: X\n---\nbody\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	st := newTestKnowledgeStore(t)
	if _, _, err := st.reindex([]string{dir}); err == nil {
		t.Fatalf("reindex over an unreadable file = nil error, want a warning-driven error")
	}
}

func TestKnowledgeQueryBundleFilter(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)
	if _, _, err := st.reindex([]string{dir}); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	// A non-matching bundle name filters everything out.
	if hits := st.query("refund", []string{"/no/such/bundle"}, 8); len(hits) != 0 {
		t.Fatalf("bundle filter should drop all hits, got %d", len(hits))
	}
	// The real bundle name still matches.
	if hits := st.query("refund", []string{dir}, 8); len(hits) == 0 {
		t.Fatal("bundle filter dropped the matching bundle")
	}
}

// TestKnowledgeQueryBundleSet: query scoped to a SET of bundles returns only
// concepts from those bundles; the empty set returns concepts from all bundles.
func TestKnowledgeQueryBundleSet(t *testing.T) {
	dirA := writeKnowledgeBundle(t)
	dirB := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)
	if _, _, err := st.reindex([]string{dirA, dirB}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	canonA := canonicalizeBundle(dirA)
	canonB := canonicalizeBundle(dirB)

	// Scope to the set {dirB}: every hit must come from dirB.
	hitsB := st.query("refund users", []string{dirB}, 8)
	if len(hitsB) == 0 {
		t.Fatal("set {dirB} returned no hits")
	}
	for _, h := range hitsB {
		if h.Bundle != canonB {
			t.Fatalf("set {dirB} leaked a hit from %q, want only %q", h.Bundle, canonB)
		}
	}

	// Scope to the set {dirA, dirB}: hits from BOTH bundles are allowed.
	hitsBoth := st.query("refund users", []string{dirA, dirB}, 16)
	seen := map[string]bool{}
	for _, h := range hitsBoth {
		seen[h.Bundle] = true
	}
	if !seen[canonA] || !seen[canonB] {
		t.Fatalf("set {dirA,dirB} missing a bundle: saw %v, want both %q and %q", seen, canonA, canonB)
	}

	// Empty set == all bundles (same coverage as the explicit 2-elem set).
	all := st.query("refund users", nil, 16)
	seenAll := map[string]bool{}
	for _, h := range all {
		seenAll[h.Bundle] = true
	}
	if !seenAll[canonA] || !seenAll[canonB] {
		t.Fatalf("empty set should search all bundles: saw %v, want both %q and %q", seenAll, canonA, canonB)
	}
}

// TestKnowledgeQueryCanonicalization: a bundle stored under its canonical path
// still matches when the filter is spelled differently — through a symlink or
// with redundant path elements (design risk #1).
func TestKnowledgeQueryCanonicalization(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)
	if _, _, err := st.reindex([]string{dir}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	canon := canonicalizeBundle(dir)

	// A symlink pointing at the bundle resolves to the same stored id.
	link := filepath.Join(t.TempDir(), "linked-bundle")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	hits := st.query("refund", []string{link}, 8)
	if len(hits) == 0 {
		t.Fatal("symlinked bundle path did not match the stored (canonical) id")
	}
	for _, h := range hits {
		if h.Bundle != canon {
			t.Fatalf("symlink filter matched %q, want canonical %q", h.Bundle, canon)
		}
	}

	// A redundantly-spelled path (dir/../base) also canonicalizes to a match.
	spelled := filepath.Join(dir, "..", filepath.Base(dir))
	if got := st.query("refund", []string{spelled}, 8); len(got) == 0 {
		t.Fatalf("redundant path %q did not match the stored id", spelled)
	}
}

func TestKnowledgeHealthAndIdempotentReindex(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)

	if _, _, err := st.reindex([]string{dir}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	h := st.health()
	if !h.OK {
		t.Error("health.OK should be true")
	}
	if h.Vector {
		t.Error("health.Vector should be false for a nil-embedder store")
	}
	if h.Concepts != 2 {
		t.Fatalf("health.Concepts = %d, want 2", h.Concepts)
	}
	if len(h.Bundles) != 1 || h.Bundles[0] != dir {
		t.Fatalf("health.Bundles = %v, want [%s]", h.Bundles, dir)
	}

	// Re-running the reindex must not duplicate concepts.
	n, _, err := st.reindex([]string{dir})
	if err != nil {
		t.Fatalf("reindex #2: %v", err)
	}
	if n != 2 {
		t.Fatalf("second reindex indexed = %d, want 2", n)
	}
	if h2 := st.health(); h2.Concepts != 2 {
		t.Fatalf("after re-reindex health.Concepts = %d, want 2 (duplicated)", h2.Concepts)
	}
}

// TestKnowledgeAdapter exercises the plugin adapter translation (Query / Reindex
// / Health) over the store, mirroring memory_plugin_test.go.
func TestKnowledgeAdapter(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	a := newKnowledgeStoreAdapter(newTestKnowledgeStore(t))

	ri, err := a.Reindex(plugin.ReindexArgs{BundlePaths: []string{dir}})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if ri.Indexed != 2 {
		t.Fatalf("Indexed = %d, want 2", ri.Indexed)
	}

	qr, err := a.Query(plugin.QueryArgs{Query: "users table", Bundles: nil, Limit: 8})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(qr.Concepts) == 0 || qr.Concepts[0].ID != "tables/users" {
		t.Fatalf("query did not surface tables/users first: %+v", qr.Concepts)
	}

	h, err := a.Health()
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.OK || h.Concepts != 2 {
		t.Fatalf("health = %+v, want OK with 2 concepts", h)
	}
}
