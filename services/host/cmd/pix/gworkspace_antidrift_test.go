// gworkspace_antidrift_test.go — repo-wide guard for the Google Workspace
// naming/install contract (fix/onboarding). The canonical forms are: the
// dependency BINARY is `gog`; the brew FORMULA is `openclaw/tap/gogcli`
// (config.GWInstallCmd/gwUpgradeCmd in gworkspace.go); the guided CLI is
// `pix gworkspace setup|status|disable`; the config key is
// `google_workspace_account`; the MCP server name is `google-workspace`
// (config.GWServerName). The retired `gog` verb tree, `gog_account` as a
// user-facing key, and a bare `brew install gog` are never allowed to
// reappear in a production (non-test) source string in this package.
//
// TestGworkspaceUsage_NamingContract (gworkspace_test.go) already checks the
// four gworkspace usage blocks. This test widens the net to every non-test
// .go file in the package, so a stray error string or comment anywhere else
// (mcp.go, secret.go, setup_models.go, gog_setup.go, ...) can't silently
// resurrect a retired command.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gwForbiddenPhrase is one retired phrase plus the reason it must never
// reappear in a string literal.
type gwForbiddenPhrase struct {
	phrase string
	why    string
}

var gwForbiddenPhrases = []gwForbiddenPhrase{
	{"brew install gog", "the canonical formula is `brew install openclaw/tap/gogcli` (config.GWInstallCmd)"},
	{"brew upgrade gog ", "the canonical formula is `brew upgrade openclaw/tap/gogcli` (gwUpgradeCmd)"},
	{"pix gog setup", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace setup`"},
	{"pix gog auth", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace`"},
	{"pix gog status", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace status`"},
	{"pix gog disable", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace disable`"},
	{"config set gog_account", "the canonical config key is `google_workspace_account`"},
	{"config set mcp gog", "the canonical MCP server name is `google-workspace` (config.GWServerName)"},
}

// TestGworkspaceNaming_NoRetiredPhrasesInProductionSource scans every non-test
// .go file's string literals (and, since a stale comment is just as
// misleading to a reader as a stale error string, every comment too) for the
// retired phrases above.
func TestGworkspaceNaming_NoRetiredPhrasesInProductionSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		check := func(text string, where string) {
			for _, f := range gwForbiddenPhrases {
				if strings.Contains(text, f.phrase) {
					t.Errorf("%s: %s contains retired phrase %q: %s", file, where, f.phrase, f.why)
				}
			}
		}

		ast.Inspect(node, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				value = lit.Value // a raw/backtick string that failed to unquote as-is
			}
			check(value, "string literal")
			return true
		})
		for _, cg := range node.Comments {
			check(cg.Text(), "comment")
		}
	}
}

// TestGworkspaceNaming_NoRetiredServerNameInAgentDocs covers the runtime files
// outside this Go package that teach the agent how to diagnose and repair the
// integration. These are easy to miss in a CLI-only rename and directly affect
// the commands the agent gives users.
func TestGworkspaceNaming_NoRetiredServerNameInAgentDocs(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	files := []string{
		"capabilities.json",
		filepath.Join("skills", "capability-routing", "SKILL.md"),
		filepath.Join("skills", "gworkspace", "SKILL.md"),
		filepath.Join("skills", "healthcheck", "SKILL.md"),
	}
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(b)
		for _, phrase := range []string{
			`"server": "gog"`,
			"pix mcp load gog",
			"lists `gog` as registered",
			"`pix-host gog`",
		} {
			if strings.Contains(text, phrase) {
				t.Errorf("%s contains retired Google Workspace server guidance %q", rel, phrase)
			}
		}
	}
}
