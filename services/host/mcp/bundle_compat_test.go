package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bundle_compat_test.go pins the sbx-v0.38 "no `mcp bundle` subcommand at
// all" compatibility contract: RunBundleAdd/RunBundleRm/RunBundleLs try
// sbx's NATIVE bundle grammar(s) first (old AND new — see
// grammar_fallback_test.go for that half), and fall back to a DIFFERENT
// command (`sbx mcp add`/`rm`/`ls`) ONLY when sbx's own parser says it does
// not recognize `mcp bundle` at all, never on an operational failure.

// --- McpCatalog is the single source of truth: anti-drift against the ------
// --- shipped config/mcp-catalog.bundle.json ---------------------------------

// catalogBundleJSONPath resolves config/mcp-catalog.bundle.json relative to
// THIS test file's own location (runtime.Caller), not the process's working
// directory — `go test` runs with the package dir as cwd, so a relative
// "../../../config/..." guess would silently break the moment this file
// moves, whereas walking up from the caller's own path never can.
func catalogBundleJSONPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// file = .../services/host/mcp/bundle_compat_test.go — up three dirs
	// (mcp -> host -> services) lands at the repo root.
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(file))))
	return filepath.Join(root, "config", "mcp-catalog.bundle.json")
}

// TestMcpCatalogMatchesShippedBundleJSON is the anti-drift test: McpCatalog
// (the Go source of truth RunBundleAdd/RunBundleRm's direct-add/rm fallback
// registers) must describe EXACTLY the same names, URLs, and order as the
// shipped config/mcp-catalog.bundle.json a consumer's native `sbx mcp bundle
// add pix-catalog --url <this file's raw URL>` fetches. Two independent
// lists that happen to agree today is how they drift silently tomorrow.
func TestMcpCatalogMatchesShippedBundleJSON(t *testing.T) {
	raw, err := os.ReadFile(catalogBundleJSONPath(t))
	if err != nil {
		t.Fatalf("reading config/mcp-catalog.bundle.json: %v", err)
	}
	var shipped []CatalogServer
	if err := json.Unmarshal(raw, &shipped); err != nil {
		t.Fatalf("parsing config/mcp-catalog.bundle.json: %v", err)
	}
	if len(shipped) != len(McpCatalog) {
		t.Fatalf("config/mcp-catalog.bundle.json has %d entries, McpCatalog has %d: %v vs %v",
			len(shipped), len(McpCatalog), shipped, McpCatalog)
	}
	for i, want := range shipped {
		got := McpCatalog[i]
		if got != want {
			t.Errorf("McpCatalog[%d] = %+v, want %+v (config/mcp-catalog.bundle.json)", i, got, want)
		}
	}
}

// TestMcpCatalogNamesDerivedFromCatalog: McpCatalogNames must contain
// EXACTLY McpCatalog's names — the classification map is a derived view, not
// an independently maintained list that could silently diverge from it.
func TestMcpCatalogNamesDerivedFromCatalog(t *testing.T) {
	if len(McpCatalogNames) != len(McpCatalog) {
		t.Fatalf("McpCatalogNames has %d entries, McpCatalog has %d", len(McpCatalogNames), len(McpCatalog))
	}
	for _, c := range McpCatalog {
		if !McpCatalogNames[c.Name] {
			t.Errorf("McpCatalogNames missing %q from McpCatalog", c.Name)
		}
	}
}

// --- shared fixture: a real "sbx" shell script answering exact argv --------

// argvReply pairs one exact `sbx <argv...>` invocation with its canned reply.
type argvReply struct {
	argv   string
	stdout string
	stderr string
	exit   int
}

