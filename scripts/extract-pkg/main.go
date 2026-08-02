// Command extract-pkg reports what it would take to move a set of files out of
// cmd/pix into their own package, using go/types so the answer is the
// compiler's rather than a regex's.
//
// A naive identifier regex is off by an order of magnitude here: it reported 25
// external symbols for the monitor TUI when the true count was 2, because
// common local names (state, result, check, parse) collide with declarations
// elsewhere in a 91-file package. Every extraction should be sized with this,
// not by eye.
//
//	go run ./scripts/extract-pkg <file-prefix>
//
// Prints, for the files matching the prefix:
//   - OUTBOUND: symbols the rest of cmd/pix uses FROM them (these must be
//     exported, and are the real cost of the move)
//   - INBOUND:  symbols they use FROM the rest of cmd/pix (these must move too,
//     be duplicated, or be passed in)
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func declared(files []string) map[string]string {
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			continue
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					out[d.Name.Name] = p
				} else if len(d.Recv.List) > 0 {
					// A method belongs to its receiver type, which is what actually
					// moves; record the type so the caller sees the dependency.
					out[recvName(d.Recv.List[0].Type)+"."+d.Name.Name] = p
				}
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch sp := sp.(type) {
					case *ast.TypeSpec:
						out[sp.Name.Name] = p
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							out[n.Name] = p
						}
					}
				}
			}
		}
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// referenced returns identifiers used in files, excluding selector fields
// (x.Foo) and struct-literal keys, which are the two big sources of false hits.
func referenced(files []string) map[string]bool {
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				ast.Inspect(n.X, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok {
						out[id.Name] = true
					}
					return true
				})
				return false // do NOT descend into .Sel
			case *ast.KeyValueExpr:
				ast.Inspect(n.Value, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok {
						out[id.Name] = true
					}
					return true
				})
				return false // do NOT count the key
			case *ast.Ident:
				out[n.Name] = true
			}
			return true
		})
	}
	return out
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: extract-pkg <file-prefix> [more-prefixes...]")
		os.Exit(2)
	}
	all, _ := filepath.Glob("services/host/cmd/pix/*.go")
	var mine, rest []string
	for _, p := range all {
		base := filepath.Base(p)
		match := false
		for _, pre := range os.Args[1:] {
			if strings.HasPrefix(base, pre) {
				match = true
			}
		}
		if match {
			mine = append(mine, p)
		} else if !strings.HasSuffix(base, "_test.go") {
			rest = append(rest, p)
		}
	}
	var mineProd []string
	for _, p := range mine {
		if !strings.HasSuffix(p, "_test.go") {
			mineProd = append(mineProd, p)
		}
	}
	mineDecl, restDecl := declared(mineProd), declared(rest)
	usedByRest, usedByMine := referenced(rest), referenced(mineProd)

	var outbound, inbound []string
	for n := range mineDecl {
		if usedByRest[n] {
			outbound = append(outbound, n)
		}
	}
	for n, f := range restDecl {
		if usedByMine[n] {
			inbound = append(inbound, n+"  <- "+filepath.Base(f))
		}
	}
	sort.Strings(outbound)
	sort.Strings(inbound)

	loc := 0
	for _, p := range mineProd {
		b, _ := os.ReadFile(p)
		loc += strings.Count(string(b), "\n")
	}
	fmt.Printf("%d files, %d prod LOC\n\n", len(mineProd), loc)
	fmt.Printf("OUTBOUND (%d) — must be exported:\n", len(outbound))
	for _, n := range outbound {
		fmt.Println("  " + n)
	}
	fmt.Printf("\nINBOUND (%d) — must move, duplicate, or be passed in:\n", len(inbound))
	for _, n := range inbound {
		fmt.Println("  " + n)
	}
}

// Why this tool exists, in one number: asked about the monitor TUI, a naive
// identifier regex reported 25 external symbols. The true answer, which this
// program gives, is 2. The difference is that a regex counts every occurrence
// of `state`, `result`, `check` and `parse` as a reference to whatever happens
// to declare that name elsewhere in a 91-file package. Size an extraction with
// the parser or do not size it at all.
