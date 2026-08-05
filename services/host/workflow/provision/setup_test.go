package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/workflow/onboard"
)

// The setup port's safety scenarios, ported from the deleted phase machine's
// suite. Every one of them runs against REAL executables and REAL files: the
// probes here exec whatever is on PATH, so each test writes a small script that
// behaves like the boundary it stands for (a key store that refuses, an ollama
// that hangs up, one that lists weights). Nothing stubs a probe's answer,
// because the property under test is precisely how a boundary's behaviour is
// classified.

// fixtureBin writes an executable script and puts its directory on PATH for the
// duration of the test. The script IS the boundary — no seam, no fake.
func fixtureBin(t *testing.T, name, script string) {
	t.Helper()
	dir := binDir(t)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func binDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if v, ok := binDirs[t.Name()]; ok {
		return v
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	binDirs[t.Name()] = dir
	t.Setenv("PATH", dir)
	t.Cleanup(func() { delete(binDirs, t.Name()) })
	return dir
}

// binDirs keeps one fixture bin dir per test so several fixtures land in the
// same PATH entry. Tests that use it therefore cannot be parallel, which is
// correct: they own the process environment.
var binDirs = map[string]string{}

func realEnv() hostenv.Env { return hostenv.Env{System: sys.Real{}, Quiet: true} }

// --- what setup may and may not do ------------------------------------------

// The surviving scope is a property of the step table, not of prose: exactly
// three capabilities carry an Apply. If a future edit gives `providers` one,
// this fails — which is the point, since setup collecting credentials again is
// the regression this port exists to prevent.
func TestSetupSteps_OnlyLaunchdPacksAndModelsApply(t *testing.T) {
	cfg := &config.Config{}
	steps := SetupSteps(cfg, realEnv(), onboard.Opts{Packs: []string{"acme/pack"}, PullModels: true}, os.Stderr)
	got := map[string]bool{}
	for _, s := range steps {
		got[s.Name] = s.Apply != nil
	}
	want := map[string]bool{"sbx": false, "launchd": true, "pack": true, "models": true, "providers": false}
	if len(got) != len(want) {
		t.Fatalf("step set = %v, want exactly %v", got, want)
	}
	for name, wantApply := range want {
		if got[name] != wantApply {
			t.Errorf("step %q has apply=%v, want %v", name, got[name], wantApply)
		}
	}
}

// Consent is structural: with no --pull-models there is no apply at all, so a
// multi-gigabyte download cannot be a side effect of running setup. A broad
// --yes is NOT consent.
func TestSetupSteps_ModelPullNeedsExplicitConsent(t *testing.T) {
	cfg := &config.Config{MemoryEmbedModel: "nomic-embed-text"}
	for _, tc := range []struct {
		name string
		opts onboard.Opts
		want bool
	}{
		{"no flags", onboard.Opts{}, false},
		{"--yes alone", onboard.Opts{AssumeYes: true}, false},
		{"--pull-models", onboard.Opts{PullModels: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range SetupSteps(cfg, realEnv(), tc.opts, os.Stderr) {
				if s.Name != "models" {
					continue
				}
				if (s.Apply != nil) != tc.want {
					t.Fatalf("models apply present = %v, want %v", s.Apply != nil, tc.want)
				}
			}
		})
	}
}

// A pack is never manufactured to turn a row green: with no --pack there is
// nothing to adopt, so the step is probe-only and the report names the command.
func TestSetupSteps_NoPackRequestedMeansNoPackApply(t *testing.T) {
	for _, s := range SetupSteps(&config.Config{}, realEnv(), onboard.Opts{}, os.Stderr) {
		if s.Name == "pack" && s.Apply != nil {
			t.Fatal("setup adopted a pack nobody asked for")
		}
	}
}

// --- the provider key probe stays tri-state ---------------------------------

func TestProviderKeysProbe_TriState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   health.Status
		fix    bool
	}{
		// The store ANSWERED and listed a key: the only ready verdict.
		{"answered with a key", "echo ANTHROPIC_API_KEY", health.StatusReady, false},
		// The store ANSWERED and listed none: a verified gap, with the one
		// command that solicits a credential.
		{"answered without keys", "echo GITHUB_TOKEN", health.StatusAbsent, true},
		// The store BROKE. This is the incident: a transient failure must never
		// read as "no key", because a no-key answer refuses a launch.
		{"store failed", "echo 'boom' >&2; exit 3", health.StatusUnknown, false},
		// The store REFUSED. That is a positive answer, and no setup step can
		// grant a permission — so it is denied, and denied is never applied.
		{"store refused", "echo 'permission denied'; exit 1", health.StatusDenied, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureBin(t, "sbx", tc.script)
			r := providerKeysProbe{Env: realEnv()}.Check(context.Background())
			if r.Effective() != tc.want {
				t.Fatalf("status = %q (%s), want %q", r.Effective(), r.Evidence, tc.want)
			}
			if (strings.TrimSpace(r.Fix) != "") != tc.fix {
				t.Errorf("fix = %q, want present=%v", r.Fix, tc.fix)
			}
		})
	}
}

// A key store that is not installed at all is unknown, not absent: we learned
// nothing about this host's keys.
func TestProviderKeysProbe_NoStoreIsUnknown(t *testing.T) {
	binDir(t) // empty PATH: no sbx anywhere
	r := providerKeysProbe{Env: realEnv()}.Check(context.Background())
	if r.Effective() != health.StatusUnknown {
		t.Fatalf("status = %q, want unknown", r.Effective())
	}
}

// --- the models probe and its apply -----------------------------------------