// installSbxScript writes a real "sbx" shell script into a PATH-isolated dir
// answering exactly the argvs in cases (each on its own stdout/stderr, never
// merged — see replyBody); any OTHER argv is a hard test failure (exit 99),
// which is how these tests also prove a fallback never runs a command beyond
// the ones it is entitled to (e.g. RunBundleAddDirect never issuing `mcp
// rm`, or stopping after the first real failure instead of trying every
// catalog entry regardless).
func withSbxOnPath(t *testing.T, bin string) func(string) (string, error) {
	t.Helper()
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	return lookPathIn(filepath.Dir(bin))
}

func installSbxScript(t *testing.T, cases []argvReply) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncase \"$*\" in\n")
	for _, c := range cases {
		b.WriteString("'" + c.argv + "')\n")
		b.WriteString(replyBody(sbxReply{stdout: c.stdout, stderr: c.stderr, exit: c.exit}))
		b.WriteString("  ;;\n")
	}
	b.WriteString("*) echo \"fixture: unexpected argv: $*\" >&2; exit 99 ;;\n esac\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	testBundleName = "pix-catalog"
	testBundleURL  = "https://example.com/bundle.json"
)

var testCatalog = []CatalogServer{
	{Name: "notion", URL: "https://mcp.notion.com/mcp"},
	{Name: "atlassian", URL: "https://mcp.atlassian.com/v1/mcp"},
	{Name: "granola", URL: "https://mcp.granola.ai/mcp"},
}

// --- RunBundleAdd ------------------------------------------------------------

func TestRunBundleAdd_NativeSuccessPassthrough(t *testing.T) {
	// The OLD grammar (sbx still has `mcp bundle add`, --url flag form)
	// works: no fallback of any kind should fire. Only the primary argv has
	// a case, so a wrong fallback attempt fails the fixture itself.
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle add pix-catalog --url https://example.com/bundle.json",
			stdout: `{"registered":"pix-catalog"}` + "\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleAdd(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testBundleURL, testCatalog)
	if err != nil {
		t.Fatalf("RunBundleAdd = %v, want nil", err)
	}
	if out.String() != `{"registered":"pix-catalog"}`+"\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty (no compat note on a native success)", errOut.String())
	}
}

func TestRunBundleAdd_LegacyPositionalGrammarPassthrough(t *testing.T) {
	// The NEW-vs-OLD --url-flag-vs-positional split still exists on some sbx
	// builds (grammar_fallback_test.go's primary contract); RunBundleAdd
	// must resolve it the SAME way and never fall further to direct add.
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle add pix-catalog --url https://example.com/bundle.json",
			stderr: "Error: unknown flag: --url\n", exit: 1},
		{argv: "mcp bundle add pix-catalog https://example.com/bundle.json",
			stdout: `{"registered":"pix-catalog"}` + "\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleAdd(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testBundleURL, testCatalog)
	if err != nil {
		t.Fatalf("RunBundleAdd = %v, want nil", err)
	}
	if out.String() != `{"registered":"pix-catalog"}`+"\n" {
		t.Errorf("stdout = %q, want ONLY the alt attempt's stdout", out.String())
	}
	if strings.Contains(errOut.String(), "no `mcp bundle`") {
		t.Errorf("stderr = %q must not print the no-bundle compat note: bundle DOES exist here", errOut.String())
	}
}

func TestRunBundleAdd_OperationalFailureNeverFallsBack(t *testing.T) {
	// A real (auth) failure on the primary grammar must be reported as-is,
	// never retried, and never escalated to the direct-add fallback: only
	// `sys.IsUsageMismatch` licenses either kind of fallback.
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle add pix-catalog --url https://example.com/bundle.json",
			stderr: "401 Unauthorized: token expired\n", exit: 1},
	})
	var out, errOut bytes.Buffer
	err := RunBundleAdd(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testBundleURL, testCatalog)
	if err == nil {
		t.Fatal("RunBundleAdd = nil, want the primary attempt's own auth error")
	}
	if !strings.Contains(errOut.String(), "401 Unauthorized") {
		t.Errorf("stderr = %q, want the original auth failure", errOut.String())
	}
	if strings.Contains(errOut.String(), "no `mcp bundle`") {
		t.Errorf("stderr = %q must not claim sbx lacks `mcp bundle` on an operational failure", errOut.String())
	}
}

