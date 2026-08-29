package provision

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/secret"
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

// TestSetupSteps_NoEnvironmentStep pins D13: `pix setup` has no environment
// step, no prompt, no probe. setupSteps is the WHOLE of what setup
// provisions (setup.go's own doc comment), so proving no step here mentions
// an environment proves the negative for the whole command, not just the
// four known steps by name.
func TestSetupSteps_NoEnvironmentStep(t *testing.T) {
	steps := setupSteps(&config.Config{}, realEnv(), Opts{}, strings.NewReader(""), io.Discard, false)
	for _, s := range steps {
		name := stepName(s)
		if strings.Contains(strings.ToLower(name), "env") {
			t.Errorf("setup step %q mentions an environment; D13 forbids an environment step, prompt, or probe in `pix setup`", name)
		}
	}
}

// neutralHostSteps pins the two probes a pack-focused test does not mean to
// exercise — sbx and launchd — to a deterministic, always-ready fixture PATH,
// and traps the real launchd installer behind a recorder that fails the test
// if it is EVER invoked. Without this seam, setupSteps' sbx/launchd probes run
// against whatever the test process's REAL PATH and REAL launchd happen to
// hold: a laptop with both present classifies the two steps differently than a
// CI box with neither, and — the actual incident this closes — a laptop where
// the pix LaunchAgent simply is not loaded yet turns the launchd row into a
// VERIFIED gap, so Run() calls the real installLaunchd (service.Install) and
// installs a REAL LaunchAgent on the developer's machine as a side effect of a
// test that is only supposed to be about pack adoption. Wiring this explicit
// fixture/seam, rather than relying on whatever PATH the test happened to
// inherit, is what makes a pack-only test's outcome depend on packs and
// nothing else.
func neutralHostSteps(t *testing.T) {
	t.Helper()
	fixtureBin(t, "sbx", "echo 'sbx version 9.9.9'")
	fixtureBin(t, "launchctl", "echo 'state = running'")
	prev := installLaunchd
	installLaunchd = func(io.Writer) error {
		t.Fatal("a pack-only test must never install the real launchd agent")
		return nil
	}
	t.Cleanup(func() { installLaunchd = prev })
}

// --- what setup may and may not do ------------------------------------------

// Scope is a property of the step table, not of prose: with every consent given
// and a terminal to ask on, exactly four capabilities carry an Apply. sbx never
// does — setup does not run a package manager behind anyone's back.
//
// `providers` used to be pinned to false here, and that rule cost more than it
// bought: providers is REQUIRED, so a host with no pack could not reach a
// passing `pix setup` at all, and the command ended by printing a repair it knew
// how to perform. What the rule was actually protecting is now the test below —
// setup may ask, and may never answer for you.
func TestSetupSteps_EveryConsentedCapabilityApplies(t *testing.T) {
	cfg := &config.Config{}
	steps := setupSteps(cfg, realEnv(), Opts{Packs: []string{"acme/pack"}, PullModels: true},
		strings.NewReader(""), os.Stderr, true)
	got := map[string]bool{}
	for _, s := range steps {
		got[s.Name] = s.Apply != nil
	}
	want := map[string]bool{"sbx": false, "launchd": true, "pack": true, "models": true, "providers": true}
	if len(got) != len(want) {
		t.Fatalf("step set = %v, want exactly %v", got, want)
	}
	for name, wantApply := range want {
		if got[name] != wantApply {
			t.Errorf("step %q has apply=%v, want %v", name, got[name], wantApply)
		}
	}
}

