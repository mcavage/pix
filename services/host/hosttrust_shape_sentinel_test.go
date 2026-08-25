package main

// hosttrust_shape_sentinel_test.go (F6/E1.4) is the structural fitness
// function proving the claim hosttrust's own doc.go/store.go comments make —
// "Record (the ONE acceptance-record shape every subject kind uses)" and
// "AcceptanceStore is a Subject-keyed map of Record" — stays true as code
// changes, not merely as a claim in a comment nobody re-checks. A pack's
// acceptance record and a future native environment's (Story E1) are
// supposed to be the SAME shape, reused through hosttrust, never grown a
// second time in whatever package needs one next.
//
// Precision matters here specifically because a NAME-based grep (like
// hostmode_gone_test.go's forbiddenSymbolViolations, which is deliberately
// blunt for retired execution symbols) would misfire on this shape in both
// directions:
//   - false positive: workflow/pack declares packActivationRecord, whose
//     name contains "Record" but whose fields (Owner/Path/MCP/
//     OllamaBridgeModel/PriorOllamaBridgeModel) have nothing to do with
//     acceptance; a name grep for "Record" would flag it for no reason.
//   - false negative: a reintroduced parallel acceptance-record struct need
//     not be named "Record" or contain that substring at all — the actual
//     risk is a second STRUCT with the same FIELD SHAPE hosttrust.Record
//     has, under any name.
//
// So this sentinel is shape-based, not name-based: it parses every non-test
// .go file outside hosttrust/ into an AST and flags a REAL (non-alias)
// struct type whose fields match hosttrust.Record's signature — a string
// field named Fingerprint plus at least one of Path/Remote/Commit also
// typed string. `type PackTrustRecord = hosttrust.Record` is a type ALIAS
// (`type X = Y`, not `type X Y`), so its RHS is a *ast.SelectorExpr, never a
// *ast.StructType this walk inspects at all — the exemption falls out of
// what an alias IS, not a name carved out for it. The same shape check,
// applied to a struct's map-valued fields, catches a second STORE
// implementation that pairs its own duplicate record type with an
// Accepted-style map, the "or store" half of the finding; holding
// `map[string]hosttrust.Record` (or an alias of it) directly — exactly what
// workflow/pack.PackTrustStore does, per hosttrust's own doc.go note that
// this is the sanctioned reuse path — never appears as a locally-declared
// struct type at all, so it can never trip either check.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// recordShapeFieldNames are hosttrust.Record's own field names (store.go):
// Path, Remote, Commit, Fingerprint, all typed string. fingerprintField is
// the field this shape treats as load-bearing — every acceptance record
// carries SOME fingerprint — combined with at least one provenance field
// also present, so an unrelated struct that merely happens to have an
// unrelated string field named "Fingerprint" alone (with no Path/Remote/
// Commit alongside it) is not enough to match.
const fingerprintField = "Fingerprint"

var provenanceFields = []string{"Path", "Remote", "Commit"}

// structFieldTypes returns name -> source-text-of-type for every field st
// declares, expanding a multi-name field (`A, B string`) to one entry per
// name. Embedded fields (no Names) are skipped: this shape check only cares
// about named string fields, and an embedded type is never itself named
// "Fingerprint".
func structFieldTypes(fset *token.FileSet, st *ast.StructType) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	if st.Fields == nil {
		return out
	}
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			out[n.Name] = f.Type
		}
	}
	return out
}

// isIdentString reports whether e is the bare identifier `string`.
func isIdentString(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "string"
}

// matchesRecordShape reports whether fields matches hosttrust.Record's
// signature: a string Fingerprint plus at least one string field among
// Path/Remote/Commit. Extra fields beyond Record's four are still a match —
// the point is "carries the same acceptance-record facts", not "has
// exactly, only, these four fields and no others".
func matchesRecordShape(fields map[string]ast.Expr) bool {
	fp, ok := fields[fingerprintField]
	if !ok || !isIdentString(fp) {
		return false
	}
	for _, name := range provenanceFields {
		if t, ok := fields[name]; ok && isIdentString(t) {
			return true
		}
	}
	return false
}

// packageStructs holds every non-alias struct type this walk found in one
// package directory, keyed by type name, so the map-field (store-shape)
// pass below can resolve a bare identifier type to the struct it names
// within the SAME package — exactly how Go itself resolves it.
type packageStructs map[string]*ast.StructType