func TestRunBundleAdd_NoBundleSubcommandFallsBackToDirectAddInCatalogOrder(t *testing.T) {
	// sbx v0.38: BOTH grammars reject "bundle" itself, not just this argv
	// shape of it. RunBundleAdd must fall back to registering every catalog
	// entry individually, in order, via direct `sbx mcp add NAME --url URL`
	// — and never issue any `mcp rm` (no case defined for one; the fixture's
	// default branch would fail the test if it tried).
	bin := installSbxScript(t, []argvReply{
		{argv: `mcp bundle add pix-catalog --url https://example.com/bundle.json`,
			stderr: `Error: unknown command "bundle" for "sbx"`, exit: 1},
		{argv: `mcp bundle add pix-catalog https://example.com/bundle.json`,
			stderr: `Error: unknown command "bundle" for "sbx"`, exit: 1},
		{argv: "mcp ls", stdout: "slack\n", exit: 0},
		{argv: "mcp add notion --url https://mcp.notion.com/mcp",
			stdout: "added notion\n", exit: 0},
		{argv: "mcp add atlassian --url https://mcp.atlassian.com/v1/mcp",
			stdout: "added atlassian\n", exit: 0},
		{argv: "mcp add granola --url https://mcp.granola.ai/mcp",
			stdout: "added granola\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleAdd(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testBundleURL, testCatalog)
	if err != nil {
		t.Fatalf("RunBundleAdd = %v, want nil", err)
	}
	wantOut := "added notion\nadded atlassian\nadded granola\n"
	if out.String() != wantOut {
		t.Errorf("stdout = %q, want %q (exactly the three direct adds, in catalog order)", out.String(), wantOut)
	}
	if !strings.Contains(errOut.String(), "no `mcp bundle` subcommand") {
		t.Errorf("stderr = %q, want the honest compat note", errOut.String())
	}
	if !strings.Contains(errOut.String(), "registering the shipped catalog entries individually") {
		t.Errorf("stderr = %q, want it to say what it did instead", errOut.String())
	}
}

func TestRunBundleAdd_NoBundleSubcommandStopsAtFirstRealAddFailure(t *testing.T) {
	// notion succeeds, atlassian fails for a REAL reason: granola must never
	// be attempted (no case defined for it — an attempt would hit the
	// fixture's exit-99 default and fail this test).
	bin := installSbxScript(t, []argvReply{
		{argv: `mcp bundle add pix-catalog --url https://example.com/bundle.json`,
			stderr: `unrecognized command 'bundle'`, exit: 1},
		{argv: `mcp bundle add pix-catalog https://example.com/bundle.json`,
			stderr: `unrecognized command 'bundle'`, exit: 1},
		{argv: "mcp ls", stdout: "", exit: 0},
		{argv: "mcp add notion --url https://mcp.notion.com/mcp",
			stdout: "added notion\n", exit: 0},
		{argv: "mcp add atlassian --url https://mcp.atlassian.com/v1/mcp",
			stderr: "gateway unreachable\n", exit: 1},
	})
	var out, errOut bytes.Buffer
	err := RunBundleAdd(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testBundleURL, testCatalog)
	if err == nil {
		t.Fatal("RunBundleAdd = nil, want atlassian's real failure")
	}
	if out.String() != "added notion\n" {
		t.Errorf("stdout = %q, want ONLY notion's (granola must never be attempted)", out.String())
	}
	if !strings.Contains(errOut.String(), "gateway unreachable") {
		t.Errorf("stderr = %q, want atlassian's real failure", errOut.String())
	}
}

func TestRunBundleAdd_SbxAbsentPrintsPrimaryWouldRun(t *testing.T) {
	var out, errOut bytes.Buffer
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	err := RunBundleAdd(lookPath, &out, &errOut, testBundleName, testBundleURL, testCatalog)
	if !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("err = %v, want ErrSbxUnavailable", err)
	}
	if !strings.Contains(errOut.String(), "would run: sbx mcp bundle add pix-catalog --url") {
		t.Errorf("stderr = %q, want the native (current) grammar's recovery command", errOut.String())
	}
}

// --- RunBundleRm -------------------------------------------------------------

func TestRunBundleRm_NativeSuccessPassthrough(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle rm pix-catalog", stdout: "removed pix-catalog\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleRm(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testCatalog)
	if err != nil {
		t.Fatalf("RunBundleRm = %v, want nil", err)
	}
	if out.String() != "removed pix-catalog\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty on a native success", errOut.String())
	}
}