// The credential rule that survived: setup may only ASK for a key, so it carries
// an Apply for `providers` exactly when there is a terminal to ask on and the
// user has not said --yes. A broad --yes suppresses questions; it does not
// answer them, and a key is the one thing setup must never pick for you. Both
// scripted shapes fall back to the report naming `pix models add`.
func TestSetupSteps_ProvidersAppliesOnlyWhenItCanAsk(t *testing.T) {
	for _, tc := range []struct {
		name        string
		opts        Opts
		interactive bool
		want        bool
	}{
		{"a terminal, no --yes: ask", Opts{}, true, true},
		{"no terminal: never ask", Opts{}, false, false},
		{"--yes is not an answer", Opts{AssumeYes: true}, true, false},
		{"neither", Opts{AssumeYes: true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range setupSteps(&config.Config{}, realEnv(), tc.opts,
				strings.NewReader(""), io.Discard, tc.interactive) {
				if s.Name == "providers" && (s.Apply != nil) != tc.want {
					t.Fatalf("providers apply present = %v, want %v", s.Apply != nil, tc.want)
				}
			}
		})
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
			for _, s := range setupSteps(cfg, realEnv(), tc.opts, strings.NewReader(""), os.Stderr, false) {
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
	// This test is about pack adoption ONLY: neutralize sbx/launchd so its
	// outcome cannot depend on this host's real PATH or real launchd state.
	neutralHostSteps(t)
	// The composition root binds pack adoption; this test is the REAL one, so it
	// wires the same adopter cmd/pix does rather than a stand-in.
	Injected.PackApply = pack.SetupAdopter(nil, nil)
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
	if err := pack.WriteManifest(root, packinfo.Manifest{Name: "work", Schema: 1,
		Proxies: []packinfo.PackProxy{{Name: "warehouse", Host: true}}}); err != nil {
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
		setupSteps(&config.Config{}, realEnv(), Opts{Packs: []string{root}}, strings.NewReader(""), io.Discard, false)...)
	if len(o.Failed) != 1 || o.Failed[0].Name != "pack" {
		t.Fatalf("outcome.Failed = %+v, want exactly one failure named pack", o.Failed)
	}
	for _, applied := range o.Applied {
		if applied == "pack" {
			t.Fatal("a refused pack must never be recorded as applied")
		}
	}

	// --- RunSetup level: the whole command aborts, never reports success ----
	if err := RunSetup(realEnv(), []string{"--pack", root}, strings.NewReader(""), io.Discard, false); err == nil {
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

// The mirror of the refusal test above: a pack apply that SUCCEEDS must be
// verified against the root it just wrote, not the root setupSteps captured
// into the pack probe BEFORE the apply ran. setupSteps builds the whole step
// table — including the pack probe — from one cfg loaded at the top of
// RunSetup; PackApply (SetupAdopter -> RunPackUse) loads and mutates its OWN
// config.Load(), a different *Config, and saves it to disk. A probe that
// captured cfg.Pack at construction time never sees that write, so the second
// check re-probes the pre-adoption (empty) root and reports the just-adopted
// pack as still absent even though `pix pack use` succeeded — exactly the
// false-negative this regression pins.
func TestPackApply_SuccessIsVerifiedAgainstTheRootApplyJustWrote(t *testing.T) {
	dir := t.TempDir()
	// This test is about pack adoption ONLY: neutralize sbx/launchd so its
	// outcome cannot depend on this host's real PATH or real launchd state.
	neutralHostSteps(t)
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	Injected.PackApply = pack.SetupAdopter(nil, nil)

	root := filepath.Join(dir, "quiet-pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A host=true proxy makes this Tier-1 (same shape as the refusal test
	// above), but here setup carries --yes, so the trust gate auto-accepts
	// instead of refusing — this test is about the SUCCESS path.
	if err := os.WriteFile(filepath.Join(root, "bin", "warehouse"), []byte("#!/bin/sh\necho warehouse\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pack.WriteManifest(root, packinfo.Manifest{Name: "quiet", Schema: 1,
		Proxies: []packinfo.PackProxy{{Name: "warehouse", Host: true}}}); err != nil {
		t.Fatal(err)
	}

	// The cfg setupSteps sees has NO pack configured — the exact stale snapshot
	// the bug froze into the probe.
	cfg := &config.Config{}
	steps := setupSteps(cfg, realEnv(), Opts{Packs: []string{root}, AssumeYes: true}, strings.NewReader(""), io.Discard, false)
	o := Run(context.Background(), Options{Budget: setupBudget}, steps...)

	if len(o.Failed) != 0 {
		t.Fatalf("pack apply failed: %+v", o.Failed)
	}
	if !slices.Contains(o.Applied, "pack") {
		t.Fatalf("pack was not recorded as applied: %+v", o.Applied)
	}
	if !o.Verified("pack") {
		after, _ := o.After.Find("pack")
		t.Fatalf("second check did not verify the adopted pack: %+v", after)
	}
	after, ok := o.After.Find("pack")
	if !ok {
		t.Fatal("no second-check result for pack")
	}
	if wantBase := filepath.Base(root); after.Detail != wantBase {
		t.Errorf("second check reports %q, want %q — it must probe the root PackApply just wrote, not the pre-apply snapshot",
			after.Detail, wantBase)
	}

	// The config ON DISK — never the stale in-memory cfg setupSteps captured —
	// now names this pack.
	got, lerr := config.Load()
	if lerr != nil {
		t.Fatalf("config.Load: %v", lerr)
	}
	if packinfo.CanonicalizePackRoot(got.Pack) != packinfo.CanonicalizePackRoot(root) {
		t.Fatalf("cfg.Pack = %q, want %q", got.Pack, root)
	}
}

// TestNeutralHostSteps_LaunchdGapNeverReachesTheRealInstaller is the seam's own
// self-test: it puts the OTHER real incident in front of the loop — a launchd
// fixture that answers "not loaded" (a verified gap, on ANY host) alongside a
// pack request — and proves the loop still calls installLaunchd (the step
// really runs; this is not a test that merely skips the row) while the call
// lands on the recorder, never on service.Install. This is the property
// TestPackApply_RefusedPackAbortsAndConfigUnchanged and
// TestPackApply_SuccessIsVerifiedAgainstTheRootApplyJustWrote rely on
// neutralHostSteps for: their own launchd fixture reports "already loaded" so
// the row is never even a gap, so THIS test is what proves the recorder
// actually intercepts the dangerous path rather than merely never being
// exercised.
func TestNeutralHostSteps_LaunchdGapNeverReachesTheRealInstaller(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	fixtureBin(t, "sbx", "echo 'sbx version 9.9.9'")
	// notLoaded's own vocabulary: this is what launchctl says for a genuinely
	// absent agent, so the probe classifies this a VERIFIED gap, not unknown.
	fixtureBin(t, "launchctl", "echo 'Could not find service' >&2; exit 1")
	prev := installLaunchd
	var recorded int
	installLaunchd = func(io.Writer) error { recorded++; return nil }
	t.Cleanup(func() { installLaunchd = prev })

	o := Run(context.Background(), Options{Budget: setupBudget}, setupSteps(&config.Config{}, realEnv(), Opts{}, strings.NewReader(""), io.Discard, false)...)

	if recorded != 1 {
		t.Fatalf("installLaunchd (the recorder) was called %d times, want exactly 1 — a verified launchd gap must still drive the loop", recorded)
	}
	if !slices.Contains(o.Applied, "launchd") {
		t.Fatalf("launchd was not recorded as applied: %+v", o.Applied)
	}
}

// --- the provider key probe stays tri-state ---------------------------------

// providersProbe pulls the providers step out of the REAL step table, built the
// way a script runs it (no terminal), and asserts on the way past that setup
// still cannot repair a key it was never allowed to ask about.
func providersProbe(t *testing.T) health.Probe {
	t.Helper()
	for _, s := range setupSteps(&config.Config{}, realEnv(), Opts{}, strings.NewReader(""), io.Discard, false) {
		if s.Name == "providers" {
			if s.Apply != nil {
				t.Fatal("with no terminal, setup must never carry an apply for provider keys")
			}
			return s.Probe
		}
	}
	t.Fatal("no providers step in the setup table")
	return nil
}

// A host whose backends carry their own credential has no key to add, so setup
// must not open a row against it — and must read that from the config as it is
// AT THE CHECK, since the pack step's own apply is what writes those backends.
func TestSetupSteps_KeylessInferenceNeedsNoProviderKey(t *testing.T) {
	fixtureBin(t, "sbx", "echo GITHUB_TOKEN") // answers, lists no model key
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PIX_CONFIG", path)

	probe := providersProbe(t)
	// Built against a config with no inference at all: the row is a real gap.
	if r := probe.Check(context.Background()); r.Effective() != health.StatusAbsent {
		t.Fatalf("status = %q (%s), want absent before the pack lands", r.Effective(), r.Evidence)
	}
	// Now the pack lands, exactly as the pack step's apply would leave it.
	if err := os.WriteFile(path, []byte(`
[inference]
  exclusive_source = "/packs/gw"
  [inference.backends.gw-anthropic]
    driver = "openai-compatible"
    base_url = "https://gw.example.com/anthropic"
    auth = "sbx-session"
    key_env = "DOCKER_TOKEN"
    credential_service = "sbx-login"
    source = "/packs/gw"
  [[inference.models]]
    model = "anthropic/claude-opus-5"
    backend = "gw-anthropic"
    upstream_id = "claude-opus-5"
    available = true
    source = "/packs/gw"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The SAME probe value, re-checked: this is the second check setup grades.
	r := probe.Check(context.Background())
	if r.Effective() != health.StatusReady {
		t.Fatalf("status = %q (%s), want ready once the keyless backends are on disk", r.Effective(), r.Evidence)
	}
	if r.Fix != "" {
		t.Errorf("fix = %q, want none: there is no key to add", r.Fix)
	}
	if !strings.Contains(r.Evidence, "gw-anthropic") {
		t.Errorf("evidence = %q, must name what answers instead", r.Evidence)
	}
}

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
		// The fixture speaks the STORE's vocabulary, which is the whole point:
		// this case used to echo ANTHROPIC_API_KEY and pass, because the probe
		// was searching for that name too. Test and code agreed with each other
		// and neither agreed with `sbx secret ls`, which lists a secret under the
		// name it was stored as — a provider name. See
		// TestProvidersProbe_MatchesTheNameTheKeyStoreLists.
		{"one key is enough", "echo anthropic", health.StatusReady},
		{"answered without keys", "echo github", health.StatusAbsent},
		// The incident: a transient failure must never read as "no key", because
		// a no-key answer refuses a launch.
		{"store failed", "echo 'boom' >&2; exit 3", health.StatusUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureBin(t, "sbx", tc.script)
			// The providers row reads the LIVE config (its keyless answer must
			// see a pack adopted by this same run), so pin it — otherwise the
			// key store's verdict is graded against whatever inference the
			// developer's own ~/.config/pix/config.toml happens to declare, and
			// a laptop running a keyless pack classifies this row differently
			// than CI. Same seam, same reason as neutralHostSteps.
			t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
			r := providersProbe(t).Check(context.Background())
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

// TestOllamaModelsProbe_ZeroTagsIsOff: a host that names no watcher/embed/
// bridge model configured nothing to pull, and that is the user's own choice
// not to run local models — a supported end state, not a capability that was
// checked and passed. Before StatusOff existed this read StatusReady, a green
// check over a probe that verified nothing.
func TestOllamaModelsProbe_ZeroTagsIsOff(t *testing.T) {
	r := ollamaModelsProbe{Env: realEnv(), Tags: nil}.Check(context.Background())
	if r.Effective() != health.StatusOff {
		t.Fatalf("status = %q, want off", r.Effective())
	}
	if r.OK() || r.Missing() {
		t.Errorf("off must be neither OK nor Missing, got OK=%v Missing=%v", r.OK(), r.Missing())
	}
	if r.Fix != "" {
		t.Errorf("an off models row must carry no fix, got %q", r.Fix)
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
	// Unprovable means unprovable: pin the config the pack and providers rows
	// re-read, so this world is the fixture's, not the developer's.
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	o := Run(context.Background(), Options{Budget: setupBudget},
		setupSteps(&config.Config{}, realEnv(), Opts{}, strings.NewReader(""), os.Stderr, false)...)
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

// --- the provider-key interview ---------------------------------------------

// noHostConfig points config.Path() at an absent file in a temp dir. The
// interview reads the host config to decide whether a key is needed at all, so
// without this a test would grade the DEVELOPER's real ~/.config/pix/config.toml
// and pass or fail on whichever laptop ran it.
func noHostConfig(t *testing.T) {
	t.Helper()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
}

// The regression this whole step exists for: on a host with no pack and no key,
// `pix setup` printed `pix models add anthropic` and exited non-zero. It now
// asks, and hands the answer to the ONE command that solicits a credential —
// setup itself neither prompts for a ref nor writes one.
func TestProvidersApply_AsksAndDelegatesTheAnswer(t *testing.T) {
	noHostConfig(t)
	var gotProvider string
	var gotInteractive bool
	prev := Injected.AddProvider
	Injected.AddProvider = func(_ hostenv.Env, _ io.Reader, _ io.Writer, interactive bool, provider string) error {
		gotProvider, gotInteractive = provider, interactive
		return nil
	}
	t.Cleanup(func() { Injected.AddProvider = prev })

	var out strings.Builder
	apply := providerKeyApply(realEnv(), Opts{}, strings.NewReader("google\n"), &out, true)
	if apply == nil {
		t.Fatal("a terminal with no --yes must carry an apply")
	}
	if err := apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if gotProvider != "google" {
		t.Errorf("delegated provider = %q, want %q — the typed answer must reach models add", gotProvider, "google")
	}
	if !gotInteractive {
		t.Error("the interview must be told it has a terminal, or it cannot ask for the ref")
	}
	// Every routable provider has to be offered, or the prompt quietly narrows
	// the host's choices to whichever one setup happens to name.
	for _, want := range []string{"anthropic", "openai", "google", "ANTHROPIC_API_KEY", "GEMINI_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the prompt never offered %q:\n%s", want, out.String())
		}
	}
}

// A bare Enter takes the default rather than doing nothing, and an explicit
// refusal is a SKIP: setup reports the command and lets the second check decide
// the exit code. Neither may abort the run — a user is allowed to say not now.
func TestProvidersApply_DefaultsAndDeclines(t *testing.T) {
	noHostConfig(t)
	for _, tc := range []struct {
		name, typed, wantProvider string
		wantSkip                  bool
	}{
		{"bare Enter takes the default", "\n", secret.ProviderKeyRefOrder[0].Name, false},
		{"explicit provider", "openai\n", "openai", false},
		{"skip", "skip\n", "", true},
		{"no", "no\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			prev := Injected.AddProvider
			Injected.AddProvider = func(_ hostenv.Env, _ io.Reader, _ io.Writer, _ bool, p string) error {
				got = p
				return nil
			}
			t.Cleanup(func() { Injected.AddProvider = prev })

			err := providerKeyApply(realEnv(), Opts{}, strings.NewReader(tc.typed), io.Discard, true)(context.Background())
			var skipped ErrSkipped
			if tc.wantSkip {
				if !errors.As(err, &skipped) {
					t.Fatalf("err = %v, want an ErrSkipped", err)
				}
				if got != "" {
					t.Errorf("declined, but %q was still wired", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got != tc.wantProvider {
				t.Errorf("provider = %q, want %q", got, tc.wantProvider)
			}
		})
	}
}

// Unwired composition FAILS rather than silently going back to printing a
// command: a setup that claims to have asked and did not is the failure mode
// this package exists to prevent.
func TestProvidersApply_UnwiredCompositionFails(t *testing.T) {
	noHostConfig(t)
	prev := Injected.AddProvider
	Injected.AddProvider = nil
	t.Cleanup(func() { Injected.AddProvider = prev })

	err := providerKeyApply(realEnv(), Opts{}, strings.NewReader("anthropic\n"), io.Discard, true)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("err = %v, want an unwired-composition failure", err)
	}
	var skipped ErrSkipped
	if errors.As(err, &skipped) {
		t.Error("a wiring bug is a failure, never a skip")
	}
}

// A pack that brings its own credential must never be asked for a key — even
// though the FIRST check graded providers before that pack was adopted. Every
// probe in a round runs before any apply, so on `setup --pack <managed-inference
// pack>` the providers gap is real when it is measured and gone by the time the
// interview would run, two steps later in the same loop. Real config on disk,
// read fresh, exactly as the second check will read it.
func TestProvidersApply_KeylessInferenceIsNeverAskedForAKey(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	cfg := &config.Config{}
	cfg.Inference.Backends = map[string]config.InferenceBackend{
		"docker-anthropic": {Driver: "native", Auth: "sbx-session", Source: "/packs/work"},
	}
	cfg.Inference.Models = []config.InferenceModelBinding{{
		Model: "anthropic/claude-opus-5", Backend: "docker-anthropic",
		Upstream: "anthropic/claude-opus-5", Available: true, Source: "/packs/work",
	}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	asked := false
	prev := Injected.AddProvider
	Injected.AddProvider = func(hostenv.Env, io.Reader, io.Writer, bool, string) error {
		asked = true
		return nil
	}
	t.Cleanup(func() { Injected.AddProvider = prev })

	var out strings.Builder
	err := providerKeyApply(realEnv(), Opts{}, strings.NewReader("anthropic\n"), &out, true)(context.Background())

	if asked {
		t.Error("a host whose backends carry their own credential was asked for a provider key")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should have been prompted, got:\n%s", out.String())
	}
	var skipped ErrSkipped
	if !errors.As(err, &skipped) {
		t.Fatalf("err = %v, want an ErrSkipped", err)
	}
	if !strings.Contains(skipped.Reason, "keyless") || !strings.Contains(skipped.Reason, "docker-anthropic") {
		t.Errorf("the skip must name what made a key unnecessary, got %q", skipped.Reason)
	}
}

// The providers row must ask the key store for the names the store actually
// holds. `sbx secret ls` lists a secret under the name it was STORED as, and
// secret.setSbxSecret stores a provider key by PROVIDER name — so setup's own
// ANTHROPIC_API_KEY/OPENAI_API_KEY/GEMINI_API_KEY list matched nothing on any
// host, ever, and `providers` stayed red with all three keys wired and
// answering. Real fixture store, real listing format, no stubbed verdict.
func TestProvidersProbe_MatchesTheNameTheKeyStoreLists(t *testing.T) {
	noHostConfig(t)
	fixtureBin(t, "sbx", `printf '%s\n' \
  "SCOPE      TYPE      NAME        SECRET" \
  "(global)   service   anthropic   (stored)" \
  "(global)   service   github      (stored)" \
  "(global)   service   google      (stored)"`)
	r := providersProbe(t).Check(context.Background())
	if !r.OK() {
		t.Fatalf("a store listing anthropic + google must satisfy the providers row, got %+v", r)
	}
	// And the same probe doctor and the launch gate use, so three surfaces
	// cannot disagree about one host.
	for _, s := range setupSteps(&config.Config{}, realEnv(), Opts{}, strings.NewReader(""), io.Discard, false) {
		if s.Name != "providers" {
			continue
		}
		if want := s.Probe.(health.ProviderKeyProbe).Want; !slices.Equal(want, secret.ModelProviders) {
			t.Errorf("Want = %v, want secret.ModelProviders (%v)", want, secret.ModelProviders)
		}
	}
}

// The other half of the same bug: a store that answers with NO provider key is
// still a verified gap, so the row stays red and the fix is still named.
func TestProvidersProbe_EmptyStoreIsStillAGap(t *testing.T) {
	noHostConfig(t)
	fixtureBin(t, "sbx", `printf '%s\n' \
  "SCOPE      TYPE      NAME        SECRET" \
  "(global)   service   github      (stored)"`)
	r := providersProbe(t).Check(context.Background())
	if r.OK() {
		t.Fatalf("a store with no model key is a gap, got %+v", r)
	}
	if r.Fix == "" {
		t.Error("a verified gap must still name its repair")
	}
}
