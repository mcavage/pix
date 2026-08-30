package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/health"
)

// These tests drive the REAL surfaces. Every exec probe runs a compiled
// fixture executable, so the classification under test is the one a user's
// host runs. The v1 probe set this file used to exercise — pack, memory (via
// the custom memory JSON-RPC identity call), launchd, MCP, daemons — was
// deleted along with packs, the Suture-supervised `pix-host serve`, and Pix's
// own MCP administration in the Pix v2 cutover
// (docs/design/pix-v2-architecture.md §14, AC-16). What survives here is
// sbx, provider keys (including the keyless-inference carve-out) and the
// GitHub secret's scope — the whole of what `Probes` still builds.
//
// The fixture is health's, reached by relative path rather than copied: two
// fixtures drifting apart is how two surfaces start disagreeing about what
// "broken" looks like.

var (
	fixtureOnce sync.Once
	fixtureBin  string
	fixtureErr  error
)

func buildFixture(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "doctor-fixture")
		if err != nil {
			fixtureErr = err
			return
		}
		src, err := os.ReadFile("../../health/testdata/fixture/main.go.txt")
		if err != nil {
			fixtureErr = err
			return
		}
		mainGo := filepath.Join(dir, "main.go")
		if err := os.WriteFile(mainGo, src, 0o644); err != nil {
			fixtureErr = err
			return
		}
		out := filepath.Join(dir, "fixture")
		cmd := exec.Command("go", "build", "-o", out, mainGo)
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			fixtureErr = errors.New(string(b))
			return
		}
		fixtureBin = out
	})
	if fixtureErr != nil {
		t.Fatalf("build fixture: %v", fixtureErr)
	}
	return fixtureBin
}

// healthyHost is a host where every probe can prove something good.
func healthyHost(t *testing.T) (*config.Config, Options) {
	t.Helper()
	bin := buildFixture(t)
	cfg := &config.Config{}
	return cfg, Options{
		Budget:       5 * time.Second,
		SbxBin:       bin,
		SbxArgs:      []string{"healthy"},
		KeyStoreBin:  bin,
		KeyStoreArgs: []string{"keys"},
		// A healthy host has a GLOBAL github secret. Left unset this would ask the
		// real sbx and report whatever the developer's machine holds.
		GitHubScope: func() (int, []string) { return health.GitHubGlobal, nil },
	}
}

func run(t *testing.T, cfg *config.Config, o Options) health.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Check(ctx, cfg, o)
}

func result(t *testing.T, s health.Snapshot, name string) health.Result {
	t.Helper()
	r, ok := s.Find(name)
	if !ok {
		t.Fatalf("no %q check in %v", name, s.Names())
	}
	return r
}

func TestProbes_CoverTheWholeHostSurface(t *testing.T) {
	names := []string{}
	for _, p := range Probes(&config.Config{}, Options{}) {
		names = append(names, p.Name())
	}
	want := []string{"sbx", "providers", "github"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("probe set = %v, want %v", names, want)
	}
}

func TestDoctor_HealthyHostIsReadyAndPrintsNoFixes(t *testing.T) {
	cfg, o := healthyHost(t)
	s := run(t, cfg, o)
	for _, name := range []string{"sbx", "providers"} {
		if r := result(t, s, name); !r.OK() {
			t.Errorf("%s = %s (%s / %s), want ready", name, r.Effective(), r.Detail, r.Evidence)
		}
	}
	if s.ExitCode() != health.ExitOK {
		t.Errorf("exit = %d, want %d", s.ExitCode(), health.ExitOK)
	}
	if fixes := s.Fixes(); len(fixes) != 0 {
		t.Errorf("fixes = %v, want none on a fully healthy host", fixes)
	}
}

// The load-bearing property of the whole model: a host nobody could inspect
// is not a broken host. Inside the sandbox sbx does not exist, and doctor
// exiting 1 there is how a team learns to ignore its exit code.
func TestDoctor_UnknownAloneIsNotAFailure(t *testing.T) {
	bin := buildFixture(t)
	cfg := &config.Config{}
	o := Options{
		Budget: 5 * time.Second,
		// Every exec probe answers in a way nothing can be concluded from.
		SbxBin:      bin,
		SbxArgs:     []string{"broken"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"crash"},
	}
	s := run(t, cfg, o)
	for _, name := range []string{"sbx", "providers"} {
		if got := result(t, s, name).Effective(); got != health.StatusUnknown {
			t.Errorf("%s = %s, want unknown", name, got)
		}
	}
	if s.ExitCode() != health.ExitOK {
		t.Fatalf("exit = %d over unknowns alone; only a VERIFIED required gap may fail", s.ExitCode())
	}
	var b strings.Builder
	if code := RunDoctor(context.Background(), cfg, "default", &b, o, false, false); code != health.ExitOK {
		t.Fatalf("RunDoctor exit = %d, want 0", code)
	}
	out := b.String()
	if !strings.Contains(out, "?") || !strings.Contains(out, "probe failed") {
		t.Errorf("doctor must say WHY a check is unknown:\n%s", out)
	}
	for _, fix := range []string{health.SbxInstallFix, health.ModelKeyFix} {
		if strings.Contains(out, fix) {
			t.Errorf("doctor invented the repair %q for something it could not verify:\n%s", fix, out)
		}
	}
}