func TestRunBundleRm_OperationalFailureNeverFallsBack(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle rm pix-catalog", stderr: "403 Forbidden: policy denied\n", exit: 1},
	})
	var out, errOut bytes.Buffer
	err := RunBundleRm(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testCatalog)
	if err == nil {
		t.Fatal("RunBundleRm = nil, want the real policy-denial error")
	}
	if !strings.Contains(errOut.String(), "403 Forbidden") {
		t.Errorf("stderr = %q, want the original failure", errOut.String())
	}
	if strings.Contains(errOut.String(), "no `mcp bundle`") {
		t.Errorf("stderr = %q must not claim sbx lacks `mcp bundle` on an operational failure", errOut.String())
	}
}

func TestRunBundleRm_NoBundleSubcommandFallsBackToDirectRmInCatalogOrder(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle rm pix-catalog", stderr: `Error: unknown command "bundle" for "sbx"`, exit: 1},
		{argv: "mcp ls", stdout: "notion\natlassian\ngranola\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://mcp.atlassian.com/v1/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect granola", stdout: `{"url":"https://mcp.granola.ai/mcp"}` + "\n", exit: 0},
		{argv: "mcp rm notion", stdout: "removed notion\n", exit: 0},
		{argv: "mcp rm atlassian", stdout: "removed atlassian\n", exit: 0},
		{argv: "mcp rm granola", stdout: "removed granola\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleRm(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testCatalog)
	if err != nil {
		t.Fatalf("RunBundleRm = %v, want nil", err)
	}
	wantOut := "removed notion\nremoved atlassian\nremoved granola\n"
	if out.String() != wantOut {
		t.Errorf("stdout = %q, want %q", out.String(), wantOut)
	}
	if !strings.Contains(errOut.String(), "no `mcp bundle` subcommand") {
		t.Errorf("stderr = %q, want the honest compat note", errOut.String())
	}
	if !strings.Contains(errOut.String(), "removing the shipped catalog entries individually") {
		t.Errorf("stderr = %q, want it to say what it did instead", errOut.String())
	}
}

func TestRunBundleRm_NoBundleSubcommandStopsAtFirstRealRmFailure(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle rm pix-catalog", stderr: "unrecognized command 'bundle'", exit: 1},
		{argv: "mcp ls", stdout: "notion\natlassian\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://mcp.atlassian.com/v1/mcp"}` + "\n", exit: 0},
		{argv: "mcp rm notion", stdout: "removed notion\n", exit: 0},
		{argv: "mcp rm atlassian", stderr: "not found\n", exit: 1},
	})
	var out, errOut bytes.Buffer
	err := RunBundleRm(withSbxOnPath(t, bin), &out, &errOut, testBundleName, testCatalog)
	if err == nil {
		t.Fatal("RunBundleRm = nil, want atlassian's real failure")
	}
	if out.String() != "removed notion\n" {
		t.Errorf("stdout = %q, want ONLY notion's (granola must never be attempted)", out.String())
	}
}

func TestRunBundleRm_SbxAbsentPrintsWouldRun(t *testing.T) {
	var out, errOut bytes.Buffer
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	err := RunBundleRm(lookPath, &out, &errOut, testBundleName, testCatalog)
	if !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("err = %v, want ErrSbxUnavailable", err)
	}
	if !strings.Contains(errOut.String(), "would run: sbx mcp bundle rm pix-catalog") {
		t.Errorf("stderr = %q, want the native rm recovery command", errOut.String())
	}
}

// --- RunBundleLs -------------------------------------------------------------

func TestRunBundleLs_NativeSuccessPassthrough(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle ls", stdout: "pix-catalog: notion, atlassian, granola\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleLs(withSbxOnPath(t, bin), &out, nil, &errOut, testCatalog, nil)
	if err != nil {
		t.Fatalf("RunBundleLs = %v, want nil", err)
	}
	if out.String() != "pix-catalog: notion, atlassian, granola\n" {
		t.Errorf("stdout = %q", out.String())
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty on a native success", errOut.String())
	}
}

func TestRunBundleLs_NoBundleSubcommandMapsToRegistrations(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle ls", stderr: `Error: unknown command "bundle" for "sbx"`, exit: 1},
		{argv: "mcp ls", stdout: "notion\nslack\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleLs(withSbxOnPath(t, bin), &out, nil, &errOut, testCatalog, nil)
	if err != nil {
		t.Fatalf("RunBundleLs = %v, want nil", err)
	}
	if !strings.HasPrefix(out.String(), "notion\nslack\n") {
		t.Errorf("stdout = %q, want the mapped `sbx mcp ls` listing", out.String())
	}
	if !strings.Contains(out.String(), mcpLsAttachmentNote) {
		t.Errorf("stdout = %q, want the usual mcp ls attachment note appended (no extra args)", out.String())
	}
	if !strings.Contains(errOut.String(), "no `mcp bundle` subcommand") {
		t.Errorf("stderr = %q, want the honest compat note", errOut.String())
	}
	for _, name := range []string{"notion", "atlassian", "granola"} {
		if !strings.Contains(errOut.String(), name) {
			t.Errorf("stderr = %q, want it to name %q so the caller knows what to look for", errOut.String(), name)
		}
	}
}

func TestRunBundleLs_NoBundleSubcommandForwardsExtraArgsAndSkipsAttachmentNote(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle ls -o json", stderr: "unrecognized command 'bundle'", exit: 1},
		{argv: "mcp ls -o json", stdout: `["notion"]` + "\n", exit: 0},
	})
	var out, errOut bytes.Buffer
	err := RunBundleLs(withSbxOnPath(t, bin), &out, nil, &errOut, testCatalog, []string{"-o", "json"})
	if err != nil {
		t.Fatalf("RunBundleLs = %v, want nil", err)
	}
	if out.String() != `["notion"]`+"\n" {
		t.Errorf("stdout = %q, want ONLY the mapped machine-readable listing, no prose note", out.String())
	}
}

func TestRunBundleLs_OperationalFailureNeverFallsBack(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp bundle ls", stderr: "connection refused\n", exit: 1},
	})
	var out, errOut bytes.Buffer
	err := RunBundleLs(withSbxOnPath(t, bin), &out, nil, &errOut, testCatalog, nil)
	if err == nil {
		t.Fatal("RunBundleLs = nil, want the real connection failure")
	}
	if !strings.Contains(errOut.String(), "connection refused") {
		t.Errorf("stderr = %q, want the original failure", errOut.String())
	}
	if strings.Contains(errOut.String(), "no `mcp bundle`") {
		t.Errorf("stderr = %q must not claim sbx lacks `mcp bundle` on an operational failure", errOut.String())
	}
}

func TestRunBundleLs_SbxAbsentPrintsWouldRun(t *testing.T) {
	var out, errOut bytes.Buffer
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	err := RunBundleLs(lookPath, &out, nil, &errOut, testCatalog, nil)
	if !errors.Is(err, ErrSbxUnavailable) {
		t.Fatalf("err = %v, want ErrSbxUnavailable", err)
	}
	if !strings.Contains(errOut.String(), "would run: sbx mcp bundle ls") {
		t.Errorf("stderr = %q, want the native ls recovery command", errOut.String())
	}
}

// --- RunBundleAddDirect: never touches a pre-existing catalog entry --------

func TestRunBundleAddDirect_OnlyEverAdds(t *testing.T) {
	// The fixture defines ONLY `mcp add` cases; if RunBundleAddDirect ever
	// issued `mcp rm` (e.g. to "clean up" before re-adding) that call would
	// hit the fixture's exit-99 default and fail this test.
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "", exit: 0},
		{argv: "mcp add notion --url https://mcp.notion.com/mcp", stdout: "added notion\n", exit: 0},
		{argv: "mcp add atlassian --url https://mcp.atlassian.com/v1/mcp", stdout: "added atlassian\n", exit: 0},
		{argv: "mcp add granola --url https://mcp.granola.ai/mcp", stdout: "added granola\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	if err := RunBundleAddDirect(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("RunBundleAddDirect = %v, want nil", err)
	}
	want := "added notion\nadded atlassian\nadded granola\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// --- Direct catalog fallback OWNERSHIP SAFETY: evidence-first classify -----
// --- before EVER mutating a same-named entry -------------------------------

// TestRunBundleAddDirect_LeavesExactMatchUnchanged: every catalog name is
// already registered at EXACTLY the shipped URL. RunBundleAddDirect must
// fetch registration evidence once (`mcp ls`), inspect each present name,
// and leave every one alone — no `mcp add` of any kind is a defined case, so
// any add attempt fails the fixture itself.
func TestRunBundleAddDirect_LeavesExactMatchUnchanged(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\natlassian\ngranola\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://mcp.atlassian.com/v1/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect granola", stdout: `{"url":"https://mcp.granola.ai/mcp"}` + "\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	if err := RunBundleAddDirect(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("RunBundleAddDirect = %v, want nil", err)
	}
	for _, name := range []string{"notion", "atlassian", "granola"} {
		if !strings.Contains(out.String(), "already registered: "+name) {
			t.Errorf("stdout = %q, want it to report %q already registered", out.String(), name)
		}
	}
}

// TestRunBundleAddDirect_PartiallyMissingSetAddsOnlyMissing: notion is
// already registered at the shipped URL, atlassian and granola are absent.
// Only the two absent entries are added; notion is left alone (no `mcp add
// notion` case is defined — an attempt would fail the fixture).
func TestRunBundleAddDirect_PartiallyMissingSetAddsOnlyMissing(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp add atlassian --url https://mcp.atlassian.com/v1/mcp", stdout: "added atlassian\n", exit: 0},
		{argv: "mcp add granola --url https://mcp.granola.ai/mcp", stdout: "added granola\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	if err := RunBundleAddDirect(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("RunBundleAddDirect = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "already registered: notion") {
		t.Errorf("stdout = %q, want notion reported already registered", out.String())
	}
	want := "added atlassian\nadded granola\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Errorf("stdout = %q, want it to end with %q", out.String(), want)
	}
}

// TestRunBundleAddDirect_CustomNotionEndpointFailsClosedWithoutOverwriting:
// notion is registered under a CUSTOM endpoint the user set up themselves.
// RunBundleAddDirect must FAIL CLOSED and never call `mcp add notion` — no
// such case is defined, so an overwrite attempt fails the fixture — and
// must never reach atlassian/granola either (no cases defined for them).
func TestRunBundleAddDirect_CustomNotionEndpointFailsClosedWithoutOverwriting(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://notion.mycompany.internal/mcp"}` + "\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := RunBundleAddDirect(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("RunBundleAddDirect = nil, want a fail-closed error for notion's custom endpoint")
	}
	if !strings.Contains(err.Error(), "notion") || !strings.Contains(err.Error(), "different endpoint") {
		t.Errorf("err = %v, want it to name notion and explain the endpoint mismatch", err)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty (nothing was added)", out.String())
	}
}

