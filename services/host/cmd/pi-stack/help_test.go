package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- wantsHelp: the shared -h/--help contract ---

func TestWantsHelp(t *testing.T) {
	yes := [][]string{
		{"-h"}, {"--help"}, {"recall", "--help"}, {"set", "-h", "x"},
	}
	for _, argv := range yes {
		if !wantsHelp(argv) {
			t.Errorf("wantsHelp(%v) = false, want true", argv)
		}
	}
	no := [][]string{
		nil, {"recall", "q"}, {"--json"}, {"--", "--help"}, {"set", "--", "-h"},
	}
	for _, argv := range no {
		if wantsHelp(argv) {
			t.Errorf("wantsHelp(%v) = true, want false", argv)
		}
	}
}

// --- B1: `run <non-dir>` must not launch ---

func TestParseRunArgs_NonDirWorkspaceRejected(t *testing.T) {
	// A known verb typo suggests the verb (and never launches).
	o, err := parseRunArgs([]string{"help"})
	if err == nil {
		t.Fatalf("parseRunArgs([help]) succeeded (workspace=%q) — should reject a non-dir", o.Workspace)
	}
	if !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), "pi-stack help") {
		t.Errorf("error = %q, want a not-a-directory + `pi-stack help` hint", err)
	}

	// A non-verb typo just reports not-a-directory.
	if _, err := parseRunArgs([]string{"nonexistent-xyz-123"}); err == nil {
		t.Error("parseRunArgs([nonexistent]) succeeded — should reject a non-dir")
	}
}

func TestParseRunArgs_ExistingDirOK(t *testing.T) {
	dir := t.TempDir()
	o, err := parseRunArgs([]string{dir})
	if err != nil {
		t.Fatalf("parseRunArgs(%q) error: %v", dir, err)
	}
	if o.Workspace != dir {
		t.Errorf("workspace = %q, want %q", o.Workspace, dir)
	}
	// The cwd default is always launchable.
	if _, err := parseRunArgs(nil); err != nil {
		t.Errorf("parseRunArgs(nil) error: %v", err)
	}
}

func TestValidateRunWorkspace(t *testing.T) {
	if err := validateRunWorkspace("."); err != nil {
		t.Errorf("cwd default should validate: %v", err)
	}
	dir := t.TempDir()
	if err := validateRunWorkspace(dir); err != nil {
		t.Errorf("existing dir should validate: %v", err)
	}
	// A regular file is not a directory.
	f := filepath.Join(dir, "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateRunWorkspace(f); err == nil {
		t.Error("a regular file should not validate as a workspace")
	}
}

// --- B3: `run --help` is a help request (sentinel), not an error ---

func TestParseRunArgs_HelpSentinel(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"--help", "extra"}} {
		if _, err := parseRunArgs(argv); err != errHelpRequested {
			t.Errorf("parseRunArgs(%v) err = %v, want errHelpRequested", argv, err)
		}
	}
}

// --- B2: `knowledge init <flag>` must not scaffold a junk bundle ---

func TestResolveKnowledgeInitArgs(t *testing.T) {
	// --help -> help sentinel, no dir.
	if dir, help, err := resolveKnowledgeInitArgs([]string{"--help"}); !help || err != nil || dir != "" {
		t.Errorf("resolveKnowledgeInitArgs([--help]) = (%q,%v,%v), want ('',true,nil)", dir, help, err)
	}
	// A flag typo -> error, no dir, no side effect.
	if dir, help, err := resolveKnowledgeInitArgs([]string{"--nope"}); err == nil || help || dir != "" {
		t.Errorf("resolveKnowledgeInitArgs([--nope]) = (%q,%v,%v), want ('',false,error)", dir, help, err)
	}
	// A real DIR resolves to its absolute form.
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "kb")
	dir, help, err := resolveKnowledgeInitArgs([]string{sub})
	if help || err != nil || dir != sub {
		t.Errorf("resolveKnowledgeInitArgs([%q]) = (%q,%v,%v), want (%q,false,nil)", sub, dir, help, err, sub)
	}
}

