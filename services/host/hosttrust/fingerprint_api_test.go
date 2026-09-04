// fingerprint_api_test.go proves the API constraint fingerprint.go's doc
// comments claim: Fingerprint no longer accepts `any`, so a caller cannot
// hand it an arbitrary, unmarshaled Go value and get back a hash of an
// encoding nobody deliberately produced. Two independent proofs, because
// either one alone leaves a gap:
//
//   - TestFingerprint_SignatureTakesCanonicalDocNotAny is a fast SOURCE check
//     (parses fingerprint.go's own AST, mirroring
//     TestLoadMutateSave_NeverAcquiresALock's technique in
//     hosttrust_test.go) that pins the declared parameter type today, so a
//     future edit that widens it back to `any`/`interface{}` fails loudly
//     without needing a subprocess build.
//   - TestFingerprint_ArbitraryStructIsACompileError is the END-TO-END proof:
//     it actually shells out to the go toolchain and shows an arbitrary
//     struct literal handed to hosttrust.Fingerprint is a COMPILE ERROR, with
//     a passing control build (Canonicalize -> Fingerprint) alongside it so a
//     broken harness can't masquerade as a passing constraint.
package hosttrust

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFingerprint_SignatureTakesCanonicalDocNotAny walks fingerprint.go's own
// AST and fails if Fingerprint's sole parameter is not the named type
// CanonicalDoc — in particular if it is `any`/`interface{}` (today's finding)
// or any other type an arbitrary struct could satisfy.
func TestFingerprint_SignatureTakesCanonicalDocNotAny(t *testing.T) {
	const path = "fingerprint.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	ast.Inspect(file, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "Fingerprint" {
			fn = fd
		}
		return true
	})
	if fn == nil {
		t.Fatal("fingerprint.go declares no top-level func Fingerprint")
	}
	var params []*ast.Field
	for _, f := range fn.Type.Params.List {
		params = append(params, f)
	}
	if len(params) != 1 {
		t.Fatalf("Fingerprint has %d parameter groups, want exactly 1 (doc CanonicalDoc)", len(params))
	}
	ident, ok := params[0].Type.(*ast.Ident)
	if !ok {
		t.Fatalf("Fingerprint's parameter type is %T, want a plain identifier naming CanonicalDoc", params[0].Type)
	}
	if ident.Name == "any" || ident.Name == "interface{}" {
		t.Fatalf("Fingerprint's parameter type is %q; it must be CanonicalDoc, not any/interface{}, so an arbitrary struct cannot be passed", ident.Name)
	}
	if ident.Name != "CanonicalDoc" {
		t.Fatalf("Fingerprint's parameter type is %q, want CanonicalDoc", ident.Name)
	}
}

// TestFingerprint_ArbitraryStructIsACompileError shells out to `go build` on
// two standalone files, both importing this package from OUTSIDE it (a fresh
// main package), pinned side by side so a broken build environment can't be
// mistaken for a passing type constraint:
//
//   - bad.go calls hosttrust.Fingerprint with a bare struct literal. This
//     MUST fail to compile.
//   - good.go runs the sanctioned Canonicalize -> Fingerprint path with the
//     same value. This MUST compile, proving bad.go's failure is about the
//     type constraint, not an unrelated build break.
func TestFingerprint_ArbitraryStructIsACompileError(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to the go toolchain; skipped in -short")
	}
	dir := t.TempDir()

	writeAndBuild := func(name, src string) (out []byte, err error) {
		srcPath := filepath.Join(dir, name+".go")
		if werr := os.WriteFile(srcPath, []byte(src), 0o644); werr != nil {
			t.Fatal(werr)
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), srcPath)
		cmd.Dir = "." // this package's own dir; go build resolves the enclosing module from here
		return cmd.CombinedOutput()
	}

	badOut, badErr := writeAndBuild("bad", `package main

import "pix/host/hosttrust"

func main() {
	_, _ = hosttrust.Fingerprint(struct{ A int }{A: 1})
}
`)
	if badErr == nil {
		t.Fatalf("hosttrust.Fingerprint(struct{...}{}) compiled successfully; want a compile error proving Fingerprint no longer accepts an arbitrary struct.\noutput:\n%s", badOut)
	}
	if !strings.Contains(string(badOut), "CanonicalDoc") {
		t.Errorf("compile error = %q, want it to name CanonicalDoc as the required argument type", badOut)
	}

	goodOut, goodErr := writeAndBuild("good", `package main

import "pix/host/hosttrust"

func main() {
	doc, err := hosttrust.Canonicalize(struct{ A int }{A: 1})
	if err != nil {
		panic(err)
	}
	if _, err := hosttrust.Fingerprint(doc); err != nil {
		panic(err)
	}
}
`)
	if goodErr != nil {
		t.Fatalf("the sanctioned Canonicalize -> Fingerprint path failed to build (so the harness itself, not the type constraint, is broken): %s", goodOut)
	}
}
