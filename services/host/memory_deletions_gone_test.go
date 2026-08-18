// memory_deletions_gone_test.go — the sentinel for U1-delete-go: a durable,
// regression-proof assertion that the host-side memory concepts deleted in
// this unit (watcher `events`, watcher valence and the reward it seeded,
// reward itself as write-path input, the legacy watcher-perishable TTL
// migration, and perishable/TTL production behavior) cannot silently come
// back.
//
// Two complementary techniques, mirroring cmd/pix/hostmode_gone_test.go: (1)
// reflection over the struct shapes involved (watchResult, rememberInput,
// plugin.RememberReq) — precise and immune to gofmt column-alignment noise,
// checking a required/banned field explicitly rather than an exact
// field-set match, so an unrelated field added later doesn't also have to
// fail; (2) a grep-based walk (LOCAL to this package, same "each package
// writes its own copy" convention) for a small set of literal,
// behavior-specific strings reflection can't see: a deleted function's
// declaration, the exact TTL-expiry SQL predicate, and the exact
// perishable-durability behavioral gate.
//
// Deliberately NOT flagged, so this stays precise instead of noisy: the
// `reward` column stays in the schema (still gettable by direct SQL, only
// its WRITE-PATH presence is banned here); a live db can still hold LEGACY
// perishable rows across the exact startup that retires them (a one-time
// SOFT delete folded into migrateMemorySchema, not the deleted PER-CALL
// sweep this file's grep guards against); and historical prose in
// comments/docs naming a deleted symbol is fine, since the grep half only
// walks non-test .go files. plugin.Hit, scoredHit, and memRow no longer
// carry a Durability field at all — the read side finished the U9
// retirement described below; that absence is reflection-checked, not
// grepped, since grep can't see a field that was removed.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"pix/host/plugin"
)

// fieldNames returns the sorted field names (exported and unexported alike —
// reflect.Type.Field sees both, only .Interface()/.Set() are restricted) of a
// struct type. Using reflection instead of a source-text check means this
// keeps working across any gofmt re-alignment of the struct's declaration.
func fieldNames(v any) []string {
	t := reflect.TypeOf(v)
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		names = append(names, t.Field(i).Name)
	}
	sort.Strings(names)
	return names
}

// TestWatchResultHasFactsAndCorrectionsButNotEventsOrValence proves the
// watcher's Facts/Corrections fields are still there and its Events
// (time-bound status) and Valence (sentiment) fields are gone, not just
// unused. This checks required/banned fields individually rather than the
// type's exact field set, so a legitimate future addition to watchResult
// (an unrelated new field) does not also have to fail this test to be valid.
func TestWatchResultHasFactsAndCorrectionsButNotEventsOrValence(t *testing.T) {
	got := fieldNames(watchResult{})
	for _, want := range []string{"Corrections", "Facts"} {
		if !hasField(got, want) {
			t.Errorf("watchResult is missing %q — got fields: %v", want, got)
		}
	}
	for _, banned := range []string{"Events", "Valence"} {
		if hasField(got, banned) {
			t.Errorf("watchResult has a %q field; Events (watcher time-bound status) and Valence (watcher sentiment) were deleted from the watcher's prompt, parse, and capture path — got fields: %v", banned, got)
		}
	}
}

