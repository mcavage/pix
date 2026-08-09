package main

import (
	"bytes"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/workflow/launch"
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
	if err := launch.ValidateRunWorkspace(".", knownVerb); err != nil {
		t.Errorf("cwd default should validate: %v", err)
	}
	dir := t.TempDir()
	if err := launch.ValidateRunWorkspace(dir, knownVerb); err != nil {
		t.Errorf("existing dir should validate: %v", err)
	}
	// A regular file is not a directory.
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := launch.ValidateRunWorkspace(f, knownVerb); err == nil {
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
	// `memory recall --help` prints kong's generated usage and NEVER opens a
	// session, so a down daemon is irrelevant to a help request. The tree is
	// driven the way the root drives it, so this asserts the shipped path.
	for _, sub := range []string{"recall", "stats"} {
		var out bytes.Buffer
		d := &cli.Deps{Out: &out, Err: &out}
		if err := cli.RunRoot[memoryCmd]("pix memory", "", "", []string{sub, "--help"}, d); err != nil {
			t.Fatalf("memory %s --help: %v", sub, err)
		}
		if !strings.Contains(out.String(), "Usage: pix memory "+sub) {
			t.Errorf("expected %s usage, got %q", sub, out.String())
		}
	}
}

// TestHelpVerb_RoutesToGeneratedUsage: every root verb is TYPED now, so no
// usage constant is left to route to — `pix help <verb>` re-enters the root as
// `<verb> --help` and prints the usage generated from the same tags that parse
// it. An unknown verb falls through to the tiered screen at exit 0, because
// `pix help <anything>` is a question, never a mistake.
func TestHelpVerb_RoutesToGeneratedUsage(t *testing.T) {
	for _, verb := range []string{"run", "config", "status", "doctor", "setup", "memory", "mem", "pack", "mcp"} {
		d, out, errb := rootDeps()
		if code := dispatch([]string{"help", verb}, d); code != 0 {
			t.Errorf("pix help %s = %d, want 0 (stderr: %s)", verb, code, errb.String())
		}
		if !strings.Contains(out.String(), "Usage: pix ") {
			t.Errorf("pix help %s = %q, want generated usage", verb, out.String())
		}
	}
	// An unknown verb is not an error: it gets the tiered screen.
	d, out, _ := rootDeps()
	if code := dispatch([]string{"help", "frobnicate"}, d); code != 0 {
		t.Errorf("pix help frobnicate = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:  pix <command>") {
		t.Errorf("pix help frobnicate = %q, want the tiered help screen", out.String())
	}
}

// TestHelpAll_ListsExpertVerbs: `help --all` must name the rare/expert verbs the
// curated Core listing hides.
func TestHelpAll_ListsExpertVerbs(t *testing.T) {
	for _, v := range []string{"mcp", "secret", "task", "version"} {
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
	if s, ok := suggestVerb("stat"); !ok || (s != "status" && s != "st") {
		t.Errorf("suggestVerb(stat) = %q,%v, want status|st,true", s, ok)
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

func TestMCPHelpListsEverySubcommand(t *testing.T) {
	// The synopsis is GENERATED from the tree that dispatches, so this asserts
	// the tree, not a constant that has to be kept in step with it.
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	if err := cli.RunRoot[mcpCmd]("pix mcp", "", "", []string{"--help"}, d); err != nil {
		t.Fatalf("mcp --help: %v", err)
	}
	for _, sub := range []string{"add", "ls", "auth"} {
		if !strings.Contains(out.String(), sub) {
			t.Errorf("mcp --help missing %q:\n%s", sub, out.String())
		}
	}
	// The three verbs that were cut. `register` vs a native add was a
	// distinction only the implementation cared about, `bundle` hardcoded three
	// SaaS vendors, and `load` attached to a running sandbox, which a recreate
	// does in a stack whose sandboxes are disposable. A user should not have to
	// pick between six verbs to register one server.
	for _, gone := range []string{"register", "bundle", "load"} {
		if strings.Contains(out.String(), gone+" ") {
			t.Errorf("mcp --help still offers the removed verb %q:\n%s", gone, out.String())
		}
	}
}
