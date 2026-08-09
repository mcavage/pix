package mcp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Covers AddRemoteServers, the one path that registers a hosted MCP server:
// evidence first (one bounded `sbx mcp ls`), classify every entry against it,
// and only then touch anything. The point of the classification is that it
// NEVER overwrites a server it does not own, which matters most for a name pix
// happens to know a URL for: your "notion" is not ours to replace.
//
// The sbx `mcp bundle` grammar-compatibility layer this file used to pin is
// gone with the `pix mcp bundle` verb it existed to serve.

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
// the ones it is entitled to (e.g. AddRemoteServers never issuing `mcp
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

// --- RunBundleRm -------------------------------------------------------------

// --- RunBundleLs -------------------------------------------------------------

// --- AddRemoteServers: never touches a pre-existing catalog entry --------

func TestAddRemoteServers_OnlyEverAdds(t *testing.T) {
	// The fixture defines ONLY `mcp add` cases; if AddRemoteServers ever
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
	if err := AddRemoteServers(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("AddRemoteServers = %v, want nil", err)
	}
	want := "added notion\nadded atlassian\nadded granola\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// --- Direct catalog fallback OWNERSHIP SAFETY: evidence-first classify -----
// --- before EVER mutating a same-named entry -------------------------------

// TestAddRemoteServers_LeavesExactMatchUnchanged: every catalog name is
// already registered at EXACTLY the shipped URL. AddRemoteServers must
// fetch registration evidence once (`mcp ls`), inspect each present name,
// and leave every one alone — no `mcp add` of any kind is a defined case, so
// any add attempt fails the fixture itself.
func TestAddRemoteServers_LeavesExactMatchUnchanged(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\natlassian\ngranola\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect atlassian", stdout: `{"url":"https://mcp.atlassian.com/v1/mcp"}` + "\n", exit: 0},
		{argv: "mcp inspect granola", stdout: `{"url":"https://mcp.granola.ai/mcp"}` + "\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	if err := AddRemoteServers(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("AddRemoteServers = %v, want nil", err)
	}
	for _, name := range []string{"notion", "atlassian", "granola"} {
		if !strings.Contains(out.String(), "already registered: "+name) {
			t.Errorf("stdout = %q, want it to report %q already registered", out.String(), name)
		}
	}
}

// TestAddRemoteServers_PartiallyMissingSetAddsOnlyMissing: notion is
// already registered at the shipped URL, atlassian and granola are absent.
// Only the two absent entries are added; notion is left alone (no `mcp add
// notion` case is defined — an attempt would fail the fixture).
func TestAddRemoteServers_PartiallyMissingSetAddsOnlyMissing(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://mcp.notion.com/mcp"}` + "\n", exit: 0},
		{argv: "mcp add atlassian --url https://mcp.atlassian.com/v1/mcp", stdout: "added atlassian\n", exit: 0},
		{argv: "mcp add granola --url https://mcp.granola.ai/mcp", stdout: "added granola\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	if err := AddRemoteServers(&out, &errOut, testCatalog); err != nil {
		t.Fatalf("AddRemoteServers = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "already registered: notion") {
		t.Errorf("stdout = %q, want notion reported already registered", out.String())
	}
	want := "added atlassian\nadded granola\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Errorf("stdout = %q, want it to end with %q", out.String(), want)
	}
}

// TestAddRemoteServers_CustomNotionEndpointFailsClosedWithoutOverwriting:
// notion is registered under a CUSTOM endpoint the user set up themselves.
// AddRemoteServers must FAIL CLOSED and never call `mcp add notion` — no
// such case is defined, so an overwrite attempt fails the fixture — and
// must never reach atlassian/granola either (no cases defined for them).
func TestAddRemoteServers_CustomNotionEndpointFailsClosedWithoutOverwriting(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"url":"https://notion.mycompany.internal/mcp"}` + "\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := AddRemoteServers(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("AddRemoteServers = nil, want a fail-closed error for notion's custom endpoint")
	}
	if !strings.Contains(err.Error(), "notion") || !strings.Contains(err.Error(), "different endpoint") {
		t.Errorf("err = %v, want it to name notion and explain the endpoint mismatch", err)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty (nothing was added)", out.String())
	}
}

// TestAddRemoteServers_CustomKindFailsClosed: notion is registered, but as
// a local command (no url/endpoint field at all) rather than the shipped
// remote URL — a different KIND of entry under the same name, not merely a
// different URL. It must classify the same as a custom endpoint: fail
// closed, never overwrite.
func TestAddRemoteServers_CustomKindFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stdout: `{"command":"/usr/local/bin/my-notion-bridge"}` + "\n", exit: 0},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := AddRemoteServers(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("AddRemoteServers = nil, want a fail-closed error for notion's custom kind")
	}
	if !strings.Contains(err.Error(), "notion") {
		t.Errorf("err = %v, want it to name notion", err)
	}
}

// TestAddRemoteServers_ListFailureFailsClosed: `sbx mcp ls` itself fails
// operationally. AddRemoteServers must fail closed WITHOUT attempting any
// add — no `mcp add`/`mcp inspect` case is defined, so any attempt fails the
// fixture.
func TestAddRemoteServers_ListFailureFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stderr: "gateway unreachable\n", exit: 1},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := AddRemoteServers(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("AddRemoteServers = nil, want a fail-closed error on an unreadable listing")
	}
	if !strings.Contains(err.Error(), "gateway unreachable") {
		t.Errorf("err = %v, want it to carry the real listing failure", err)
	}
}

// TestAddRemoteServers_UnreadableInspectionFailsClosed: notion IS listed as
// registered, but BOTH `mcp inspect` and `mcp get` fail operationally. This
// must never be read as "must be absent" (which would license an overwriting
// add) — it fails closed instead.
func TestAddRemoteServers_UnreadableInspectionFailsClosed(t *testing.T) {
	bin := installSbxScript(t, []argvReply{
		{argv: "mcp ls", stdout: "notion\n", exit: 0},
		{argv: "mcp inspect notion", stderr: "timed out\n", exit: 1},
		{argv: "mcp get notion", stderr: "timed out\n", exit: 1},
	})
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+oldPath)
	var out, errOut bytes.Buffer
	err := AddRemoteServers(&out, &errOut, testCatalog)
	if err == nil {
		t.Fatal("AddRemoteServers = nil, want a fail-closed error on an unreadable existing registration")
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty (nothing was added over an unverifiable entry)", out.String())
	}
}

// --- RunBundleRmDirect: only ever removes an exact shipped-URL match -------
