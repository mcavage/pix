package provision

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/workflow/pack"
)

// Setup's safety scenarios, every one against REAL executables and REAL files:
// the probes exec whatever is on PATH, so each test writes a small script that
// behaves like the boundary it stands for (a key store that refuses, an ollama
// that hangs up, one that lists weights). Nothing stubs a probe's answer — how a
// boundary's behaviour is CLASSIFIED is the property under test.

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

// Scope is a property of the step table, not of prose: exactly three
// capabilities carry an Apply. If a future edit gives `providers` one, this
// fails — setup collecting credentials again is the regression it prevents.
func TestSetupSteps_OnlyLaunchdPacksAndModelsApply(t *testing.T) {
	cfg := &config.Config{}
	steps := setupSteps(cfg, realEnv(), Opts{Packs: []string{"acme/pack"}, PullModels: true}, os.Stderr)
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

// Consent is structural: a step carries an Apply only because a flag asked for
// it. Without --pull-models a multi-gigabyte download cannot be a side effect of
// running setup, and without --pack no pack is manufactured to turn a row green.
// A broad --yes is NOT consent — it suppresses questions, it does not answer them.
func TestSetupSteps_ApplyOnlyWhenAFlagAskedForIt(t *testing.T) {
	cfg := &config.Config{MemoryEmbedModel: "nomic-embed-text"}
	for _, tc := range []struct {
		name, step string
		opts       Opts
		want       bool
	}{
		{"no flags: no pull", "models", Opts{}, false},
		{"--yes alone is not consent", "models", Opts{AssumeYes: true}, false},
		{"--pull-models", "models", Opts{PullModels: true}, true},
		{"no flags: no pack adoption", "pack", Opts{}, false},
		{"--pack", "pack", Opts{Packs: []string{"acme/pack"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range setupSteps(cfg, realEnv(), tc.opts, os.Stderr) {
				if s.Name == tc.step && (s.Apply != nil) != tc.want {
					t.Fatalf("%s apply present = %v, want %v", tc.step, s.Apply != nil, tc.want)
				}
			}
		})
	}
}

// A pack RunPackUse refuses (Tier-1, non-TTY, no --yes) must abort the ENTIRE
// setup. The discarded error this guards against let a refused pack fall through
// to "setup complete": PackProbe is not required (a host with no pack is fine),
// so the second check alone never caught it. Real pack, real gate, real config.
func TestPackApply_RefusedPackAbortsAndConfigUnchanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))

	root := filepath.Join(dir, "work-pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The wrapper must actually exist to fingerprint (else the gate fails on a
	// missing file, not on the TTY refusal this test is about).
	if err := os.WriteFile(filepath.Join(root, "bin", "warehouse"), []byte("#!/bin/sh\necho warehouse\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A host=true proxy is a Tier-1 host-exec facet on its own: no MCP, no
	// classifier wiring needed to force the gate.
	if err := pack.WriteManifest(root, pack.Manifest{Name: "work", Schema: 1,
		Proxies: []pack.PackProxy{{Name: "warehouse", Host: true}}}); err != nil {
		t.Fatal(err)
	}

	// --- unit level: packApply itself must surface RunPackUse's error -------
	apply := packApply(realEnv(), Opts{Packs: []string{root}}, io.Discard)
	if apply == nil {
		t.Fatal("--pack must produce an apply")
	}
	if err := apply(context.Background()); err == nil {
		t.Fatal("a Tier-1 pack refused non-interactively must fail the apply, not swallow the error")
	} else if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the propagated refusal must name --yes, got: %v", err)
	}

	// --- provision-loop level: Failed, never Applied -------------------------
	o := Run(context.Background(), Options{Budget: setupBudget},
		setupSteps(&config.Config{}, realEnv(), Opts{Packs: []string{root}}, io.Discard)...)
	if len(o.Failed) != 1 || o.Failed[0].Name != "pack" {
		t.Fatalf("outcome.Failed = %+v, want exactly one failure named pack", o.Failed)
	}
	for _, applied := range o.Applied {
		if applied == "pack" {
			t.Fatal("a refused pack must never be recorded as applied")
		}
	}

	// --- RunSetup level: the whole command aborts, never reports success ----
	if err := RunSetup(realEnv(), []string{"--pack", root}, io.Discard); err == nil {
		t.Fatal("RunSetup must abort on a refused pack, not report success")
	} else if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("RunSetup's error must carry the pack's own refusal, got: %v", err)
	}

	// --- and nothing committed: no later apply, config still has no pack ----
	cfg, lerr := config.Load()
	if lerr != nil {
		t.Fatalf("config.Load: %v", lerr)
	}
	if cfg.Pack != "" {
		t.Errorf("a refused pack must never be adopted; cfg.Pack = %q", cfg.Pack)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "pack-trust.json")); !os.IsNotExist(statErr) {
		t.Error("no Tier-1 acceptance may be recorded on refusal")
	}
}

