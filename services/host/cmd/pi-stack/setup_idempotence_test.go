// setup_idempotence_test.go — U-W2.03 / AC-P0-305.
//
// Two consecutive `pi-stack setup` runs must produce BYTE-IDENTICAL rendered
// verdicts, and the second must be a cheap no-op.
//
// This is the cheapest possible detector for "we printed an intention": a
// report that says "✓ pulled qwen3.5:9b" because THIS run pulled it renders
// differently on the second run, when there was nothing to pull. A report
// derived purely from post-mutation probes cannot tell the two runs apart,
// because the host is in the same state both times. So the byte comparison
// below is not a formatting test — it is the AC-P0-302 invariant, observed
// from outside.
package main

import (
	"bytes"
	"strings"
	"testing"
)

// renderedVerdicts extracts the report section (everything the user reads as a
// verdict) from a setup transcript. The phases above it legitimately differ
// between runs — a first run creates a pack and pulls models, a second finds
// both already there — and that difference is exactly what must NOT reach the
// verdicts.
func renderedVerdicts(t *testing.T, transcript string) string {
	t.Helper()
	i := strings.Index(transcript, "Setup summary:")
	if i < 0 {
		t.Fatalf("no report section in the transcript:\n%s", transcript)
	}
	return transcript[i:]
}

// Two consecutive runs over the same host render the same verdicts, byte for
// byte, and the second one pulls nothing and creates nothing.
func TestSetup_TwoRuns_ByteIdenticalVerdicts_SecondIsNoOp(t *testing.T) {
	w := &ollamaWorld{}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)

	var first bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &first, false); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, first.String())
	}
	firstPulls := w.count("ollama pull")
	if firstPulls == 0 {
		t.Fatalf("the first run was expected to pull the configured tags:\n%v", w.calls)
	}
	firstInits := w.count("git -C")

	var second bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &second, false); err != nil {
		t.Fatalf("second run failed: %v\n%s", err, second.String())
	}

	got, want := renderedVerdicts(t, second.String()), renderedVerdicts(t, first.String())
	if got != want {
		t.Errorf("two consecutive runs rendered different verdicts.\nfirst:\n%s\nsecond:\n%s", want, got)
	}
	if n := w.count("ollama pull") - firstPulls; n != 0 {
		t.Errorf("the second run must pull nothing, got %d pulls:\n%v", n, w.calls)
	}
	if n := w.count("git -C") - firstInits; n != 0 {
		t.Errorf("the second run must not re-create the default pack, got %d git calls:\n%v", n, w.calls)
	}
}

// The same property with nothing to do at all: a host that is already fully
// set up renders identically on every run.
func TestSetup_AlreadyReady_RunsAreIdentical(t *testing.T) {
	w := &ollamaWorld{have: map[string]bool{"qwen3.5:9b": true, "nomic-embed-text": true}}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)

	var a, b bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &a, false); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, a.String())
	}
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &b, false); err != nil {
		t.Fatalf("second run failed: %v\n%s", err, b.String())
	}
	if renderedVerdicts(t, a.String()) != renderedVerdicts(t, b.String()) {
		t.Errorf("an already-ready host must render identically.\nfirst:\n%s\nsecond:\n%s", a.String(), b.String())
	}
	if n := w.count("ollama pull"); n != 0 {
		t.Errorf("nothing was missing; nothing may be pulled:\n%v", w.calls)
	}
}
