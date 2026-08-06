package main

// processboundary_test.go enforces the second half of the layering contract
// arch_test.go started. arch_test.go proves imports point DOWN; this proves the
// PROCESS BOUNDARY points down too: only L4 may end the process or reach for a
// global stream.
//
//	L4  cmd/pix, .   own os.Exit, os.Stdout, os.Stderr
//	L0-L3  everything else   RETURNS a typed error, writes to an INJECTED writer
//
// The rule is not aesthetic. A package that calls os.Exit cannot be tested
// without re-execing the test binary (pack alone had eight such subprocess
// tests, each hiding its assertion behind an exit status), and one that writes
// to os.Stdout pollutes a --json answer from a layer that cannot know one was
// requested. It is also how a capability ends up with a SECOND exit-code table
// (secret's own os.Exit(3) beside cmd/pix's dispatch), which is how the two
// drift about what 3 means.
//
// There is no debt list. It used to be a ratchet over L3 alone, counting the
// writes each workflow still owed; L0-L3 owe none now, so the guard states the
// property instead of a budget. The ONE exemption is named and narrow: package
// sys IS the OS seam, and its Real exec may hand the terminal to a child
// process — see streamSeams and the test that pins what that exemption covers.
//
// Not in scope, deliberately: log.Printf in the daemon-side packages (monitor's
// ingest loop, supervise's tree). That is the standard logger, whose
// destination the daemon's own main sets; it is a logging-configuration
// question, not a command writing over a user's stdout. log.Fatal/Fatalf/
// Fatalln ARE in scope, by contrast: each prints then calls os.Exit(1) under
// the hood, so it is os.Exit wearing the logger's clothes, not a logging
// question. Same for the two builtins, print and println: no import needed,
// so the original AST walk — gated on "does this file import os or fmt" —
// never even looked at them, despite both writing straight to os.Stderr.

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

// processExits are the os identifiers that END or BYPASS the caller's control
// of the process. os.Stdin is deliberately absent: it is read through the
// prompt seams the interactive flows already thread (in io.Reader, tty bool),
// and pack's remaining two reads are a signature change (its trust gate takes
// the reader from its caller), tracked separately from this guard.
var processExits = map[string]bool{"Exit": true, "Stdout": true, "Stderr": true}

// logExits are the log-package funcs that end the process exactly like
// os.Exit — log.Fatal/Fatalf/Fatalln each print to os.Stderr then call
// os.Exit(1) internally (see the log package source). A package below L4
// calling one of these has found the same bypass as calling os.Exit
// directly, just spelled through the standard logger. log.Panic/Panicf/
// Panicln are deliberately absent: a panic can be recovered by a caller, so
// it is not the same unconditional process-ending violation.
var logExits = map[string]bool{"Fatal": true, "Fatalf": true, "Fatalln": true}

// builtinStderrWrites are the two predeclared (universe-scope, no import)
// functions that write straight to os.Stderr, unbuffered — the same class of
// violation as fmt.Print writing to os.Stdout, just with no package
// qualifier to key off of, so they need their own AST shape (a bare
// *ast.Ident call, not a *ast.SelectorExpr).
var builtinStderrWrites = map[string]bool{"print": true, "println": true}

// streamSeams are the packages allowed to name os.Stdout/os.Stderr, and the
// reason. sys is the only one: the package exists to BE the seam between this
// program and the OS, and RunInteractive's contract is that a child (browser
// OAuth, `op signin`) inherits the real terminal — a pipe would break the very
// interaction it is for, so "inject a writer" has no meaning here. The
// exemption covers stream HANDOVER only: os.Exit is banned in sys like
// everywhere else below L4, and TestStreamSeamIsOnlyAChildProcessHandover pins
// each exempted reference to an exec.Cmd assignment.
var streamSeams = map[string]string{
	"sys": "the OS seam itself: Real.RunInteractive hands the terminal to a child process",
}