// TestRunBundleAddDirect_CustomKindFailsClosed: notion is registered, but as
// a local command (no url/endpoint field at all) rather than the shipped
// remote URL — a different KIND of entry under the same name, not merely a
// different URL. It must classify the same as a custom endpoint: fail
// closed, never overwrite.
func TestRunBundleAddDirect_CustomKindFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"command":"/usr/local/bin/my-notion-bridge"}` + "\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := RunBundleAddDirect(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("RunBundleAddDirect = nil, want a fail-closed error for notion's custom kind")
	}
	if !strings.Contains(err.Error(), "notion") {
		t.Errorf("err = %v, want it to name notion", err)
	}
}

// TestRunBundleAddDirect_ListFailureFailsClosed: `sbx mcp ls` itself fails
// operationally. RunBundleAddDirect must fail closed WITHOUT attempting any
// add — no `mcp add`/`mcp inspect` case is defined, so any attempt fails the
// fixture.
func TestRunBundleAddDirect_ListFailureFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stderr: "gateway unreachable\n", exit: 1},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := RunBundleAddDirect(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("RunBundleAddDirect = nil, want a fail-closed error on an unreadable listing")
	}
	if !strings.Contains(err.Error(), "gateway unreachable") {
		t.Errorf("err = %v, want it to carry the real listing failure", err)
	}
}

// TestRunBundleAddDirect_UnreadableInspectionFailsClosed: notion IS listed as
// registered, but BOTH `mcp inspect` and `mcp get` fail operationally. This
// must never be read as "must be absent" (which would license an overwriting
// add) — it fails closed instead.
func TestRunBundleAddDirect_UnreadableInspectionFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stderr: "timed out\n", exit: 1},
		{argv: "mcp get notion", stderr: "timed out\n", exit: 1},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := RunBundleAddDirect(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("RunBundleAddDirect = nil, want a fail-closed error on an unreadable existing registration")
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty (nothing was added over an unverifiable entry)", out.String())
	}
}

// --- RunBundleRmDirect: only ever removes an exact shipped-URL match -------

// TestRunBundleRmDirect_RemovesOnlyExactMatches: the full catalog set is
// registered at exactly the shipped URLs. Every entry is removed.
func TestRunBundleRmDirect_RemovesOnlyExactMatches(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\natlassian\ngranola\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://mcp.atlassian.com/v1/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect granola", stdout: `{"url":"https://mcp.granola.ai/mcp"}` + "\n", exit: 0},
		{argv: "mcp rm notion", stdout: "removed notion\n", exit: 0},
		{argv: "mcp rm atlassian", stdout: "removed atlassian\n", exit: 0},
		{argv: "mcp rm granola", stdout: "removed granola\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	if err := RunBundleRmDirect(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("RunBundleRmDirect = %v, want nil", err)
	}
	want := "removed notion\nremoved atlassian\nremoved granola\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// TestRunBundleRmDirect_SkipsAbsentAndCustomEntries: notion is absent,
// atlassian is registered under a CUSTOM endpoint, granola matches the
// shipped URL exactly. Only granola is removed; no `mcp rm notion` or `mcp
// rm atlassian` case is defined, so either attempt would fail the fixture.
func TestRunBundleRmDirect_SkipsAbsentAndCustomEntries(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "atlassian\ngranola\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://atlassian.mycompany.internal/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect granola", stdout: `{"url":"https://mcp.granola.ai/mcp"}` + "\n", exit: 0},
		{argv: "mcp rm granola", stdout: "removed granola\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	if err := RunBundleRmDirect(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("RunBundleRmDirect = %v, want nil", err)
	}
	if out.String() != "removed granola\n" {
		t.Errorf("stdout = %q, want ONLY granola's removal (notion absent, atlassian custom)", out.String())
	}
}

