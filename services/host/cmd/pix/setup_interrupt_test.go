// setup_interrupt_test.go — U-W2.03 / AC-P0-304: interruption is a first-class
// state.
//
// A setup that dies partway through (SIGINT, a closed laptop, a dropped SSH
// session) leaves a host in some partial state. The next run must report EXACT
// partial progress — and it must derive that progress FROM PROBES, never from a
// journal file. A journal is state that can itself be stale, and trusting
// recorded state over observed state is this product's core defect; a journal
// that says "keys: done" is worth nothing after someone rotated a vault item.
//
// An interrupt is modelled here as running a PREFIX of the fixed mutation step
// table (setupMutationOrder) and then starting over, which is exactly what a
// SIGINT between two phases does: the steps that ran, ran; the rest never did.
// Every cut point between each pair of steps is exercised.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/workflow/onboard"
	"regexp"
	"strings"
	"testing"
)

// tmpPath scrubs the per-subtest temp root out of a transcript: each cut point
// gets its own hermetic host, so the absolute pack path legitimately differs
// while the VERDICT it carries must not.
//
// Built from os.TempDir() rather than a literal "/tmp" — t.TempDir() lives under
// $TMPDIR, which is /var/folders/… on macOS, so a hardcoded /tmp scrubbed
// nothing there and every subtest diffed its own temp path against the
// baseline's. Both the raw root and its symlink-resolved form are matched, since
// a rendered path may have been canonicalized on its way out.
var tmpPath = regexp.MustCompile(strings.Join(tmpRootPatterns(), "|"))

func tmpRootPatterns() []string {
	seen := map[string]bool{}
	var pats []string
	for _, root := range []string{os.TempDir(), "/tmp"} {
		root = strings.TrimSuffix(filepath.Clean(root), string(os.PathSeparator))
		for _, r := range []string{root, resolveThroughMissing(root)} {
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			pats = append(pats, regexp.QuoteMeta(r)+`/[^ )\n]+`)
		}
	}
	return pats
}

func scrubbed(s string) string { return tmpPath.ReplaceAllString(s, "<tmp>") }

// runMutationPrefix runs the first n mutation steps and stops, simulating a
// SIGINT delivered immediately after step n.
func runMutationPrefix(t *testing.T, env hostenv.Env, n int) {
	t.Helper()
	opts, err := onboard.ParseOnboardArgs([]string{"--yes", "--pull-models"})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := takeSetupInventory(env, opts)
	if err != nil {
		t.Fatal(err)
	}
	var models setupModelsOutcome
	steps := setupMutationSteps(env, inv, opts, strings.NewReader(""), &bytes.Buffer{}, false, &models, &setupPromptBudget{})
	if n > len(steps) {
		t.Fatalf("cut point %d exceeds the %d-step table", n, len(steps))
	}
	if _, err := runSetupMutations(steps[:n]); err != nil {
		t.Fatalf("prefix of %d steps failed: %v", n, err)
	}
}

// Interrupted after every possible step, the NEXT full run completes and lands
// on the same verdicts as an uninterrupted run. That is what "each step is
// individually idempotent" buys: resume is just re-running the command.
func TestSetup_InterruptedAtEveryPhase_NextRunCompletesIdentically(t *testing.T) {
	// The uninterrupted baseline.
	baseEnv := modelsSetupEnv(t, &ollamaWorld{})
	stubProvisionKeysOK(t)
	var base bytes.Buffer
	if err := setupHostPhase(baseEnv, []string{"--yes", "--pull-models"}, strings.NewReader(""), &base, false); err != nil {
		t.Fatalf("baseline run failed: %v\n%s", err, base.String())
	}
	want := scrubbed(renderedVerdicts(t, base.String()))

	for cut := 0; cut <= len(setupMutationOrder); cut++ {
		name := "after-none"
		if cut > 0 {
			name = "after-" + setupMutationOrder[cut-1]
		}
		t.Run(name, func(t *testing.T) {
			w := &ollamaWorld{}
			env := modelsSetupEnv(t, w) // a fresh host per cut point
			stubProvisionKeysOK(t)
			runMutationPrefix(t, env, cut)

			var out bytes.Buffer
			if err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &out, false); err != nil {
				t.Fatalf("resume after %s failed: %v\n%s", name, err, out.String())
			}
			if got := scrubbed(renderedVerdicts(t, out.String())); got != want {
				t.Errorf("resume after %s rendered different verdicts.\nwant:\n%s\ngot:\n%s", name, want, got)
			}
			// Resume must not redo finished work: a tag the interrupted run
			// already pulled is not pulled again.
			for tag, pulls := range map[string]int{"qwen3.5:9b": 1, "nomic-embed-text": 1} {
				if n := w.count("ollama pull " + tag); n > pulls {
					t.Errorf("resume re-pulled %s %d times:\n%v", tag, n, w.calls)
				}
			}
		})
	}
}

// The resume path reads PROBES, not the receipt. A receipt left behind by an
// interrupted run that claims everything is ready must change nothing about
// what the next run reports: the models are still missing, and the report
// still says so.
func TestSetup_StaleReceiptIsNeverTrustedOverProbes(t *testing.T) {
	w := &ollamaWorld{} // nothing pulled
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)

	// Forge the optimistic journal an interrupted run could have left.
	stateDir, err := setupReceiptStateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	forged := setupModelsReceipt{Schema: 1, Consent: "--pull-models", Models: []setupModelReceiptEntry{
		{Tag: "qwen3.5:9b", Status: "ready"}, {Tag: "nomic-embed-text", Status: "ready"},
	}}
	b, _ := json.Marshal(forged)
	if err := os.WriteFile(filepath.Join(stateDir, "setup", "models.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("setup failed: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "not pulled:") {
		t.Errorf("a stale receipt must never be read back as readiness; the probe says the tags are missing, got:\n%s", s)
	}
	if strings.Contains(renderedVerdicts(t, s), "✓ local models") {
		t.Errorf("verdicts must come from the probe, not the receipt, got:\n%s", s)
	}
}
