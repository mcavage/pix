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

// --- RunSbxGrammarFallback: real exec, real grammar mismatch ---------------

// installSbxGrammarFixture writes a real "sbx" shell script into a
// PATH-isolated dir. It answers the OLD `--url`-flag grammar with rejectOld
// (exit 1) and the NEW positional grammar with acceptNew (exit 0) — the exact
// shape RunSbxGrammarFallback must retry through.
func installSbxGrammarFixture(t *testing.T, oldReply, newReply string, oldExit, newExit int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"'mcp bundle add pix-catalog --url https://example.com/bundle.json')\n" +
		"  printf %s " + shQuote(oldReply) + "\n" +
		"  exit " + itoa(oldExit) + "\n" +
		"  ;;\n" +
		"'mcp bundle add pix-catalog https://example.com/bundle.json')\n" +
		"  printf %s " + shQuote(newReply) + "\n" +
		"  exit " + itoa(newExit) + "\n" +
		"  ;;\n" +
		"*) echo \"fixture: unexpected argv: $*\" >&2; exit 99 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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
	bin := installSbxGrammarFixture(t,
		"Error: unknown flag: --url\nUsage:\n  sbx mcp bundle add NAME URL", "registered: pix-catalog",
		1, 0)
	primary := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	alt := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	out, errOut, err := runSbxGrammarFallbackAgainst(t, bin, primary, alt)
	if err != nil {
		t.Fatalf("RunSbxGrammarFallback = %v, want nil (fallback should have succeeded)", err)
	}
	if !strings.Contains(out.String(), "registered: pix-catalog") {
		t.Errorf("stdout = %q, want the fallback attempt's own output", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want empty on eventual success", errOut.String())
	}
}

func TestRunSbxGrammarFallback_NeverRetriesOperationalFailure(t *testing.T) {
	// The primary grammar fails for a REAL reason (an expired/missing auth
	// token) — not a grammar mismatch. Even though the alternate grammar
	// would succeed if tried (oldExit=1 with an auth message, newExit=0), the
	// retry must never fire: a wrong retry here would silently mask an auth
	// gap as success.
	bin := installSbxGrammarFixture(t, "401 Unauthorized: token expired", "registered: pix-catalog", 1, 0)
	primary := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	alt := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	_, errOut, err := runSbxGrammarFallbackAgainst(t, bin, primary, alt)
	if err == nil {
		t.Fatal("RunSbxGrammarFallback = nil, want the primary attempt's own error (no grammar retry for an auth failure)")
	}
	if !strings.Contains(errOut.String(), "401 Unauthorized") {
		t.Errorf("stderr = %q, want the ORIGINAL auth failure, not a retried/rewritten one", errOut.String())
	}
}

func TestRunSbxGrammarFallback_BothGrammarsFailingReportsTheRetry(t *testing.T) {
	// A recognized usage mismatch DOES license one retry; if the alternate
	// grammar also fails (for whatever reason), the bounded retry stops there
	// — no further guessing — and reports that final attempt.
	bin := installSbxGrammarFixture(t,
		"Error: unknown flag: --url", "fixture: something went wrong", 1, 1)
	primary := BundleAddArgs("pix-catalog", "https://example.com/bundle.json")
	alt := BundleAddArgsPositional("pix-catalog", "https://example.com/bundle.json")
	_, errOut, err := runSbxGrammarFallbackAgainst(t, bin, primary, alt)
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
