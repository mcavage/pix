package main

// health_fix_live_test.go — DX finding 1: a repair command doctor/health
// prints must be a REAL, parseable pix invocation, not prose that only looks
// like one. A fix that names a command which does not start the thing it
// claims to tells a stuck user to run something that
// cannot fix anything. Likewise `pix serve restart` was never a verb kong
// answers to; the only real path is stop-then-start (or a genuine `restart`
// subcommand, which does not exist).
//
// liveVerb proves "real" the same way a user would discover it: parse the
// exact command through the production root parser with a trailing --help,
// which resolves the subcommand tree without executing anything (install a
// launchd unit, stop a live daemon, etc).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// liveVerb reports whether "pix <cmd>" resolves to a real subcommand of the
// root parser. cmd must NOT include the leading "pix".
func liveVerb(t *testing.T, cmd string) bool {
	t.Helper()
	d, _, _ := rootDeps()
	argv := append(strings.Fields(cmd), "--help")
	err := runRootParse(argv, d)
	// --help always returns nil on a resolvable subcommand (it prints help and
	// stops before Run). A kong parse/usage error means the verb does not exist.
	return err == nil
}

// eachPixCommand splits a Fix string on "&&" into its individual "pix ..."
// invocations, stripping the leading "pix" token from each.
func eachPixCommand(t *testing.T, fix string) []string {
	t.Helper()
	var cmds []string
	for _, part := range strings.Split(fix, "&&") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.HasPrefix(part, "pix ") {
			t.Fatalf("fix segment %q is not a pix invocation", part)
		}
		cmds = append(cmds, strings.TrimSpace(strings.TrimPrefix(part, "pix")))
	}
	return cmds
}

// TestEveryHealthFix_IsALiveCommand enumerates health's `*Fix` constants FROM
// ITS SOURCE rather than listing them here.
//
// This test used to check exactly one constant, ServeRestartFix, by hand — so
// the property it claims to hold ("a repair command doctor prints is a real
// pix invocation") was only ever proven for one of them, and a fix naming a
// deleted verb passed. That is not hypothetical: `pix models route` was
// deleted while three surfaces still told users to run it, and `pix mcp
// bundle`/`load` outlived the verbs too. Deriving the list from the source is
// what makes a NEW fix constant covered the moment it is written, with nothing
// to remember.
func TestEveryHealthFix_IsALiveCommand(t *testing.T) {
	fixes := healthFixConstants(t)
	if len(fixes) < 4 {
		t.Fatalf("found only %d Fix constants in health/probes.go (%v); the extractor has drifted from the source", len(fixes), fixes)
	}
	for name, fix := range fixes {
		// A %s-templated fix (SecretSetFix, ServiceEnableFix) carries a
		// placeholder the caller fills; substitute something harmless so the
		// VERB is what gets parsed, which is all this test is about.
		concrete := strings.ReplaceAll(fix, "%s", "placeholder")
		cmds := eachPixCommand(t, concrete)
		if len(cmds) == 0 {
			t.Errorf("health.%s = %q has no pix invocations", name, fix)
			continue
		}
		for _, cmd := range cmds {
			if !liveVerb(t, cmd) {
				t.Errorf("health.%s = %q names %q, which is not a real pix subcommand", name, fix, cmd)
			}
		}
	}
}

// healthFixConstants reads health/probes.go and returns every `<Name>Fix = "pix
// ..."` constant in it. Source-derived on purpose: a hand-written list in a
// test is the thing that went stale here in the first place.
func healthFixConstants(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "health", "probes.go"))
	if err != nil {
		t.Fatalf("read health/probes.go: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*([A-Za-z]+Fix)\s*=\s*"([^"]*)"`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if strings.HasPrefix(m[2], "pix ") {
			out[m[1]] = m[2]
		}
	}
	return out
}
