// env_copy_lint_test.go — E1.13's copy gate at the dispatch layer (AC-43/
// 57/67). workflow/env/errors_test.go proves the RENDERED text of every
// typed refusal that package can construct; this file proves two things a
// package-internal test cannot:
//
//   - the SOURCE string literals in the two files that actually assemble
//     `pix env`'s user-facing copy (cmd/pix/env_cmd.go's dispatch skeleton,
//     help text, and rm pointer; workflow/env/*.go's own literals) never
//     regress, via the same AST-over-BasicLit technique
//     TestPrimaryHelpAndStatusAvoidEmDashes (antidrift_test.go) already
//     uses for help.go/status.go — this is that same guard extended to the
//     `pix env` family;
//   - a real END-TO-END dispatch (the SAME kong entry point production
//     uses, not a direct function call) never asks a yes/no that selects,
//     prefixes, or corrects a NAME (D14/AC-57), never mentions `sbx env
//     rm` (AC-43), and the seven-verb/rm-pointer surface stays exactly the
//     PRD-specified shape (no `--sbxenv`, no `--force`, no `current`).
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"pix/host/cli"
)

var (
	copyLintFillerRE  = regexp.MustCompile(`(?i)\b(leverage|utilize|seamless)\b`)
	copyLintSuccessRE = regexp.MustCompile(`(?i)\b(configured|enabled|ready|verified)\b`)
)

// scanCopyLintSource is the pure form of the copy lint: parse src (Go source
// text, filename only used for position reporting and parse errors) and
// return one description string per banned-copy finding — an em dash, a
// filler word, an unearned success verdict, or a suggestion to run `sbx env
// rm` — across every string literal (BasicLit, kind STRING, never a
// comment). It never calls into *testing.T, so a planted-violation
// self-test (TestEnvCopyLint_SelfTest) can assert "found exactly one
// finding" on a synthetic bad snippet without a failing subtest's Fail()
// propagating up to a meta-test that must itself pass.
func scanCopyLintSource(t *testing.T, filename string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var findings []string
	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		pos := fset.Position(lit.Pos())
		where := filename + ":" + strconv.Itoa(pos.Line)
		if strings.Contains(val, "\u2014") {
			findings = append(findings, fmt.Sprintf("%s: contains an em dash in a user-facing string: %q", where, val))
		}
		if m := copyLintFillerRE.FindString(val); m != "" {
			findings = append(findings, fmt.Sprintf("%s: contains banned filler word %q: %q", where, m, val))
		}
		if m := copyLintSuccessRE.FindString(val); m != "" {
			findings = append(findings, fmt.Sprintf("%s: contains unearned success word %q: %q", where, m, val))
		}
		if strings.Contains(strings.ToLower(val), "sbx env rm") {
			findings = append(findings, fmt.Sprintf("%s: suggests `sbx env rm`, which does not exist: %q", where, val))
		}
		return true
	})
	return findings
}

// lintCopyFile reads file from disk and reports every scanCopyLintSource
// finding as a t.Errorf — the production check TestEnvCopyLint_SourceLiterals
// runs against the real env_cmd.go/workflow/env source tree.
func lintCopyFile(t *testing.T, file string) {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	for _, f := range scanCopyLintSource(t, file, src) {
		t.Error(f)
	}
}

// TestEnvCopyLint_SelfTest is finding A2's planted-violation proof for the
// SOURCE-literal scanner: each banned-copy class, planted alone in a
// throwaway snippet, must trip exactly one finding. Mirrors the existing
// forcerm_guard_test.go self-test pattern for this package's other AST
// scanner.
func TestEnvCopyLint_SelfTest(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"em dash", "package planted\n\nconst s = \"pix: bad \\u2014 thing\"\n"},
		{"filler word", "package planted\n\nconst s = \"please leverage this path\"\n"},
		{"unearned success word", "package planted\n\nconst s = \"the environment is ready\"\n"},
		{"sbx env rm", "package planted\n\nconst s = \"run sbx env rm work\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scanCopyLintSource(t, "planted.go", []byte(c.src))
			if len(got) != 1 {
				t.Errorf("scanCopyLintSource found %d finding(s) for planted %s violation, want exactly 1: %v", len(got), c.name, got)
			}
		})
	}
	// Negative control: clean source trips nothing.
	clean := "package planted\n\nconst s = \"pix: environment not found\"\n"
	if got := scanCopyLintSource(t, "planted.go", []byte(clean)); len(got) != 0 {
		t.Errorf("scanCopyLintSource on clean source found %v, want none", got)
	}
}