// TestBelowL4NeverTouchesTheProcessBoundary walks every production package the
// layer map places below L4 — L0 foundation, L1 capability, L2 readiness, L3
// workflow — not just the workflows. A capability that exits the process is
// exactly as untestable as a workflow that does, and secret/service/mcp/cli
// each proved it: 33 process-boundary uses between them, every one of which
// made a real behaviour (a rejected env-var name, a refused `serve install`
// argument) reachable only through a subprocess.
func TestBelowL4NeverTouchesTheProcessBoundary(t *testing.T) {
	root := hostModuleRoot(t)
	var detail []string
	for _, pkg := range belowL4Packages(t) {
		_, exempt := streamSeams[pkg]
		for _, hit := range processBoundaryHits(t, filepath.Join(root, pkg)) {
			if exempt && !strings.HasSuffix(hit, "os.Exit") {
				// A named seam; TestStreamSeamIsOnlyAChildProcessHandover checks
				// its shape, and the os.Exit half stays banned above.
				continue
			}
			detail = append(detail, pkg+": "+hit)
		}
	}
	sort.Strings(detail)
	if len(detail) > 0 {
		t.Errorf("%d process-boundary use(s) below L4 — a package below the command layer returns a typed error and writes to an injected writer; only cmd/pix (and the pix-host root) own os.Exit/os.Stdout/os.Stderr:\n  %s",
			len(detail), strings.Join(detail, "\n  "))
	}
}

// TestStreamSeamIsOnlyAChildProcessHandover keeps the sys exemption from
// becoming a licence. Every os.Stdout/os.Stderr in an exempted package must
// appear on a line that assigns into an exec.Cmd's own streams: handing the
// terminal to a child is the seam's job, and printing from underneath the
// whole program is not.
func TestStreamSeamIsOnlyAChildProcessHandover(t *testing.T) {
	root := hostModuleRoot(t)
	for pkg := range streamSeams {
		dir := filepath.Join(root, pkg)
		for _, hit := range processBoundaryHits(t, dir) {
			file, line, ok := strings.Cut(hit, ":")
			if !ok {
				t.Fatalf("unparseable hit %q", hit)
			}
			line, _, _ = strings.Cut(line, ":")
			src := sourceLine(t, filepath.Join(dir, file), line)
			if !strings.Contains(src, "cmd.Std") {
				t.Errorf("%s/%s: %q is not a child-process stream handover — the %s exemption covers only that (%s)",
					pkg, hit, strings.TrimSpace(src), pkg, streamSeams[pkg])
			}
		}
	}
}

// TestNothingBelowL4CallsOsExit states the half of the rule that has NO
// exemption, separately from the streams: below the command layer, a failure is
// a returned value. Stated on its own so the sys stream seam can never be read
// as covering an exit too.
func TestNothingBelowL4CallsOsExit(t *testing.T) {
	root := hostModuleRoot(t)
	for _, pkg := range belowL4Packages(t) {
		for _, hit := range processBoundaryHits(t, filepath.Join(root, pkg)) {
			if strings.HasSuffix(hit, "os.Exit") {
				t.Errorf("%s/%s: only L4 ends the process — return a typed error (cli.SilentError carries an exit code for a message you already printed)", pkg, hit)
			}
		}
	}
}

// belowL4Packages is every package arch_test.go's layer map places below the
// command layer, sorted. Reusing that map is deliberate: a package added to the
// architecture is placed exactly once, and both halves of the contract (imports
// point down, the process boundary points down) then cover it automatically.
func belowL4Packages(t *testing.T) []string {
	t.Helper()
	root := hostModuleRoot(t)
	var pkgs []string
	for pkg, layer := range pkgLayer {
		if layer >= layerCommand || layer < 0 {
			continue
		}
		// The map names a few packages that were deleted rather than moved
		// (knowledge, okf): arch_test.go keeps the entry as the record of where
		// they sat. Nothing to walk, and nothing to claim about them.
		if fi, err := os.Stat(filepath.Join(root, pkg)); err != nil || !fi.IsDir() {
			continue
		}
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	return pkgs
}

// sourceLine returns line n (1-based, as a decimal string) of a file.
func sourceLine(t *testing.T, path, n string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(string(b), "\n")
	var i int
	if _, err := fmt.Sscanf(n, "%d", &i); err != nil || i < 1 || i > len(lines) {
		t.Fatalf("%s: no line %s", path, n)
	}
	return lines[i-1]
}

// TestPackOwnsNoProcessBoundary is the U08d regression, stated as its own
// named claim: `pix pack` is the verb tree that used to exit the process from
// L3, and it must stay clean.
func TestPackOwnsNoProcessBoundary(t *testing.T) {
	if _, exempt := streamSeams["workflow/pack"]; exempt {
		t.Fatal("workflow/pack must never be granted a process-boundary exemption: its verbs return errors and cmd/pix maps them to exit codes")
	}
	if hits := processBoundaryHits(t, filepath.Join(hostModuleRoot(t), "workflow", "pack")); len(hits) != 0 {
		t.Errorf("workflow/pack reached the process boundary again:\n  %s", strings.Join(hits, "\n  "))
	}
}

// hostModuleRoot resolves the services/host module root. The test binary runs
// in the package directory, which IS the module root for package main.
func hostModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s does not look like the services/host module root: %v", root, err)
	}
	return root
}

