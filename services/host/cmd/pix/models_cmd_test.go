package main

// These tests are the point of Phase 2, and they are worth reading as a
// contrast rather than just as coverage.
//
// The equivalent assertions before the migration required re-execing the test
// binary as a subprocess (see gworkspace_test.go, doctor_providers_test.go,
// mcp_receipt_wiring_test.go, models_rename_test.go — all of which do exactly
// that) because a verb parsed os.Args, wrote to os.Stdout and called os.Exit.
// You cannot assert on an exit code you cannot catch, so the tests forked.
//
// Here the command is a struct, its output is a buffer, and its exit code is
// the return value of a function. No fork, no global state, no timing.

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/sys/systest"
)

// testDeps is a command's whole world: an in-memory config and buffers.
func testDeps(cfg *config.Config) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	d := &cli.Deps{
		Sys: &systest.Fake{}, Out: &out, Err: &errb,
		In: strings.NewReader(""), Interactive: false,
	}
	if cfg != nil {
		d.SetConfig(cfg)
	}
	return d, &out, &errb
}

// TestModelsStatus_RendersToDepsOut: output goes to cli.Deps.Out, not the process's
// stdout. That is the property that removes the need for a subprocess.
//
// E3.3: the bare screen is FACTS ONLY — MODEL/BACKEND/SOURCE, no WHY, no
// score, no price, no wired/unwired/retired taxonomy.
func TestModelsStatus_RendersToDepsOut(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		AllowedModels: []string{"anthropic/claude-opus-5"},
		Backends: map[string]config.InferenceBackend{
			"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
		},
		Models: []config.InferenceModelBinding{{
			Model: "anthropic/claude-opus-5", Backend: "anthropic",
			Upstream: "anthropic/claude-opus-5", Available: true,
			Verified: true, VerifiedBy: "probe",
		}},
	}}
	d, out, _ := testDeps(cfg)
	if err := runRootParse([]string{"models"}, d); err != nil {
		t.Fatalf("bare `models` must succeed: %v", err)
	}
	for _, want := range []string{"MODEL", "BACKEND", "SOURCE", "anthropic/claude-opus-5", "machine config"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status screen missing %q:\n%s", want, out.String())
		}
	}
	for _, banned := range []string{"WHY", "wired", "unwired", "retired", "Runtime", "verified"} {
		if strings.Contains(out.String(), banned) {
			t.Errorf("status screen must be facts-only, got banned %q in:\n%s", banned, out.String())
		}
	}
}

// TestModelsAdd_RejectsOllamaFlagsOnAKeyedProvider: a usage error is exit 2 and
// carries its own message. Previously this was an os.Exit(2) that a test could
// only observe by forking.
func TestModelsAdd_RejectsOllamaFlagsOnAKeyedProvider(t *testing.T) {
	d, _, _ := testDeps(&config.Config{})
	err := runRootParse([]string{"models", "add", "anthropic", "--local"}, d)
	if err == nil {
		t.Fatal("--local on a keyed provider must be rejected")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("exit code = %d, want 2 (a user's invocation was wrong)", got)
	}
	if !strings.Contains(err.Error(), "only apply to ollama") {
		t.Errorf("error must name the constraint, got %q", err)
	}
}

// TestModelsAdd_ValidatesProviderFromOneList is the regression guard for the
// reported bug. The hand-rolled version validated the provider against a list
// it maintained by hand, separately from setup.ProviderNames(), and ollama was
// missing from it — `pix models add ollama` answered "unknown provider". The
// enum tag makes the accepted set and the help text the same declaration.
func TestModelsAdd_ValidatesProviderFromOneList(t *testing.T) {
	for _, ok := range []string{"anthropic", "openai", "google", "gemini", "ollama"} {
		var c ModelsAddCmd
		if err := kongParseInto(t, &c, []string{"add", ok}); err != nil {
			t.Errorf("%s must be an accepted provider: %v", ok, err)
		}
	}
	d, _, _ := testDeps(&config.Config{})
	err := runRootParse([]string{"models", "add", "bogus"}, d)
	if cli.ExitCode(err) != 2 {
		t.Fatalf("an unknown provider is a usage error, got exit %d (%v)", cli.ExitCode(err), err)
	}
	if !strings.Contains(err.Error(), "ollama") {
		t.Errorf("the rejection must list ollama among the choices, got %q", err)
	}
}

// kongParseInto parses argv far enough to validate it, without running. It
// exists so the enum check above does not need a cli.Deps or a filesystem.
func kongParseInto(t *testing.T, _ *ModelsAddCmd, argv []string) error {
	t.Helper()
	d, _, _ := testDeps(&config.Config{Inference: config.InferenceConfig{ExclusiveSource: "/packs/stop"}})
	err := runRootParse(append([]string{"models"}, argv...), d)
	// An exclusive pack refuses AFTER parsing, which is exactly the signal we
	// want: the provider name was accepted, and the command stopped for an
	// unrelated, deliberate reason.
	if err != nil && strings.Contains(err.Error(), "owns inference") {
		return nil
	}
	return err
}

// TestModelsHasNoScoredVerbOrFlag: every scored surface went with the router
// (Wave F). `ls`/`show`/`pick`/`route`/`compile` existed only to explain a
// resolver's choice, and `--catalog` only to drop the host filter from those
// listings. A model is chosen by name now, so each of these must get the
// ordinary unknown-command answer rather than a deprecation notice.
func TestModelsHasNoScoredVerbOrFlag(t *testing.T) {
	for _, argv := range [][]string{
		{"models", "route"},
		{"models", "compile"},
		{"models", "ls"},
		{"models", "show"},
		{"models", "pick", "overlord"},
		{"models", "--catalog"},
		{"models", "add", "anthropic", "--catalog"},
	} {
		d, _, _ := testDeps(&config.Config{})
		if err := runRootParse(argv, d); err == nil {
			t.Errorf("`pix %s` parsed; it must not exist on the user CLI", strings.Join(argv, " "))
		}
	}
	// What survives: the fact screen and wiring a provider in.
	if got := reflect.TypeOf(ModelsCmd{}).NumField(); got != 2 {
		t.Errorf("ModelsCmd has %d verbs, want exactly Status + Add", got)
	}
}

// TestExitCodeTable pins the ONE place a Go error becomes a process exit code.
// It replaces 266 individual os.Exit decisions.
func TestExitCodeTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"failure", cli.Usagef("x").(cli.UsageError).Err, 1},
		{"bad invocation", cli.Usagef("bad flag"), 2},
		{"already reported", cli.SilentError{Code: 7}, 7},
	}
	for _, tc := range cases {
		if got := cli.ExitCode(tc.err); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}