// --- the provider key probe stays tri-state ---------------------------------

// The tri-state classification itself is health.ProviderKeyProbe's (proven in
// health/probes_test.go, in both any-of and all-of modes). What setup owns is
// the WIRING: any-of over the env vars a launch needs, so the row that would
// otherwise refuse a launch says "unknown" when the store merely broke, and
// "ready" as soon as ONE key is listed. Real `sbx` fixtures, through the real
// step table.
func TestSetupSteps_ProviderKeysAreAnyOfAndTriState(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         health.Status
	}{
		{"one key is enough", "echo ANTHROPIC_API_KEY", health.StatusReady},
		{"answered without keys", "echo GITHUB_TOKEN", health.StatusAbsent},
		// The incident: a transient failure must never read as "no key", because
		// a no-key answer refuses a launch.
		{"store failed", "echo 'boom' >&2; exit 3", health.StatusUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureBin(t, "sbx", tc.script)
			var probe health.Probe
			for _, s := range setupSteps(&config.Config{}, realEnv(), Opts{}, io.Discard) {
				if s.Name == "providers" {
					probe = s.Probe
					if s.Apply != nil {
						t.Fatal("setup must never carry an apply for provider keys")
					}
				}
			}
			r := probe.Check(context.Background())
			if r.Effective() != tc.want {
				t.Fatalf("status = %q (%s), want %q", r.Effective(), r.Evidence, tc.want)
			}
		})
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

// Ollama absent is unknown and optional: a host without it runs Pix fine.
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

// The apply pulls ONLY tags the listing positively lacked, proven by letting a
// real `ollama` fixture record its own argv.
func TestModelsApply_PullsOnlyConfirmedMissingTags(t *testing.T) {
	dir := binDir(t)
	log := filepath.Join(dir, "pulls.log")
	fixtureBin(t, "ollama", `case "$1" in
list) echo "nomic-embed-text:latest	abc" ;;
pull) echo "$2" >> `+log+` ;;
esac`)
	cfg := &config.Config{MemoryEmbedModel: "nomic-embed-text", MemoryWatcherModel: "qwen3:4b"}
	apply := modelsApply(realEnv(), cfg, Opts{PullModels: true})
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

// An unreadable listing pulls NOTHING: the probe's unknown rule, at the mutation.
func TestModelsApply_UnreadableListingPullsNothing(t *testing.T) {
	dir := binDir(t)
	log := filepath.Join(dir, "pulls.log")
	fixtureBin(t, "ollama", `case "$1" in
list) exit 7 ;;
pull) echo "$2" >> `+log+` ;;
esac`)
	cfg := &config.Config{MemoryEmbedModel: "nomic-embed-text"}
	err := modelsApply(realEnv(), cfg, Opts{PullModels: true})(context.Background())
	if err == nil {
		t.Fatal("an unreadable listing must fail the apply, not silently pull")
	}
	if _, statErr := os.Stat(log); statErr == nil {
		t.Fatal("a pull ran off an unreadable listing")
	}
}

// --- the loop's verdict is the second check ---------------------------------

// End to end over the real step table in a real (broken) world: nothing can be
// proven, and the verdict comes from the second check alone.
func TestSetupSteps_UnprovableWorldIsNotReady(t *testing.T) {
	fixtureBin(t, "sbx", "exit 4")
	o := Run(context.Background(), Options{Budget: setupBudget},
		setupSteps(&config.Config{}, realEnv(), Opts{}, os.Stderr)...)
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

// The pack-arg shorthand is the CLI signature Story11 builds on; it is
// unchanged by the fold. (Flag arity is no longer a separate table: kong owns
// the user-facing grammar and ParseSetupArgs owns the host argv, so the third
// copy that had to agree with both is gone.)
func TestSetupCLISignatureIsPreserved(t *testing.T) {
	if got := NormalizeSetupPackArg("acme/work-pack"); got != "https://github.com/acme/work-pack.git" {
		t.Errorf("owner/repo shorthand = %q", got)
	}
	if got := NormalizeSetupPackArg("./local/pack"); got != "./local/pack" {
		t.Errorf("a path must be left alone, got %q", got)
	}
}