// shapeViolation is one sentinel finding: the file/type that tripped it and
// which half of the finding (record vs store) it is.
type shapeViolation struct {
	file string
	typ  string
	kind string // "record" | "store"
}

func (v shapeViolation) String() string {
	return fmt.Sprintf("%s: type %s (%s shape)", v.file, v.typ, v.kind)
}

// scanTrustShapeViolations walks root for non-test .go files (skipping
// hosttrust itself, the canonical owner) and returns every second
// acceptance-record or acceptance-store shape it finds, grouped by package
// directory so the store-shape pass can resolve a package-local type name.
func scanTrustShapeViolations(t *testing.T, root string) []shapeViolation {
	t.Helper()
	fset := token.NewFileSet()

	// dirFiles: package directory -> relpath -> *ast.File, gathered first so
	// both passes (record shape, then store shape) see the WHOLE package,
	// not just one file at a time — a struct and the store that references
	// it are free to live in different files of the same package, as
	// workflow/pack's own truststore.go/pack.go split already does for the
	// legitimate case.
	type parsedFile struct {
		rel  string
		file *ast.File
	}
	dirFiles := map[string][]parsedFile{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "corpus", "hosttrust":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.Dir(rel)
		dirFiles[dir] = append(dirFiles[dir], parsedFile{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	var violations []shapeViolation
	for dir, files := range dirFiles {
		structs := packageStructs{}
		fileOf := map[string]string{} // type name -> relpath it was declared in
		var recordLike []struct {
			name, file string
		}

		for _, pf := range files {
			for _, decl := range pf.file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if ts.Assign != token.NoPos {
						// `type X = Y`: an alias, not a new struct — e.g.
						// PackTrustRecord = hosttrust.Record. Never a
						// candidate for either shape check below.
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					structs[ts.Name.Name] = st
					fileOf[ts.Name.Name] = pf.rel
					if matchesRecordShape(structFieldTypes(fset, st)) {
						recordLike = append(recordLike, struct{ name, file string }{ts.Name.Name, pf.rel})
					}
				}
			}
		}

		for _, r := range recordLike {
			violations = append(violations, shapeViolation{file: r.file, typ: r.name, kind: "record"})
		}
		if len(recordLike) == 0 {
			continue
		}
		recordNames := map[string]bool{}
		for _, r := range recordLike {
			recordNames[r.name] = true
		}

		// Store-shape pass: a struct in this SAME package holding a
		// map[string]<one of the record-like types above> is a second
		// acceptance-store paired with its own duplicate record — the
		// "or store" half of the finding. A struct holding
		// map[string]hosttrust.Record (a *ast.SelectorExpr, never a bare
		// *ast.Ident resolvable in this package's own struct set) can never
		// match this lookup, which is exactly the sanctioned-reuse case
		// this sentinel must let through unflagged.
		for name, st := range structs {
			if recordNames[name] {
				continue // the record struct itself, already reported above
			}
			if st.Fields == nil {
				continue
			}
			for _, f := range st.Fields.List {
				mt, ok := f.Type.(*ast.MapType)
				if !ok || !isIdentString(mt.Key) {
					continue
				}
				valIdent, ok := mt.Value.(*ast.Ident)
				if !ok || !recordNames[valIdent.Name] {
					continue
				}
				violations = append(violations, shapeViolation{file: fileOf[name], typ: name, kind: "store"})
			}
		}
		_ = dir
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].typ < violations[j].typ
	})
	return violations
}

// TestOnlyHosttrustDefinesTheAcceptanceRecordShape (F6) is the sentinel
// itself: services/host, everywhere except the hosttrust package that owns
// this shape by design, must contain no second struct matching
// hosttrust.Record's field signature and no second store pairing one with
// an Accepted-style map — in workflow/pack, a future workflow/env, or
// anywhere else. workflow/pack's own PackTrustRecord alias and unrelated
// packActivationRecord are exactly the two cases this must NOT flag; see
// TestTrustShapeSentinelAllowsTheSanctionedAliasAndUnrelatedRecord below for
// the standing proof that it doesn't.
func TestOnlyHosttrustDefinesTheAcceptanceRecordShape(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	violations := scanTrustShapeViolations(t, root)
	for _, v := range violations {
		t.Errorf("%s — a second trust acceptance-record/store shape; hosttrust.Record and its AcceptanceStore are the ONE shape (F6/E1.4, doc.go); reuse hosttrust here (an alias or a direct map[string]hosttrust.Record) instead of a parallel definition", v)
	}
}

