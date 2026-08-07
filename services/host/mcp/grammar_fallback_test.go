package mcp

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
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

func TestBundleAddArgs(t *testing.T) {
	got := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	want := []string{"mcp", "bundle", "add", "pix-catalog", "--url", "https://example.com/bundle.json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("BundleAddArgs = %v, want %v", got, want)
	}
}

func TestBundleAddArgsPositional(t *testing.T) {
	got := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	want := []string{"mcp", "bundle", "add", "pix-catalog", "https://example.com/bundle.json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("BundleAddArgsPositional = %v, want %v", got, want)
	}
	if contains(got, []string{"--url"}) {
		t.Errorf("positional grammar must not carry --url, got %v", got)
	}
}

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

// runSbxGrammarFallbackAgainst points exec.LookPath's "sbx" resolution at bin
// by prepending its directory to PATH — RunSbxGrammarFallback always execs
// the literal name "sbx" (matching RunSbxMcpCore's own convention), so the
// fixture must be reachable under that name from PATH, not just from a
// lookPath closure.
func runSbxGrammarFallbackAgainst(t *testing.T, bin string, primary, alt []string) (out, errOut bytes.Buffer, err error) {
	t.Helper()
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	err = RunSbxGrammarFallback(lookPathIn(filepath.Dir(bin)), &out, &errOut, primary, alt)
	return out, errOut, err
}

func TestRunSbxGrammarFallback_RetriesOnRecognizedUsageMismatch(t *testing.T) {
	// The primary grammar's own parser rejects the argv on stderr (a real
	// cobra parser writes usage complaints there, never stdout); the
	// alternate grammar succeeds with warnings on stderr AND structured JSON
	// on stdout — the two streams RunSbxGrammarFallback must keep apart, and
	// only the alt attempt's streams (not the primary parser's noise) must
	// reach the caller.
	bin := installSbxGrammarFixture(t,
		sbxReply{stderr: "Error: unknown flag: --url\nUsage:\n  sbx mcp bundle add NAME URL", exit: 1},
		sbxReply{stdout: `{"registered":"pix-catalog"}` + "\n", stderr: "warning: falling back to legacy positional grammar\n", exit: 0})
	primary := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	alt := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	out, errOut, err := runSbxGrammarFallbackAgainst(t, bin, primary, alt)
	if err != nil {
		t.Fatalf("RunSbxGrammarFallback = %v, want nil (fallback should have succeeded)", err)
	}
	if out.String() != `{"registered":"pix-catalog"}`+"\n" {
		t.Errorf("stdout = %q, want ONLY the alt attempt's structured stdout", out.String())
	}
	if errOut.String() != "warning: falling back to legacy positional grammar\n" {
		t.Errorf("stderr = %q, want ONLY the alt attempt's warning — no primary parser noise", errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), "unknown flag") {
		t.Errorf("primary grammar's parser noise leaked through: out=%q errOut=%q", out.String(), errOut.String())
	}
}

func TestRunSbxGrammarFallback_NeverRetriesOperationalFailure(t *testing.T) {
	// The primary grammar fails for a REAL reason (an expired/missing auth
	// token), printed on stderr with a structured error body on stdout — not
	// a grammar mismatch. Even though the alternate grammar would succeed if
	// tried, the retry must never fire: a wrong retry here would silently
	// mask an auth gap as success. No retry means no alt run at all: the
	// primary's OWN stdout and stderr, kept separate, are the final report.
	bin := installSbxGrammarFixture(t,
		sbxReply{stdout: `{"error":"unauthorized"}` + "\n", stderr: "401 Unauthorized: token expired\n", exit: 1},
		sbxReply{stdout: `{"registered":"pix-catalog"}` + "\n", exit: 0})
	primary := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	alt := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	out, errOut, err := runSbxGrammarFallbackAgainst(t, bin, primary, alt)
	if err == nil {
		t.Fatal("RunSbxGrammarFallback = nil, want the primary attempt's own error (no grammar retry for an auth failure)")
	}
	if !strings.Contains(errOut.String(), "401 Unauthorized") {
		t.Errorf("stderr = %q, want the ORIGINAL auth failure, not a retried/rewritten one", errOut.String())
	}
	if !strings.Contains(out.String(), `"unauthorized"`) {
		t.Errorf("stdout = %q, want the primary attempt's own structured stdout preserved separately from stderr", out.String())
	}
	if strings.Contains(out.String(), "registered") {
		t.Errorf("stdout = %q, must not contain the untried alt attempt's output", out.String())
	}
}

func TestRunSbxGrammarFallback_BothGrammarsFailingReportsTheRetry(t *testing.T) {
	// A recognized usage mismatch DOES license one retry; if the alternate
	// grammar also fails (for whatever reason), the bounded retry stops there
	// — no further guessing — and reports THAT final attempt's streams only,
	// never the primary parser's rejected-argv noise.
	bin := installSbxGrammarFixture(t,
		sbxReply{stderr: "Error: unknown flag: --url\n", exit: 1},
		sbxReply{stdout: `{"error":"gateway unreachable"}` + "\n", stderr: "fixture: something went wrong\n", exit: 1})
	primary := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	alt := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	out, errOut, err := runSbxGrammarFallbackAgainst(t, bin, primary, alt)
	if err == nil {
		t.Fatal("RunSbxGrammarFallback = nil, want an error (both grammars failed)")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v (%T), want *exec.ExitError", err, err)
	}
	if !strings.Contains(errOut.String(), "something went wrong") {
		t.Errorf("stderr = %q, want the retried (alt-grammar) attempt's own failure", errOut.String())
	}
	if !strings.Contains(out.String(), "gateway unreachable") {
		t.Errorf("stdout = %q, want the retried attempt's own stdout, kept separate from stderr", out.String())
	}
	if strings.Contains(out.String()+errOut.String(), "unknown flag") {
		t.Errorf("primary grammar's parser noise leaked through: out=%q errOut=%q", out.String(), errOut.String())
	}
}

func TestRunSbxGrammarFallback_SbxAbsentPrintsPrimaryWouldRun(t *testing.T) {
	var out, errOut bytes.Buffer
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	primary := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	alt := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	if err := RunSbxGrammarFallback(lookPath, &out, &errOut, primary, alt); !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("err = %v, want ErrSbxUnavailable", err)
	}
	if !strings.Contains(errOut.String(), "would run: sbx mcp bundle add pix-catalog --url") {
		t.Errorf("stderr = %q, want the primary (current) grammar's recovery command", errOut.String())
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
