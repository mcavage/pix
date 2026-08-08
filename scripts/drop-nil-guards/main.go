// Command drop-nil-guards deletes the guards that a non-nullable sys.System
// made dead code.
//
// `env.run == nil` is now a comparison against a method value, which Go
// evaluates as constant false. That is not a style nit: 118 of these encoded
// 14 mutually inconsistent answers to "what does a missing seam mean", and
// three shipped bugs came from the disagreement. Deleting them is the point of
// the refactor, not a side effect of it.
//
// Four shapes, each resolved by what the condition is now KNOWN to be:
//
//	if x.M == nil { ... }            -> delete (condition is false)
//	if x.M != nil { BODY }           -> unwrap to BODY (condition is true)
//	x.M != nil && REST               -> REST
//	x.M == nil || REST               -> REST
//
// An `if ... == nil { ... } else { E }` keeps E, and an `if != nil {B} else {C}`
// keeps B. Anything else is reported and left for a human: this program refuses
// to guess at control flow it does not recognise.
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

// methods promoted from sys.System onto shellEnv.
var sysMethods = map[string]bool{
	"Run": true, "LookPath": true, "RunTimed": true, "RunInteractive": true,
	"RunInteractiveQuiet": true, "ReadFile": true, "WriteFile": true, "IsFile": true,
	"Mode": true, "Lock": true, "Getenv": true, "HomeDir": true, "Getwd": true,
	"StateDir": true, "Executable": true, "DialLocal": true,
}

type edit struct {
	start, end int
	text       string
}

// nilCmp reports whether e is `<x>.<SysMethod> == nil` / `!= nil`, and which.
func nilCmp(e ast.Expr) (isNil bool, negated bool) {
	be, ok := e.(*ast.BinaryExpr)
	if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
		return false, false
	}
	id, ok := be.Y.(*ast.Ident)
	if !ok || id.Name != "nil" {
		return false, false
	}
	sel, ok := be.X.(*ast.SelectorExpr)
	if !ok || !sysMethods[sel.Sel.Name] {
		return false, false
	}
	return true, be.Op == token.NEQ
}

func main() {
	files, _ := filepath.Glob("services/host/cmd/pix/*.go")
	sort.Strings(files)
	var dropped, unwrapped, manual int
	for _, path := range files {
		src, _ := os.ReadFile(path)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			continue
		}
		base := fset.File(f.Pos()).Base()
		off := func(p token.Pos) int { return int(p) - base }
		text := string(src)

		var edits []edit
		ast.Inspect(f, func(n ast.Node) bool {
			// Compound conditions: drop the always-known conjunct/disjunct.
			if be, ok := n.(*ast.BinaryExpr); ok && (be.Op == token.LAND || be.Op == token.LOR) {
				if is, neg := nilCmp(be.X); is {
					if (be.Op == token.LAND && neg) || (be.Op == token.LOR && !neg) {
						edits = append(edits, edit{off(be.Pos()), off(be.Y.Pos()), ""})
						dropped++
					}
				}
				return true
			}
			ifs, ok := n.(*ast.IfStmt)
			if !ok || ifs.Init != nil {
				return true
			}
			is, neg := nilCmp(ifs.Cond)
			if !is {
				return true
			}
			body := strings.TrimSpace(text[off(ifs.Body.Lbrace)+1 : off(ifs.Body.Rbrace)])
			switch {
			case !neg && ifs.Else == nil: // if == nil { dead }
				edits = append(edits, edit{off(ifs.Pos()), off(ifs.End()), ""})
				dropped++
			case !neg: // if == nil { dead } else { live }
				blk, ok := ifs.Else.(*ast.BlockStmt)
				if !ok {
					manual++
					fmt.Fprintf(os.Stderr, "%s:%d: else-if chain, left alone\n", path, fset.Position(ifs.Pos()).Line)
					return true
				}
				keep := strings.TrimSpace(text[off(blk.Lbrace)+1 : off(blk.Rbrace)])
				edits = append(edits, edit{off(ifs.Pos()), off(ifs.End()), keep})
				unwrapped++
			default: // if != nil { live } [else { dead }]
				edits = append(edits, edit{off(ifs.Pos()), off(ifs.End()), body})
				unwrapped++
			}
			return true
		})
		if len(edits) == 0 {
			continue
		}
		// Drop NESTED edits before applying. An outer `if != nil` and an inner
		// one both match, and applying both against original offsets splices the
		// outer's stale end-offset into already-shifted text — which silently ate
		// the head of the following statement. Keep the outermost only; a rerun
		// picks up whatever it revealed, so the loop converges.
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		var top []edit
		for _, e := range edits {
			if len(top) > 0 && e.start < top[len(top)-1].end {
				continue
			}
			top = append(top, e)
		}
		edits = top
		sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
		out := text
		for _, e := range edits {
			out = out[:e.start] + e.text + out[e.end:]
		}
		os.WriteFile(path, []byte(out), 0o644)
	}
	fmt.Printf("dropped %d dead guards, unwrapped %d live ones (%d left for a human)\n",
		dropped, unwrapped, manual)
}