// TestKnowledgeInitHelp_NoSideEffects: `knowledge init --help` must not create a
// bundle dir, a `--help` directory, or touch config.
func TestKnowledgeInitHelp_NoSideEffects(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)

	// Run from a temp cwd so a stray filepath.Abs("--help") would land here.
	cwd, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// Capture stdout so the printed usage doesn't pollute test output, and so we
	// can assert usage was printed.
	old := os.Stdout
	rp, wp, _ := os.Pipe()
	os.Stdout = wp
	runKnowledgeInit([]string{"--help"})
	_ = wp.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rp)

	if !strings.Contains(buf.String(), "knowledge init") {
		t.Errorf("expected usage on stdout, got %q", buf.String())
	}
	if _, err := os.Stat(filepath.Join(tmp, "--help")); err == nil {
		t.Error("`knowledge init --help` created a `--help` directory")
	}
	if _, err := os.Stat(cfgFile); err == nil {
		t.Error("`knowledge init --help` wrote config — expected no side effects")
	}
	if _, err := os.Stat(defaultKnowledgeDir()); err == nil {
		t.Error("`knowledge init --help` scaffolded the default bundle dir")
	}
}

// --- S1: flagSet help bool + dispatch help routing ---

func TestFlagSetHelp(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"q", "--help"}} {
		fs := newFlagSet()
		if _, err := fs.parse(argv); err != nil {
			t.Fatalf("parse(%v): %v", argv, err)
		}
		if !fs.help {
			t.Errorf("parse(%v) did not set fs.help", argv)
		}
	}
	// No help token -> help stays false.
	fs := newFlagSet()
	if _, err := fs.parse([]string{"q", "--json"}); err != nil {
		t.Fatal(err)
	}
	if fs.help {
		t.Error("fs.help set without a help token")
	}
}

