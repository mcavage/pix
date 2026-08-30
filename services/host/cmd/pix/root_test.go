package main

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alecthomas/kong"

	"pix/host/cli"
	"pix/host/workflow/launch"
)

// root_test.go pins the ONE ROOT contract: kong's root tree is the only
// parser, the only dispatcher and the only source of the verb list, and the
// tiered help it fronts still behaves exactly as it did when main.go carried a
// switch.

func rootDeps() (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb}, &out, &errb
}

// runRootParse drives the REAL root parser over a full argv, which is how a
// verb's flags reach it in production. Tests used to call cli.Run[T] on one
// verb's subtree; there is no such subtree any more, and parsing the argv a
// user actually types is the stronger assertion anyway.
func runRootParse(argv []string, d *cli.Deps) error {
	return cli.RunRoot[rootCmd]("pix", "", helpText, argv, d)
}

// rootVerbs is every name the root answers to, aliases included.
func rootVerbs() []string {
	var out []string
	for v := range knownVerbs() {
		out = append(out, v)
	}
	return out
}

// TestRootOwnsEveryVerb: every verb the launcher answers to is a child of the
// kong root. The list is the one users type; it was main.go's switch.
func TestRootOwnsEveryVerb(t *testing.T) {
	got := map[string]bool{}
	for _, v := range rootVerbs() {
		got[v] = true
	}
	// The v2 accepted surface (docs/design/pix-v2-surface.md §3, root.go's
	// own doc comment): run, ls, rm, task, setup, doctor, reset, env, secret,
	// version, help. A removed verb (status, config, serve, mcp, models,
	// agent, pack, memory, and every v1 alias) belongs in a NEGATIVE test
	// (TestDeletionSweep_NoServeOrPackVerbInHelpText et al in pix_test.go),
	// never a positive membership assertion here.
	for _, want := range []string{
		"run", "ls", "rm", "version",
		"doctor", "setup", "reset", "secret", "env", "task", "help",
	} {
		if !got[want] {
			t.Errorf("verb %q is not a child of the kong root (got %v)", want, rootVerbs())
		}
	}
}

// TestKnownVerbsDerivedFromRoot: the did-you-mean set is DERIVED, so a verb
// can never be dispatchable and unknown to the suggester at the same time.
func TestKnownVerbsDerivedFromRoot(t *testing.T) {
	for _, v := range rootVerbs() {
		if !knownVerbs()[v] {
			t.Errorf("root verb %q missing from the derived knownVerbs set", v)
		}
	}
	if len(knownVerbs()) < 10 {
		t.Fatalf("knownVerbs has %d entries; the derivation stopped working", len(knownVerbs()))
	}
}

// TestTieredHelpStaysShort: the landing screen is a curated document with a
// budget. `help --all` is where the whole surface lives.
func TestTieredHelpStaysShort(t *testing.T) {
	if n := len(strings.Split(strings.TrimRight(helpText, "\n"), "\n")); n > 25 {
		t.Errorf("tiered `pix help` is %d lines, budget is 25 — move detail to `help --all`", n)
	}
}

