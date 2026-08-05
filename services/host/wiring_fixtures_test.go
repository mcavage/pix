package main

// wiring_fixtures_test.go plants positive and negative fixture packages
// (testdata/wiring/positive, testdata/wiring/negative — a literal "testdata"
// directory, so neither is a package the real architecture guard, `go
// build ./...`, or `go vet ./...` ever compiles or scans) and points
// funcValuedGlobalHits directly at them. This is the proof that the detector
// itself — not just "today's zero known instances" — catches every shape the
// task calls out (a function literal, an identifier bound to a func, a
// selector resolving to a func) and allows every shape it must (time.Second,
// io.Discard, a same-package data selector, a cross-package data selector,
// ordinary construction via a CallExpr).

import (
	"sort"
	"strings"
	"testing"
)

// TestFuncValuedGlobalHits_PositiveFixtures proves no false NEGATIVE: every
// var in the planted positive fixture is a function-valued seam, and every
// one of them must be reported.
func TestFuncValuedGlobalHits_PositiveFixtures(t *testing.T) {
	dir := "testdata/wiring/positive"
	want := []string{
		"FuncLiteral",
		"VarBoundToFunc",
		"VarBoundToSelectorFunc",
		"CliSelectorFunc",
		"ExplicitFuncTypeVar",
		"NamedFuncTypeVar",
		"ChainedVarBoundToFunc",
	}
	got := hitNames(t, dir)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("funcValuedGlobalHits(%s):\n  got:  %v\n  want: %v", dir, got, want)
	}
}

// TestFuncValuedGlobalHits_NegativeFixtures proves no false POSITIVE: none of
// the vars in the planted negative fixture are function-valued, so none of
// them may be reported — this is the regression test for the original guard,
// which flagged time.Second, io.Discard and every other bare selector
// regardless of what it resolved to.
func TestFuncValuedGlobalHits_NegativeFixtures(t *testing.T) {
	dir := "testdata/wiring/negative"
	if got := hitNames(t, dir); len(got) != 0 {
		t.Errorf("funcValuedGlobalHits(%s) = %v, want none — every var here is data, not a seam", dir, got)
	}
}

// hitNames runs funcValuedGlobalHits and returns just the sorted var names
// (dropping "file:line: " so the assertions above read as plain name lists).
func hitNames(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	for _, hit := range funcValuedGlobalHits(t, dir) {
		if i := strings.LastIndex(hit, " "); i >= 0 {
			names = append(names, hit[i+1:])
		}
	}
	sort.Strings(names)
	return names
}
