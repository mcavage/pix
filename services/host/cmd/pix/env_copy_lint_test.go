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

// lintCopyFile scans every string literal (BasicLit, kind STRING — never a
// comment) in file for the family's banned copy: an em dash, a filler
// word, an unearned success verdict, or a suggestion to run `sbx env rm`.
func lintCopyFile(t *testing.T, file string) {
	t.Helper()
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
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
		where := file + ":" + strconv.Itoa(pos.Line)
		if strings.Contains(val, "\u2014") {
			t.Errorf("%s: contains an em dash in a user-facing string: %q", where, val)
		}
		if m := copyLintFillerRE.FindString(val); m != "" {
			t.Errorf("%s: contains banned filler word %q: %q", where, m, val)
		}
		if m := copyLintSuccessRE.FindString(val); m != "" {
			t.Errorf("%s: contains unearned success word %q: %q", where, m, val)
		}
		if strings.Contains(strings.ToLower(val), "sbx env rm") {
			t.Errorf("%s: suggests `sbx env rm`, which does not exist: %q", where, val)
		}
		return true
	})
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
