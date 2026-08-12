package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/inference"
	"pix/host/rpc"
)

// These tests drive the REAL surfaces. Every exec probe runs a compiled
// fixture executable, every service probe dials a real listener, and every
// file probe reads a real directory. Nothing here stubs a probe: the failure
// modes are produced by a process, a socket and a filesystem, so the
// classification under test is the one a user's host runs.
//
// The fixture is health's, reached by relative path rather than copied: two
// fixtures drifting apart is how two surfaces start disagreeing about what
// "broken" looks like. Its source lives at
// ../../health/testdata/fixture/main.go.txt — a .txt, not a .go file, on
// purpose: it is a real, complete Go program, but naming it main.go would
// make it a second copy of production Go source outside any *_test.go file,
// indistinguishable from shipped code to a LOC/production-metrics scanner
// (anything that globs *.go and excludes *_test.go, same as `go build ./...`
// itself). Reading it as data and writing it into a temp dir as main.go right
// before the compile keeps this a REAL compiled executable — same exec, same
// classification under test — while its source counts as test fixture data,
// not production Go.

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

// memoryUnit serves the identity JSON-RPC on a real listener and returns its
// port, so the memory probe makes a real call for a real answer.
func memoryUnit(t *testing.T, name string, ready bool) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"name": name, "port": 0, "ready": ready},
		})
	}))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	return n
}

