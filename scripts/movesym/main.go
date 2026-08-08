// Command movesym relocates top-level declarations between files in the same
// Go package, carrying their doc comments.
//
// It exists because the single most common move in this drain is not "move a
// file" but "move a symbol home": a capability's own vocabulary declared in
// whichever file happened to need it first. `secret` went from 13 inbound
// references to 0 that way, without a single file changing directory.
//
//	go run ./scripts/movesym <dst.go> <src.go> <symbol> [more symbols...]
//
// Uses go/parser for the extents, so a doc comment, a `const (...)` group
// member, or a string literal containing a brace cannot confuse it — all three
// broke the hand-rolled version. A symbol inside a grouped declaration moves
// as the WHOLE group, since splitting an iota block silently renumbers it.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: movesym <dst.go> <src.go> <symbol> [more...]")
		os.Exit(2)
	}
	dst, src, want := os.Args[1], os.Args[2], map[string]bool{}
	for _, s := range os.Args[3:] {
		want[s] = true
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buf, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	type span struct{ start, end int }
	var spans []span
	var moved []string
	for _, d := range f.Decls {
		names := declNames(d)
		hit := false
		for _, n := range names {
			if want[n] {
				hit = true
			}
		}
		if !hit {
			continue
		}
		start := fset.Position(d.Pos()).Offset
		// A doc comment is part of the declaration for every purpose except the
		// AST's own Pos(), so take it explicitly or the move orphans it.
		if doc := declDoc(d); doc != nil {
			start = fset.Position(doc.Pos()).Offset
		}
		spans = append(spans, span{start, fset.Position(d.End()).Offset})
		moved = append(moved, names...)
	}
	if len(spans) == 0 {
		fmt.Fprintf(os.Stderr, "movesym: none of %v found in %s\n", os.Args[3:], src)
		os.Exit(1)
	}

	// Read the destination BEFORE touching the source. Doing it the other way
	// round deletes the declarations and then fails on an unreadable dst, which
	// loses them outright — it did, once.
	dstBuf, err := os.ReadFile(dst)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var carried []string
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	out := string(buf)
	for _, s := range spans {
		carried = append([]string{strings.TrimRight(out[s.start:s.end], "\n")}, carried...)
		// Swallow the blank line the declaration left behind.
		end := s.end
		for end < len(out) && out[end] == '\n' {
			end++
		}
		out = out[:s.start] + out[end:]
	}
	if err := os.WriteFile(src, []byte(out), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	body := strings.TrimRight(string(dstBuf), "\n") + "\n\n" + strings.Join(carried, "\n\n") + "\n"
	if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("moved %v: %s -> %s\n", moved, src, dst)
}

func declNames(d ast.Decl) []string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			return []string{d.Name.Name}
		}
		return nil // a method moves with its type, not on its own
	case *ast.GenDecl:
		var out []string
		for _, sp := range d.Specs {
			switch sp := sp.(type) {
			case *ast.TypeSpec:
				out = append(out, sp.Name.Name)
			case *ast.ValueSpec:
				for _, n := range sp.Names {
					out = append(out, n.Name)
				}
			}
		}
		return out
	}
	return nil
}

func declDoc(d ast.Decl) *ast.CommentGroup {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return d.Doc
	case *ast.GenDecl:
		return d.Doc
	}
	return nil
}
