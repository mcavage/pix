// setup_requested_test.go — U-W2.02 / AC-P0-209, AC-P0-210: `requested` is
// `optional` PROMOTED to blocking for one invocation only.
//
// The contract, in one sentence and tested three ways:
//
//	`pix setup --pull-models` with Ollama down exits 1. `pix setup`
//	with the same Ollama down exits 0 with an optional ⚠ row. Stale optional
//	config never blocks unrelated repair.
//
// The first two clauses are the same host, the same probe and the same
// verdicts — only the FLAG differs, which is the whole point of putting
// promotion in the readiness type rather than in each command's flag handling.
// The third is the reason promotion is per-invocation: an optional integration
// somebody configured months ago must never block today's unrelated repair.
package main

import (
	"bytes"
	"errors"
	"pix/host/readiness"
	"pix/host/workflow/man"
	"pix/host/workflow/onboard"
	"strings"
	"testing"

	"pix/host/config"
)

// normalizeCopy collapses runs of whitespace so a sentence can be asserted
// verbatim regardless of how the surrounding help text or roff source wraps it.
func normalizeCopy(s string) string { return strings.Join(strings.Fields(s), " ") }

// requestedExitContract is the AC-P0-210 sentence, which must appear verbatim
// in `pix help setup` AND the man page.
const requestedExitContract = "pix setup --pull-models with Ollama down exits 1. " +
	"pix setup with the same Ollama down exits 0 with an optional ⚠ row. " +
	"Stale optional config never blocks unrelated repair."

// --pull-models is an explicit request for the model axes. With Ollama down
// nothing can be pulled and nothing can be proven, so the request positively
// failed: setup returns an error (exit 1), naming the axes.
func TestSetupRequested_PullModelsWithOllamaDown_Fails(t *testing.T) {
	w := &ollamaWorld{listErr: true}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatalf("--pull-models with Ollama down must fail (exit 1), got success:\n%s", out.String())
	}
	var usage errUsage
	if errors.As(err, &usage) {
		t.Errorf("a failed request is exit 1, not a usage error (exit 2), got: %v", err)
	}
	if !strings.Contains(err.Error(), "model.watcher") {
		t.Errorf("the error must name the requested axes that did not end ready, got: %v", err)
	}
	if n := w.count("ollama pull"); n != 0 {
		t.Errorf("nothing may be pulled when the probe could not confirm a tag missing, got %d pulls:\n%v", n, w.calls)
	}
}

// The SAME host without the flag: the model axes stay optional, so setup
// succeeds (exit 0) and the gap is an optional ⚠ row, not a failure.
func TestSetupRequested_NoFlagWithOllamaDown_SucceedsWithOptionalWarnRow(t *testing.T) {
	w := &ollamaWorld{listErr: true}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("an unrequested optional axis must never block setup, got: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "⚠ local models") {
		t.Errorf("the same gap must still be reported as an optional ⚠ row, got:\n%s", out.String())
	}
}

// Stale optional config never blocks unrelated repair: a Google Workspace
// account configured long ago whose authorization no longer probes healthy is
// a ⚠/✗ row, not a reason to fail a run that asked for nothing.
func TestSetupRequested_StaleOptionalConfigNeverBlocks(t *testing.T) {
	w := &ollamaWorld{gogAuthErr: true}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.GogAccount = "stale@example.com"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("stale optional config must not block an unrelated repair, got: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "stale@example.com") {
		t.Errorf("the stale account must still be reported, got:\n%s", out.String())
	}
}

// The flag→axis mapping is the ONLY thing setup owns; the promotion rule
// itself lives in the readiness type.
func TestSetupRequestedAxes_FlagMapping(t *testing.T) {
	if got := setupRequestedAxes(onboard.Opts{}); len(got) != 0 {
		t.Errorf("no flags must promote nothing, got %v", got)
	}
	got := readiness.AxisNames(setupRequestedAxes(onboard.Opts{PullModels: true, GoogleWorkspace: true, Mcp: []string{"slack"}}))
	want := []string{"gworkspace", "mcp:slack", "model.bridge", "model.embed", "model.watcher", "ollama.host"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("setupRequestedAxes = %v, want %v", got, want)
	}
}

// RequestedShortfall reports only PROMOTED axes that did not end ready, and
// never invents one for an axis the snapshot does not contain.
func TestRequestedShortfall_OnlyPromotedAndPresentAxes(t *testing.T) {
	req := readiness.Request{
		Axes:      []readiness.Axis{readiness.AxisPack, readiness.AxisGworkspace, readiness.AxisModelWatcher},
		Requested: []readiness.Axis{readiness.AxisGworkspace, readiness.AxisModelWatcher, readiness.AxisModelEmbed}, // embed has no builder
	}
	s := readiness.Build(req, map[readiness.Axis]readiness.AxisBuilder{
		readiness.AxisPack: func() []readiness.Check {
			return []readiness.Check{{Label: "pack", Verdict: readiness.VerdictTodo}}
		},
		readiness.AxisGworkspace: func() []readiness.Check {
			return []readiness.Check{{Label: "gw", Verdict: readiness.VerdictUnverifiable}}
		},
		readiness.AxisModelWatcher: func() []readiness.Check {
			return []readiness.Check{{Label: "watcher", Verdict: readiness.VerdictReady}}
		},
	})
	got := readiness.AxisNames(s.RequestedShortfall(req))
	if strings.Join(got, ",") != "gworkspace" {
		t.Errorf("RequestedShortfall = %v, want [gworkspace] (pack was not requested, watcher is ready, embed is absent)", got)
	}
}

// The contract sentence is user-facing copy, so it is pinned in both places a
// user reads it.
func TestRequestedExitContract_InHelpAndMan(t *testing.T) {
	if !strings.Contains(normalizeCopy(setupUsage), requestedExitContract) {
		t.Errorf("`pix help setup` must state the exit contract verbatim:\n%s", requestedExitContract)
	}
	b, err := man.Source(), error(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The man source escapes dashes and marks up commands; unescape those two
	// mechanical transforms before comparing the sentence.
	man := normalizeCopy(strings.NewReplacer(`\-`, "-", `.B `, "", `.BR `, "", `"`, "", "\n", " ").Replace(string(b)))
	if !strings.Contains(man, normalizeCopy(requestedExitContract)) {
		t.Errorf("the man page must state the exit contract verbatim:\n%s", requestedExitContract)
	}
}
