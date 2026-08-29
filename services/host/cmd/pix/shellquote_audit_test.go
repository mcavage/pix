// shellquote_audit_test.go — 811dbde9's post-merge review BLOCK: every
// user-visible, runnable `pix ...` command line this package (env_cmd.go)
// and workflow/env's own source assemble must shell-quote (sys.ShellQuote)
// every dynamic NAME/PATH/ROOT/CWD token before interpolating it into that
// line, so a copy-pasted retry survives a real POSIX shell intact rather
// than splitting into extra arguments or, worse, a second command.
//
// This file has two parts:
//   - a dispatch-level end-to-end proof for `pix env show`'s own flag-
//     conflict retry (C6), the one runnable-command refusal that lives in
//     cmd/pix rather than workflow/env;
//   - scanEnvShellQuoteSource, an AST source scanner (the same
//     BasicLit/CallExpr technique env_copy_lint_test.go already uses for
//     the banned-copy scan) that proves the property holds for every
//     Sprintf/Fprintf/Errorf call in env_cmd.go and workflow/env/*.go,
//     not merely the cases this test suite happens to exercise directly.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"pix/host/sys"
)

// ── dispatch-level: env show's flag-conflict retry (C6) ──────────────────

// TestEnvShow_FlagConflictRetryShellQuotesInjectedName is C6's own
// shell-injection roundtrip proof: a NAME containing a space and shell
// metacharacters, given alongside a conflicting flag pair, comes back in
// the refusal's `retry:` line shell-quoted — tokenizing, through a REAL
// `sh -c`, to the exact original NAME as one argument.
func TestEnvShow_FlagConflictRetryShellQuotesInjectedName(t *testing.T) {
	const payload = `needs quoting; $(rm -rf /) & more`
	d, _, errb := envDeps(t)
	code := dispatch([]string{"env", "show", payload, "--path", "--json"}, d)
	if code != 2 {
		t.Fatalf("dispatch = %d, want 2 (usage refusal)", code)
	}
	retry := extractRetryLine(t, errb.String())
	argv := shellTokenize(t, retry)
	if len(argv) < 3 || argv[0] != "pix" || argv[1] != "env" || argv[2] != "show" {
		t.Fatalf("retry %q did not tokenize to a `pix env show ...` argv: %v", retry, argv)
	}
	if argv[3] != payload {
		t.Errorf("retry %q tokenized to argv %v, want argv[3] == %q (the original NAME, intact)", retry, argv, payload)
	}
}

// ── source scanner ─────────────────────────────────────────────────────

// pixCommandVerbRE finds a `%s`/`%v`/`%d` format verb (never `%q`, whose
// Go-syntax quoting is a display concern this scanner does not police)
// that appears somewhere after the FIRST `pix env `/`pix rm `/`pix run `
// command mention on its own line of a folded format string — i.e., a
// verb actually inside a runnable command's argument position, not one
// that merely happens to share a line with an unrelated later mention of
// "pix" in prose.
var pixCommandMentionRE = regexp.MustCompile(`pix (env \S+|rm|run) `)
var formatVerbRE = regexp.MustCompile(`%[svd]`)

// foldStringLiteral evaluates e as a compile-time-constant string built
// from BasicLit STRING nodes joined by `+` (exactly the multi-line
// concatenation shape every format string in this family uses) and
// returns (value, true); anything else (a variable, a call result, ...)
// returns ("", false) — the scanner simply skips a call it cannot fold,
// the same conservative stance scanCopyLintSource's BasicLit-only walk
// already takes for a literal it cannot statically read.
func foldStringLiteral(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := foldStringLiteral(v.X)
		if !ok {
			return "", false
		}
		r, ok := foldStringLiteral(v.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	case *ast.ParenExpr:
		return foldStringLiteral(v.X)
	default:
		return "", false
	}
}

