package okf

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeBundle materializes the SPEC example bundle plus a few tolerance cases
// on disk and returns the root dir.
func writeBundle(t *testing.T, withIndex bool) string {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"datasets/sales.md": `---
type: dataset
title: Sales
description: The sales dataset.
resource: https://example.com/sales
tags:
  - revenue
  - finance
timestamp: 2024-01-02T03:04:05Z
owner: data-team
freshness: daily
---
The sales dataset covers all closed deals.

See [orders](/tables/orders.md) and [customers](tables/customers.md).

# Citations
- https://example.com/spec
- Internal doc 42
`,
		"tables/orders.md": `---
type: table
title: Orders
---
Orders table. Links to [sales](/datasets/sales.md) and an [external](https://example.com).
`,
		// No frontmatter at all -> tolerated, empty type.
		"tables/customers.md": `Just a body, no frontmatter here.

Points at [orders](/tables/orders.md) and a [broken link](/tables/does-not-exist.md).
`,
	}
	if withIndex {
		files["index.md"] = `# Index

- [Sales](/datasets/sales.md)
- [Orders](/tables/orders.md)
`
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

func TestParseConcept(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		data  string
		check func(t *testing.T, c *Concept)
	}{
		{
			name: "required type and fields",
			path: "datasets/sales.md",
			data: "---\ntype: dataset\ntitle: Sales\n---\nbody\n",
			check: func(t *testing.T, c *Concept) {
				if c.Type != "dataset" {
					t.Errorf("type = %q, want dataset", c.Type)
				}
				if c.Title != "Sales" {
					t.Errorf("title = %q, want Sales", c.Title)
				}
				if c.ID != "datasets/sales" {
					t.Errorf("id = %q, want datasets/sales", c.ID)
				}
				if c.Path != "/datasets/sales.md" {
					t.Errorf("path = %q, want /datasets/sales.md", c.Path)
				}
			},
		},
		{
			name: "extras preserved",
			path: "x.md",
			data: "---\ntype: t\nowner: data-team\nfreshness: daily\n---\nb\n",
			check: func(t *testing.T, c *Concept) {
				if c.Extra["owner"] != "data-team" {
					t.Errorf("extra owner = %v", c.Extra["owner"])
				}
				if c.Extra["freshness"] != "daily" {
					t.Errorf("extra freshness = %v", c.Extra["freshness"])
				}
				if _, ok := c.Extra["type"]; ok {
					t.Errorf("known field type leaked into Extra")
				}
			},
		},
		{
			name: "tags slice",
			path: "x.md",
			data: "---\ntype: t\ntags:\n  - a\n  - b\n---\nbody\n",
			check: func(t *testing.T, c *Concept) {
				if !reflect.DeepEqual(c.Tags, []string{"a", "b"}) {
					t.Errorf("tags = %v, want [a b]", c.Tags)
				}
			},
		},
		{
			name: "citations and links extracted",
			path: "x.md",
			data: "---\ntype: t\n---\nText [a](/tables/a.md) and [ext](https://x.com).\n\n# Citations\n- src one\n- src two\n",
			check: func(t *testing.T, c *Concept) {
				if !reflect.DeepEqual(c.Links, []string{"/tables/a.md"}) {
					t.Errorf("links = %v, want [/tables/a.md]", c.Links)
				}
				if !reflect.DeepEqual(c.Citations, []string{"src one", "src two"}) {
					t.Errorf("citations = %v", c.Citations)
				}
			},
		},
		{
			name: "missing frontmatter tolerated",
			path: "tables/customers.md",
			data: "Just a body.\n\n[link](/tables/orders.md)\n",
			check: func(t *testing.T, c *Concept) {
				if c.Type != "" {
					t.Errorf("type = %q, want empty", c.Type)
				}
				if c.ID != "tables/customers" {
					t.Errorf("id = %q", c.ID)
				}
				if c.Body == "" {
					t.Errorf("body should be the whole file")
				}
				if !reflect.DeepEqual(c.Links, []string{"/tables/orders.md"}) {
					t.Errorf("links = %v", c.Links)
				}
			},
		},
		{
			name: "crlf frontmatter",
			path: "x.md",
			data: "---\r\ntype: t\r\ntitle: T\r\n---\r\nbody line\r\n",
			check: func(t *testing.T, c *Concept) {
				if c.Type != "t" || c.Title != "T" {
					t.Errorf("type=%q title=%q", c.Type, c.Title)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseConcept(tt.path, []byte(tt.data))
			if err != nil {
				t.Fatalf("ParseConcept: %v", err)
			}
			if c == nil {
				t.Fatal("nil concept")
			}
			tt.check(t, c)
		})
	}
}

func TestReadBundle(t *testing.T) {
	dir := writeBundle(t, true)
	b, err := ReadBundle(dir)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	concepts := b.Concepts()
	if len(concepts) != 3 {
		t.Fatalf("got %d concepts, want 3: %v", len(concepts), conceptIDs(concepts))
	}

	// ID derivation + keyed lookup.
	sales := b.Concept("datasets/sales")
	if sales == nil {
		t.Fatal("missing datasets/sales concept")
	}
	if sales.Type != "dataset" {
		t.Errorf("sales type = %q", sales.Type)
	}
	// Extras preserved.
	if sales.Extra["owner"] != "data-team" {
		t.Errorf("sales owner extra = %v", sales.Extra["owner"])
	}
	// Citations extracted.
	want := []string{"https://example.com/spec", "Internal doc 42"}
	if !reflect.DeepEqual(sales.Citations, want) {
		t.Errorf("sales citations = %v, want %v", sales.Citations, want)
	}
	// Links: internal ones kept, external dropped.
	wantLinks := []string{"/tables/orders.md", "tables/customers.md"}
	if !reflect.DeepEqual(sales.Links, wantLinks) {
		t.Errorf("sales links = %v, want %v", sales.Links, wantLinks)
	}

	// Missing-frontmatter concept.
	cust := b.Concept("tables/customers")
	if cust == nil {
		t.Fatal("missing tables/customers")
	}
	if cust.Type != "" {
		t.Errorf("customers type = %q, want empty", cust.Type)
	}
	// Broken link is still recorded (permissive).
	if !contains(cust.Links, "/tables/does-not-exist.md") {
		t.Errorf("broken link not recorded: %v", cust.Links)
	}

	// index.md captured, not a concept.
	if b.Index() == "" {
		t.Error("expected index body")
	}
	if b.Concept("index") != nil {
		t.Error("index.md should not be a concept")
	}
}

func TestReadBundleMissingIndexTolerated(t *testing.T) {
	dir := writeBundle(t, false)
	b, err := ReadBundle(dir)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}
	if b.Index() != "" {
		t.Errorf("expected empty index, got %q", b.Index())
	}
	if len(b.Concepts()) != 3 {
		t.Errorf("got %d concepts, want 3", len(b.Concepts()))
	}
}