func TestDoctor_VerifiedGapsFailWithTheExactFix(t *testing.T) {
	bin := buildFixture(t)
	missing := filepath.Join(t.TempDir(), "no-sbx-here")
	cfg := &config.Config{}
	o := Options{
		Budget: 5 * time.Second,
		SbxBin: missing,
		// The key store ANSWERED and listed no model key. That, and only
		// that, is a no-key verdict.
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"},
	}
	s := run(t, cfg, o)
	want := map[string]string{
		"sbx":       health.SbxInstallFix,
		"providers": health.ModelKeyFix,
	}
	for name, fix := range want {
		r := result(t, s, name)
		if !r.Missing() {
			t.Errorf("%s = %s, want a verified gap", name, r.Effective())
		}
		if r.Fix != fix {
			t.Errorf("%s fix = %q, want %q", name, r.Fix, fix)
		}
	}
	if s.ExitCode() != health.ExitNotReady {
		t.Errorf("exit = %d, want %d", s.ExitCode(), health.ExitNotReady)
	}
	var b strings.Builder
	if code := RunDoctor(context.Background(), cfg, "default", &b, o, false, false); code != health.ExitNotReady {
		t.Fatalf("RunDoctor exit = %d, want %d", code, health.ExitNotReady)
	}
	for _, fix := range want {
		if !strings.Contains(b.String(), fix) {
			t.Errorf("doctor omitted the exact fix %q:\n%s", fix, b.String())
		}
	}
}

// keylessGatewayConfig is the shape a keyless inference backend writes: every
// model is reached over a Docker login session the sbx proxy injects inside
// the sandbox, so no anthropic/openai/google key is ever read.
func keylessGatewayConfig() *config.Config {
	backend := func(name string) config.InferenceBackend {
		return config.InferenceBackend{Driver: "openai-compatible", BaseURL: "https://gw.example.com/" + name,
			Auth: "sbx-session", KeyEnv: "DOCKER_TOKEN", CredentialService: "sbx-login"}
	}
	return &config.Config{
		Inference: config.InferenceConfig{
			Backends: map[string]config.InferenceBackend{"gw-anthropic": backend("anthropic"), "gw-openai": backend("openai")},
			Models: []config.InferenceModelBinding{
				{Model: "anthropic/claude-opus-5", Backend: "gw-anthropic", Upstream: "claude-opus-5", Available: true},
				{Model: "openai/gpt-5.6-sol", Backend: "gw-openai", Upstream: "gpt-5.6-sol", Available: true},
			},
		},
	}
}

