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
	"strings"
	"testing"

	"pix/host/health"
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

func TestServeRestartFix_IsALiveCommand(t *testing.T) {
	cmds := eachPixCommand(t, health.ServeRestartFix)
	if len(cmds) == 0 {
		t.Fatalf("health.ServeRestartFix = %q has no pix invocations", health.ServeRestartFix)
	}
	for _, cmd := range cmds {
		if !liveVerb(t, cmd) {
			t.Errorf("health.ServeRestartFix = %q names %q, which is not a real pix subcommand", health.ServeRestartFix, cmd)
		}
	}
}
