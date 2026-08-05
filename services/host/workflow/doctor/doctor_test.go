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
		out := filepath.Join(dir, "fixture")
		cmd := exec.Command("go", "build", "-o", out, "../../health/testdata/fixture")
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
		MonitorPort:   deadPort(t),
		UID:           501,
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
	want := []string{"sbx", "pack", "providers", "memory", "monitor", "launchd", "mcp"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("probe set = %v, want %v", names, want)
	}
}

// A capability the host did not enable is OPTIONAL, not absent from the
// report: a line a reader cannot see is a fact they cannot act on.
func TestProbes_RequirementFollowsTheConfiguredServices(t *testing.T) {
	off := Probes(&config.Config{}, Options{})
	on := Probes(&config.Config{Services: []string{"memory", "monitor"}}, Options{})
	for i, p := range off {
		if p.Name() == "memory" || p.Name() == "monitor" {
			if p.Required() {
				t.Errorf("%s is required on a host that did not enable it", p.Name())
			}
			if !on[i].Required() {
				t.Errorf("%s is optional on a host that DID enable it", p.Name())
			}
		}
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
		t.Errorf("exit = %d, want %d (monitor is off, which is absence, not failure)", s.ExitCode(), health.ExitOK)
	}
	if fixes := s.Fixes(); len(fixes) != 1 || fixes[0] != health.MonitorStartFix {
		t.Errorf("fixes = %v, want only the optional monitor one", fixes)
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
		MemoryPort: deadPort(t), MonitorPort: deadPort(t), UID: 501,
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
		MemoryPort: deadPort(t), MonitorPort: deadPort(t), UID: 501,
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

// The memory line reports the SUPERVISOR's unit, not "something holds the
// port": a surviving daemon from an older install answers a dial perfectly.
func TestDoctor_MemoryReportsTheUnitNotThePort(t *testing.T) {
	bin := buildFixture(t)
	cfg := &config.Config{Services: []string{"memory"}}
	base := Options{Budget: 5 * time.Second, SbxBin: bin, SbxArgs: []string{"healthy"},
		KeyStoreBin: bin, KeyStoreArgs: []string{"keys"}, LaunchctlBin: bin, LaunchctlArgs: []string{"healthy"},
		MonitorPort: deadPort(t), UID: 501}

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
		MemoryPort: deadPort(t), MonitorPort: deadPort(t), UID: 501}

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
		MemoryPort: deadPort(t), MonitorPort: deadPort(t), UID: 501}

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
