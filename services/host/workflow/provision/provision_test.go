package provision

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/health"
)

// These tests provision REAL state: each step's probe reads the filesystem and
// each apply writes it (one by exec'ing a real script), so "check, apply, check
// again" runs end to end. Nothing fakes a probe's answer — the only way a step
// goes green is by actually creating the thing.

// packStep provisions a pack root: the probe is the production PackProbe, the
// apply writes the manifest it looks for.
func packStep(root string) Step { return namedPackStep("pack", root) }

func namedPackStep(name, root string) Step {
	return Step{
		Name:  name,
		Probe: health.PackProbe{Root: root},
		Apply: func(context.Context) error {
			if err := os.MkdirAll(root, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, "pack.toml"), []byte("name = \"work\"\n"), 0o644)
		},
	}
}

// namedLyingStep claims success without doing anything — the failure mode the
// second check exists to catch.
func namedLyingStep(name, root string) Step {
	return Step{Name: name, Probe: health.PackProbe{Root: root}, Apply: func(context.Context) error { return nil }}
}

func TestApplyThenSecondCheckIsTheOnlySuccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pack")
	o := Run(context.Background(), Options{}, packStep(root))

	if o.Before.Ready() && len(o.Before.Gaps()) == 0 {
		t.Fatal("the first check should have found the gap")
	}
	if len(o.Applied) != 1 || o.Applied[0] != "pack" {
		t.Fatalf("Applied = %v, want [pack]", o.Applied)
	}
	if !o.Verified("pack") {
		t.Fatalf("pack should be verified by the SECOND check: %+v", o.After.Results)
	}
	if o.ExitCode() != health.ExitOK {
		t.Errorf("exit = %d, want %d", o.ExitCode(), health.ExitOK)
	}
	// The second check is a real re-probe, not a replay of the first.
	if before, _ := o.Before.Find("pack"); before.OK() {
		t.Error("the first check must not have been overwritten by the second")
	}
}

func TestApplyThatLiesIsNotSuccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pack")
	o := Run(context.Background(), Options{}, namedLyingStep("pack", root))

	if o.Verified("pack") {
		t.Fatal("a step whose apply returned nil but changed nothing must NEVER count as verified")
	}
	if len(o.Unverified) != 1 || o.Unverified[0].Name != "pack" {
		t.Fatalf("Unverified = %+v, want the pack step", o.Unverified)
	}
	if !strings.Contains(o.Unverified[0].Reason, "second check") {
		t.Errorf("the reason must name the second check, got %q", o.Unverified[0].Reason)
	}
}

func TestAlreadyReadyIsNeverMutated(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte("name = \"work\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	applied := false
	step := Step{Name: "pack", Probe: health.PackProbe{Root: root}, Apply: func(context.Context) error {
		applied = true
		return nil
	}}
	o := Run(context.Background(), Options{}, step)
	if applied {
		t.Error("provisioning must not touch a capability the first check already proved ready")
	}
	if !o.Verified("pack") {
		t.Error("an already-ready step stays verified")
	}
	if len(o.Applied) != 0 {
		t.Errorf("Applied = %v, want none", o.Applied)
	}
}

// unknownProbe is a probe that could not reach its boundary — exactly the state a
// real timeout produces, and the one answer provisioning must refuse to act on.
type readyProbe struct{ name string }

func (p readyProbe) Name() string   { return p.name }
func (p readyProbe) Required() bool { return true }
func (p readyProbe) Check(context.Context) health.Result {
	return health.Result{Name: p.name, Status: health.StatusReady, Detail: "fine"}
}

type unknownProbe struct{ name string }

func (p unknownProbe) Name() string   { return p.name }
func (p unknownProbe) Required() bool { return true }
func (p unknownProbe) Check(context.Context) health.Result {
	return health.Result{Name: p.name, Status: health.StatusUnknown, Detail: "probe timed out"}
}

func TestUnknownIsNeverApplied(t *testing.T) {
	applied := false
	step := Step{Name: "sbx", Probe: unknownProbe{"sbx"}, Apply: func(context.Context) error {
		applied = true
		return nil
	}}
	o := Run(context.Background(), Options{}, step)
	if applied {
		t.Fatal("provisioning must never mutate on an unknown: a probe that could not see is not evidence of a gap")
	}
	if len(o.Skipped) != 1 || !strings.Contains(o.Skipped[0].Reason, "unknown") {
		t.Fatalf("Skipped = %+v, want an unknown skip", o.Skipped)
	}
	// Unknown alone is not a failure, but it is not success either.
	if o.ExitCode() != health.ExitOK {
		t.Errorf("exit = %d, want %d — unknown alone must not fail", o.ExitCode(), health.ExitOK)
	}
	if o.Verified("sbx") {
		t.Error("an unknown step is never verified")
	}
}

// deniedProbe stands in for a capability the org refused: applying cannot help.
type deniedProbe struct{}

func (deniedProbe) Name() string   { return "policy" }
func (deniedProbe) Required() bool { return true }
func (deniedProbe) Check(context.Context) health.Result {
	return health.Result{Name: "policy", Status: health.StatusDenied, Detail: "refused by policy", Fix: "ask your admin"}
}

func TestDeniedIsReportedNotApplied(t *testing.T) {
	applied := false
	o := Run(context.Background(), Options{}, Step{Name: "policy", Probe: deniedProbe{}, Apply: func(context.Context) error {
		applied = true
		return nil
	}})
	if applied {
		t.Fatal("a policy denial is not a setup step; provisioning must not try to apply it")
	}
	if o.ExitCode() != health.ExitNotReady {
		t.Errorf("a required denial must fail, exit = %d", o.ExitCode())
	}
}

func TestApplyErrorIsRecordedAndOtherStepsStillRun(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	bad := Step{Name: "bad", Probe: health.PackProbe{Root: filepath.Join(dir, "bad")},
		Apply: func(context.Context) error { return errors.New("no permission to write") }}
	o := Run(context.Background(), Options{}, bad, namedPackStep("good", good))

	if o.Verified("good") == false {
		t.Errorf("a failing step must not stop the ones after it: %+v", o.After.Results)
	}
	if len(o.Failed) != 1 || o.Failed[0].Name != "bad" {
		t.Fatalf("Failed = %+v, want the bad step", o.Failed)
	}
	if o.Failed[0].Err == nil {
		t.Error("a failed apply must carry its error")
	}
	if o.Verified("bad") {
		t.Error("a step whose apply failed can never be verified")
	}
}

// The apply here execs a real script, so the loop is proven across a process
// boundary too.
func TestApplyRunsRealCommandAndSecondCheckSeesIt(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "pack")
	script := filepath.Join(dir, "install.sh")
	body := "#!/bin/sh\nmkdir -p \"$1\" && printf 'name = \"work\"\\n' > \"$1/pack.toml\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	step := Step{Name: "pack", Probe: health.PackProbe{Root: root}, Apply: func(ctx context.Context) error {
		return exec.CommandContext(ctx, script, root).Run()
	}}
	o := Run(context.Background(), Options{}, step)
	if !o.Verified("pack") {
		t.Fatalf("the second check must see what the command actually did: %+v", o.After.Results)
	}
}

