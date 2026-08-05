package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/hostenv/hostenvtest"
	"pix/host/mcp"
	"pix/host/memory"
	"pix/host/rpc"
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
	for _, verb := range []string{"run", "memory", "config", "status", "pack", "doctor", "mcp", "serve", "setup", "version", "reset"} {
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
		if !strings.Contains(helpAllText, v) {
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

// TestDoctorJSONView round-trips the doctor report through the REAL serializer
// and asserts the JSON actually carries the groups, their checks, and each
// check's ok|todo|info state (not merely that it is valid JSON). The fixture is
// a MIXED environment (sbx + keys present, ollama absent, no MCP) so all three
// states appear: ok (providers/memory), todo (models/gog CLI), info (empty mcp).
func TestDoctorJSONView(t *testing.T) {
	f := hostenvtest.Env{
		Present: map[string]bool{"sbx": true}, // ollama + gog absent -> TODOs
		Output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "google-workspace\n",
		},
		Ports: map[int]bool{11435: true}, // memory up -> an OK check
	}
	cfg := defaultCfg()
	r := doctor.RunDoctor(cfg, f.Build())
	r.Services, r.MCP = cfg.Services, cfg.MCP
	v := doctor.JsonView(r, "default")

	// Serialize through cli.WriteJSONOut (the same path `doctor --json` uses) and parse.
	var buf bytes.Buffer
	if err := cli.WriteJSONOut(&buf, v); err != nil {
		t.Fatalf("cli.WriteJSONOut: %v", err)
	}
	var got doctor.DoctorJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, buf.String())
	}

	if len(got.Groups) == 0 {
		t.Fatal("serialized doctor JSON has no groups")
	}
	if got.Verdict != "outstanding" {
		t.Errorf("mixed env verdict = %q, want outstanding", got.Verdict)
	}
	if len(got.Todos) == 0 {
		t.Error("expected serialized todos for a mixed env")
	}

	// Every check must carry a valid state; a todo check must carry its command.
	seen := map[string]bool{}
	nChecks := 0
	for _, g := range got.Groups {
		if g.Title == "" {
			t.Error("a serialized group has an empty title")
		}
		for _, c := range g.Checks {
			nChecks++
			switch c.State {
			case "ok", "todo", "info", "warn":
				// warn is additive over v1 (doctor_json.go's stateName): an
				// unverifiable axis (e.g. "ollama in sandbox" with no sandbox
				// yet) renders warn, never a silent ok/todo/info substitute.
				seen[c.State] = true
			default:
				t.Errorf("check %q has invalid state %q", c.Label, c.State)
			}
			if c.State == "todo" && c.Todo == "" {
				t.Errorf("todo check %q has an empty todo command", c.Label)
			}
		}
	}
	if nChecks == 0 {
		t.Fatal("serialized doctor JSON has no checks")
	}
	for _, want := range []string{"ok", "todo", "info"} {
		if !seen[want] {
			t.Errorf("expected at least one %q check in serialized JSON, groups=%+v", want, got.Groups)
		}
	}
	// The serialized bytes literally carry the state strings (belt-and-suspenders
	// against a doctor.JsonView that renders states as ints or drops them).
	for _, want := range []string{`"state": "ok"`, `"state": "todo"`, `"state": "info"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("serialized doctor JSON missing %s", want)
		}
	}
	// The parsed todos match the report's own todos() (serialization preserved them).
	if strings.Join(got.Todos, "\n") != strings.Join(r.Todos(), "\n") {
		t.Errorf("serialized todos %v != report todos %v", got.Todos, r.Todos())
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

func TestMCPUsageListsEverySubcommand(t *testing.T) {
	const want = "usage: pix mcp <register|ls|load|auth|bundle> [args]"
	if !strings.Contains(mcp.McpUsage, want) {
		t.Fatalf("mcp usage synopsis missing subcommands: %q", mcp.McpUsage)
	}
}
