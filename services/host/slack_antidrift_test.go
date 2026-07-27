// slack_antidrift_test.go — repo-wide guard for the Slack credential model
// (docs/design/slack-setup.md, fix/onboarding). SLACK_TOKEN is always a
// single named person's xoxp- user token: every Slack call the server makes
// (services/host/slack.go) runs AS that token's owner. It is never described
// as a generic "bot/user" token (that phrasing implies it could stand in for
// the workspace, not a specific person), and it is never handed to a second
// person to reuse. This test scans the production source and templates that
// describe SLACK_TOKEN — services/host/*.go, services/host/config/config.go,
// and config/op-refs.env.example — so that wrong wording can't quietly creep
// back into any of them.
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

// slackForbiddenPhrase is one wording pattern that must never describe
// SLACK_TOKEN, plus the reason it is wrong.
type slackForbiddenPhrase struct {
	phrase string
	why    string
}

var slackForbiddenPhrases = []slackForbiddenPhrase{
	{"bot/user token", "SLACK_TOKEN is always a specific person's xoxp- user token, never an either-or bot/user token (see docs/design/slack-setup.md)"},
	{"bot or user token", "SLACK_TOKEN is always a specific person's xoxp- user token, not ambiguously a bot token"},
	{"user/bot token", "SLACK_TOKEN is always a specific person's xoxp- user token, not ambiguously a bot token"},
	{"shared employee token", "shared employee tokens are forbidden; SLACK_TOKEN is per-user (docs/design/slack-setup.md)"},
	{"shared team token", "shared team tokens are forbidden; SLACK_TOKEN is per-user (docs/design/slack-setup.md)"},
	{"share this token", "a personal SLACK_TOKEN must never be handed to a second person to reuse"},
	{"share the token", "a personal SLACK_TOKEN must never be handed to a second person to reuse"},
	{"share your slack token", "a personal SLACK_TOKEN must never be handed to a second person to reuse"},
	{"everyone can use the same slack token", "a personal SLACK_TOKEN must never be handed to a second person to reuse"},
}

// checkTextForSlackDrift reports (via t.Errorf) every forbidden phrase found
// in text, case-insensitively, attributing the failure to file/where.
func checkTextForSlackDrift(t *testing.T, file, where, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, f := range slackForbiddenPhrases {
		if strings.Contains(lower, f.phrase) {
			t.Errorf("%s: %s contains forbidden Slack credential wording %q: %s", file, where, f.phrase, f.why)
		}
	}
}

// checkGoFileForSlackDrift parses a Go source file and scans every string
// literal and comment for forbidden phrases.
func checkGoFileForSlackDrift(t *testing.T, path string) {
	t.Helper()
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
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
		checkTextForSlackDrift(t, path, "string literal", value)
		return true
	})
	for _, cg := range node.Comments {
		checkTextForSlackDrift(t, path, "comment", cg.Text())
	}
}

// TestSlackCredentialWording_NoForbiddenPhrasesInHostSource scans every
// non-test .go file in this package (services/host, the pix-host binary that
// owns slack.go) for wording that describes SLACK_TOKEN as a generic
// bot/user token or suggests sharing it.
func TestSlackCredentialWording_NoForbiddenPhrasesInHostSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		checkGoFileForSlackDrift(t, file)
	}
}

// TestSlackCredentialWording_NoForbiddenPhrasesInConfigTemplate scans
// services/host/config/config.go, which embeds OpRefsTemplate — the seed
// content written into a fresh op-refs.env — so the seeded comment above
// SLACK_TOKEN can't drift back to describing it as a shared bot/user token.
func TestSlackCredentialWording_NoForbiddenPhrasesInConfigTemplate(t *testing.T) {
	checkGoFileForSlackDrift(t, filepath.Join("config", "config.go"))
}

// TestSlackCredentialWording_NoForbiddenPhrasesInOpRefsExample scans the
// repo's config/op-refs.env.example (the file OpRefsTemplate is kept in sync
// with) as plain text, since it isn't Go source.
func TestSlackCredentialWording_NoForbiddenPhrasesInOpRefsExample(t *testing.T) {
	path := filepath.Join("..", "..", "config", "op-refs.env.example")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	checkTextForSlackDrift(t, path, "file", string(data))
}