func TestMemoryHelp_NoRPC(t *testing.T) {
	// `memory recall --help` must print usage and NOT hit the (down) daemon.
	var out bytes.Buffer
	if err := dispatchMemory("recall", []string{"--help"}, rpcClient{Port: 1}, &out, "default"); err != nil {
		t.Fatalf("memory recall --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pi-stack memory recall") {
		t.Errorf("expected recall usage, got %q", out.String())
	}
	// stats/learnings likewise.
	out.Reset()
	if err := dispatchMemory("stats", []string{"--help"}, rpcClient{Port: 1}, &out, "default"); err != nil {
		t.Fatalf("memory stats --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pi-stack memory stats") {
		t.Errorf("expected stats usage, got %q", out.String())
	}
}

func TestVerbUsage_Routing(t *testing.T) {
	for _, verb := range []string{"run", "memory", "knowledge", "config", "status", "profile", "doctor", "mcp", "serve", "setup", "version"} {
		u, ok := verbUsage(verb)
		if !ok || strings.TrimSpace(u) == "" {
			t.Errorf("verbUsage(%q) = (%q,%v), want non-empty usage", verb, u, ok)
		}
	}
	// Aliases route too.
	if _, ok := verbUsage("kb"); !ok {
		t.Error("verbUsage(kb) should route to knowledge usage")
	}
	if _, ok := verbUsage("mem"); !ok {
		t.Error("verbUsage(mem) should route to memory usage")
	}
	// Unknown verb: no usage.
	if _, ok := verbUsage("frobnicate"); ok {
		t.Error("verbUsage(frobnicate) should be unknown")
	}
}

// --- S2: status + doctor flag validation ---

func TestParseStatusArgs(t *testing.T) {
	if j, err := parseStatusArgs([]string{"--json"}); err != nil || !j {
		t.Errorf("--json = (%v,%v), want (true,nil)", j, err)
	}
	if j, err := parseStatusArgs(nil); err != nil || j {
		t.Errorf("no args = (%v,%v), want (false,nil)", j, err)
	}
	if _, err := parseStatusArgs([]string{"--help"}); err != errHelpRequested {
		t.Errorf("--help err = %v, want errHelpRequested", err)
	}
	if _, err := parseStatusArgs([]string{"--jsom"}); err == nil {
		t.Error("--jsom (typo) should be a usage error")
	}
}

func TestParseDoctorArgs(t *testing.T) {
	if j, err := parseDoctorArgs([]string{"--json"}); err != nil || !j {
		t.Errorf("--json = (%v,%v), want (true,nil)", j, err)
	}
	if _, err := parseDoctorArgs([]string{"--help"}); err != errHelpRequested {
		t.Errorf("--help err = %v, want errHelpRequested", err)
	}
	if _, err := parseDoctorArgs([]string{"--bogus"}); err == nil {
		t.Error("--bogus should be a usage error")
	}
}

func TestParseSetupArgs_Help(t *testing.T) {
	o, err := parseSetupArgs([]string{"--help"})
	if err != nil || !o.help {
		t.Errorf("parseSetupArgs([--help]) = (%+v,%v), want help=true,nil", o, err)
	}
	if _, err := parseSetupArgs([]string{"--bogus"}); err == nil {
		t.Error("--bogus should be a usage error")
	}
}

// TestDoctorJSONView round-trips the doctor report through the REAL serializer
// and asserts the JSON actually carries the groups, their checks, and each
// check's ok|todo|info state (not merely that it is valid JSON). The fixture is
// a MIXED environment (sbx + keys present, ollama absent, no MCP) so all three
// states appear: ok (providers/memory), todo (models/gog CLI), info (empty mcp).
func TestDoctorJSONView(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true}, // ollama + gog absent -> TODOs
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11435: true}, // memory up -> an OK check
	}
	cfg := defaultCfg()
	r := runDoctor(cfg, f.env())
	r.services, r.mcp = cfg.Services, cfg.MCP
	v := r.jsonView("default")

	// Serialize through writeJSONOut (the same path `doctor --json` uses) and parse.
	var buf bytes.Buffer
	if err := writeJSONOut(&buf, v); err != nil {
		t.Fatalf("writeJSONOut: %v", err)
	}
	var got doctorJSON
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
			case "ok", "todo", "info":
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
	// against a jsonView that renders states as ints or drops them).
	for _, want := range []string{`"state": "ok"`, `"state": "todo"`, `"state": "info"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("serialized doctor JSON missing %s", want)
		}
	}
	// The parsed todos match the report's own todos() (serialization preserved them).
	if strings.Join(got.Todos, "\n") != strings.Join(r.todos(), "\n") {
		t.Errorf("serialized todos %v != report todos %v", got.Todos, r.todos())
	}
}

// TestRunVerb_HelpSkipsOnboarding is the F1 gate: `run --help` prints run usage
// and returns BEFORE first-run onboarding, so a config-less host is not dropped
// into the setup prompt. firstRunHook is swapped for a spy that must never fire
// on a help short-circuit.
func TestRunVerb_HelpSkipsOnboarding(t *testing.T) {
	old := firstRunHook
	defer func() { firstRunHook = old }()

	for _, argv := range [][]string{{"--help"}, {"-h"}, {"somedir", "--help"}} {
		called := false
		firstRunHook = func() bool { called = true; return true }

		old := os.Stdout
		rp, wp, _ := os.Pipe()
		os.Stdout = wp
		runVerb(argv)
		_ = wp.Close()
		os.Stdout = old
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rp)

		if called {
			t.Errorf("runVerb(%v) invoked first-run onboarding — help must short-circuit first", argv)
		}
		if !strings.Contains(buf.String(), "usage: pi-stack run") {
			t.Errorf("runVerb(%v) = %q, want run usage", argv, buf.String())
		}
	}
}
