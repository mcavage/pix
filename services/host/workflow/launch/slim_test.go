// slim_test.go — U04f's structural guard on the lifecycle domain: the shapes
// the create/attach/teardown state machine must KEEP being, expressed against
// the AST rather than against a line count.
//
// Two rules, both regressions this package actually had:
//
//  1. ONE parser per sbx listing format. `sbx ls --json` was parsed by two
//     functions (session.go's sbxJSONEntry and reap.go's probeInstance) and the
//     plain `sbx ls` table by two more, so "what does a row mean" had four
//     places to disagree.
//  2. NO exported helper that only this package (or only its tests) calls. A
//     launcher-internal read/write pair does not become API by being spelled
//     with a capital letter, and every extra export is one more thing a future
//     caller can wire itself to.
package launch

import (
	"go/ast"
	"strings"
	"testing"
)

// TestLifecycle_OneParserPerSbxListing pins rule 1: exactly one function reads
// `sbx ls --json`, and exactly one classifies a plain `sbx ls` table row.
func TestLifecycle_OneParserPerSbxListing(t *testing.T) {
	_, files := parsePackageFiles(t)
	jsonParsers, rowParsers := map[string]bool{}, map[string]bool{}
	for name, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			calls := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, isIdent := sel.X.(*ast.Ident); isIdent {
					calls[pkg.Name+"."+sel.Sel.Name] = true
				}
				return true
			})
			if calls["sandbox.ParseList"] {
				jsonParsers[name+"."+fn.Name.Name] = true
			}
			// Splitting a listing into lines AND a line into columns is the
			// table-row read; either alone is ordinary string handling.
			if calls["strings.Split"] && calls["strings.Fields"] {
				rowParsers[name+"."+fn.Name.Name] = true
			}
		}
	}
	if len(jsonParsers) != 1 {
		t.Errorf("`sbx ls --json` must be parsed in exactly ONE place, found %d: %v", len(jsonParsers), keysOf(jsonParsers))
	}
	if len(rowParsers) != 1 {
		t.Errorf("an `sbx ls` table row must be classified in exactly ONE place, found %d: %v", len(rowParsers), keysOf(rowParsers))
	}
}

// TestLifecycle_NoPackageOnlyExports pins rule 2 by name: each of these was an
// export with no caller outside this package, and each is now either unexported
// or gone. Named rather than computed so a re-export is a compile-time-visible
// intent, not an accident.
func TestLifecycle_NoPackageOnlyExports(t *testing.T) {
	retired := []string{
		// launcher-internal lease/session state plumbing
		"LeaseRoot", "LeaseDirFor", "LifecycleIdentity", "SetSessionKeep",
		"WriteSessionFingerprint", "ReadSessionFingerprint",
		"WriteSessionInvocation", "ReadSessionInvocation",
		// teardown internals
		"OrphanCandidates", "SweepOrphans", "TeardownJournalPath",
		// test-only journal reader: a test reads the JSONL it pinned itself
		"ReadTeardownJournal",
	}
	assertNoTopLevelNames(t, retired)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// assertNoTopLevelNames fails for every name still declared at package scope in
// a production file (func, type, const or var).
func assertNoTopLevelNames(t *testing.T, names []string) {
	t.Helper()
	_, files := parsePackageFiles(t)
	banned := map[string]bool{}
	for _, n := range names {
		banned[n] = true
	}
	for file, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && banned[d.Name.Name] {
					t.Errorf("%s: %s is declared again; it had no caller outside this package", file, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					for _, id := range specNames(spec) {
						if banned[id] {
							t.Errorf("%s: %s is declared again; it had no caller outside this package", file, id)
						}
					}
				}
			}
		}
	}
}

func specNames(spec ast.Spec) []string {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return []string{s.Name.Name}
	case *ast.ValueSpec:
		var out []string
		for _, id := range s.Names {
			out = append(out, id.Name)
		}
		return out
	}
	return nil
}

// TestLifecycle_NoStoryArchaeologyInComments keeps the domain's comments about
// the CODE rather than about the tickets that produced it. The story ids live
// in git history and in docs/design; a comment that spends its lines narrating
// which story deleted what is the archaeology this slim removed, and it grows
// without bound because every future story adds a paragraph.
func TestLifecycle_NoStoryArchaeologyInComments(t *testing.T) {
	_, files := parsePackageFiles(t)
	for name, f := range files {
		for _, group := range f.Comments {
			for _, c := range group.List {
				for _, tag := range []string{"U04a", "U04b", "U04c", "U04c1", "U04c2", "U04d", "U04e", "U11g"} {
					if strings.Contains(c.Text, tag+":") || strings.Contains(c.Text, tag+" ") {
						t.Errorf("%s: comment narrates story %s (%q) — keep the WHY, drop the archaeology",
							name, tag, strings.TrimSpace(c.Text))
					}
				}
			}
		}
	}
}