// TestRunBundleRmDirect_RerunnableAfterFirstError: the first run removes
// notion, then hits a REAL failure removing atlassian and stops (granola
// never attempted). A SECOND run against updated evidence — notion now
// genuinely absent, atlassian still an exact match, granola an exact match —
// must pick up cleanly: it must never re-attempt `mcp rm notion` (no case
// defined for a second removal) and must finish removing atlassian and
// granola.
func TestRunBundleRmDirect_RerunnableAfterFirstError(t *testing.T) {
	firstRun := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\natlassian\ngranola\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://mcp.atlassian.com/v1/mcp"}` + "\n", exit: 0},
		{argv: "mcp rm notion", stdout: "removed notion\n", exit: 0},
		{argv: "mcp rm atlassian", stderr: "gateway unreachable\n", exit: 1},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(firstRun)+string(os.PathListSeparator)+oldPath)
	var out1, errOut1 bytes.Buffer
	err := RunBundleRmDirect(&out1, &errOut1, testCatalog)
	if err == nil {
		t.Fatal("first RunBundleRmDirect = nil, want atlassian's real failure")
	}
	if out1.String() != "removed notion\n" {
		t.Fatalf("first run stdout = %q, want ONLY notion's removal (granola never attempted)", out1.String())
	}

	// Second run: fresh evidence reflects notion is now genuinely gone.
	secondRun := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "atlassian\ngranola\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://mcp.atlassian.com/v1/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect granola", stdout: `{"url":"https://mcp.granola.ai/mcp"}` + "\n", exit: 0},
		{argv: "mcp rm atlassian", stdout: "removed atlassian\n", exit: 0},
		{argv: "mcp rm granola", stdout: "removed granola\n", exit: 0},
	})
	t.Setenv("PATH", filepath.Dir(secondRun)+string(os.PathListSeparator)+oldPath)
	var out2, errOut2 bytes.Buffer
	if err := RunBundleRmDirect(&out2, &errOut2, testCatalog); err != nil {
		t.Fatalf("second RunBundleRmDirect = %v, want nil (cleanup completes on rerun)", err)
	}
	want := "removed atlassian\nremoved granola\n"
	if out2.String() != want {
		t.Errorf("second run stdout = %q, want %q (notion never re-attempted)", out2.String(), want)
	}
}

