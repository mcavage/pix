package main

import (
	"bytes"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/workflow/doctor"
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
	o, err := launch.ParseRunArgs([]string{"help"})
	if err == nil {
		t.Fatalf("launch.ParseRunArgs([help]) succeeded (workspace=%q) — should reject a non-dir", o.Workspace)
	}
	if !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), "pix help") {
		t.Errorf("error = %q, want a not-a-directory + `pix help` hint", err)
	}

	// A non-verb typo just reports not-a-directory.
	if _, err := launch.ParseRunArgs([]string{"nonexistent-xyz-123"}); err == nil {
		t.Error("launch.ParseRunArgs([nonexistent]) succeeded — should reject a non-dir")
	}
}

func TestParseRunArgs_ExistingDirOK(t *testing.T) {
	dir := t.TempDir()
	o, err := launch.ParseRunArgs([]string{dir})
	if err != nil {
		t.Fatalf("launch.ParseRunArgs(%q) error: %v", dir, err)
	}
	if o.Workspace != dir {
		t.Errorf("workspace = %q, want %q", o.Workspace, dir)
	}
	// The cwd default is always launchable.
	if _, err := launch.ParseRunArgs(nil); err != nil {
		t.Errorf("launch.ParseRunArgs(nil) error: %v", err)
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

// --- B3: `run --help` is a help request (sentinel), not an error ---

func TestParseRunArgs_HelpSentinel(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"--help", "extra"}} {
		if _, err := launch.ParseRunArgs(argv); err != cli.ErrHelpRequested {
			t.Errorf("launch.ParseRunArgs(%v) err = %v, want cli.ErrHelpRequested", argv, err)
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

func TestVerbUsage_Routing(t *testing.T) {
	// Typed verbs are absent on purpose: they have no usage constant, and
	// runHelp re-enters the root as `<verb> --help` for them instead.
	for _, verb := range []string{"run", "config", "status", "doctor", "setup"} {
		u, ok := verbUsage(verb)
		if !ok || strings.TrimSpace(u) == "" {
			t.Errorf("verbUsage(%q) = (%q,%v), want non-empty usage", verb, u, ok)
		}
	}
	// A typed verb reached through the last passthrough bridge routes to its
	// GENERATED usage, never a constant.
	for _, verb := range []string{"memory", "mem", "pack", "mcp"} {
		u, ok := verbUsage(verb)
		if !ok || !strings.Contains(u, "Usage: pix ") {
			t.Errorf("verbUsage(%q) = (%q,%v), want generated usage", verb, u, ok)
		}
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

func TestParseStatusArgs(t *testing.T) {
	if j, err := doctor.ParseStatusArgs([]string{"--json"}); err != nil || !j {
		t.Errorf("--json = (%v,%v), want (true,nil)", j, err)
	}
	if j, err := doctor.ParseStatusArgs(nil); err != nil || j {
		t.Errorf("no args = (%v,%v), want (false,nil)", j, err)
	}
	if _, err := doctor.ParseStatusArgs([]string{"--help"}); err != cli.ErrHelpRequested {
		t.Errorf("--help err = %v, want cli.ErrHelpRequested", err)
	}
	if _, err := doctor.ParseStatusArgs([]string{"--jsom"}); err == nil {
		t.Error("--jsom (typo) should be a usage error")
	}
}

func TestParseDoctorArgs(t *testing.T) {
	if j, v, err := doctor.ParseDoctorArgs([]string{"--json"}); err != nil || !j || v {
		t.Errorf("--json = (%v,%v,%v), want (true,false,nil)", j, v, err)
	}
	if j, v, err := doctor.ParseDoctorArgs([]string{"--verbose"}); err != nil || j || !v {
		t.Errorf("--verbose = (%v,%v,%v), want (false,true,nil)", j, v, err)
	}
	if _, _, err := doctor.ParseDoctorArgs([]string{"--help"}); err != cli.ErrHelpRequested {
		t.Errorf("--help err = %v, want cli.ErrHelpRequested", err)
	}
	if _, _, err := doctor.ParseDoctorArgs([]string{"--bogus"}); err == nil {
		t.Error("--bogus should be a usage error")
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

// TestRunVerb_HelpPrintsUsage is the F1 gate: `run --help` prints run usage and
// returns. `run` NEVER onboards (onboarding is opt-in and in-session), so there
// is no first-run hook to reach; a help request just short-circuits to usage.
func TestRunVerb_HelpPrintsUsage(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"somedir", "--help"}} {
		old := os.Stdout
		rp, wp, _ := os.Pipe()
		os.Stdout = wp
		runVerb(argv)
		_ = wp.Close()
		os.Stdout = old
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rp)

		if !strings.Contains(buf.String(), "usage: pix run") {
			t.Errorf("runVerb(%v) = %q, want run usage", argv, buf.String())
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
	for _, sub := range []string{"register", "ls", "load", "auth", "bundle"} {
		if !strings.Contains(out.String(), sub) {
			t.Errorf("mcp --help missing %q:\n%s", sub, out.String())
		}
	}
}
