package main

// processboundary_fixtures_test.go plants positive and negative fixture
// packages (testdata/processboundary/positive, .../negative — literal
// "testdata" directories, so neither is ever compiled or vetted by `go build
// ./...`, `go vet ./...`, or `go test -race ./...`, all of which expand
// "./..." and skip that name) and points processBoundaryHits directly at
// them. This is the proof that the detector itself — not just "today's
// real packages happen to be clean" — catches every bypass this guard was
// rewritten to close (an aliased os selector, log.Fatal/Fatalf/Fatalln
// through both a plain and an aliased log import, and the two builtins
// print/println) and allows every lookalike that must not be flagged
// (an unrelated type's same-named method, a shadowed "os" parameter, a
// package that aliases itself to "log" without BEING the log package, a
// locally shadowed print/println, an unrelated bare Fatal/Exit func, and
// the log/fmt calls this guard has always allowed).

import (
	"sort"
	"strings"
	"testing"
)

// TestProcessBoundaryHits_PositiveFixtures proves no false NEGATIVE: every
// bypass planted in the positive fixture is reported, including the two
// spellings (plain and aliased) of log.Fatal.
func TestProcessBoundaryHits_PositiveFixtures(t *testing.T) {
	dir := "testdata/processboundary/positive"
	want := []string{
		"os.Exit",                         // AliasedOsExit, via o.Exit
		"os.Stdout",                       // AliasedOsStdout, via o.Stdout
		"os.Stderr",                       // AliasedOsStderr, via o.Stderr
		"log.Fatal -> os.Exit",            // AliasedLogFatal, via l.Fatal
		"log.Fatalf -> os.Exit",           // AliasedLogFatalf, via l.Fatalf
		"log.Fatalln -> os.Exit",          // AliasedLogFatalln, via l.Fatalln
		"log.Fatal -> os.Exit",            // PlainLogFatal, via log.Fatal (no alias)
		"os.Stderr (via builtin print)",   // BuiltinPrint
		"os.Stderr (via builtin println)", // BuiltinPrintln
	}
	got := hitWhats(t, dir)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("processBoundaryHits(%s):\n  got:  %v\n  want: %v", dir, got, want)
	}
}

// TestProcessBoundaryHits_NegativeFixtures proves no false POSITIVE: every
// lookalike planted in the negative fixture — an unrelated type reusing a
// banned method name, a shadowed "os" parameter, a package that merely
// calls itself "log" at an import alias, a locally shadowed print/println,
// an unrelated bare Fatal/Exit func, and the log/fmt calls this guard has
// always allowed — must produce ZERO hits.
func TestProcessBoundaryHits_NegativeFixtures(t *testing.T) {
	dir := "testdata/processboundary/negative"
	if got := hitWhats(t, dir); len(got) != 0 {
		t.Errorf("processBoundaryHits(%s) = %v, want none — every reference here is a lookalike, not a real bypass", dir, got)
	}
}

// hitWhats runs processBoundaryHits and returns just the sorted "what" half
// of each "file:line: what" hit, so the assertions above read as plain
// content lists rather than caring which line planted them.
func hitWhats(t *testing.T, dir string) []string {
	t.Helper()
	var whats []string
	for _, hit := range processBoundaryHits(t, dir) {
		_, what, ok := strings.Cut(hit, ": ")
		if !ok {
			t.Fatalf("unparseable hit %q", hit)
		}
		whats = append(whats, what)
	}
	sort.Strings(whats)
	return whats
}