// implicitStdout are the fmt helpers that write to os.Stdout WITHOUT naming
// it. `fmt.Print(Description)` is the same violation as `fmt.Fprint(os.Stdout,
// Description)` — it was how `serve install --help` printed from L1 — and a
// guard that only looked for the identifier missed it entirely.
var implicitStdout = map[string]bool{"Print": true, "Printf": true, "Println": true}

// processBoundaryHits parses every non-test .go file in dir and reports each
// os.Exit / os.Stdout / os.Stderr reference, and each implicit-stdout fmt call,
// as "file:line: os.X". An AST walk rather than a grep: a grep counts the word
// in a comment (this file's own prose would trip it) and misses nothing a
// compiler would see.
func processBoundaryHits(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	var hits []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			// The local identifiers THIS file uses for os/fmt/log — its plain
			// name, an alias, or none if the path isn't imported at all.
			// Resolved per file (imports are file-scoped in Go), and through
			// the alias rather than the literal package name: `import o "os"`
			// then `o.Exit(...)` used to be invisible to this guard because it
			// only ever looked for the identifier "os".
			osNames := importedNames(file, "os")
			fmtNames := importedNames(file, "fmt")
			logNames := importedNames(file, "log")
			ast.Inspect(file, func(n ast.Node) bool {
				// print(...) / println(...): a bare call to the builtin, not a
				// selector, so it needs its own shape check and runs
				// regardless of what this file imports. ident.Obj != nil means
				// this file (or an enclosing scope within it) itself declares
				// something named print/println — a local func or var
				// shadowing the builtin is not the builtin, and flagging it
				// would be exactly the "unrelated local func" false positive
				// this guard must not produce.
				if call, ok := n.(*ast.CallExpr); ok {
					if ident, ok := call.Fun.(*ast.Ident); ok && ident.Obj == nil && builtinStderrWrites[ident.Name] {
						pos := fset.Position(ident.Pos())
						hits = append(hits, fmt.Sprintf("%s:%d: os.Stderr (via builtin %s)", filepath.Base(pos.Filename), pos.Line, ident.Name))
					}
					return true
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Obj != nil {
					return true
				}
				var what string
				switch {
				case osNames[ident.Name] && processExits[sel.Sel.Name]:
					what = "os." + sel.Sel.Name
				case fmtNames[ident.Name] && implicitStdout[sel.Sel.Name]:
					what = "os.Stdout (via fmt." + sel.Sel.Name + ")"
				case logNames[ident.Name] && logExits[sel.Sel.Name]:
					// Phrased to END in the literal "os.Exit": both
					// TestNothingBelowL4CallsOsExit and the sys stream-seam
					// exemption above key off that exact suffix to recognize a
					// process-ending hit regardless of which spelling reached
					// it, and log.Fatal* must be recognized as exactly that —
					// no exemption, same as a direct os.Exit.
					what = "log." + sel.Sel.Name + " -> os.Exit"
				default:
					return true
				}
				pos := fset.Position(sel.Pos())
				hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.Base(pos.Filename), pos.Line, what))
				return true
			})
		}
	}
	sort.Strings(hits)
	return hits
}

// importedNames returns every local identifier this file uses to refer to
// the package imported from path: its own name for a plain import (`"os"` ->
// "os"), an explicit alias (`o "os"` -> "o"), or an empty set if the file
// doesn't import path at all. Resolving through the alias — not just the
// literal package name importsPlain used to check for — is what closes the
// hole an aliased `import o "os"` or `import l "log"` opened: renaming an
// import at the call site used to be a free pass through every check below.
// A blank import ("_") contributes no usable identifier and a dot import
// (".") expands to bare identifiers instead of a qualifier — both left
// unhandled here, same as this file's own "not in scope" carve-outs above.
func importedNames(file *ast.File, path string) map[string]bool {
	names := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Path.Value != `"`+path+`"` {
			continue
		}
		switch {
		case imp.Name == nil:
			names[path] = true // os, fmt, log: package name == import path (no "/")
		case imp.Name.Name == "_" || imp.Name.Name == ".":
			// no usable qualified identifier from either form
		default:
			names[imp.Name.Name] = true
		}
	}
	return names
}
