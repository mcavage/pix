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
// question, not a command writing over a user's stdout.

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
			// A local identifier named os/fmt cannot be the stdlib package.
			usesOS, usesFmt := importsPlain(file, "os"), importsPlain(file, "fmt")
			if !usesOS && !usesFmt {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
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
				case usesOS && ident.Name == "os" && processExits[sel.Sel.Name]:
					what = "os." + sel.Sel.Name
				case usesFmt && ident.Name == "fmt" && implicitStdout[sel.Sel.Name]:
					what = "os.Stdout (via fmt." + sel.Sel.Name + ")"
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

// importsPlain reports whether the file imports path under its own name, which
// is what makes `os.Exit` in it the stdlib call rather than a local variable's
// field.
func importsPlain(file *ast.File, path string) bool {
	for _, imp := range file.Imports {
		if imp.Path.Value == `"`+path+`"` && imp.Name == nil {
			return true
		}
	}
	return false
}