// TestTrustShapeSentinelDetectsAPlantedRecordDuplicate proves the record
// half actually fires: a genuinely independent struct sharing
// hosttrust.Record's field shape, under a name with no relation to
// "Record" at all, must be caught.
func TestTrustShapeSentinelDetectsAPlantedRecordDuplicate(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\n" +
		"type SomethingElseEntirely struct {\n" +
		"\tPath        string\n" +
		"\tFingerprint string\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	violations := scanTrustShapeViolations(t, dir)
	if len(violations) != 1 || violations[0].typ != "SomethingElseEntirely" || violations[0].kind != "record" {
		t.Fatalf("violations = %v, want exactly one record-shape hit on SomethingElseEntirely", violations)
	}
}

// TestTrustShapeSentinelDetectsAPlantedStoreDuplicate proves the store half:
// a struct pairing its own duplicate record type with an Accepted-style map
// must be caught as a SECOND violation (the record) plus a THIRD (the
// store), distinguishing the two kinds in the report.
func TestTrustShapeSentinelDetectsAPlantedStoreDuplicate(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\n" +
		"type LocalRecord struct {\n" +
		"\tRemote      string\n" +
		"\tFingerprint string\n" +
		"}\n\n" +
		"type LocalStore struct {\n" +
		"\tAccepted map[string]LocalRecord\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	violations := scanTrustShapeViolations(t, dir)
	if len(violations) != 2 {
		t.Fatalf("violations = %v, want exactly 2 (one record, one store)", violations)
	}
	kinds := map[string]bool{violations[0].kind: true, violations[1].kind: true}
	if !kinds["record"] || !kinds["store"] {
		t.Fatalf("violations = %v, want one of each kind", violations)
	}
}

// TestTrustShapeSentinelAllowsTheSanctionedAliasAndUnrelatedRecord is the
// negative-space proof: the two on-disk patterns hosttrust's own doc.go
// blesses, and the one unrelated struct that merely shares a name
// substring, must never trip either check. It plants a MINIMAL reproduction
// of workflow/pack/truststore.go's actual shape (a type alias to a
// synthetic stand-in for hosttrust.Record, a store holding an alias-typed
// map directly, and an unrelated "*Record"-suffixed struct with different
// fields) so the assertion is about the CHECK's precision, not about
// re-parsing the real package on every run.
func TestTrustShapeSentinelAllowsTheSanctionedAliasAndUnrelatedRecord(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\n" +
		"type upstreamRecord struct {\n" +
		"\tPath        string\n" +
		"\tFingerprint string\n" +
		"}\n\n" +
		// The sanctioned alias: `type X = Y`, never a *ast.StructType.
		"type PackTrustRecord = upstreamRecord\n\n" +
		// Holding the ALIASED type's map directly — the sanctioned reuse
		// path — must not read as a second store around a second record;
		// there is no second record here at all, only a reference to the
		// alias.
		"type PackTrustStore struct {\n" +
		"\tAccepted map[string]PackTrustRecord\n" +
		"}\n\n" +
		// Unrelated struct sharing the "Record" name substring only —
		// packActivationRecord's real-world shape (Owner/Path, no
		// Fingerprint) — must not be flagged by a shape check.
		"type packActivationRecord struct {\n" +
		"\tOwner string\n" +
		"\tPath  string\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// upstreamRecord itself IS a genuine, real struct with the record
	// shape — it stands in for hosttrust.Record, which this synthetic
	// package cannot import. In the real repo hosttrust.Record lives in the
	// exempted hosttrust/ directory and is never scanned at all; this test
	// only needs to prove the ALIAS, the direct-map reuse, and the
	// unrelated struct are each individually clean, so it filters
	// upstreamRecord's own (expected, and in real life exempt) hit back out
	// before asserting.
	var got []shapeViolation
	for _, v := range scanTrustShapeViolations(t, dir) {
		if v.typ == "upstreamRecord" {
			continue
		}
		got = append(got, v)
	}
	if len(got) != 0 {
		t.Fatalf("violations = %v, want none: the alias, the direct aliased-map reuse, and the unrelated struct must all be allowed", got)
	}
}
