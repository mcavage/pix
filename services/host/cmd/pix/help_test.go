package main

import (
	"bytes"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/mcp"
	"pix/host/memory"
	"pix/host/rpc"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
	"strings"
	"testing"
)

// --- wantsHelp: the shared -h/--help contract ---

func TestWantsHelp(t *testing.T) {
	yes := [][]string{
		{"-h"}, {"--help"}, {"recall", "--help"}, {"set", "-h", "x"},
	}
	for _, argv := range yes {
		if !cli.WantsHelp(argv) {
			t.Errorf("cli.WantsHelp(%v) = false, want true", argv)
		}
	}
	no := [][]string{
		nil, {"recall", "q"}, {"--json"}, {"--", "--help"}, {"set", "--", "-h"},
	}
	for _, argv := range no {
		if cli.WantsHelp(argv) {
			t.Errorf("cli.WantsHelp(%v) = true, want false", argv)
		}
	}
}

// --- B1: `run <non-dir>` must not launch ---

func TestParseRunArgs_NonDirWorkspaceRejected(t *testing.T) {
	// A known verb typo suggests the verb (and never launches).
	o, err := parseRunOpts([]string{"help"})
	if err == nil {
		t.Fatalf("run help succeeded (workspace=%q) — should reject a non-dir", o.Workspace)
	}
	if !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), "pix help") {
		t.Errorf("error = %q, want a not-a-directory + `pix help` hint", err)
	}

	// A non-verb typo just reports not-a-directory.
	if _, err := parseRunOpts([]string{"nonexistent-xyz-123"}); err == nil {
		t.Error("run nonexistent succeeded — should reject a non-dir")
	}
}

func TestParseRunArgs_ExistingDirOK(t *testing.T) {
	dir := t.TempDir()
	o, err := parseRunOpts([]string{dir})
	if err != nil {
		t.Fatalf("run %q error: %v", dir, err)
	}
	if o.Workspace != dir {
		t.Errorf("workspace = %q, want %q", o.Workspace, dir)
	}
	// The cwd default is always launchable.
	if _, err := parseRunOpts(nil); err != nil {
		t.Errorf("bare run error: %v", err)
	}
}

func TestValidateRunWorkspace(t *testing.T) {
	if err := launch.ValidateRunWorkspace("."); err != nil {
		t.Errorf("cwd default should validate: %v", err)
	}
	dir := t.TempDir()
	if err := launch.ValidateRunWorkspace(dir); err != nil {
		t.Errorf("existing dir should validate: %v", err)
	}
	// A regular file is not a directory.
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := launch.ValidateRunWorkspace(f); err == nil {
		t.Error("a regular file should not validate as a workspace")
	}
}

// --- B3: `run --help` is a help request (generated usage), not an error ---

func TestRunHelpIsGeneratedUsage(t *testing.T) {
	for _, argv := range [][]string{{"run", "--help"}, {"run", "-h"}, {"run", "--help", "extra"}} {
		d, out, errb := rootDeps()
		if code := dispatch(argv, d); code != 0 {
			t.Errorf("dispatch(%v) = %d, want 0 (stderr: %s)", argv, code, errb.String())
		}
		if !strings.Contains(out.String(), "Usage: pix run") {
			t.Errorf("dispatch(%v) stdout = %q, want the generated run usage", argv, out.String())
		}
	}
}

// --- S1: cli.FlagSet help bool + dispatch help routing ---

func TestFlagSetHelp(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"q", "--help"}} {
		fs := cli.NewFlagSet()
		if _, err := fs.Parse(argv); err != nil {
			t.Fatalf("parse(%v): %v", argv, err)
		}
		if !fs.Help {
			t.Errorf("parse(%v) did not set fs.Help", argv)
		}
	}
	// No help token -> help stays false.
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	if _, err := fs.Parse([]string{"q", "--json"}); err != nil {
		t.Fatal(err)
	}
	if fs.Help {
		t.Error("fs.Help set without a help token")
	}
}

// TestFlagSetJSONOptIn proves --json is a recognized flag ONLY after
// enableJSON(); on a command that never opts in it is an unknown-flag usage
// error rather than a silently-swallowed no-op.
func TestFlagSetJSONOptIn(t *testing.T) {
	// Not enabled: --json is rejected.
	fs := cli.NewFlagSet()
	if _, err := fs.Parse([]string{"--json"}); !cli.IsUsage(err) {
		t.Errorf("parse([--json]) without enableJSON = %v, want usage error", err)
	}
	// Enabled: --json is accepted and sets fs.Json.
	fs = cli.NewFlagSet()
	fs.EnableJSON()
	if _, err := fs.Parse([]string{"--json"}); err != nil {
		t.Fatalf("parse([--json]) with enableJSON: %v", err)
	}
	if !fs.Json {
		t.Error("enableJSON()+--json did not set fs.Json")
	}
}

