package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/sys/systest"
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
		// A healthy host configures a GITHUB_TOKEN ref, so every sandbox pix
		// launches is given a github credential. Left unset this would read
		// the developer's own refs file.
		GitHubScope: func() (int, []string) { return health.GitHubGlobal, nil },
		// The provider verdict is THIS PIX_HOME's refs evidence now, not the
		// global sbx store: a healthy host declares filled op:// refs for all
		// three model providers.
		ProviderRefs: func() ([]string, bool) { return []string{"anthropic", "openai", "google"}, true },
		// ...and holds no host-global sbx secret for pix to ignore. Left unset
		// this would ask the real sbx.
		GlobalSecrets: func() ([]string, bool) { return nil, true },
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
	want := []string{"sbx", "providers", "github", "sbx-globals", "ollama"}
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
		// The refs file exists and could not be read: UNKNOWN, never a
		// no-key verdict.
		ProviderRefs: func() ([]string, bool) { return nil, false },
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
		// The refs file ANSWERED and declares no model provider ref. That,
		// and only that, is a no-key verdict — a host-global sbx secret
		// never earns one either way.
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"},
		ProviderRefs: func() ([]string, bool) { return nil, true },
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
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"},
		ProviderRefs: func() ([]string, bool) { return nil, true }}

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

// TestUnreadableConfigSnapshot_IsABlockingRequiredGap pins the surviving
// caller: `pix doctor` (cmd/pix/doctor_cmd.go) renders this snapshot when
// workspace config will not load. Exit 1 is correct here (unlike the deleted
// `pix status`, doctor is allowed to fail a shell on a verified gap), and the
// evidence must show the parse error — there is no Fix command because
// `pix config path` is not a real verb.
func TestUnreadableConfigSnapshot_IsABlockingRequiredGap(t *testing.T) {
	s := UnreadableConfigSnapshot(errors.New("boom"))
	if s.ExitCode() != health.ExitNotReady {
		t.Errorf("exit = %d, want %d", s.ExitCode(), health.ExitNotReady)
	}
	if len(s.Blocking()) != 1 {
		t.Errorf("want exactly one blocking result, got %v", s.Blocking())
	}
	if fixes := s.Fixes(); len(fixes) != 0 {
		t.Errorf("want no fix command (pix config path is not a real verb), got %v", fixes)
	}
	var out strings.Builder
	health.RenderDoctorWith(&out, s, health.DoctorOpts{})
	if !strings.Contains(out.String(), "boom") {
		t.Errorf("doctor must show the load error as evidence:\n%s", out.String())
	}
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

// ollamaTagsEnv builds an Options.Env whose OLLAMA_HOST resolves to a fake
// /api/tags server, so the ollama row in Probes() exercises the real
// inference.DetectOllama path end to end rather than the zero-value
// "not installed" default healthyHost otherwise gets.
func ollamaTagsEnv(t *testing.T, hostOverride string, body string) hostenv.Env {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host := hostOverride
	if host == "" {
		host = u.Host
	}
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		GetenvFn:   func(name string) string { return host },
	}
	return hostenv.Env{System: fake}
}

func TestDoctor_OllamaRow_LocalEndpointEmbedModelPresent(t *testing.T) {
	cfg, o := healthyHost(t)
	cfg.MemoryEmbedModel = "nomic-embed-text"
	o.Env = ollamaTagsEnv(t, "", `{"models":[{"name":"nomic-embed-text","size":274000000}]}`)
	s := run(t, cfg, o)
	r := result(t, s, "ollama")
	if !r.OK() {
		t.Errorf("ollama = %s (%s), want ready: a local endpoint listing the configured embed model", r.Effective(), r.Detail)
	}
	if r.Fix != "" {
		t.Errorf("Fix = %q, want none on a ready row", r.Fix)
	}
}

func TestDoctor_OllamaRow_LocalEndpointMissingEmbedModelOffersPull(t *testing.T) {
	cfg, o := healthyHost(t)
	cfg.MemoryEmbedModel = "nomic-embed-text"
	o.Env = ollamaTagsEnv(t, "", `{"models":[{"name":"qwen3.5:9b","size":6600000000}]}`)
	s := run(t, cfg, o)
	r := result(t, s, "ollama")
	if r.Effective() != health.StatusAbsent {
		t.Fatalf("ollama = %s, want absent (embed model not pulled)", r.Effective())
	}
	if r.Fix != "ollama pull nomic-embed-text" {
		t.Errorf("Fix = %q, want the local pull command", r.Fix)
	}
	// Required() is false, so a missing embed model must never fail doctor's
	// own exit code — Ollama is an optional capability.
	if s.ExitCode() != health.ExitOK {
		t.Errorf("exit = %d, want %d: an optional row must never block", s.ExitCode(), health.ExitOK)
	}
}

// TestDoctor_OllamaRow_RemoteEndpointModeDetected proves the row's MODE
// classification is wired end to end off the real OLLAMA_HOST string — the
// Fix-suppression behavior itself (a reachable remote host with a missing
// model gets no pull command) is unit-tested directly against OllamaProbe in
// health/ollama_test.go, where the endpoint can be injected without a real
// network hop.
func TestDoctor_OllamaRow_RemoteEndpointModeDetected(t *testing.T) {
	cfg, o := healthyHost(t)
	cfg.MemoryEmbedModel = "nomic-embed-text"
	// team-ollama.internal never actually answers in this test, so the row
	// reports absent (unreachable) — but it must still report absent, never
	// a false ready, and its Fix must never claim this host can start a
	// daemon on someone else's machine.
	o.Env = ollamaTagsEnv(t, "team-ollama.internal:11434", `{"models":[]}`)
	s := run(t, cfg, o)
	r := result(t, s, "ollama")
	if r.OK() {
		t.Fatalf("ollama = %s, want NOT ready against an address nothing answers", r.Effective())
	}
	if strings.Contains(r.Fix, "start Ollama") {
		t.Errorf("Fix = %q, must not tell the user to start a daemon they do not own", r.Fix)
	}
}

func TestDoctor_OllamaRow_NotInstalledIsOptionalNotAGap(t *testing.T) {
	cfg, o := healthyHost(t)
	s := run(t, cfg, o)
	r := result(t, s, "ollama")
	if r.Effective() != health.StatusOff {
		t.Errorf("ollama = %s, want off (no host env wired — zero-value \"not installed\")", r.Effective())
	}
	if r.Required {
		t.Error("ollama row must never be required")
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
