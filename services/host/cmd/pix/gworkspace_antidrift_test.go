// gworkspace_antidrift_test.go — repo-wide guard for the Google Workspace
// naming/install contract (fix/onboarding). The canonical forms are: the
// dependency BINARY is `gog`; the brew FORMULA is `openclaw/tap/gogcli`
// (gwInstallCmd/gwUpgradeCmd in gworkspace.go); the guided CLI is
// `pix gworkspace setup|status|disable`; the config key is
// `google_workspace_account`; the MCP server name is `google-workspace`
// (gwServerName). The retired `gog` verb tree, `gog_account` as a
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
	{"brew install gog", "the canonical formula is `brew install openclaw/tap/gogcli` (gwInstallCmd)"},
	{"brew upgrade gog ", "the canonical formula is `brew upgrade openclaw/tap/gogcli` (gwUpgradeCmd)"},
	{"pix gog setup", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace setup`"},
	{"pix gog auth", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace`"},
	{"pix gog status", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace status`"},
	{"pix gog disable", "the `gog` verb tree is deleted (6b39a69); the guided CLI is `pix gworkspace disable`"},
	{"config set gog_account", "the canonical config key is `google_workspace_account`"},
	{"config set mcp gog", "the canonical MCP server name is `google-workspace` (gwServerName)"},
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