func TestMemoryHelp_NoRPC(t *testing.T) {
	// `memory recall --help` must print usage and NOT hit the (down) daemon.
	var out bytes.Buffer
	if err := memory.Dispatch("recall", []string{"--help"}, rpc.Client{Port: 1}, &out, "default"); err != nil {
		t.Fatalf("memory recall --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pix memory recall") {
		t.Errorf("expected recall usage, got %q", out.String())
	}
	// stats/learnings likewise.
	out.Reset()
	if err := memory.Dispatch("stats", []string{"--help"}, rpc.Client{Port: 1}, &out, "default"); err != nil {
		t.Fatalf("memory stats --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pix memory stats") {
		t.Errorf("expected stats usage, got %q", out.String())
	}
}

func TestVerbUsage_Routing(t *testing.T) {
	// The MIGRATED verbs are absent on purpose: they have no usage constant,
	// and `pix help <typed verb>` re-enters the root as `<verb> --help`.
	for _, verb := range []string{"memory", "config", "pack", "mcp"} {
		u, ok := verbUsage(verb)
		if !ok || strings.TrimSpace(u) == "" {
			t.Errorf("verbUsage(%q) = (%q,%v), want non-empty usage", verb, u, ok)
		}
	}
	// Aliases route too.
	if _, ok := verbUsage("mem"); !ok {
		t.Error("verbUsage(mem) should route to memory usage")
	}
	// Unknown verb: no usage.
	if _, ok := verbUsage("frobnicate"); ok {
		t.Error("verbUsage(frobnicate) should be unknown")
	}
}

// TestHelpAll_ListsExpertVerbs: `help --all` must name the rare/expert verbs the
// curated Core listing hides.
func TestHelpAll_ListsExpertVerbs(t *testing.T) {
	for _, v := range []string{"mcp", "secret", "state", "reset", "task", "version"} {
		if !strings.Contains(helpAll(), v) {
			t.Errorf("help --all missing %q", v)
		}
	}
}

// TestSuggestVerb: a near-miss typo suggests the closest verb; a far-off input
// yields no suggestion.
func TestSuggestVerb(t *testing.T) {
	if s, ok := suggestVerb("memoyr"); !ok || s != "memory" {
		t.Errorf("suggestVerb(memoyr) = %q,%v, want memory,true", s, ok)
	}
	if s, ok := suggestVerb("stat"); !ok || (s != "status" && s != "state") {
		t.Errorf("suggestVerb(stat) = %q,%v, want status|state,true", s, ok)
	}
	if s, ok := suggestVerb("zzzzzzzz"); ok {
		t.Errorf("suggestVerb(zzzzzzzz) = %q,%v, want no suggestion", s, ok)
	}
}

// --- S2: status + doctor flag validation ---

// TestStatusAndDoctorFlagsAreTyped: --json/--verbose are struct fields now, so
// the flags that parse and the flags that are documented are one declaration —
// and a typo is a usage error (exit 2) instead of a silently ignored token.
func TestStatusAndDoctorFlagsAreTyped(t *testing.T) {
	root, err := parseRoot([]string{"status", "--json"})
	if err != nil || !root.Status.JSON {
		t.Errorf("status --json: json=%v err=%v", root.Status.JSON, err)
	}
	root, err = parseRoot([]string{"doctor", "--json", "--verbose"})
	if err != nil || !root.Doctor.JSON || !root.Doctor.Verbose {
		t.Errorf("doctor --json --verbose: %+v err=%v", root.Doctor, err)
	}
	for _, argv := range [][]string{{"status", "--jsom"}, {"doctor", "--bogus"}} {
		d, _, errb := rootDeps()
		if code := dispatch(argv, d); code != 2 {
			t.Errorf("dispatch(%v) = %d, want 2 (usage error)", argv, code)
		}
		if !strings.Contains(errb.String(), "unknown flag") {
			t.Errorf("dispatch(%v) stderr = %q, want an unknown-flag message", argv, errb.String())
		}
	}
}

func TestParseOnboardArgs_Help(t *testing.T) {
	o, err := onboard.ParseOnboardArgs([]string{"--help"})
	if err != nil || !o.Help {
		t.Errorf("onboard.ParseOnboardArgs([--help]) = (%+v,%v), want help=true,nil", o, err)
	}
	if _, err := onboard.ParseOnboardArgs([]string{"--bogus"}); err == nil {
		t.Error("--bogus should be a usage error")
	}
}

func TestMCPUsageListsEverySubcommand(t *testing.T) {
	const want = "usage: pix mcp <register|ls|load|auth|bundle> [args]"
	if !strings.Contains(mcp.McpUsage, want) {
		t.Fatalf("mcp usage synopsis missing subcommands: %q", mcp.McpUsage)
	}
}
