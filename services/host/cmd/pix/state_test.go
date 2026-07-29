package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout swapped for a pipe and returns whatever
// fn printed. It mirrors the os.Pipe swap idiom used in help_test.go
// (TestKnowledgeInitHelp_NoSideEffects).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wp
	fn()
	_ = wp.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rp)
	return buf.String()
}

// TestRunState_RoutesToAliasesViaHelp exercises routing through the
// side-effect-free --help seam: each subcommand + --help prints that verb's own
// usage (no exec, no config, no filesystem move).
func TestRunState_RoutesToAliasesViaHelp(t *testing.T) {
	cases := map[string]string{
		"backup":  "usage: pix backup",
		"restore": "usage: pix restore",
		"reset":   "usage: pix reset",
	}
	for sub, want := range cases {
		out := captureStdout(t, func() { runState([]string{sub, "--help"}) })
		if !strings.Contains(out, want) {
			t.Errorf("state %s --help = %q, want %q", sub, out, want)
		}
	}
}

// TestRunState_BareUsage: a bare noun prints the group usage (exit 0).
func TestRunState_BareUsage(t *testing.T) {
	out := captureStdout(t, func() { runState(nil) })
	if !strings.Contains(out, "usage: pix state") {
		t.Errorf("bare state: %q", out)
	}
}

// TestState_KnownAndRoutable: state is a known verb and routes to its usage so
// `pix help state` and the suggester find it.
func TestState_KnownAndRoutable(t *testing.T) {
	if !knownVerbs["state"] {
		t.Error("state missing from knownVerbs")
	}
	if u, ok := verbUsage("state"); !ok || u == "" {
		t.Error("verbUsage(state) empty")
	}
}

// TestLegacyLifecycleAliasesPreserved is the constraint-c regression: the four
// flat lifecycle verbs stay dispatchable + documented even after grouping.
func TestLegacyLifecycleAliasesPreserved(t *testing.T) {
	for _, v := range []string{"backup", "restore", "reset"} {
		if !knownVerbs[v] {
			t.Errorf("%s dropped from knownVerbs", v)
		}
		if _, ok := verbUsage(v); !ok {
			t.Errorf("verbUsage(%s) gone", v)
		}
	}
}
