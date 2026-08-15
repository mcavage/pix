package main

import (
	"strings"
	"testing"
)

// run_cmd_test.go pins the two argv SHAPES the typed run grammar cannot
// express on its own, both of which used to live in a hand-rolled parser: the
// `--` pi passthrough (kong would otherwise feed the first pi arg to DIR) and
// the bare-DIR alias.

func TestUATSmokeProviderKeyGateBypassIsExplicit(t *testing.T) {
	t.Setenv("PIX_UAT_SMOKE", "")
	if uatSmokeSkipsProviderKeyGate() {
		t.Fatal("empty PIX_UAT_SMOKE bypassed the provider-key gate")
	}
	t.Setenv("PIX_UAT_SMOKE", "true")
	if uatSmokeSkipsProviderKeyGate() {
		t.Fatal("non-canonical PIX_UAT_SMOKE bypassed the provider-key gate")
	}
	t.Setenv("PIX_UAT_SMOKE", "1")
	if !uatSmokeSkipsProviderKeyGate() {
		t.Fatal("PIX_UAT_SMOKE=1 did not select the isolated smoke path")
	}
}

func TestRunPassthrough_TailReachesPiVerbatim(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		args    []string
		wantWS  string
		wantPi  []string
		wantDev bool
	}{
		{args: []string{"--", "-p", "hi"}, wantWS: ".", wantPi: []string{"-p", "hi"}},
		{args: []string{"--dev", "--", "--help"}, wantWS: ".", wantPi: []string{"--help"}, wantDev: true},
		{args: []string{dir, "--", "-p", "hi", "--model=x"}, wantWS: dir, wantPi: []string{"-p", "hi", "--model=x"}},
		{args: []string{dir}, wantWS: dir},
	} {
		o, err := parseRunOpts(tc.args)
		if err != nil {
			t.Fatalf("run %v: %v", tc.args, err)
		}
		if o.Workspace != tc.wantWS || o.Dev != tc.wantDev {
			t.Errorf("run %v = {ws:%q dev:%v}, want {%q %v}", tc.args, o.Workspace, o.Dev, tc.wantWS, tc.wantDev)
		}
		if strings.Join(o.Passthrough, "|") != strings.Join(tc.wantPi, "|") {
			t.Errorf("run %v passthrough = %q, want %q", tc.args, o.Passthrough, tc.wantPi)
		}
	}
}

// A `--` tail is pi's, so a -h inside it is NOT a help request for pix.
func TestRunPassthrough_HelpAfterTerminatorIsPis(t *testing.T) {
	o, err := parseRunOpts([]string{"--", "--help"})
	if err != nil {
		t.Fatalf("run -- --help: %v", err)
	}
	if strings.Join(o.Passthrough, "|") != "--help" {
		t.Errorf("passthrough = %q, want [--help]", o.Passthrough)
	}
}

// rewriteRunPassthrough is a REWRITE, not a parse: it fires only on `run` and
// leaves everything before the first `--` alone.
func TestRewriteRunPassthrough_OnlyTheTail(t *testing.T) {
	got := strings.Join(rewriteRunPassthrough([]string{"run", "--dev", "d", "--", "-p", "x"}), " ")
	if want := "run --dev d --pi-arg=-p --pi-arg=x"; got != want {
		t.Errorf("rewrite = %q, want %q", got, want)
	}
	for _, argv := range [][]string{{"run", "--dev"}, {"run"}} {
		before := strings.Join(argv, " ")
		if after := strings.Join(rewriteRunPassthrough(argv), " "); after != before {
			t.Errorf("rewrite(%q) = %q, want unchanged", before, after)
		}
	}
	// A non-run verb keeps its `--` verbatim (the legacy adapters own it).
	if got := strings.Join(normalizeArgv([]string{"memory", "set", "--", "-h"}), " "); got != "memory set -- -h" {
		t.Errorf("normalizeArgv rewrote a non-run verb: %q", got)
	}
}

// TestBareDirIsRun: `pix DIR` is `pix run DIR`, and a bare non-directory is a
// verb typo (exit 2), never a launch.
func TestBareDirIsRun(t *testing.T) {
	d, _, errb := rootDeps()
	if code := dispatch([]string{"nonexistent-xyz-123"}, d); code != 2 {
		t.Errorf("bare unknown token = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "no command named") {
		t.Errorf("stderr = %q, want a did-you-mean report", errb.String())
	}
	// The normalized argv for an existing directory is the `run` verb, with the
	// pi tail rewritten for run's grammar.
	dir := t.TempDir()
	got := strings.Join(normalizeArgv(append([]string{"run"}, dir, "--", "-p", "x")), " ")
	if want := "run " + dir + " --pi-arg=-p --pi-arg=x"; got != want {
		t.Errorf("bare-dir normalization = %q, want %q", got, want)
	}
}