// verbGroups reads rootCmd's own `group:` struct tags via reflection — the
// SAME tags root.go's doc comment says are the one source of truth `pix help
// --all` renders its tiers from — keyed by the lowercase verb name every
// other helper here already uses.
func verbGroups() map[string]string {
	out := map[string]string{}
	t := reflect.TypeOf(rootCmd{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if _, ok := f.Tag.Lookup("cmd"); !ok {
			continue
		}
		out[strings.ToLower(f.Name)] = f.Tag.Get("group")
	}
	return out
}

// TestTieredHelpTiersMatchGeneratedGroups is the anti-drift guard DX finding
// 4's short-help curation needed: helpText hand-curates its own tier headings
// (deliberately merging some generated groups for brevity, e.g. "Data, models
// & observability" folds root.go's Data and Models & agents
// groups into one heading), but a heading whose text is a VERBATIM match for
// one of rootCmd's real `group:` names must not silently list a verb from a
// DIFFERENT group under it — that is not curation, it is drift (exactly the
// bug this test was added to catch: a verb sat under the literal
// "Setup & health" heading while rootCmd tags it a different group).
// A heading whose text does not literally match any real group name is a
// deliberate multi-group merge or the "More" overflow line and is skipped.
func TestTieredHelpTiersMatchGeneratedGroups(t *testing.T) {
	groups := verbGroups()
	realGroupNames := map[string]bool{}
	for _, g := range groups {
		realGroupNames[g] = true
	}

	var heading string
	for _, line := range strings.Split(helpText, "\n") {
		trimmed := strings.TrimRight(line, " ")
		switch {
		case trimmed == "":
			heading = ""
		case !strings.HasPrefix(line, " "):
			// A non-indented, non-blank line starts a new section. Only a line whose
			// full text is a real group name is treated as a checkable heading;
			// everything else (the title, "Usage:", "New here?", "Learn a command:",
			// a curated/merged heading like "More") is prose this test has nothing to
			// check, so it is ignored rather than misparsed as a verb bullet.
			if realGroupNames[trimmed] {
				heading = trimmed
			} else {
				heading = ""
			}
		case heading != "":
			verb := strings.Fields(strings.TrimSpace(line))[0]
			if got := groups[verb]; got != heading {
				t.Errorf("helpText lists %q under the %q heading, but rootCmd tags it group:%q — either the tier drifted or the verb needs re-tagging", verb, heading, got)
			}
		}
	}
}

// TestRootHelpIsTheCuratedScreen: a root help request prints the tiered text,
// not kong's generated command listing.
func TestRootHelpIsTheCuratedScreen(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		d, out, _ := rootDeps()
		if code := dispatch(argv, d); code != 0 {
			t.Errorf("dispatch(%v) = %d, want 0", argv, code)
		}
		if !strings.Contains(out.String(), "Learn a command:  pix help run") {
			t.Errorf("dispatch(%v) printed %q, want the tiered help screen", argv, out.String())
		}
	}
	// `help --all` still reveals the full tier.
	d, out, _ := rootDeps()
	if code := dispatch([]string{"help", "--all"}, d); code != 0 {
		t.Errorf("help --all exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Parallel work") {
		t.Errorf("help --all printed %q, want the full listing", out.String())
	}
}

// TestDispatch_BareNonTTY_RefusesWithNoCreate (Story04c): a bare positional
// naming a real directory (never the explicit `run` verb) on a
// non-interactive terminal (d.Interactive == false, the zero value rootDeps
// already uses) must refuse outright — exit 2, guidance on stderr ONLY
// (nothing on stdout, which a script may be capturing), and it must never
// even reach the root parser (no create, no attach, no side effect).
func TestDispatch_BareNonTTY_RefusesWithNoCreate(t *testing.T) {
	dir := t.TempDir()
	d, out, errb := rootDeps()
	d.Interactive = false

	code := dispatch([]string{dir}, d)

	if code != 2 {
		t.Errorf("dispatch(%v) = %d, want 2", dir, code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty (bare non-TTY must never print to stdout)", out.String())
	}
	abs, _ := filepath.Abs(dir)
	if !strings.Contains(errb.String(), abs) {
		t.Errorf("stderr = %q, want it to name the resolved path %q", errb.String(), abs)
	}
	if !strings.Contains(errb.String(), "pix run ") {
		t.Errorf("stderr = %q, want explicit `pix run` guidance", errb.String())
	}
}

// TestDispatch_BareInteractive_StillLaunches: the SAME bare positional on an
// interactive terminal is unaffected by the new refusal — it still
// re-normalizes to `run DIR` (proven here by the exit code/behavior being
// whatever `run` itself would produce, not the bare-refusal's exit 2/stderr
// shape). A workspace with nothing else configured will fail further down
// run's own pipeline (no sbx, etc.) but must NOT fail with the bare-refusal
// message.
// bareLaunchDeps builds the Deps for a bare-positional dispatch test with sbx
// forced ABSENT: run's own `LookPath("sbx")` gates everything past it (key
// bootstrap, probing an existing sandbox, and eventually a real `exec.Command
// ("sbx", ...)` with the process's own stdio inherited), so a dispatch test
// that leaves PATH as whatever the test process inherited is only "safe" by
// the accident of sbx not being installed in THAT environment. On a real pix
// host — which by this repo's own design HAS sbx installed — the same test
// would actually create (and leak) a real sandbox. Pointing PATH at an empty
// directory makes sbx-absent the test's explicit, host-independent contract
// rather than a lucky accident of wherever `go test` happens to run.
func bareLaunchDeps(t *testing.T) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	return rootDeps()
}

func TestDispatch_BareInteractive_StillLaunches(t *testing.T) {
	dir := t.TempDir()
	d, _, errb := bareLaunchDeps(t)
	d.Interactive = true

	_ = dispatch([]string{dir}, d)

	if strings.Contains(errb.String(), "non-interactive terminal") {
		t.Errorf("an interactive bare launch must not hit the non-TTY refusal, stderr = %q", errb.String())
	}
}

// TestDispatch_BareInteractive_NeverTouchesARealSandbox is the side-effect
// proof bareLaunchDeps exists for: with sbx forced absent, `pix DIR` must fail
// CLOSED — sandbox state it cannot determine is never treated as "absent"
// (safety invariant 6's sibling for sbx itself) — rather than reaching the
// real `exec.Command("sbx", ...)` spawn buried at the bottom of runLaunch. The
// exact refusal text is PlanSandboxLaunch's own "could not determine whether
// sandbox ... exists", which only prints if run stopped BEFORE that exec, so
// asserting it is evidence no subprocess was spawned, not just an absence of
// a crash.
func TestDispatch_BareInteractive_NeverTouchesARealSandbox(t *testing.T) {
	dir := t.TempDir()
	d, _, errb := bareLaunchDeps(t)
	d.Interactive = true

	code := dispatch([]string{dir}, d)

	if code == 0 {
		t.Fatalf("a launch with sbx forced absent must not report success, stderr = %q", errb.String())
	}
	if !strings.Contains(errb.String(), "could not determine whether sandbox") {
		t.Errorf("expected the fail-closed sbx-unknown refusal (proof the real `sbx` exec was never reached), got stderr = %q", errb.String())
	}
	for _, leak := range []string{"attaching to running sandbox", "starting + attaching", "exec sbx:"} {
		if strings.Contains(errb.String(), leak) {
			t.Errorf("stderr mentions %q — this test must never reach the real sbx exec path, stderr = %q", leak, errb.String())
		}
	}
}

// TestMigratedVerbHelpIsGenerated: a migrated verb's help comes from its
// struct tags (kong's "Usage:"), goes to STDOUT, and exits 0.
func TestMigratedVerbHelpIsGenerated(t *testing.T) {
	for verb, want := range map[string]string{
		"ls":     "Usage: pix ls",
		"secret": "Usage: pix secret",
		"rm":     "Usage: pix rm",
		"task":   "Usage: pix task",
		"run":    "Usage: pix run",
		"env":    "Usage: pix env",
		"doctor": "Usage: pix doctor",
		"setup":  "Usage: pix setup",
		"reset":  "Usage: pix reset",
	} {
		d, out, errb := rootDeps()
		if code := dispatch([]string{verb, "--help"}, d); code != 0 {
			t.Errorf("`%s --help` exit = %d, want 0 (stderr: %s)", verb, code, errb.String())
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("`%s --help` stdout = %q, want %q", verb, out.String(), want)
		}
	}
}

// TestExitMapper: one mapper turns a command error into 0/1/2, and a
// SilentError's own code (3) survives it.
func TestExitMapper(t *testing.T) {
	for _, argv := range [][]string{
		{"ls", "--this-is-not-a-real-flag-9x7z"},
		{"rm", "--this-is-not-a-real-flag-9x7z"},
		{"task", "--this-is-not-a-real-flag-9x7z"},
		{"run", "--this-is-not-a-real-flag-9x7z"},
		{"env", "--this-is-not-a-real-flag-9x7z"},
		{"doctor", "--this-is-not-a-real-flag-9x7z"},
		{"setup", "--this-is-not-a-real-flag-9x7z"},
		{"reset", "--this-is-not-a-real-flag-9x7z"},
	} {
		d, _, errb := rootDeps()
		if code := dispatch(argv, d); code != 2 {
			t.Errorf("dispatch(%v) = %d, want 2 (usage error)", argv, code)
		}
		if !strings.Contains(errb.String(), "unknown flag") {
			t.Errorf("dispatch(%v) stderr = %q, want an unknown-flag message", argv, errb.String())
		}
	}
	if got := cli.ExitCode(cli.SilentError{Code: 3}); got != 3 {
		t.Errorf("ExitCode(SilentError{3}) = %d, want 3", got)
	}
}

// TestLegacyVerbsArePassthrough: the remaining passthrough commands receive
// their argv VERBATIM — kong must not parse, reject or reorder a flag that
// belongs to a hand-rolled loop (`serve install`/`uninstall`) or a token that
// is a question rather than a grammar (`help`).
func TestLegacyVerbsArePassthrough(t *testing.T) {
	for _, verb := range []string{"help"} {
		var gotVerb string
		var gotArgs []string
		testSeams.legacy = func(v string, a []string) { gotVerb, gotArgs = v, a }
		d, _, _ := rootDeps()
		code := dispatch([]string{verb, "--dev", "--", "--help"}, d)
		testSeams.legacy = nil
		if code != 0 {
			t.Errorf("dispatch(%s ...) = %d, want 0", verb, code)
		}
		if gotVerb != verb {
			t.Errorf("argv reached %q, want the %q adapter", gotVerb, verb)
		}
		if strings.Join(gotArgs, " ") != "--dev -- --help" {
			t.Errorf("%s adapter got %q, want the argv verbatim", verb, gotArgs)
		}
	}
}

// TestVersionIsTyped: version has no flags at all, so it is a typed command
// whose usage is entirely generated — the last hand-written usage constant in
// main.go went with it.
func TestVersionIsTyped(t *testing.T) {
	d, out, _ := rootDeps()
	if code := dispatch([]string{"version"}, d); code != 0 {
		t.Fatalf("`version` exit = %d, want 0", code)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Errorf("`version` printed %q, want %q", out.String(), version)
	}
	d, _, _ = rootDeps()
	if code := dispatch([]string{"version", "extra"}, d); code != 2 {
		t.Errorf("`version extra` exit = %d, want 2", code)
	}
}

func TestBareDevFlagMeansRunDev(t *testing.T) {
	got := strings.Join(normalizeArgv([]string{"--dev"}), " ")
	if got != "run --dev" {
		t.Errorf("normalizeArgv(--dev) = %q, want %q", got, "run --dev")
	}

	// Like bare `pix` and `pix DIR`, the shorthand is implicit and therefore
	// must not launch from a script or pipe. The explicit spelling stays the
	// non-interactive escape hatch.
	d, _, errOut := rootDeps()
	d.Interactive = false
	if code := dispatch([]string{"--dev"}, d); code != 2 {
		t.Fatalf("non-interactive pix --dev exit = %d, want 2", code)
	}
	if got := errOut.String(); !strings.Contains(got, "pix run --dev") || !strings.Contains(got, "non-interactive") {
		t.Fatalf("non-interactive refusal = %q, want explicit recovery", got)
	}
}

// TestTaskNameThenVerbRewrite: `pix task foo path` is an argv-SHAPE decision
// (it reads naturally in `cd "$(pix task foo path)"`), normalized before the
// parser sees it, and never fired for a real subcommand.
func TestTaskNameThenVerbRewrite(t *testing.T) {
	got := strings.Join(normalizeArgv([]string{"task", "foo", "path"}), " ")
	if got != "task path foo" {
		t.Errorf("normalizeArgv(task foo path) = %q, want %q", got, "task path foo")
	}
	for _, argv := range [][]string{
		{"task", "ls", "path"}, {"task", "path", "foo"}, {"task", "new", "foo"}, {"ls"},
	} {
		before := strings.Join(argv, " ")
		if after := strings.Join(normalizeArgv(argv), " "); after != before {
			t.Errorf("normalizeArgv(%q) rewrote to %q, want unchanged", before, after)
		}
	}
}

// TestBareTaskPrintsUsage: bare `pix task` is help, exit 0 — the fast path the
// hand-rolled seam owned is now kong's default command.
func TestBareTaskPrintsUsage(t *testing.T) {
	d, out, _ := rootDeps()
	if code := dispatch([]string{"task"}, d); code != 0 {
		t.Errorf("bare `task` exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage: pix task") {
		t.Errorf("bare `task` printed %q, want task usage", out.String())
	}
}

// parseRoot parses a full argv against the REAL root and returns the populated
// tree, for tests that assert on WHAT a flag parsed into rather than on what
// running it does.
func parseRoot(argv []string) (rootCmd, error) {
	var root rootCmd
	parser, err := kong.New(&root, kong.Name("pix"), kong.Exit(func(int) {}))
	if err != nil {
		return root, err
	}
	_, err = parser.Parse(normalizeArgv(argv))
	return root, err
}

// parseRunOpts parses `pix run ARGS...` and returns the launch options it
// resolves to. It replaces the hand-rolled launch.ParseRunArgs the tests used
// to call directly: the grammar under test is now the one users type.
func parseRunOpts(args []string) (launch.RunOpts, error) {
	root, err := parseRoot(append([]string{"run"}, args...))
	if err != nil {
		return launch.RunOpts{}, err
	}
	return root.Run.opts()
}
