package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/knowledge"
	"pix/host/memory"
	"pix/host/rpc"
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
	if !strings.Contains(err.Error(), "not a directory") || !strings.Contains(err.Error(), "pix help") {
		t.Errorf("error = %q, want a not-a-directory + `pix help` hint", err)
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
	if dir, help, err := knowledge.ResolveInitArgs([]string{"--help"}); !help || err != nil || dir != "" {
		t.Errorf("knowledge.ResolveInitArgs([--help]) = (%q,%v,%v), want ('',true,nil)", dir, help, err)
	}
	// A flag typo -> error, no dir, no side effect.
	if dir, help, err := knowledge.ResolveInitArgs([]string{"--nope"}); err == nil || help || dir != "" {
		t.Errorf("knowledge.ResolveInitArgs([--nope]) = (%q,%v,%v), want ('',false,error)", dir, help, err)
	}
	// A real DIR resolves to its absolute form.
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "kb")
	dir, help, err := knowledge.ResolveInitArgs([]string{sub})
	if help || err != nil || dir != sub {
		t.Errorf("knowledge.ResolveInitArgs([%q]) = (%q,%v,%v), want (%q,false,nil)", sub, dir, help, err, sub)
	}
}

// TestKnowledgeInitHelp_NoSideEffects: `knowledge init --help` must not create a
// bundle dir, a `--help` directory, or touch config.
func TestKnowledgeInitHelp_NoSideEffects(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.toml")
	t.Setenv("PIX_CONFIG", cfgFile)

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
	knowledge.RunKnowledgeInit([]string{"--help"})
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
	if _, err := os.Stat(knowledge.DefaultKnowledgeDir()); err == nil {
		t.Error("`knowledge init --help` scaffolded the default bundle dir")
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
	for _, verb := range []string{"run", "memory", "knowledge", "config", "status", "pack", "doctor", "mcp", "serve", "setup", "version", "reset"} {
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

// TestHelpAll_ListsExpertVerbs: `help --all` must name the rare/expert verbs the
// curated Core listing hides.
func TestHelpAll_ListsExpertVerbs(t *testing.T) {
	for _, v := range []string{"mcp", "secret", "restore", "reset", "man", "version"} {
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

// TestSuggestVerb_RetiredGog: the DELETED `gog` verb tree (6b39a69) must
// yield a did-you-mean pointing at its replacement, `gworkspace` — never a
// silent alias, and never "no suggestion" (edit distance between "gog" and
// "gworkspace" is far larger than suggestVerb's levenshtein-2 window, so this
// only works because retiredVerbs carries it explicitly).
func TestSuggestVerb_RetiredGog(t *testing.T) {
	if knownVerbs["gog"] {
		t.Error("the `gog` verb is deleted with no alias; it must not be a known verb")
	}
	if _, ok := verbUsage("gog"); ok {
		t.Error("`pix help gog` must not resolve to a usage page for a deleted verb")
	}
	s, ok := suggestVerb("gog")
	if !ok || s != "gworkspace" {
		t.Errorf("suggestVerb(gog) = %q,%v, want gworkspace,true", s, ok)
	}
	msg, launch := classifyBareArg("gog")
	if launch || !strings.Contains(msg, `Did you mean "gworkspace"?`) {
		t.Errorf("`pix gog ...` must print a did-you-mean and exit 2, got %q (launch=%v)", msg, launch)
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
	if j, v, err := parseDoctorArgs([]string{"--json"}); err != nil || !j || v {
		t.Errorf("--json = (%v,%v,%v), want (true,false,nil)", j, v, err)
	}
	if j, v, err := parseDoctorArgs([]string{"--verbose"}); err != nil || j || !v {
		t.Errorf("--verbose = (%v,%v,%v), want (false,true,nil)", j, v, err)
	}
	if _, _, err := parseDoctorArgs([]string{"--help"}); err != errHelpRequested {
		t.Errorf("--help err = %v, want errHelpRequested", err)
	}
	if _, _, err := parseDoctorArgs([]string{"--bogus"}); err == nil {
		t.Error("--bogus should be a usage error")
	}
}

func TestParseOnboardArgs_Help(t *testing.T) {
	o, err := parseOnboardArgs([]string{"--help"})
	if err != nil || !o.help {
		t.Errorf("parseOnboardArgs([--help]) = (%+v,%v), want help=true,nil", o, err)
	}
	if _, err := parseOnboardArgs([]string{"--bogus"}); err == nil {
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
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11435: true}, // memory up -> an OK check
	}
	cfg := defaultCfg()
	r := runDoctor(cfg, f.env())
	r.Services, r.MCP = cfg.Services, cfg.MCP
	v := jsonView(r, "default")

	// Serialize through cli.WriteJSONOut (the same path `doctor --json` uses) and parse.
	var buf bytes.Buffer
	if err := cli.WriteJSONOut(&buf, v); err != nil {
		t.Fatalf("cli.WriteJSONOut: %v", err)
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
	// against a jsonView that renders states as ints or drops them).
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
	if !strings.Contains(mcpUsage, want) {
		t.Fatalf("mcp usage synopsis missing subcommands: %q", mcpUsage)
	}
}