// TestRunBundleRmDirect_ListFailureFailsClosed: `sbx mcp ls` fails
// operationally. RunBundleRmDirect must fail closed WITHOUT removing
// anything — no `mcp rm`/`mcp inspect` case is defined, so any attempt fails
// the fixture.
func TestRunBundleRmDirect_ListFailureFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stderr: "connection refused\n", exit: 1},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := RunBundleRmDirect(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("RunBundleRmDirect = nil, want a fail-closed error on an unreadable listing")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want it to carry the real listing failure", err)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty (nothing was removed)", out.String())
	}
}

// TestRunBundleRmDirect_UnreadableInspectionFailsClosed: atlassian IS listed
// as registered, but BOTH `mcp inspect` and `mcp get` fail operationally.
// This must never be read as "must be absent" or "must be custom" (either
// of which would silently skip removing — or wrongly attempt to remove — a
// server this host may actually own): it fails closed instead.
func TestRunBundleRmDirect_UnreadableInspectionFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "atlassian\n", exit: 0},
		{argv: "mcp inspect atlassian", stderr: "timed out\n", exit: 1},
		{argv: "mcp get atlassian", stderr: "timed out\n", exit: 1},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := RunBundleRmDirect(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("RunBundleRmDirect = nil, want a fail-closed error on an unreadable existing registration")
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty (nothing was removed)", out.String())
	}
}
