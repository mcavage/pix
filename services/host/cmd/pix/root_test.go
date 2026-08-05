package main

import (
	"bytes"
	"pix/host/cli"
	"strings"
	"testing"
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
	for _, want := range []string{
		"run", "status", "st", "ls", "rm", "version", "config", "serve",
		"doctor", "setup", "mcp", "pack", "secret", "memory", "mem",
		"monitor", "models", "agent", "reset", "state", "task", "help",
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

// TestMigratedVerbHelpIsGenerated: a migrated verb's help comes from its
// struct tags (kong's "Usage:"), goes to STDOUT, and exits 0.
func TestMigratedVerbHelpIsGenerated(t *testing.T) {
	for verb, want := range map[string]string{
		"ls":      "Usage: pix ls",
		"models":  "Usage: pix models",
		"agent":   "Usage: pix agent",
		"secret":  "Usage: pix secret",
		"rm":      "Usage: pix rm",
		"reset":   "Usage: pix reset",
		"serve":   "Usage: pix serve",
		"task":    "Usage: pix task",
		"monitor": "Usage: pix monitor",
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
		{"monitor", "--this-is-not-a-real-flag-9x7z"},
		{"task", "--this-is-not-a-real-flag-9x7z"},
		{"reset", "--this-is-not-a-real-flag-9x7z"},
		{"serve", "--this-is-not-a-real-flag-9x7z"},
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

// TestLegacyVerbsArePassthrough: an unmigrated verb receives its argv
// VERBATIM — kong must not parse, reject or reorder a flag that belongs to a
// hand-rolled loop, which is what makes the adapter behaviour-preserving.
func TestLegacyVerbsArePassthrough(t *testing.T) {
	for _, verb := range []string{"run", "status", "config", "doctor", "setup", "mcp", "pack", "memory", "state", "help"} {
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