// A host whose models need no provider key must not be told it has none.
func TestDoctor_KeylessInferenceIsNotAMissingKey(t *testing.T) {
	bin := buildFixture(t)
	cfg := keylessGatewayConfig()
	// The key store ANSWERS, and lists no model key: the strongest possible
	// no-key evidence. It still must not decide this axis.
	o := Options{Budget: 5 * time.Second, SbxBin: bin, SbxArgs: []string{"healthy"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"}}

	s := run(t, cfg, o)
	r := result(t, s, "providers")
	if !r.OK() {
		t.Errorf("providers = %s (%s / %s), want ready on a keyless host", r.Effective(), r.Detail, r.Evidence)
	}
	if r.Fix != "" {
		t.Errorf("providers fix = %q, want none: there is no key to add", r.Fix)
	}
	for _, want := range []string{"gw-anthropic", "gw-openai", "sbx-session"} {
		if !strings.Contains(r.Evidence, want) {
			t.Errorf("evidence %q omits %q", r.Evidence, want)
		}
	}
	if s.ExitCode() != health.ExitOK {
		t.Errorf("exit = %d, want %d: nothing required is missing", s.ExitCode(), health.ExitOK)
	}
	var b strings.Builder
	if code := RunDoctor(context.Background(), cfg, "default", &b, o, false, false); code != health.ExitOK {
		t.Fatalf("RunDoctor exit = %d, want %d:\n%s", code, health.ExitOK, b.String())
	}
	if strings.Contains(b.String(), health.ModelKeyFix) {
		t.Errorf("doctor prescribed %q on a host that reads no provider key:\n%s", health.ModelKeyFix, b.String())
	}
}

func TestStatus_AlwaysExitsZeroAndSendsYouToDoctor(t *testing.T) {
	bin := buildFixture(t)
	cfg := &config.Config{}
	o := Options{Budget: 5 * time.Second, SbxBin: filepath.Join(t.TempDir(), "gone"),
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"}}

	var b strings.Builder
	code := RenderStatus(context.Background(), cfg, "default", &b, o, false)
	if code != health.ExitOK {
		t.Fatalf("status exit = %d; the landing screen must never fail a shell", code)
	}
	out := b.String()
	if !strings.Contains(out, health.DoctorCommand) {
		t.Errorf("status must point at doctor:\n%s", out)
	}
	if !strings.Contains(out, "issues") {
		t.Errorf("status must count what is outstanding:\n%s", out)
	}
	for _, fix := range []string{health.SbxInstallFix, health.ModelKeyFix} {
		if strings.Contains(out, fix) {
			t.Errorf("status printed the repair %q; that is doctor's job:\n%s", fix, out)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n > 7 {
		t.Errorf("status is the glance, got %d lines:\n%s", n, out)
	}
}

// status and doctor may differ in DEPTH, never in FACTS. They ask the same
// probes, so a gap one names is a gap the other names.
func TestStatusAndDoctorTellTheSameStory(t *testing.T) {
	bin := buildFixture(t)
	cfg := &config.Config{}
	o := Options{Budget: 5 * time.Second, SbxBin: bin, SbxArgs: []string{"healthy"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"}}

	var statusOut, doctorOut strings.Builder
	RenderStatus(context.Background(), cfg, "p", &statusOut, o, true)
	RunDoctor(context.Background(), cfg, "p", &doctorOut, o, true, false)

	var st, dr ReportJSONView
	if err := json.Unmarshal([]byte(statusOut.String()), &st); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	if err := json.Unmarshal([]byte(doctorOut.String()), &dr); err != nil {
		t.Fatalf("doctor --json: %v", err)
	}
	if st.Exit != dr.Exit || st.Exit != health.ExitNotReady {
		t.Errorf("status exit field %d, doctor %d; both must publish the verdict %d", st.Exit, dr.Exit, health.ExitNotReady)
	}
	if st.Verdict != dr.Verdict || st.Ready != dr.Ready {
		t.Errorf("verdict/ready disagree: %+v vs %+v", st, dr)
	}
	if fmt.Sprint(names(st)) != fmt.Sprint(names(dr)) {
		t.Errorf("check sets disagree: %v vs %v", names(st), names(dr))
	}
	for _, c := range st.Checks {
		if c.Status != "absent" && c.Status != "denied" && c.Fix != "" {
			t.Errorf("%s is %s and still carries the fix %q", c.Name, c.Status, c.Fix)
		}
	}
}

func names(v ReportJSONView) []string {
	var out []string
	for _, c := range v.Checks {
		out = append(out, c.Name+"="+c.Status)
	}
	return out
}

func TestReportJSON_SchemaAndFixes(t *testing.T) {
	cfg, o := healthyHost(t)
	var b strings.Builder
	RunDoctor(context.Background(), cfg, "work", &b, o, true, false)
	var v ReportJSONView
	if err := json.Unmarshal([]byte(b.String()), &v); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, b.String())
	}
	if v.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", v.SchemaVersion, SchemaVersion)
	}
	if v.Profile != "work" || v.ConfigPath == "" {
		t.Errorf("profile/config_path missing: %+v", v)
	}
	if !v.Ready || v.Verdict != "ready" || v.Exit != health.ExitOK {
		t.Errorf("a healthy host reported %q ready=%v exit=%d", v.Verdict, v.Ready, v.Exit)
	}
	if len(v.Checks) != len(Probes(cfg, o)) {
		t.Errorf("checks = %d, want one per probe (%d)", len(v.Checks), len(Probes(cfg, o)))
	}
}

func TestDoctor_VerboseAddsEvidenceForReadyChecks(t *testing.T) {
	cfg, o := healthyHost(t)
	var concise, verbose strings.Builder
	RunDoctor(context.Background(), cfg, "", &concise, o, false, false)
	RunDoctor(context.Background(), cfg, "", &verbose, o, false, true)
	if len(verbose.String()) <= len(concise.String()) {
		t.Errorf("--verbose added nothing:\n%s", verbose.String())
	}
	if strings.Contains(concise.String(), "sbx --version = ") {
		t.Errorf("the concise report proves what is already green:\n%s", concise.String())
	}
	if !strings.Contains(verbose.String(), "sbx --version = ") {
		t.Errorf("--verbose must show the evidence:\n%s", verbose.String())
	}
}

// TestDoctor_SbxAloneIsARequiredGap: SbxProbe.Required() is unconditionally
// true (health/probes.go), so a missing sbx alone, with every other check
// healthy, is ALSO a required, exit-1-causing gap.
func TestDoctor_SbxAloneIsARequiredGap(t *testing.T) {
	cfg, o := healthyHost(t)
	o.SbxBin = filepath.Join(t.TempDir(), "no-sbx-here")
	s := run(t, cfg, o)
	r := result(t, s, "sbx")
	if !r.Blocking() {
		t.Fatalf("sbx = %+v, want a blocking (required+missing) gap", r)
	}
	if s.ExitCode() != health.ExitNotReady {
		t.Errorf("exit = %d, want %d: sbx alone must be enough to fail doctor", s.ExitCode(), health.ExitNotReady)
	}
}
