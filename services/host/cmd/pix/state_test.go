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
	// backup/restore are RETIRED (W1 U01a); reset is the group's only live
	// subcommand. The retired pair is covered by the retirement contract
	// (retired_test.go + corpus/retired_dispatch_test.go), which asserts the
	// notice and exit 2 out of process — it cannot be exercised in-process.
	cases := map[string]string{
		"reset": "Usage: pix reset",
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

// TestLegacyLifecycleAliasesPreserved: the lifecycle verbs that SURVIVED
// retirement stay dispatchable + documented. backup/restore left with W1 U01a.
func TestLegacyLifecycleAliasesPreserved(t *testing.T) {
	for _, v := range []string{"reset"} {
		if !knownVerbs[v] {
			t.Errorf("%s dropped from knownVerbs", v)
		}
		if _, ok := verbUsage(v); !ok {
			t.Errorf("verbUsage(%s) gone", v)
		}
	}
}
