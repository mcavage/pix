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
		"reset": "Usage: pix state reset",
	}
	for sub, want := range cases {
		d, out, _ := rootDeps()
		if code := dispatch([]string{"state", sub, "--help"}, d); code != 0 {
			t.Errorf("state %s --help exit = %d, want 0", sub, code)
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("state %s --help = %q, want %q", sub, out.String(), want)
		}
	}
}

// TestRunState_BareUsage: a bare noun prints the group usage (exit 0).
func TestRunState_BareUsage(t *testing.T) {
	d, out, _ := rootDeps()
	if code := dispatch([]string{"state", "--help"}, d); code != 0 {
		t.Errorf("state --help exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage: pix state") {
		t.Errorf("bare state: %q", out.String())
	}
}

// TestState_KnownAndRoutable: state is a known verb and routes to its usage so
// `pix help state` and the suggester find it.
func TestState_KnownAndRoutable(t *testing.T) {
	if !knownVerbs()["state"] {
		t.Error("state missing from knownVerbs")
	}
	d, out, _ := rootDeps()
	if code := dispatch([]string{"help", "state"}, d); code != 0 || !strings.Contains(out.String(), "Usage: pix state") {
		t.Errorf("`pix help state` = %q (exit %d), want the generated usage", out.String(), code)
	}
}

// TestLegacyLifecycleAliasesPreserved: the lifecycle verbs that SURVIVED
// retirement stay dispatchable + documented. backup/restore left with W1 U01a.
func TestLegacyLifecycleAliasesPreserved(t *testing.T) {
	for _, v := range []string{"reset"} {
		if !knownVerbs()[v] {
			t.Errorf("%s dropped from knownVerbs", v)
		}
		d, out, _ := rootDeps()
		if code := dispatch([]string{"help", v}, d); code != 0 {
			t.Errorf("`pix help %s` exit = %d, want 0", v, code)
		}
		if !strings.Contains(out.String(), "Usage: pix "+v) {
			t.Errorf("`pix help %s` printed %q, want the generated usage", v, out.String())
		}
	}
}