func TestOllamaModelsProbe_ClassifiesTheListing(t *testing.T) {
	listing := "NAME\tID\nnomic-embed-text:latest\tabc\nqwen3:4b\tdef\n"
	for _, tc := range []struct {
		name   string
		script string
		tags   []string
		want   health.Status
	}{
		{"all pulled", "echo '" + listing + "'", []string{"nomic-embed-text", "qwen3:4b"}, health.StatusReady},
		{"one missing", "echo '" + listing + "'", []string{"nomic-embed-text", "gemma3:27b"}, health.StatusAbsent},
		// `ollama list` broke: unknown, so the loop pulls nothing. Pulling a tag
		// we could not prove missing is how a probe failure becomes a download.
		{"list failed", "exit 1", []string{"nomic-embed-text"}, health.StatusUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureBin(t, "ollama", tc.script)
			r := ollamaModelsProbe{Env: realEnv(), Tags: tc.tags}.Check(context.Background())
			if r.Effective() != tc.want {
				t.Fatalf("status = %q (%s), want %q", r.Effective(), r.Evidence, tc.want)
			}
		})
	}
}

// Ollama absent is unknown and optional: a host without it runs Pix fine, and
// setup never installs it.
func TestOllamaModelsProbe_NoOllamaIsUnknownAndOptional(t *testing.T) {
	binDir(t)
	p := ollamaModelsProbe{Env: realEnv(), Tags: []string{"nomic-embed-text"}}
	if p.Required() {
		t.Error("local models must not be required")
	}
	if r := p.Check(context.Background()); r.Effective() != health.StatusUnknown {
		t.Fatalf("status = %q, want unknown", r.Effective())
	}
}

// The apply pulls ONLY tags the listing positively lacked, and it proves it by
// letting a real `ollama` fixture record its own argv.
func TestModelsApply_PullsOnlyConfirmedMissingTags(t *testing.T) {
	dir := binDir(t)
	log := filepath.Join(dir, "pulls.log")
	fixtureBin(t, "ollama", `case "$1" in
list) echo "nomic-embed-text:latest	abc" ;;
pull) echo "$2" >> `+log+` ;;
esac`)
	cfg := &config.Config{MemoryEmbedModel: "nomic-embed-text", MemoryWatcherModel: "qwen3:4b"}
	apply := modelsApply(realEnv(), cfg, onboard.Opts{PullModels: true})
	if apply == nil {
		t.Fatal("--pull-models must produce an apply")
	}
	if err := apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("no pull was attempted: %v", err)
	}
	if strings.TrimSpace(string(got)) != "qwen3:4b" {
		t.Fatalf("pulled %q, want only the missing tag qwen3:4b", strings.TrimSpace(string(got)))
	}
}

// An unreadable listing means the apply pulls NOTHING and says why. This is the
// same rule as the probe's unknown, enforced at the mutation.
func TestModelsApply_UnreadableListingPullsNothing(t *testing.T) {
	dir := binDir(t)
	log := filepath.Join(dir, "pulls.log")
	fixtureBin(t, "ollama", `case "$1" in
list) exit 7 ;;
pull) echo "$2" >> `+log+` ;;
esac`)
	cfg := &config.Config{MemoryEmbedModel: "nomic-embed-text"}
	err := modelsApply(realEnv(), cfg, onboard.Opts{PullModels: true})(context.Background())
	if err == nil {
		t.Fatal("an unreadable listing must fail the apply, not silently pull")
	}
	if _, statErr := os.Stat(log); statErr == nil {
		t.Fatal("a pull ran off an unreadable listing")
	}
}

// --- the loop's verdict is the second check ---------------------------------

// End to end over the real step table with a real (broken) world: no step can
// be proven, and the run's verdict comes from the second check alone.
func TestSetupSteps_UnprovableWorldIsNotReady(t *testing.T) {
	fixtureBin(t, "sbx", "exit 4")
	o := Run(context.Background(), Options{Budget: SetupBudget},
		SetupSteps(&config.Config{}, realEnv(), onboard.Opts{}, os.Stderr)...)
	if o.Verified("providers") {
		t.Fatal("a broken key store must never verify providers")
	}
	// sbx and providers are required and unproven, but both are UNKNOWN here:
	// a host we could not check does not fail the process.
	for _, name := range []string{"sbx", "providers"} {
		r, ok := o.After.Find(name)
		if !ok {
			t.Fatalf("no second-check result for %q", name)
		}
		if r.Effective() != health.StatusUnknown {
			t.Errorf("%s = %q, want unknown", name, r.Effective())
		}
	}
	if o.ExitCode() != health.ExitOK {
		t.Errorf("exit = %d, want %d: unknown alone must not fail", o.ExitCode(), health.ExitOK)
	}
	if len(o.Applied) != 0 {
		t.Errorf("applied %v; nothing unknown may be mutated", o.Applied)
	}
}

// FlagTakesValue and the pack-arg shorthand are the CLI signature Story11
// builds on; they are unchanged by the port.
func TestSetupCLISignatureIsPreserved(t *testing.T) {
	for _, f := range []string{"--account", "--credentials", "--knowledge", "--mcp", "--model", "--models", "--pack", "--with"} {
		if !FlagTakesValue(f) {
			t.Errorf("%s must still consume a value", f)
		}
	}
	if FlagTakesValue("--yes") || FlagTakesValue("--pull-models") {
		t.Error("a boolean flag must not swallow the next token")
	}
	if got := NormalizeSetupPackArg("acme/work-pack"); got != "https://github.com/acme/work-pack.git" {
		t.Errorf("owner/repo shorthand = %q", got)
	}
	if got := NormalizeSetupPackArg("./local/pack"); got != "./local/pack" {
		t.Errorf("a path must be left alone, got %q", got)
	}
}
