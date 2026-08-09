package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// grammar_fallback_test.go pins the sbx-v0.38-compatibility contract: a
// bounded, ONE-shot retry with a KNOWN alternate grammar, gated ONLY on a
// recognized CLI usage mismatch — never on an auth/policy/operational
// failure, and never looping past the one known alternate.

// --- BundleAddArgs / BundleAddArgsPositional --------------------------------

// --- RunSbxGrammarFallback: real exec, real grammar mismatch, real streams -

// sbxReply is one grammar-variant's canned response: SEPARATE stdout/stderr
// text plus an exit code, so a fixture can pin exactly which stream a real
// sbx build's message lands on — parser/usage complaints are conventionally
// stderr, structured command output is conventionally stdout — and
// RunSbxGrammarFallback's stream-separation contract can be exercised
// against a real subprocess, not a CombinedOutput() string.
type sbxReply struct {
	stdout string
	stderr string
	exit   int
}

// installSbxGrammarFixture writes a real "sbx" shell script into a
// PATH-isolated dir. It answers the OLD `--url`-flag grammar's exact argv
// with oldReply and the NEW positional grammar's exact argv with newReply —
// the two invocations RunSbxGrammarFallback must choose between — each
// writing its stdout/stderr text to the ACTUAL corresponding file
// descriptor, not a single merged stream.
func installSbxGrammarFixture(t *testing.T, oldReply, newReply sbxReply) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"'mcp bundle add pix-catalog --url https://example.com/bundle.json')\n" +
		replyBody(oldReply) +
		"  ;;\n" +
		"'mcp bundle add pix-catalog https://example.com/bundle.json')\n" +
		replyBody(newReply) +
		"  ;;\n" +
		"*) echo \"fixture: unexpected argv: $*\" >&2; exit 99 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// replyBody renders one sbxReply's case-branch body: a stdout printf to fd 1
// when non-empty, a SEPARATE stderr printf to fd 2 (`>&2`) when non-empty,
// then the branch's exit code — never one printf covering both streams.
func replyBody(r sbxReply) string {
	body := ""
	if r.stdout != "" {
		body += "  printf %s " + shQuote(r.stdout) + "\n"
	}
	if r.stderr != "" {
		body += "  printf %s " + shQuote(r.stderr) + " >&2\n"
	}
	body += "  exit " + itoa(r.exit) + "\n"
	return body
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func lookPathIn(dir string) func(string) (string, error) {
	return func(name string) (string, error) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return p, nil
	}
}

// --- detectLegacyPositionalURL: bounded, read-only help-output detection ---

func TestDetectLegacyPositionalURL_HelpShowsURLFlagStaysCurrent(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			return "Flags:\n  --local\n  --url string   remote endpoint or manifest URL\n", false, nil
		},
	}}
	if detectLegacyPositionalURL(env) {
		t.Error("help text documents --url; must stay on the current --url grammar")
	}
}

func TestDetectLegacyPositionalURL_HelpOmitsURLFlagGoesLegacy(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			return "Usage:\n  sbx mcp add NAME [URL] [flags]\n\nFlags:\n  --local\n  --command string\n", false, nil
		},
	}}
	if !detectLegacyPositionalURL(env) {
		t.Error("help text has no --url flag; must switch to the legacy positional grammar")
	}
}

func TestDetectLegacyPositionalURL_UnknownStaysCurrent(t *testing.T) {
	cases := []struct {
		name string
		fn   func(name string, args ...string) (string, bool, error)
	}{
		{"help failed", func(string, ...string) (string, bool, error) { return "", false, errors.New("boom") }},
		{"help timed out", func(string, ...string) (string, bool, error) { return "", true, nil }},
		{"help empty", func(string, ...string) (string, bool, error) { return "  \n", false, nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := hostenv.Env{System: &systest.Fake{RunTimedFn: tc.fn}}
			if detectLegacyPositionalURL(env) {
				t.Errorf("%s: an unreadable help probe must NEVER flip behavior away from the current grammar", tc.name)
			}
		})
	}
}

// --- AddArgs respects LegacyPositionalURL -----------------------------------

func TestAddArgs_LegacyPositionalURL(t *testing.T) {
	containers := map[string]config.MCPContainer{
		"notion-ish": {Manifest: "https://example.com/mcp/x/server.json"},
		"meetings":   {RemoteURL: "https://app.trymeetings.com/mcp"},
	}
	reg := McpRegistrar{containers: containers, LegacyPositionalURL: true}

	manifest := strings.Join(reg.AddArgs("notion-ish"), " ")
	if manifest != "mcp add notion-ish --local https://example.com/mcp/x/server.json" {
		t.Errorf("legacy manifest AddArgs = %q", manifest)
	}
	remote := strings.Join(reg.AddArgs("meetings"), " ")
	if remote != "mcp add meetings https://app.trymeetings.com/mcp" {
		t.Errorf("legacy remote AddArgs = %q", remote)
	}
	if strings.Contains(manifest, "--url") || strings.Contains(remote, "--url") {
		t.Errorf("legacy grammar must never carry --url: manifest=%q remote=%q", manifest, remote)
	}
}