// isShellQuoteCall reports whether e is (possibly parenthesized) a direct
// call to sys.ShellQuote.
func isShellQuoteCall(e ast.Expr) bool {
	call, ok := unwrapParen(e).(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "sys" && sel.Sel.Name == "ShellQuote"
}

func unwrapParen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// collectShellQuotedIdents walks fn's body for `x := sys.ShellQuote(...)`
// (or `x = sys.ShellQuote(...)`, or a multi-name `x, y := sys.ShellQuote(a), sys.ShellQuote(b)`)
// and returns the set of local identifier names bound to an ALREADY
// shell-quoted value — this family's dominant style computes one quoted
// variable once (e.g. `name := sys.ShellQuote(e.Name)`) and reuses it
// across several command lines, rather than calling sys.ShellQuote again
// at each interpolation site.
func collectShellQuotedIdents(fn *ast.FuncDecl) map[string]bool {
	safe := map[string]bool{}
	if fn.Body == nil {
		return safe
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) && len(assign.Rhs) != 1 {
				continue
			}
			var rhs ast.Expr
			switch {
			case len(assign.Rhs) == len(assign.Lhs):
				rhs = assign.Rhs[i]
			case len(assign.Rhs) == 1:
				rhs = assign.Rhs[0] // e.g. a single multi-value call; isShellQuoteCall will reject it, which is correct (ShellQuote is single-value)
			}
			if rhs != nil && isShellQuoteCall(rhs) {
				safe[id.Name] = true
			}
		}
		return true
	})
	return safe
}

// argIsSafe reports whether arg — the expression bound to a format verb
// this scanner judged to be inside a runnable-command argument position —
// needs no further quoting: a direct sys.ShellQuote(...) call, a local
// identifier already proven quoted by collectShellQuotedIdents, or a
// string literal (always static, never attacker-controlled).
func argIsSafe(arg ast.Expr, quotedIdents map[string]bool) bool {
	if isShellQuoteCall(arg) {
		return true
	}
	if id, ok := unwrapParen(arg).(*ast.Ident); ok && quotedIdents[id.Name] {
		return true
	}
	if _, ok := foldStringLiteral(arg); ok {
		return true
	}
	return false
}

// scanShellQuoteSource parses src and returns one finding per
// Sprintf/Fprintf/Errorf call whose (foldable) format string contains a
// runnable `pix env `/`pix rm `/`pix run ` command mention followed, on
// the SAME line, by a %s/%v/%d verb whose corresponding argument is not
// judged safe by argIsSafe.
func scanShellQuoteSource(t *testing.T, filename string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var findings []string
	for _, decl := range node.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		quotedIdents := collectShellQuotedIdents(fn)
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "fmt" {
				return true
			}
			var fmtIdx, argStart int
			switch sel.Sel.Name {
			case "Sprintf", "Errorf":
				fmtIdx, argStart = 0, 1
			case "Fprintf":
				fmtIdx, argStart = 1, 2
			default:
				return true
			}
			if fmtIdx >= len(call.Args) {
				return true
			}
			fmtVal, ok := foldStringLiteral(call.Args[fmtIdx])
			if !ok {
				return true // a runtime format string (e.g. a `headline` parameter) — its OWN construction site is scanned independently
			}
			args := call.Args[argStart:]
			pos := fset.Position(call.Pos())
			where := filename + ":" + strconv.Itoa(pos.Line)
			findings = append(findings, checkFormatString(where, fmtVal, args, quotedIdents)...)
			return true
		})
	}
	return findings
}

// checkFormatString walks fmtVal line by line, mapping each %s/%v/%d verb
// (in left-to-right order across the WHOLE string, matching fmt's own
// argument-consumption order; %w is skipped — it always binds an error,
// never a command token) to its positional arg in args, and flags any
// verb that appears after a `pix env `/`pix rm `/`pix run ` mention on its
// own line whose mapped arg is not argIsSafe.
func checkFormatString(where, fmtVal string, args []ast.Expr, quotedIdents map[string]bool) []string {
	var findings []string
	argIdx := 0
	for _, line := range strings.Split(fmtVal, "\n") {
		mention := pixCommandMentionRE.FindStringIndex(line)
		verbs := regexp.MustCompile(`%[a-zA-Z%]`).FindAllStringIndex(line, -1)
		for _, v := range verbs {
			verb := line[v[0]:v[1]]
			if verb == "%%" {
				continue // literal percent, consumes no arg
			}
			if verb == "%w" {
				argIdx++ // consumes an arg (the wrapped error), never a command token
				continue
			}
			isCommandVerb := formatVerbRE.MatchString(verb) && mention != nil && v[0] > mention[0]
			if argIdx >= len(args) {
				continue // malformed/unfoldable verb count mismatch — not this scanner's concern
			}
			arg := args[argIdx]
			argIdx++
			if isCommandVerb && !argIsSafe(arg, quotedIdents) {
				findings = append(findings, where+": dynamic token in a runnable `"+strings.TrimSpace(line)+"` line is not sys.ShellQuote'd")
			}
		}
	}
	return findings
}