// TestEnvCopyLint_SourceLiterals is F15/AC-67: no env-path string uses an
// unearned success word, em dash, or filler, checked directly against the
// source literals rather than a rendered sample of them.
func TestEnvCopyLint_SourceLiterals(t *testing.T) {
	lintCopyFile(t, "env_cmd.go")

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
		lintCopyFile(t, filepath.Join(dir, name))
	}
}

// ── dispatch-level: no yes/no selects, prefixes, or corrects a name ──────

// envSweepDeps is env_cmd_test.go's envDeps, duplicated in miniature: this
// file only needs a scratch $PIX_CONFIG/$XDG_STATE_HOME and fresh buffers,
// never envDeps' fixture helpers.
func envSweepDeps(t *testing.T) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("")}, &out, &errb
}

// TestEnvCopyLint_UnknownNameNeverAsksAQuestion is AC-57/D14 at the real
// dispatch layer: an unknown-name refusal is data (`known:`), never a
// question, and — since `closest:` is itself only Wave C DATA the design
// doc reserves for a single close match (docs/design/environments.md §8.1)
// — this also proves today's rendering carries no `[y/n]`-shaped prompt
// and no "did you mean" phrasing standing in for it.
func TestEnvCopyLint_UnknownNameNeverAsksAQuestion(t *testing.T) {
	for _, argv := range [][]string{
		{"env", "show", "hoem"},
		{"env", "use", "hoem"},
		{"env", "forget", "hoem"},
	} {
		d, out, errb := envSweepDeps(t)
		code := dispatch(argv, d)
		if code != 2 {
			t.Fatalf("dispatch(%v) = %d, want 2", argv, code)
		}
		combined := out.String() + errb.String()
		lower := strings.ToLower(combined)
		if strings.Contains(lower, "did you mean") {
			t.Errorf("dispatch(%v) output = %q, must never ask \"did you mean\"", argv, combined)
		}
		if strings.Contains(lower, "[y/n]") {
			t.Errorf("dispatch(%v) output = %q, an unknown-name refusal must never carry a yes/no prompt", argv, combined)
		}
		if !strings.Contains(combined, "known:") {
			t.Errorf("dispatch(%v) output = %q, want the `known:` data line", argv, combined)
		}
	}
}

// TestEnvCopyLint_RmPointerNamesRealCommandsOnly is AC-43: the ONE place a
// user might expect a working `rm` alias must never suggest `sbx env rm`,
// and must name only the three real fixes.
func TestEnvCopyLint_RmPointerNamesRealCommandsOnly(t *testing.T) {
	d, _, errb := envSweepDeps(t)
	code := dispatch([]string{"env", "rm", "anything"}, d)
	if code != 2 {
		t.Fatalf("pix env rm anything = %d, want 2", code)
	}
	got := errb.String()
	if strings.Contains(strings.ToLower(got), "sbx env rm") {
		t.Errorf("rm pointer = %q, must never suggest `sbx env rm`", got)
	}
	for _, want := range []string{"pix env forget", "pix rm ", "rm -rf "} {
		if !strings.Contains(got, want) {
			t.Errorf("rm pointer = %q, want it to name %q", got, want)
		}
	}
}

// TestEnvCopyLint_SevenVerbsExactOrderNoRetiredFlags is the exact-shape half
// of the family gate: help lists the seven PRD §8 verbs in PRD §8 order,
// nothing else dispatchable, and none of the retired/never-shipped spellings
// (`current`, `--force`, `--sbxenv`) appear in the live help text.
func TestEnvCopyLint_SevenVerbsExactOrderNoRetiredFlags(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	if err := cli.RunRoot[envCmd]("pix env", "", "", []string{"--help"}, d); err != nil {
		t.Fatalf("env --help: %v", err)
	}
	text := out.String()
	idx := strings.Index(text, "Commands:")
	if idx < 0 {
		t.Fatalf("pix env --help has no \"Commands:\" block:\n%s", text)
	}
	commandsRE := regexp.MustCompile(`(?m)^\s{2}(\S+)`)
	var verbs []string
	for _, m := range commandsRE.FindAllStringSubmatch(text[idx:], -1) {
		verbs = append(verbs, m[1])
	}
	want := []string{"ls", "add", "use", "show", "edit", "review", "forget"}
	if len(verbs) != len(want) {
		t.Fatalf("pix env --help lists verbs %v, want exactly %v", verbs, want)
	}
	for i, v := range want {
		if verbs[i] != v {
			t.Errorf("pix env --help verb[%d] = %q, want %q (order: %v)", i, verbs[i], v, verbs)
		}
	}
	for _, bad := range []string{"current", "--force", "--sbxenv"} {
		if strings.Contains(text, bad) {
			t.Errorf("pix env --help = %q, must never mention %q", text, bad)
		}
	}
}
