package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeFixture builds a real two-package module on disk (no mocks, no
// go/types stubs) and returns its root.
func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n\ngo 1.26\n"), 0o644))

	must(os.MkdirAll(filepath.Join(root, "alpha"), 0o755))
	must(os.WriteFile(filepath.Join(root, "alpha", "alpha.go"), []byte(`package alpha

import "os"

// Used is called from beta; UnusedExport never is.
func Used() int { return 1 }

func UnusedExport() int { return 2 }

var Global1 = 1
var global2 = 2

func init() {}

func exit() {
	if Used() == 0 {
		os.Exit(1)
	}
}
`), 0o644))
	must(os.WriteFile(filepath.Join(root, "alpha", "alpha_cmd.go"), []byte(`package alpha

func Run() {}
`), 0o644))
	must(os.WriteFile(filepath.Join(root, "alpha", "alpha_test.go"), []byte(`package alpha

import "testing"

func TestAlpha(t *testing.T) {
	if Used() != 1 {
		t.Fatal("bad")
	}
}
`), 0o644))

	must(os.MkdirAll(filepath.Join(root, "beta"), 0o755))
	must(os.WriteFile(filepath.Join(root, "beta", "beta.go"), []byte(`package beta

import (
	"fixture/alpha"
)

func Call() int {
	go func() {}()
	ch := make(chan int)
	_ = ch
	return alpha.Used()
}
`), 0o644))

	return root
}

func TestScan_CountsLOCExportsGlobalsExitsEdges(t *testing.T) {
	root := writeFixture(t)
	report, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}

	a, ok := report.Packages["fixture/alpha"]
	if !ok {
		t.Fatalf("fixture/alpha not found in %v", report.Packages)
	}
	if a.Exports != 4 { // Used, UnusedExport, Global1, Run
		t.Errorf("alpha exports = %d, want 4 (Used, UnusedExport, Global1, Run)", a.Exports)
	}
	if a.ExportsUsedExternally != 1 { // only Used is called from beta
		t.Errorf("alpha exports_used_externally = %d, want 1", a.ExportsUsedExternally)
	}
	if a.Globals != 2 { // Global1 + global2
		t.Errorf("alpha globals = %d, want 2", a.Globals)
	}
	if a.Init != 1 {
		t.Errorf("alpha init = %d, want 1", a.Init)
	}
	if a.Exits != 1 {
		t.Errorf("alpha exits = %d, want 1", a.Exits)
	}
	if a.ParserFamilies["cmd"] != 1 || a.ParserFamilies["core"] != 1 || a.ParserFamilies["test"] != 1 {
		t.Errorf("alpha parser_families = %v, want cmd:1 core:1 test:1", a.ParserFamilies)
	}
	if a.TestLOC == 0 {
		t.Errorf("alpha test_loc = 0, want > 0")
	}
	if a.ProdLOC == 0 {
		t.Errorf("alpha prod_loc = 0, want > 0")
	}

	b, ok := report.Packages["fixture/beta"]
	if !ok {
		t.Fatalf("fixture/beta not found in %v", report.Packages)
	}
	if b.Edges != 1 {
		t.Errorf("beta edges = %d, want 1 (imports fixture/alpha)", b.Edges)
	}
	if b.Streams != 2 { // one GoStmt + one ChanType
		t.Errorf("beta streams = %d, want 2", b.Streams)
	}
}

func TestCountLines(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 2}, // no trailing newline still counts the last line
	}
	for _, c := range cases {
		if got := countLines([]byte(c.in)); got != c.want {
			t.Errorf("countLines(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCheckBudgets_ShrinkOnly(t *testing.T) {
	root := writeFixture(t)
	report, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}

	budgetsPath := filepath.Join(t.TempDir(), "budgets.json")

	// Absent budgets file: no violations (a package never recorded cannot fail).
	violations, err := checkBudgets(report, budgetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected no violations against a missing budgets file, got %v", violations)
	}

	// Seed the budgets file from the current metrics: must pass immediately —
	// this is the "does not make the current baseline fail" requirement.
	if err := writeBudgetsFile(report, budgetsPath); err != nil {
		t.Fatal(err)
	}
	violations, err = checkBudgets(report, budgetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected no violations right after seeding, got %v", violations)
	}

	// Now write a budgets file with a ceiling BELOW the current value: the
	// check must fail loud and name the package + field.
	tight := Budgets{Schema: 1, Packages: map[string]Budget{
		"fixture/alpha": {ProdLOC: 1, Exports: 1, Globals: 0, Edges: 0, Exits: 0},
	}}
	writeJSON(t, budgetsPath, tight)
	violations, err = checkBudgets(report, budgetsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) == 0 {
		t.Fatal("expected violations against a tighter-than-current budget, got none")
	}
}

func TestWriteBudgetsFile_RefusesToRaiseCeiling(t *testing.T) {
	root := writeFixture(t)
	report, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	budgetsPath := filepath.Join(t.TempDir(), "budgets.json")

	// Seed a budget stricter than the current tree (simulating a prior commit
	// that shrank the code further than now).
	strict := Budgets{Schema: 1, Packages: map[string]Budget{
		"fixture/alpha": {ProdLOC: 1, Exports: 1, Globals: 0, Edges: 0, Exits: 0},
	}}
	writeJSON(t, budgetsPath, strict)

	if err := writeBudgetsFile(report, budgetsPath); err == nil {
		t.Fatal("expected writeBudgetsFile to refuse raising an existing ceiling, got nil error")
	}
}

func TestWriteBudgetsFile_RatchetsDownOnShrink(t *testing.T) {
	root := writeFixture(t)
	report, err := scan(root)
	if err != nil {
		t.Fatal(err)
	}
	budgetsPath := filepath.Join(t.TempDir(), "budgets.json")

	// Seed a LOOSER budget than current: writeBudgetsFile must ratchet it down
	// to the current (smaller) value, never leave slack sitting in the file.
	loose := Budgets{Schema: 1, Packages: map[string]Budget{
		"fixture/alpha": {ProdLOC: 999, Exports: 999, Globals: 999, Edges: 999, Exits: 999},
	}}
	writeJSON(t, budgetsPath, loose)

	if err := writeBudgetsFile(report, budgetsPath); err != nil {
		t.Fatal(err)
	}
	got, err := loadBudgets(budgetsPath)
	if err != nil {
		t.Fatal(err)
	}
	alpha := got.Packages["fixture/alpha"]
	current := toBudget(report.Packages["fixture/alpha"])
	if alpha != current {
		t.Errorf("ratcheted budget = %+v, want it pulled down to current %+v", alpha, current)
	}
}

func TestParseCoverage(t *testing.T) {
	log := filepath.Join(t.TempDir(), "cov.log")
	content := "ok  \tfixture/alpha\t0.010s\tcoverage: 82.3% of statements\n" +
		"?   \tfixture/beta\t[no test files]\n"
	if err := os.WriteFile(log, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cov, err := parseCoverage(log)
	if err != nil {
		t.Fatal(err)
	}
	if cov["fixture/alpha"] != 82.3 {
		t.Errorf("fixture/alpha coverage = %v, want 82.3", cov["fixture/alpha"])
	}
	if _, ok := cov["fixture/beta"]; ok {
		t.Errorf("fixture/beta has no test files and must be ABSENT, not zero")
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
