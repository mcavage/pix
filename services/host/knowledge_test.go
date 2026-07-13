package main

import (
	"os"
	"path/filepath"
	"testing"

	"pi-stack/host/plugin"
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
	if len(bundles) != 1 || bundles[0] != dir {
		t.Fatalf("bundles = %v, want [%s]", bundles, dir)
	}

	// Keyword match should rank the refund policy first and carry its citations
	// plus a non-empty snippet drawn from the body.
	hits := st.query("refund approval", "", 8)
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
	if top.Bundle != dir {
		t.Errorf("top hit bundle = %q, want %q", top.Bundle, dir)
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

func TestKnowledgeQueryBundleFilter(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)
	if _, _, err := st.reindex([]string{dir}); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	// A non-matching bundle name filters everything out.
	if hits := st.query("refund", "/no/such/bundle", 8); len(hits) != 0 {
		t.Fatalf("bundle filter should drop all hits, got %d", len(hits))
	}
	// The real bundle name still matches.
	if hits := st.query("refund", dir, 8); len(hits) == 0 {
		t.Fatal("bundle filter dropped the matching bundle")
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

	qr, err := a.Query(plugin.QueryArgs{Query: "users table", Limit: 8})
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