// hasField reports whether names contains name.
func hasField(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// TestRememberInputDroppedDurabilityTTLAndReward proves the WRITE-side
// perishable/TTL/reward knobs (durability, ttlDays, reward) are all gone from
// rememberInput: every row this binary writes is now durable, with no
// expiry, and reward is no longer caller-configurable input at all (the
// column stays in the schema, inert, defaulting to 0).
func TestRememberInputDroppedDurabilityTTLAndReward(t *testing.T) {
	got := fieldNames(rememberInput{})
	for _, banned := range []string{"durability", "ttlDays", "reward"} {
		if hasField(got, banned) {
			t.Errorf("rememberInput has a %q field; durability/TTL/reward were deleted as write-path input — got fields: %v", banned, got)
		}
	}
}

// TestRememberReqDroppedDurabilityTTLAndReward is
// TestRememberInputDroppedDurabilityTTLAndReward's sibling over the exported
// plugin wire struct (plugin.RememberReq), which memory_plugin.go and
// serve_plugin.go both build from rememberInput.
func TestRememberReqDroppedDurabilityTTLAndReward(t *testing.T) {
	got := fieldNames(plugin.RememberReq{})
	for _, banned := range []string{"Durability", "TTLDays", "Reward"} {
		if hasField(got, banned) {
			t.Errorf("plugin.RememberReq has a %q field; durability/TTL/reward were deleted as write-path input — got fields: %v", banned, got)
		}
	}
}

// TestHitDroppedDurabilityReadSide proves the U9 schema retirement finished
// the job memory_legacy_behavior_test.go started: plugin.Hit, scoredHit
// (which feeds it), and memRow (the row this package scans out of SQL before
// scoring) no longer carry a Durability field at all. No reader — not the
// sandbox extension, not the host CLI — ever consumed it after U4, so the
// read thread is gone end-to-end; only the DB column and its "durable"
// INSERT literal remain, for on-disk compatibility.
func TestHitDroppedDurabilityReadSide(t *testing.T) {
	for name, v := range map[string]any{
		"plugin.Hit": plugin.Hit{},
		"scoredHit":  scoredHit{},
		"memRow":     memRow{},
	} {
		if hasField(fieldNames(v), "Durability") {
			t.Fatalf("%s still has a Durability field; the U9 schema retirement deleted this read thread end-to-end — only the DB column and its INSERT literal survive", name)
		}
	}
}

// forbiddenMemorySymbols are literal, behavior-specific strings that only
// ever existed to implement the deleted legacy-TTL migration or the
// perishable/TTL production behavior. None of them can appear in a non-test
// .go file under this module without one of those behaviors having come
// back. Each is chosen to be unambiguous: a function declaration (fixed,
// gofmt-stable formatting), an exact SQL predicate, and an exact behavioral
// gate — never a bare identifier like "Durability" or "TTLDays", both of
// which legitimately still appear elsewhere in lowercase, inert form:
// memory.go's CREATE TABLE schema still declares a `durability` column,
// remember() still writes the fixed literal `const durability = "durable"`
// into it, and this unit's own explanatory comments name the column in
// prose — so a bare-word check would flag the very things this test file
// exists to protect.
var forbiddenMemorySymbols = []string{
	// migrateLegacyWatcherPerishableTTL (and its test) were deleted outright.
	"func migrateLegacyWatcherPerishableTTL(",
	// The TTL-expiry sweep recall() used to run before every call ("no
	// background deletion" now, and no expiry to sweep).
	"expires_at IS NOT NULL AND expires_at < ? AND deleted_at IS NULL",
	// The exact gate that turned a caller-supplied durability into TTL/expiry
	// behavior in remember().
	`if durability == "perishable"`,
	// The periodic-synthesis-ticker's env knob and the on-demand synthesize
	// JSON-RPC/plugin surface it fed were both deleted outright (U9): no caller
	// ever invoked "synthesize" outside this module's own tests.
	"MEMORY_SYNTH_MS",
	"func (s *memStore) synthesize(",
}

// memoryModuleRoot resolves the services/host module root: this test file
// lives there directly (package main, not a subpackage), so it is simply the
// absolute form of ".".
func memoryModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("resolved %s does not look like the services/host module root: %v", root, err)
	}
	return root
}

// memorySymbolViolations walks root for non-test .go files containing any of
// symbols, returning one "relpath: symbol" string per hit. Pure — no
// *testing.T — so TestMemorySymbolSentinelDetectsAPlantedViolation below can
// prove it actually fires on a planted violation, not just that it has only
// ever been observed passing.
func memorySymbolViolations(root string, symbols []string) ([]string, error) {
	var violations []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		content := string(b)
		for _, sym := range symbols {
			if strings.Contains(content, sym) {
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, fmt.Sprintf("%s: %s", rel, sym))
			}
		}
		return nil
	})
	return violations, err
}

// TestNoLegacyTTLOrPerishableProductionSymbols greps every non-test .go file
// under services/host for the deleted TTL-migration/perishable symbols. This
// is the sentinel: it fails loudly the moment any of them is reintroduced,
// wherever in the module that happens.
func TestNoLegacyTTLOrPerishableProductionSymbols(t *testing.T) {
	root := memoryModuleRoot(t)
	violations, err := memorySymbolViolations(root, forbiddenMemorySymbols)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, v := range violations {
		t.Errorf("%s — the legacy watcher-perishable TTL migration and perishable/TTL production behavior were deleted (U1-delete-go); this must not come back", v)
	}
}

// TestMemorySymbolSentinelDetectsAPlantedViolation is the plausibility
// assertion for the sentinel above: it plants one forbidden symbol into a
// throwaway tree and proves memorySymbolViolations actually reports it, then
// proves the SAME symbol in a _test.go file is correctly ignored — so a
// passing TestNoLegacyTTLOrPerishableProductionSymbols is evidence the code
// is clean, not evidence the check forgot how to fail.
func TestMemorySymbolSentinelDetectsAPlantedViolation(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "planted.go")
	if err := os.WriteFile(planted, []byte("package x\n\nfunc migrateLegacyWatcherPerishableTTL() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	violations, err := memorySymbolViolations(dir, forbiddenMemorySymbols)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("expected memorySymbolViolations to catch the planted func migrateLegacyWatcherPerishableTTL(), but it found nothing — a sentinel that never fires has never been proven to work")
	}

	// The exact same symbol in a _test.go file (e.g. this file's own docstring,
	// or memory_schema_version_test.go's header, both of which name the deleted
	// function) must NOT be reported.
	testFile := filepath.Join(dir, "planted_test.go")
	if err := os.WriteFile(testFile, []byte("package x\n\nfunc migrateLegacyWatcherPerishableTTL() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(planted) // isolate: only the _test.go file remains
	isolated, err := memorySymbolViolations(dir, forbiddenMemorySymbols)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated) != 0 {
		t.Fatalf("memorySymbolViolations must ignore _test.go files, got: %v", isolated)
	}
}