func lintShellQuoteFile(t *testing.T, file string) {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	for _, f := range scanShellQuoteSource(t, file, src) {
		t.Error(f)
	}
}

// TestShellQuoteAudit_SelfTest is the scanner's own planted-violation
// proof: a synthetic snippet shaped exactly like the pre-811dbde9-fix bug
// (a bare, unquoted `name` interpolated into a `pix env ...` command line)
// must trip exactly one finding, and the identical snippet with
// sys.ShellQuote applied — either inline or via a pre-computed local — must
// trip none.
func TestShellQuoteAudit_SelfTest(t *testing.T) {
	const bad = `package planted

import "fmt"

func f(name string) string {
	return fmt.Sprintf("pix: retry: pix env review %s", name)
}
`
	if got := scanShellQuoteSource(t, "planted.go", []byte(bad)); len(got) != 1 {
		t.Errorf("scanShellQuoteSource on the unquoted-name snippet found %d finding(s), want exactly 1: %v", len(got), got)
	}

	const goodInline = `package planted

import (
	"fmt"

	"pix/host/sys"
)

func f(name string) string {
	return fmt.Sprintf("pix: retry: pix env review %s", sys.ShellQuote(name))
}
`
	if got := scanShellQuoteSource(t, "planted.go", []byte(goodInline)); len(got) != 0 {
		t.Errorf("scanShellQuoteSource on the inline-quoted snippet found %v, want none", got)
	}

	const goodLocal = `package planted

import (
	"fmt"

	"pix/host/sys"
)

func f(name string) string {
	quoted := sys.ShellQuote(name)
	return fmt.Sprintf("pix: retry: pix env review %s\n     also: pix env use %s", quoted, quoted)
}
`
	if got := scanShellQuoteSource(t, "planted.go", []byte(goodLocal)); len(got) != 0 {
		t.Errorf("scanShellQuoteSource on the local-quoted-variable snippet found %v, want none", got)
	}

	// Negative control: a %s NOT in a runnable-command position (no `pix
	// env `/`pix rm `/`pix run ` mention on that line at all) is never
	// flagged, quoted or not — most of this package's %s usage (an
	// informational "known: %s" data field, an error being %w-wrapped, ...)
	// falls here.
	const notACommand = `package planted

import "fmt"

func f(name string) string {
	return fmt.Sprintf("pix: environment %q is the current default.\n     default: %s", name, name)
}
`
	if got := scanShellQuoteSource(t, "planted.go", []byte(notACommand)); len(got) != 0 {
		t.Errorf("scanShellQuoteSource on a non-command %%s found %v, want none", got)
	}
}

// TestShellQuoteAudit_SourceLiterals is the production check: every
// dynamic token in a runnable `pix ...` command line, across env_cmd.go
// and every non-test file in workflow/env, is sys.ShellQuote'd.
func TestShellQuoteAudit_SourceLiterals(t *testing.T) {
	lintShellQuoteFile(t, "env_cmd.go")

	dir := filepath.Join("..", "..", "workflow", "env")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		lintShellQuoteFile(t, filepath.Join(dir, name))
	}
}

// pinSysImported keeps the `pix/host/sys` import live for isShellQuoteCall's
// doc reference even if every other use in this file were ever trimmed.
var _ = sys.ShellQuote
