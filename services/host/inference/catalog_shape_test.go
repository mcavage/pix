package inference

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// forbiddenCatalogSubstrings are the shapes a SCORED router catalog carries
// that a literal model fact never may: a price, a measured or seeded accuracy,
// a score, a policy/intent/preference, or a resolver's ranking. F12
// (architecture.md), same rule hardware_shape_test.go applies to the local
// rung table: the catalog Model must never grow one of these fields again, one
// field at a time, and bring the deleted router back with it.
//
// "local hardware" is banned here for the other half of E4.3: RAM, download
// size and KV cost live exactly once, in hardware.go's rung table. A second
// copy on the catalog row is a duplicate that will disagree.
var forbiddenCatalogSubstrings = []string{
	"price", "cost", "usd", "mtok", "pricing",
	"accuracy", "score", "benchmark", "rank", "recommend",
	"intent", "routing", "route", "objective", "policy", "prefer", "fallback", "why",
	"ram", "download", "kv",
}

// TestCatalogModelShapeHasNoScoredField reflects over Model's field names AND
// its JSON tags (case-insensitively): a scored field renamed in Go but still
// spelled `input_per_mtok` on the wire is the same regression.
func TestCatalogModelShapeHasNoScoredField(t *testing.T) {
	typ := reflect.TypeOf(Model{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		for _, spelling := range []string{f.Name, f.Tag.Get("json")} {
			lower := strings.ToLower(spelling)
			for _, bad := range forbiddenCatalogSubstrings {
				if strings.Contains(lower, bad) {
					t.Errorf("Model.%s (json %q) contains forbidden substring %q: the catalog carries LITERAL model facts only — no price, score, policy, ranking, or local-hardware fact (those live in hardware.go's rung table)", f.Name, f.Tag.Get("json"), bad)
				}
			}
		}
	}
}

// TestShippedCatalogJSONHasNoScoredKey is the data-level companion: the struct
// can be clean while the shipped JSON still carries the scored keys, waiting
// for a field to be added back that reads them.
func TestShippedCatalogJSONHasNoScoredKey(t *testing.T) {
	raw, err := catalogDefaults.ReadFile("catalog/models.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Models) == 0 {
		t.Fatal("the shipped catalog is empty")
	}
	for _, m := range doc.Models {
		for key := range m {
			lower := strings.ToLower(key)
			for _, bad := range forbiddenCatalogSubstrings {
				if strings.Contains(lower, bad) {
					t.Errorf("shipped catalog row carries forbidden key %q (matched %q)", key, bad)
				}
			}
		}
	}
}

// TestShippedCatalogIsLiteralAndValid proves the file is real facts and not a
// hollow fixture that passes the two bans above for the wrong reason.
func TestShippedCatalogIsLiteralAndValid(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog() error = %v", err)
	}
	if err := ValidateCatalog(c); err != nil {
		t.Fatalf("ValidateCatalog() error = %v", err)
	}
	for _, m := range c.Models {
		if m.Provider == "" || m.Label == "" {
			t.Errorf("catalog row %+v is missing an identity fact", m)
		}
	}
}

// TestCatalogNeverDuplicatesLocalHardwareFacts pins E4.3's table as the ONE
// home for local hardware: every local catalog row must resolve to a rung, and
// the rung is where its RAM/download/KV facts come from.
func TestCatalogNeverDuplicatesLocalHardwareFacts(t *testing.T) {
	c, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Models {
		if !m.Local || !m.Available {
			continue
		}
		rung, ok := LocalOllamaRungFor(m.ID)
		if !ok {
			t.Errorf("local catalog model %q has no rung in hardware.go's table; the RAM/download facts it needs have no home", m.ID)
			continue
		}
		if rung.ContextWindow != m.ContextWindow {
			t.Errorf("rung %q declares context %d but the catalog says %d; the rung table is canonical for the RAM-budgeted context", m.ID, rung.ContextWindow, m.ContextWindow)
		}
	}
}