func TestCancelledContextStopsApplyingAndClaimsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := filepath.Join(t.TempDir(), "pack")
	o := Run(ctx, Options{}, packStep(root))
	if o.Verified("pack") {
		t.Fatal("a cancelled provision must never report success")
	}
	if _, err := os.Stat(filepath.Join(root, "pack.toml")); err == nil {
		t.Error("a cancelled provision must not have applied anything")
	}
}

func TestRenderNamesWhatWasAppliedAndWhatRemains(t *testing.T) {
	dir := t.TempDir()
	o := Run(context.Background(), Options{}, namedPackStep("ok", filepath.Join(dir, "ok")), namedLyingStep("nope", filepath.Join(dir, "nope")))
	var b strings.Builder
	o.Render(&b)
	out := b.String()
	for _, want := range []string{"applied", "ok", "NOT verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("render omitted %q:\n%s", want, out)
		}
	}
}

func TestBudgetIsHonoured(t *testing.T) {
	o := Run(context.Background(), Options{Budget: 50 * time.Millisecond}, Step{
		Name:  "slow",
		Probe: slowProbe{},
	})
	if r, _ := o.After.Find("slow"); r.Effective() != health.StatusUnknown {
		t.Errorf("a probe past the budget must be unknown, got %q", r.Effective())
	}
}

type slowProbe struct{}

func (slowProbe) Name() string   { return "slow" }
func (slowProbe) Required() bool { return true }
func (slowProbe) Check(ctx context.Context) health.Result {
	<-ctx.Done()
	return health.Result{Name: "slow", Status: health.StatusUnknown, Detail: "gave up"}
}

// TestOutcomeRender_FailedPhaseIsNotHeadlinedReady: `pix setup` prints a probe
// snapshot, and that snapshot grades REQUIRED CAPABILITIES. A setup PHASE can
// fail while every required capability still passes — which printed a literal
// `✓ ready` two lines above "setup could not apply pack". The snapshot was not
// wrong; the report was claiming something narrower than it had checked.
func TestOutcomeRender_FailedPhaseIsNotHeadlinedReady(t *testing.T) {
	after := health.Run(context.Background(), time.Second, readyProbe{"sbx"})
	var buf bytes.Buffer
	Outcome{After: after, Failed: []Failure{{Name: "pack", Err: errors.New("boom")}}}.Render(&buf)
	out := buf.String()
	if strings.Contains(out, "✓ ready") {
		t.Errorf("a failed phase must not be headlined ready:\n%s", out)
	}
	if !strings.Contains(out, "setup did not finish") || !strings.Contains(out, "pack") {
		t.Errorf("the headline must name what failed:\n%s", out)
	}
	// And with nothing failed, the snapshot's own verdict stands.
	var clean bytes.Buffer
	Outcome{After: after}.Render(&clean)
	if !strings.Contains(clean.String(), "ready") {
		t.Errorf("with no failures the snapshot headline must be used:\n%s", clean.String())
	}
}