// deadPort returns a port nothing is listening on, so a probe gets a real
// connection refused rather than a faked one.
func deadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func packDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("name = \"fixture\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// healthyHost is a host where every probe can prove something good.
func healthyHost(t *testing.T) (*config.Config, Options) {
	t.Helper()
	bin := buildFixture(t)
	cfg := &config.Config{Services: []string{"memory"}, Pack: packDir(t)}
	return cfg, Options{
		Budget:        5 * time.Second,
		SbxBin:        bin,
		SbxArgs:       []string{"healthy"},
		KeyStoreBin:   bin,
		KeyStoreArgs:  []string{"keys"},
		LaunchctlBin:  bin,
		LaunchctlArgs: []string{"healthy"},
		MemoryPort:    memoryUnit(t, rpc.MemoryName, true),
		UID:           501,
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
	want := []string{"sbx", "pack", "providers", "memory", "launchd", "mcp", "daemons", "github"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("probe set = %v, want %v", names, want)
	}
}

// A capability the host did not enable is OPTIONAL, not absent from the
// report: a line a reader cannot see is a fact they cannot act on.
//
// "Enabled" is serve's OWN resolution, and the load-bearing half of it is that
// an EMPTY `services` means ALL, not none (resolveServices: "an empty result
// means all"). Reading empty as none is how doctor came to call memory OPTIONAL
// on a config where serve starts it — so a memory unit that was genuinely down
// could not fail readiness on the commonest config there is.
func TestProbes_RequirementFollowsTheConfiguredServices(t *testing.T) {
	required := func(cfg *config.Config, name string) bool {
		t.Helper()
		for _, p := range Probes(cfg, Options{}) {
			if p.Name() == name {
				return p.Required()
			}
		}
		t.Fatalf("no %q probe in the set", name)
		return false
	}
	if !required(&config.Config{Services: []string{"memory"}}, "memory") {
		t.Error("memory is optional on a host that explicitly enabled it")
	}
	// The regression: no `services` key at all is every service, because that is
	// what `pix-host serve` does with it.
	if !required(&config.Config{}, "memory") {
		t.Error("memory is optional on a host with no `services` key, but serve starts it there")
	}
	if !required(&config.Config{Services: []string{"  "}}, "memory") {
		t.Error("a whitespace-only services entry must resolve like empty (all), not like a named set")
	}
	// And a config that names a DIFFERENT service is the one case where memory
	// is genuinely not enabled.
	if required(&config.Config{Services: []string{"something-else"}}, "memory") {
		t.Error("memory is required on a host whose services name something else")
	}
}

func TestDoctor_HealthyHostIsReadyAndPrintsNoFixes(t *testing.T) {
	cfg, o := healthyHost(t)
	s := run(t, cfg, o)
	for _, name := range []string{"sbx", "pack", "providers", "memory", "launchd"} {
		if r := result(t, s, name); !r.OK() {
			t.Errorf("%s = %s (%s / %s), want ready", name, r.Effective(), r.Detail, r.Evidence)
		}
	}
	if s.ExitCode() != health.ExitOK {
		t.Errorf("exit = %d, want %d", s.ExitCode(), health.ExitOK)
	}
	// With the monitor retired, a host where every probe proved something good
	// has NOTHING to repair: the one fix this report used to always carry was
	// the optional monitor's, on a host that had not enabled it.
	if fixes := s.Fixes(); len(fixes) != 0 {
		t.Errorf("fixes = %v, want none on a fully healthy host", fixes)
	}
}

// The load-bearing property of the whole model: a host nobody could inspect
// is not a broken host. Inside the sandbox neither sbx nor launchctl exists,
// and doctor exiting 1 there is how a team learns to ignore its exit code.
func TestDoctor_UnknownAloneIsNotAFailure(t *testing.T) {
	bin := buildFixture(t)
	cfg := &config.Config{Pack: packDir(t)}
	o := Options{
		Budget: 5 * time.Second,
		// Every exec probe answers in a way nothing can be concluded from.
		SbxBin: bin, SbxArgs: []string{"broken"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"crash"},
		LaunchctlBin: bin, LaunchctlArgs: []string{"malformed"},
		// A LIVE memory unit, deliberately: this test is about unknowns, and a
		// refused port is not one. It is a verified gap, and on a host that runs
		// memory (no `services` key = all) failing over it is correct — so a dead
		// port here would prove something other than what the name claims.
		MemoryPort: memoryUnit(t, rpc.MemoryName, true), UID: 501,
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
	cfg := &config.Config{Services: []string{"memory"}}
	o := Options{
		Budget: 5 * time.Second,
		SbxBin: missing,
		// The key store ANSWERED and listed no model key. That, and only
		// that, is a no-key verdict.
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"},
		LaunchctlBin: bin, LaunchctlArgs: []string{"notloaded"},
		MemoryPort: deadPort(t), UID: 501,
	}
	s := run(t, cfg, o)
	want := map[string]string{
		"sbx":       health.SbxInstallFix,
		"providers": health.ModelKeyFix,
		"memory":    health.ServeStartFix,
		"launchd":   health.ServeInstallFix,
		"pack":      health.PackUseFix,
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

// keylessPackConfig is the shape a pack with its own gateway backends writes:
// every model is reached over a Docker login session the sbx proxy injects
// inside the sandbox, so no anthropic/openai/google key is ever read.
func keylessPackConfig(packRoot string) *config.Config {
	backend := func(name string) config.InferenceBackend {
		return config.InferenceBackend{Driver: "openai-compatible", BaseURL: "https://gw.example.com/" + name,
			Auth: "sbx-session", KeyEnv: "DOCKER_TOKEN", CredentialService: "sbx-login", Source: packRoot}
	}
	return &config.Config{
		Services: []string{"memory"}, Pack: packRoot,
		Inference: config.InferenceConfig{
			ExclusiveSource: packRoot,
			Backends:        map[string]config.InferenceBackend{"gw-anthropic": backend("anthropic"), "gw-openai": backend("openai")},
			Models: []config.InferenceModelBinding{
				{Model: "anthropic/claude-opus-5", Backend: "gw-anthropic", Upstream: "claude-opus-5", Available: true, Source: packRoot},
				{Model: "openai/gpt-5.6-sol", Backend: "gw-openai", Upstream: "gpt-5.6-sol", Available: true, Source: packRoot},
			},
		},
	}
}

// A host whose models need no provider key must not be told it has none. This
// is the exact host a private inference pack produces: `pix run` launches it
// (its gate skips the key probe on keyless inference), and doctor used to call
// the same host broken and prescribe `pix models add anthropic` — a key that,
// once added, nothing would read.
func TestDoctor_KeylessInferenceIsNotAMissingKey(t *testing.T) {
	bin := buildFixture(t)
	pack := packDir(t)
	cfg := keylessPackConfig(pack)
	// The key store ANSWERS, and lists no model key: the strongest possible
	// no-key evidence. It still must not decide this axis.
	o := Options{Budget: 5 * time.Second, SbxBin: bin, SbxArgs: []string{"healthy"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"}, LaunchctlBin: bin, LaunchctlArgs: []string{"healthy"},
		MemoryPort: memoryUnit(t, rpc.MemoryName, true), UID: 501}

	s := run(t, cfg, o)
	r := result(t, s, "providers")
	if !r.OK() {
		t.Errorf("providers = %s (%s / %s), want ready on a keyless host", r.Effective(), r.Detail, r.Evidence)
	}
	if r.Fix != "" {
		t.Errorf("providers fix = %q, want none: there is no key to add", r.Fix)
	}
	// The evidence must NAME what answers instead, or the green is unfalsifiable.
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

// The evidence doctor renders and the predicate `pix run`'s gate decides on
// are one question, so they must answer together on every config shape —
// including the ones where a provider key IS this host's credential. A
// KeylessBackends non-empty where the gate is false would be doctor greening
// an axis the same host would be refused a launch over.
func TestKeylessInference_AgreesWithTheLaunchGate(t *testing.T) {
	pack := packDir(t)
	keyed := keylessPackConfig(pack)
	keyed.Inference.Backends["gw-anthropic"] = config.InferenceBackend{Driver: "openai-compatible",
		BaseURL: "https://api.anthropic.com", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY", Source: pack}

	cases := map[string]*config.Config{
		"keyless pack":      keylessPackConfig(pack),
		"1password backend": keyed,
		"no inference":      {Services: []string{"memory"}},
		"nil":               nil,
	}
	for name, cfg := range cases {
		gate := inference.Configured(cfg) && !inference.InferenceNeedsOnePassword(cfg)
		if got := inference.KeylessBackends(cfg) != ""; got != gate {
			t.Errorf("%s: keyless evidence = %v, launch gate = %v", name, got, gate)
		}
	}
}

// A binding whose backend the config never defines resolves to nothing. That
// is not a keyless host, and must fall through to the key store rather than
// green-light the axis on an empty set.
func TestKeylessInference_EmptyBackendSetIsNotKeyless(t *testing.T) {
	pack := packDir(t)
	cfg := keylessPackConfig(pack)
	cfg.Inference.Backends = map[string]config.InferenceBackend{}
	if got := inference.KeylessBackends(cfg); got != "" {
		t.Errorf("KeylessBackends = %q, want empty when no backend backs a binding", got)
	}
}

// The memory line reports the SUPERVISOR's unit, not "something holds the
// port": a surviving daemon from an older install answers a dial perfectly.
func TestDoctor_MemoryReportsTheUnitNotThePort(t *testing.T) {
	bin := buildFixture(t)
	cfg := &config.Config{Services: []string{"memory"}}
	base := Options{Budget: 5 * time.Second, SbxBin: bin, SbxArgs: []string{"healthy"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"keys"}, LaunchctlBin: bin, LaunchctlArgs: []string{"healthy"},
		UID: 501}

	imposter := base
	imposter.MemoryPort = memoryUnit(t, "something-else", true)
	r := result(t, run(t, cfg, imposter), "memory")
	if !r.Missing() || r.Fix != health.ServeRestartFix {
		t.Errorf("a port held by another service = %s / %q, want a gap with %q", r.Effective(), r.Fix, health.ServeRestartFix)
	}

	notReady := base
	notReady.MemoryPort = memoryUnit(t, rpc.MemoryName, false)
	if r := result(t, run(t, cfg, notReady), "memory"); !r.Missing() {
		t.Errorf("a unit answering ready=false = %s, want a gap", r.Effective())
	}
}

func TestStatus_AlwaysExitsZeroAndSendsYouToDoctor(t *testing.T) {
	bin := buildFixture(t)
	cfg := &config.Config{Services: []string{"memory"}}
	o := Options{Budget: 5 * time.Second, SbxBin: filepath.Join(t.TempDir(), "gone"),
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"}, LaunchctlBin: bin, LaunchctlArgs: []string{"notloaded"},
		MemoryPort: deadPort(t), UID: 501}

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
	for _, fix := range []string{health.SbxInstallFix, health.ModelKeyFix, health.ServeStartFix} {
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
	cfg := &config.Config{Services: []string{"memory"}, Pack: packDir(t)}
	o := Options{Budget: 5 * time.Second, SbxBin: bin, SbxArgs: []string{"healthy"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"nokeys"}, LaunchctlBin: bin, LaunchctlArgs: []string{"healthy"},
		MemoryPort: deadPort(t), UID: 501}

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

// TestDoctor_SbxAloneIsARequiredGap: DX finding 7 — docs/reference.md used to
// say exit 1 happens "only when a core requirement (a model provider key, or
// the config file itself)" verified a gap, omitting sbx — but SbxProbe.
// Required() is unconditionally true (health/probes.go), so a missing sbx
// alone, with every other check healthy, is ALSO a required, exit-1-causing
// gap. This pins that contract so the doc's list can be checked against it.
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