// TestReadBundleSkipsSymlinks proves F-D: a symlink inside the bundle pointing at
// a file OUTSIDE the bundle tree is NOT read or indexed (os.ReadFile would
// otherwise follow the link and leak the outside file's contents as a concept).
func TestReadBundleSkipsSymlinks(t *testing.T) {
	dir := writeBundle(t, false)

	// A secret file OUTSIDE the bundle tree.
	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("---\ntype: secret\ntitle: leaked\n---\nBEGIN PRIVATE KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the bundle, named like a real concept, pointing at it.
	link := filepath.Join(dir, "secrets.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	b, err := ReadBundle(dir)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}

	// The symlinked file must NOT be indexed as a concept.
	if c := b.Concept("secrets"); c != nil {
		t.Fatalf("symlinked file was indexed as a concept: %+v", c)
	}
	// Only the 3 real files remain.
	if n := len(b.Concepts()); n != 3 {
		t.Errorf("got %d concepts, want 3 (symlink must be skipped): %v", n, conceptIDs(b.Concepts()))
	}
	// The skip is recorded as a warning.
	var warned bool
	for _, w := range b.Warnings {
		if strings.Contains(w, "symlink") && strings.Contains(w, "secrets.md") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a skip-symlink warning, got %v", b.Warnings)
	}
}

func conceptIDs(cs []*Concept) []string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	return ids
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
