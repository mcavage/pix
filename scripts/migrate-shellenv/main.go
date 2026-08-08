// Command migrate-shellenv rewrites `shellEnv{...}` composite literals onto
// sys.System, using Go's own parser to find them.
//
// A hand-rolled brace matcher was tried first and is the wrong tool: Go source
// contains braces inside line comments, rune literals ('{'), and raw strings,
// and a scanner that does not know Go grammar mis-pairs them. go/parser knows.
// We use it only to LOCATE each literal and its key/value spans, then do
// textual surgery at those byte offsets — so the diff stays minimal and every
// comment and line break outside the literal is untouched.
//
// Emits the rewritten file only when something changed. The compiler and the
// test suite are the proof; this program deliberately makes no attempt to be
// clever about cases it does not recognise, and reports them instead.
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

// shellEnv field -> systest.Fake field. Fields absent from this map stay on
// shellEnv (domain probes and flags that are not OS seams).
var sysFields = map[string]string{
	"run": "RunFn", "lookPath": "LookPathFn", "probe": "RunTimedFn",
	"runInteractive": "RunInteractiveFn", "runInteractiveQuiet": "RunInteractiveQuietFn",
	"readFile": "ReadFileFn", "writeFile": "WriteFileFn", "statFile": "IsFileFn",
	"fileMode": "ModeFn", "flock": "LockFn", "getenv": "GetenvFn",
	"homeDir": "HomeDirFn", "getwd": "GetwdFn", "stateDir": "StateDirFn",
	"executable": "ExecutableFn", "dial": "DialLocalFn",
}

type edit struct{ start, end int; text string }

func main() {
	dir := "services/host/cmd/pix"
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	sort.Strings(files)
	changed, lits, skipped := 0, 0, 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		base := fset.File(f.Pos()).Base()
		off := func(p token.Pos) int { return int(p) - base }

		var edits []edit
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			id, ok := cl.Type.(*ast.Ident)
			if !ok || id.Name != "shellEnv" {
				return true
			}
			lits++
			var sysKV, keepKV []string
			for _, e := range cl.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					skipped++
					fmt.Fprintf(os.Stderr, "%s:%d: positional shellEnv literal, left alone\n",
						path, fset.Position(e.Pos()).Line)
					return true
				}
				key := kv.Key.(*ast.Ident).Name
				val := string(src[off(kv.Value.Pos()):off(kv.Value.End())])
				if fake, isSys := sysFields[key]; isSys {
					sysKV = append(sysKV, fake+": "+val)
				} else {
					keepKV = append(keepKV, key+": "+val)
				}
			}
			var b strings.Builder
			b.WriteString("shellEnv{System: &systest.Fake{")
			for i, kv := range sysKV {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(kv)
			}
			b.WriteString("}")
			for _, kv := range keepKV {
				b.WriteString(", " + kv)
			}
			b.WriteString("}")
			edits = append(edits, edit{off(cl.Pos()), off(cl.End()), b.String()})
			return true
		})
		if len(edits) == 0 {
			continue
		}
		sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
		out := string(src)
		for _, e := range edits {
			out = out[:e.start] + e.text + out[e.end:]
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		changed++
	}
	fmt.Printf("%d literals rewritten across %d files (%d skipped)\n", lits-skipped, changed, skipped)
}
