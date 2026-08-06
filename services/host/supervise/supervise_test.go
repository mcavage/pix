// supervise_test.go — the supervision tree is proven against a REAL go-plugin
// fixture binary (testdata/fixture), not a mock: every test execs a process,
// completes the actual handshake and makes real net/rpc calls. The failure
// modes (crash, unhealthy, stubborn stop, swapped binary, dead reattach target,
// wedged start) are produced by the fixture itself.
package supervise

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/thejerf/suture/v4"

	"pix/host/plugin"
)

var (
	fixtureOnce sync.Once
	fixtureBin  string
	fixtureErr  error
)

// packageDir returns the directory containing this test file, resolved via
// runtime.Caller rather than the process's actual working directory. `go
// test` sets the process CWD to the package directory, which is why a bare
// os.ReadFile("testdata/...") and an unset cmd.Dir on the `go build` below
// have always worked — but that is an implicit invariant of the test
// runner, not something compileFixture asserts. Pinning it explicitly means
// module resolution for the fixture's "pix/host/plugin" import (and the
// cached github.com/hashicorp/go-plugin dependency) survives even if a
// caller changes the working directory (t.Chdir, a future TestMain, or this
// helper getting called from somewhere else). See TestFixtureBuildSurvivesCWDChange.
func packageDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("supervise: runtime.Caller failed to resolve package directory")
	}
	return filepath.Dir(file)
}

// compileFixture reads testdata/fixture/main.go.txt (or any srcRelPath, path
// relative to this package's own directory), writes it into a fresh temp dir
// as main.go, and compiles it there. Building it via cmd.Dir = packageDir(),
// rather than an inherited process CWD, is what makes module resolution for
// "pix/host/plugin" and the go-plugin dependency CWD-independent.
func compileFixture(srcRelPath string) (string, error) {
	pkgDir := packageDir()
	dir, err := os.MkdirTemp("", "supervise-fixture")
	if err != nil {
		return "", err
	}
	src, err := os.ReadFile(filepath.Join(pkgDir, srcRelPath))
	if err != nil {
		return "", err
	}
	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo, src, 0o644); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "fixture")
	cmd := exec.Command("go", "build", "-o", out, mainGo)
	cmd.Dir = pkgDir // pin module resolution explicitly; do not rely on inherited CWD
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", errors.New(string(b))
	}
	return out, nil
}

// buildFixture compiles testdata/fixture once per test binary. The fixture's
// source lives at testdata/fixture/main.go.txt — a .txt, not a .go file, on
// purpose: it is a real, complete go-plugin program, but naming it main.go
// would make it a second copy of production Go source sitting outside any
// *_test.go file, indistinguishable from shipped code to a LOC/production-
// metrics scanner (anything that globs *.go and excludes *_test.go, same as
// `go build ./...` itself). Reading it as data and writing it into a temp dir
// as main.go right before the compile keeps this a REAL compiled executable
// — same handshake, same net/rpc calls, same classification under test —
// while its source counts as test fixture data, not production Go.
func buildFixture(t *testing.T) (string, string) {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureBin, fixtureErr = compileFixture("testdata/fixture/main.go.txt")
	})
	if fixtureErr != nil {
		t.Fatalf("build fixture plugin: %v", fixtureErr)
	}
	sha, err := FileSHA256(fixtureBin)
	must(t, err)
	return fixtureBin, sha
}

// TestFixtureBuildSurvivesCWDChange is the regression test for the concern
// that compiling the fixture from an absolute temp-dir path could lose
// module resolution for its "pix/host/plugin" import and its cached
// github.com/hashicorp/go-plugin dependency. It moves the process's working
// directory away from the package directory (something buildFixture's old,
// CWD-implicit form would have broken under) and gives itself a private,
// empty GOCACHE (equivalent to a clean cache, but scoped to this test —
// `go clean -cache` is process-global and would race every other package's
// build under a parallel `go test ./...`), then proves compileFixture still
// resolves both offline under GOPROXY=off. Before pinning cmd.Dir explicitly
// in compileFixture, this test failed with "package pix/host/plugin is not
// in std" and "go.mod file not found".
func TestFixtureBuildSurvivesCWDChange(t *testing.T) {
	if testing.Short() {
		t.Skip("forces a cold `go build` with an empty GOCACHE; slow by design, covered by the untimed race/metrics CI jobs")
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOCACHE", filepath.Join(t.TempDir(), "gocache"))
	t.Chdir(t.TempDir()) // simulate a caller whose process CWD is not this package

	bin, err := compileFixture("testdata/fixture/main.go.txt")
	if err != nil {
		t.Fatalf("fixture build lost module resolution after a CWD change: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("compiled fixture binary missing: %v", err)
	}
}

// testBudgets shrink the pinned budgets so a test runs in seconds; the
// DEFAULTS are asserted separately (TestDefaultBudgetsPinned).
func testBudgets() Budgets {
	b := DefaultBudgets()
	b.Handshake = 20 * time.Second
	b.HealthInterval = 150 * time.Millisecond
	b.HealthTimeout = 2 * time.Second
	b.Drain = 300 * time.Millisecond
	b.Stop = 3 * time.Second
	b.FailureBackoff = 150 * time.Millisecond
	return b
}

func testTree(t *testing.T, mutate func(*Config)) *Tree {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		StageDir:  filepath.Join(dir, "stage"),
		StateDir:  filepath.Join(dir, "state"),
		Budgets:   testBudgets(),
		Plugins:   plugin.PluginMap,
		Handshake: plugin.Handshake,
		Logf:      func(string, ...any) {},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	tr := NewTree(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	tr.Start(ctx)
	t.Cleanup(func() { tr.Stop(); cancel() })
	return tr
}

// fixtureHealth is the generic health check the fixture binary (which serves a
// MemoryStore — the retained generic capability, standing in for whatever real
// unit kind a test wants to exercise) answers through Health(). It is a
// stand-in for the dormant CredentialBroker's Check(), removed with that seam.
func fixtureHealth(impl any) error {
	m, ok := impl.(plugin.MemoryStore)
	if !ok || m == nil {
		return errors.New("not a fixture memory store")
	}
	_, err := m.Health()
	return err
}

func fixtureUnit(name, bin, sha string, grant ...string) UnitSpec {
	return UnitSpec{Name: name, Kind: "memory", Path: bin, SHA: sha,
		EnvAllow: []string{"PATH", "HOME", "TMPDIR"}, EnvGrant: grant}
}

// describe returns the fixture's Health(), which carries the generation tag
// (WatcherModel) and pid (CaptureReason) so a test can tell two generations of
// the same unit apart — the same role BrokerInfo/Describe() played before the
// dormant CredentialBroker seam was removed.
func describe(t *testing.T, h *Holder) plugin.Health {
	t.Helper()
	m, ok := h.Get().(plugin.MemoryStore)
	if !ok || m == nil {
		t.Fatalf("holder does not carry a dispensed fixture store: %T", h.Get())
	}
	info, err := m.Health()
	if err != nil {
		t.Fatalf("Health over real rpc: %v", err)
	}
	return info
}

func waitFor(t *testing.T, what string, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// must fails the test on any error the assertion is not about.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sawEvent reports whether the tree recorded a typed event for a unit.
func sawEvent(tr *Tree, unit string, typ EventType) bool {
	for _, e := range tr.Events() {
		if e.Unit == unit && e.Type == typ {
			return true
		}
	}
	return false
}

// startOrphan stands in for a dead supervisor's child: a live, already
// handshaked plugin process nobody is currently supervising.
func startOrphan(t *testing.T, bin string, spec UnitSpec) *goplugin.Client {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = FilterEnv(spec.EnvAllow, spec.EnvGrant)
	c := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: plugin.Handshake, Plugins: plugin.PluginMap, Cmd: cmd,
	})
	if _, err := c.Client(); err != nil {
		t.Fatalf("start the pre-existing plugin: %v", err)
	}
	t.Cleanup(c.Kill)
	return c
}

// PINNED, not config: a regression that widens the stop budget past the
// launchd stop grace, or makes a probe slower than the interval.
func TestDefaultBudgetsPinned(t *testing.T) {
	b := DefaultBudgets()
	for name, c := range map[string][2]time.Duration{
		"handshake":      {b.Handshake, 30 * time.Second},
		"healthInterval": {b.HealthInterval, 5 * time.Second},
		"healthTimeout":  {b.HealthTimeout, 3 * time.Second},
		"drain":          {b.Drain, 5 * time.Second},
		"stop":           {b.Stop, 15 * time.Second},
		"failureBackoff": {b.FailureBackoff, 2 * time.Second},
	} {
		if c[0] != c[1] {
			t.Errorf("budget %s = %v, want %v", name, c[0], c[1])
		}
	}
	if b.HealthTimeout >= b.HealthInterval {
		t.Error("health timeout must be shorter than the interval or probes stack up")
	}
	if b.Drain >= b.Stop {
		t.Error("drain must fit inside the stop budget")
	}
	if b.HealthFailures < 1 {
		t.Error("health failures must be at least 1")
	}
}

func TestUnitSpecValidateFailsClosed(t *testing.T) {
	ok := UnitSpec{Name: "memory", Kind: "memory", SelfExec: true}
	if err := ok.Validate(); err != nil {
		t.Fatalf("self-exec unit should validate: %v", err)
	}
	sha := strings.Repeat("ab", 32)
	bad := []struct {
		name string
		spec UnitSpec
		want string
	}{
		{"no name", UnitSpec{Kind: "memory", SelfExec: true}, "name"},
		{"no kind", UnitSpec{Name: "x", SelfExec: true}, "kind"},
		{"neither", UnitSpec{Name: "x", Kind: "memory"}, "exactly one"},
		{"both", UnitSpec{Name: "x", Kind: "memory", SelfExec: true, Path: "/bin/true", SHA: sha}, "exactly one"},
		{"unpinned", UnitSpec{Name: "x", Kind: "memory", Path: "/bin/true"}, "unpinned"},
		{"short sha", UnitSpec{Name: "x", Kind: "memory", Path: "/bin/true", SHA: "abc"}, "sha256"},
		{"relative path", UnitSpec{Name: "x", Kind: "memory", Path: "rel/bin", SHA: sha}, "absolute"},
		{"env value", UnitSpec{Name: "x", Kind: "memory", SelfExec: true, EnvAllow: []string{"FOO=bar"}}, "reference name"},
		{"env secret ref", UnitSpec{Name: "x", Kind: "memory", SelfExec: true, EnvAllow: []string{"op://vault/i/f"}}, "reference name"},
		{"grant shape", UnitSpec{Name: "x", Kind: "memory", SelfExec: true, EnvGrant: []string{"NOEQUALS"}}, "KEY=VALUE"},
	}
	for _, c := range bad {
		err := c.spec.Validate()
		if err == nil {
			t.Errorf("%s: expected refusal, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

// NewExternalUnit is how a pack-admitted [[services]] entry is wired in, so the
// same fail-closed validation applies to a pack as to a config block.
func TestNewExternalUnitIsTheWiringSeam(t *testing.T) {
	bin, sha := buildFixture(t)
	u, err := NewExternalUnit("packsvc", "memory", bin, sha, []string{"--flag"}, []string{"PATH"})
	if err != nil {
		t.Fatalf("NewExternalUnit: %v", err)
	}
	if u.SelfExec || u.Path != bin || u.SHA != sha || len(u.Argv) != 1 {
		t.Fatalf("unexpected unit: %+v", u)
	}
	if _, err := NewExternalUnit("packsvc", "memory", bin, "", nil, nil); err == nil {
		t.Fatal("an unpinned pack service must be refused")
	}
}

// The supervisor execs a STAGED copy it hashed itself, so swapping the
// original between verification and exec cannot change what runs (TOCTOU).
func TestStageVerifiedExecutable(t *testing.T) {
	bin, sha := buildFixture(t)
	stage := t.TempDir()
	staged, err := StageExecutable(stage, "unit", bin, sha)
	if err != nil {
		t.Fatalf("StageExecutable: %v", err)
	}
	// The staged copy lives in the unit's OWN subdirectory of the stage dir
	// (R1-1: no shared, flat namespace a sibling's name could collide into).
	if wantDir := filepath.Join(stage, "unit"); filepath.Dir(staged) != wantDir {
		t.Errorf("staged copy %q is not inside its unit dir %q", staged, wantDir)
	}
	if got, _ := FileSHA256(staged); got != sha {
		t.Errorf("staged copy sha = %s, want %s", got, sha)
	}
	fi, err := os.Stat(staged)
	must(t, err)
	if fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("staged copy is group/world writable: %v", fi.Mode())
	}
	// A swapped original does not change the staged bytes.
	must(t, os.WriteFile(bin+".swap", []byte("evil"), 0o755))
	if got, _ := FileSHA256(staged); got != sha {
		t.Errorf("staged copy changed after the source was swapped")
	}
	// A mismatched pin is refused, and nothing is left in the stage dir.
	if _, err := StageExecutable(stage, "bad", bin, strings.Repeat("0", 64)); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected a sha256 mismatch refusal, got %v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(stage, "bad"))
	for _, e := range ents {
		t.Errorf("a refused binary was left staged: %s", e.Name())
	}
}

// R1-1: a unit name that is a string-prefix of, or embeds the shape of,
// another unit's stage filenames ("a" vs "a-" vs "a.stage-x") must never let
// one unit's stage sweep touch a sibling's files — not by luck of a length
// check, but because each unit stages into its OWN exclusive directory.
func TestStageIsolatesPrefixOverlappingUnitNames(t *testing.T) {
	bin, sha := buildFixture(t)
	stage := t.TempDir()

	// A sibling whose in-flight temp file, once "a" is trimmed as a string
	// prefix, reproduces the exact "stage-" temp-file shape the old flat
	// sweep matched on (the concrete R1-1 collision).
	victimDir := filepath.Join(stage, "a.stage-foo")
	must(t, os.MkdirAll(victimDir, 0o700))
	victimTemp := filepath.Join(victimDir, "stage-inflight")
	must(t, os.WriteFile(victimTemp, []byte("tmp"), 0o600))

	// A sibling named with a trailing hyphen, and one that is a plain
	// character-extension of "a" — both from the review's own examples.
	hyphenSibling, err := StageExecutable(stage, "a-", bin, sha)
	must(t, err)
	extendingSibling, err := StageExecutable(stage, "ab", bin, sha)
	must(t, err)

	staged, err := StageExecutable(stage, "a", bin, sha)
	must(t, err)

	if _, err := os.Stat(victimTemp); err != nil {
		t.Errorf("unit %q swept sibling %q's in-flight temp file: %v", "a", "a.stage-foo", err)
	}
	for name, path := range map[string]string{"a-": hyphenSibling, "ab": extendingSibling, "a (own copy)": staged} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("sweep for unit %q removed sibling/own copy %q: %v", "a", name, err)
		}
	}
}

// R1-1 hardening: a unit name is a stage-directory component, not a free
// string — path separators and traversal segments are refused rather than
// silently escaping the stage dir.
func TestStageRefusesUnsafeUnitNames(t *testing.T) {
	bin, sha := buildFixture(t)
	stage := t.TempDir()
	for _, unit := range []string{"../escape", "a/b", ".", "..", ""} {
		if _, err := StageExecutable(stage, unit, bin, sha); err == nil {
			t.Errorf("StageExecutable(%q) should have been refused", unit)
		}
	}
}

// The child gets ONLY allowlisted names plus explicit grants — proven by
// reading the env the REAL child received.
func TestChildEnvIsAllowlisted(t *testing.T) {
	bin, sha := buildFixture(t)
	t.Setenv("PIX_BROKER_AUTH", "super-secret-bearer")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak-me")
	dump := filepath.Join(t.TempDir(), "env.txt")

	tr := testTree(t, nil)
	_, err := tr.Add(fixtureUnit("envunit", bin, sha, "FIXTURE_ENV_DUMP="+dump), fixtureHealth)
	must(t, err)
	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("fixture did not dump its env: %v", err)
	}
	env := string(got)
	for _, forbidden := range []string{"PIX_BROKER_AUTH=", "AWS_SECRET_ACCESS_KEY="} {
		if strings.Contains(env, forbidden) {
			t.Errorf("child env leaked %s:\n%s", forbidden, env)
		}
	}
	// The explicit grant, an allowlisted name and go-plugin's own cookie all
	// survive the filter.
	for _, want := range []string{"FIXTURE_ENV_DUMP=" + dump, "PATH=", plugin.Handshake.MagicCookieKey + "="} {
		if !strings.Contains(env, want) {
			t.Errorf("child env is missing %q:\n%s", want, env)
		}
	}
}

func TestUnitStartsHealthyReportsStatusAndIsRestartedOnCrash(t *testing.T) {
	bin, sha := buildFixture(t)
	tr := testTree(t, nil)
	h, err := tr.Add(fixtureUnit("crasher", bin, sha, "FIXTURE_TAG=gen1", "FIXTURE_CRASH_MS=400"), fixtureHealth)
	must(t, err)
	if got := describe(t, h).WatcherModel; got != "gen1" {
		t.Errorf("dispensed the wrong generation: %q", got)
	}
	first, ok := tr.Unit("crasher")
	if !ok {
		t.Fatal("no status for the unit")
	}
	if first.State != UnitRunning || !first.HealthOK || first.PID <= 0 || first.Restarts != 0 || first.Reattached {
		t.Errorf("unexpected status: %+v", first)
	}
	if first.Kind != "memory" || first.Name != "crasher" || !alive(first.PID) {
		t.Errorf("status identity/liveness wrong: %+v", first)
	}
	if len(tr.Events()) == 0 || tr.Events()[0].Type != EventStarted {
		t.Errorf("expected a typed 'started' event, got %+v", tr.Events())
	}

	// The fixture crashes 400ms in: Suture restarts it (no hand-rolled
	// watchdog) and the holder swaps to the NEW process, transparently.
	waitFor(t, "restart", 20*time.Second, func() bool {
		st, _ := tr.Unit("crasher")
		return st.Restarts >= 1 && st.State == UnitRunning && st.PID != first.PID
	})
	st, _ := tr.Unit("crasher")
	if !alive(st.PID) {
		t.Fatalf("restarted pid %d is not alive", st.PID)
	}
	if got := describe(t, h).CaptureReason; got != strconv.Itoa(st.PID) {
		t.Errorf("holder still points at the dead generation (pid %s, want %d)", got, st.PID)
	}
	if !sawEvent(tr, "crasher", EventExited) {
		t.Errorf("no typed exit event recorded: %+v", tr.Events())
	}
}

// A unit that never comes up is REMOVED from the tree, not left behind it.
// Both branches of Add's failure path are covered: the unit that answers but is
// unhealthy ("the port is bound" is not health) and the unit whose start wedges
// past the budget. Either way the caller gets the real error AND the child
// supervisor is gone — without that, Suture keeps restarting, with backoff,
// forever, a unit `serve` already gave up on: a background restart leak.
func TestFailedStartIsRemovedFromTheTree(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises real Suture backoff/wedged-past-budget timing; covered by the untimed race/metrics CI jobs")
	}
	bin, sha := buildFixture(t)
	spawns := func(log string) int {
		raw, _ := os.ReadFile(log)
		return len(strings.Fields(string(raw)))
	}

	t.Run("unhealthy", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "spawns")
		tr := testTree(t, nil)
		_, err := tr.Add(fixtureUnit("sick", bin, sha, "FIXTURE_UNHEALTHY=1", "FIXTURE_SPAWN_LOG="+log), fixtureHealth)
		if err == nil {
			t.Fatal("Add must fail when the unit never passes its first health probe")
		}
		if !strings.Contains(err.Error(), "health") {
			t.Errorf("error should name the health failure, got %v", err)
		}
		if !sawEvent(tr, "sick", EventHealthFailed) {
			t.Errorf("no typed health-failure event: %+v", tr.Events())
		}
		if st, _ := tr.Unit("sick"); st.State != UnitFailed || st.PID != 0 {
			t.Errorf("failed unit status = %+v, want failed with no pid", st)
		}
		// The leak assertion: many backoff intervals later, the tree has not
		// spawned the unit again.
		ran := spawns(log)
		if ran == 0 {
			t.Fatal("fixture never ran; the spawn log is empty")
		}
		time.Sleep(10 * testBudgets().FailureBackoff)
		if got := spawns(log); got != ran {
			t.Errorf("unit was restarted %d more times after Add failed (spawns %d -> %d)", got-ran, ran, got)
		}
	})

	t.Run("wedged past the budget", func(t *testing.T) {
		// A FIFO as the unit's executable wedges staging in the blocking open,
		// so the unit never signals and Add hits its timeout instead.
		dir := t.TempDir()
		fifo := filepath.Join(dir, "wedged")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("no fifo support: %v", err)
		}
		t.Cleanup(func() {
			// Unwedge the staging goroutine: a non-blocking write-open lets the
			// blocked read-open return, and the copy then fails the pin.
			if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = f.Close()
			}
		})
		tr := testTree(t, func(c *Config) {
			c.Budgets.Handshake = 700 * time.Millisecond
			c.Budgets.HealthTimeout = 300 * time.Millisecond
		})
		start := time.Now()
		_, err := tr.Add(fixtureUnit("wedged", fifo, sha), fixtureHealth)
		if err == nil || !strings.Contains(err.Error(), "did not become healthy") {
			t.Fatalf("a wedged start must time out, got %v", err)
		}
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("Add blocked for %v on a wedged unit", took)
		}
		if st, _ := tr.Unit("wedged"); st.State != UnitFailed {
			t.Errorf("wedged unit State = %q, want %q", st.State, UnitFailed)
		}
	})

	t.Run("poisoned", func(t *testing.T) {
		// A unit whose pinned bytes changed is PERMANENT (ErrDoNotRestart), and
		// the root plus every sibling survives it.
		raw, err := os.ReadFile(bin)
		must(t, err)
		copyBin := filepath.Join(t.TempDir(), "fixture")
		must(t, os.WriteFile(copyBin, raw, 0o755))
		tr := testTree(t, nil)
		good, err := tr.Add(fixtureUnit("good", bin, sha, "FIXTURE_TAG=good"), fixtureHealth)
		if err != nil {
			t.Fatalf("Add good: %v", err)
		}
		// Swap the pinned binary, then start a unit that pins the ORIGINAL sha.
		must(t, os.WriteFile(copyBin, append(raw, '\n'), 0o755))
		_, err = tr.Add(fixtureUnit("poisoned", copyBin, sha), fixtureHealth)
		if err == nil {
			t.Fatal("a unit whose pinned bytes changed must refuse to start")
		}
		if !errors.Is(err, suture.ErrDoNotRestart) {
			t.Errorf("a poisoned unit must be permanent (ErrDoNotRestart), got %v", err)
		}
		time.Sleep(10 * testBudgets().FailureBackoff)
		if st, _ := tr.Unit("poisoned"); st.State != UnitFailed || st.PID != 0 {
			t.Errorf("poisoned unit came back: %+v", st)
		}
		// Root and sibling survive: the good unit still answers real RPC, and
		// the root still accepts new work.
		if got := describe(t, good).WatcherModel; got != "good" {
			t.Errorf("sibling died with the poisoned unit (tag %q)", got)
		}
		if _, err := tr.Add(fixtureUnit("later", bin, sha), fixtureHealth); err != nil {
			t.Fatalf("root supervisor stopped accepting units: %v", err)
		}
	})
}

// Stop drains, kills within the stop budget, and clears the holder so a late
// caller gets "unavailable".
func TestStopDrainsAndKillsWithinBudget(t *testing.T) {
	bin, sha := buildFixture(t)
	tr := testTree(t, nil)
	h, err := tr.Add(fixtureUnit("stopper", bin, sha, "FIXTURE_STUBBORN=1"), fixtureHealth)
	must(t, err)
	pid, _ := strconv.Atoi(describe(t, h).CaptureReason)

	start := time.Now()
	tr.Stop()
	if took := time.Since(start); took > 2*testBudgets().Stop {
		t.Errorf("Stop took %v, well past the stop budget", took)
	}
	waitFor(t, "the child to be reaped", 5*time.Second, func() bool { return !alive(pid) })
	if h.Get() != nil {
		t.Errorf("holder still dispenses after stop: %T", h.Get())
	}
	if st, _ := tr.Unit("stopper"); st.State != UnitStopped {
		t.Errorf("State after stop = %q, want %q", st.State, UnitStopped)
	}
}

// A HARD supervisor death (SIGKILL) leaves a live plugin process behind. The
// next supervisor reattaches to it — identity, pid ownership and socket
// ownership all verified — instead of orphaning it and spawning a duplicate.
func TestReattachAfterHardSupervisorDeath(t *testing.T) {
	if testing.Short() {
		t.Skip("real process spawn/kill + reattach timing; covered by the untimed race/metrics CI jobs")
	}
	bin, sha := buildFixture(t)
	state := filepath.Join(t.TempDir(), "state")

	spec := fixtureUnit("survivor", bin, sha, "FIXTURE_TAG=gen1")
	rc := startOrphan(t, bin, spec).ReattachConfig()
	if err := SaveReattach(state, spec, rc, plugin.ProtocolVersion); err != nil {
		t.Fatalf("SaveReattach: %v", err)
	}

	tr := testTree(t, func(c *Config) { c.StateDir = state })

	// The SAME spec (the identity fingerprint covers the env surface now): a
	// FRESH spawn would answer a new pid; a reattach answers the orphan's.
	h, err := tr.Add(spec, fixtureHealth)
	must(t, err)
	info := describe(t, h)
	if info.WatcherModel != "gen1" || info.CaptureReason != strconv.Itoa(rc.Pid) {
		t.Fatalf("did not reattach: tag=%q pid=%s, want gen1/%d", info.WatcherModel, info.CaptureReason, rc.Pid)
	}
	st, _ := tr.Unit("survivor")
	if !st.Reattached || st.PID != rc.Pid || st.State != UnitRunning {
		t.Errorf("status does not report the reattach: %+v", st)
	}
}

// Reattach state is IDENTITY-CHECKED: a state file written for a different
// executable, or a dead pid, is discarded and a fresh child is spawned.
func TestReattachRefusesForeignOrDeadState(t *testing.T) {
	bin, sha := buildFixture(t)

	t.Run("dead pid", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		spec := fixtureUnit("dead", bin, sha, "FIXTURE_TAG=fresh")
		dead := exec.Command(bin)
		dead.Env = []string{}
		must(t, dead.Start())
		pid := dead.Process.Pid
		_ = dead.Process.Kill()
		_, _ = dead.Process.Wait()
		if err := SaveReattach(state, spec, &goplugin.ReattachConfig{Pid: pid, Protocol: goplugin.ProtocolNetRPC,
			Addr:            &net.UnixAddr{Name: filepath.Join(dir, "gone.sock"), Net: "unix"},
			ProtocolVersion: plugin.ProtocolVersion}, plugin.ProtocolVersion); err != nil {
			t.Fatal(err)
		}
		tr := testTree(t, func(c *Config) { c.StateDir = state })
		h, err := tr.Add(spec, fixtureHealth)
		must(t, err)
		if got := describe(t, h); got.WatcherModel != "fresh" || got.CaptureReason == strconv.Itoa(pid) {
			t.Errorf("reattached to a dead pid: %+v", got)
		}
		if st, _ := tr.Unit("dead"); st.Reattached {
			t.Error("status claims a reattach that cannot have happened")
		}
	})

	t.Run("foreign identity", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		orphan := startOrphan(t, bin, fixtureUnit("foreign", bin, sha, "FIXTURE_TAG=gen1"))
		// State saved under the SAME unit name but a different pinned identity.
		other := fixtureUnit("foreign", bin, strings.Repeat("cd", 32), "FIXTURE_TAG=gen1")
		must(t, SaveReattach(state, other, orphan.ReattachConfig(), plugin.ProtocolVersion))
		tr := testTree(t, func(c *Config) { c.StateDir = state })
		spec2 := fixtureUnit("foreign", bin, sha, "FIXTURE_TAG=gen2")
		h, err := tr.Add(spec2, fixtureHealth)
		must(t, err)
		if got := describe(t, h).WatcherModel; got != "gen2" {
			t.Errorf("reattached across an identity change (tag %q)", got)
		}
	})
}

// M2: processAlive requires the pid to exist AND be OURS. A signal-0 EPERM
// (the pid exists but belongs to someone else — after pid reuse, anybody) is a
// refusal, not proof of our child.
func TestProcessAliveRequiresSameUIDOwnership(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("our own pid must be alive and ours")
	}
	dead := exec.Command("true")
	must(t, dead.Start())
	pid := dead.Process.Pid
	_, _ = dead.Process.Wait()
	if processAlive(pid) {
		t.Errorf("a reaped pid %d reported alive", pid)
	}
	// A live process owned by ANOTHER uid: pid 1 stands in wherever we cannot
	// signal it. The OLD code returned true here (EPERM taken as "alive").
	if syscall.Kill(1, 0) == nil {
		t.Skip("pid 1 is signalable from this uid; no foreign-uid pid to probe")
	}
	if processAlive(1) {
		t.Error("a foreign-uid pid reported as ours — EPERM is not proof")
	}
}

// M2 fixture: a recorded pid REUSED by a process that is not ours is refused
// before any socket is touched — even when the recorded unix socket is live
// and ours — and the unit comes up fresh.
func TestReattachRefusesReusedPid(t *testing.T) {
	if syscall.Kill(1, 0) == nil {
		t.Skip("pid 1 is signalable from this uid; cannot stand in for a reused pid")
	}
	bin, sha := buildFixture(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	spec := fixtureUnit("reused", bin, sha, "FIXTURE_TAG=fresh")
	sock := filepath.Join(dir, "live.sock")
	l, err := net.Listen("unix", sock)
	must(t, err)
	t.Cleanup(func() { l.Close() })
	must(t, SaveReattach(state, spec, &goplugin.ReattachConfig{
		Pid: 1, Protocol: goplugin.ProtocolNetRPC, ProtocolVersion: plugin.ProtocolVersion,
		Addr: &net.UnixAddr{Name: sock, Net: "unix"}}, plugin.ProtocolVersion))
	tr := testTree(t, func(c *Config) { c.StateDir = state })
	_, err = tr.Add(spec, fixtureHealth)
	must(t, err)
	if st, _ := tr.Unit("reused"); st.Reattached || st.PID == 1 {
		t.Errorf("adopted a reused pid: %+v", st)
	}
	if !sawEvent(tr, "reused", EventReattachRejected) {
		t.Errorf("no typed reattach-rejected event: %+v", tr.Events())
	}
}

// M2: reattach targets are UNIX SOCKETS WE OWN, or nothing. A tcp address, a
// regular file wearing the recorded path, and a live socket that is not our
// child all get rejected — and the unit comes up FRESH instead.
func TestReattachRefusesForeignSockets(t *testing.T) {
	if testing.Short() {
		t.Skip("real process spawn + socket reattach timing; covered by the untimed race/metrics CI jobs")
	}
	bin, sha := buildFixture(t)
	cases := []struct {
		name string
		addr func(t *testing.T, dir string) net.Addr
	}{
		{"tcp address", func(t *testing.T, dir string) net.Addr {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			must(t, err)
			t.Cleanup(func() { l.Close() })
			return l.Addr()
		}},
		{"regular file", func(t *testing.T, dir string) net.Addr {
			path := filepath.Join(dir, "not-a-socket")
			must(t, os.WriteFile(path, []byte("x"), 0o600))
			return &net.UnixAddr{Name: path, Net: "unix"}
		}},
		{"imposter unix socket", func(t *testing.T, dir string) net.Addr {
			path := filepath.Join(dir, "imposter.sock")
			l, err := net.Listen("unix", path)
			must(t, err)
			go func() { // accepts and hangs up: never speaks our protocol
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
			t.Cleanup(func() { l.Close() })
			return &net.UnixAddr{Name: path, Net: "unix"}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			spec := fixtureUnit("sockets", bin, sha, "FIXTURE_TAG=fresh")
			// A live pid WE own, so a rejection is attributable to the endpoint alone.
			orphanPid := startOrphan(t, bin, spec).ReattachConfig().Pid
			must(t, SaveReattach(state, spec, &goplugin.ReattachConfig{
				Pid: orphanPid, Protocol: goplugin.ProtocolNetRPC,
				ProtocolVersion: plugin.ProtocolVersion, Addr: c.addr(t, dir)}, plugin.ProtocolVersion))
			tr := testTree(t, func(cfg *Config) { cfg.StateDir = state })
			h, err := tr.Add(spec, fixtureHealth)
			must(t, err)
			if got := describe(t, h).CaptureReason; got == strconv.Itoa(orphanPid) {
				t.Errorf("adopted the orphan through a foreign endpoint (pid %s)", got)
			}
			st, _ := tr.Unit("sockets")
			if st.Reattached {
				t.Errorf("status claims a reattach through a foreign endpoint: %+v", st)
			}
			if !sawEvent(tr, "sockets", EventReattachRejected) {
				t.Errorf("no typed reattach-rejected event: %+v", tr.Events())
			}
		})
	}
}

// M4: a reattach can be fully OWNERSHIP-VERIFIED (identity, uid-owned pid,
// uid-owned unix socket — verifyReattachTarget passes) yet still fail to
// reconnect, because the persisted socket exists and is ours but nothing is
// listening on it anymore. Left alone, that live orphan keeps holding
// whatever the unit exclusively owns (memory's store flock is the motivating
// case) forever, deadlocking every subsequent spawn attempt the same way.
// tryReattach must terminate exactly this VERIFIED orphan before falling back
// to a fresh spawn — and must NEVER do so for a pid that failed any
// ownership check (the sibling refusal tests above all hit a `reason != ""`
// before killVerifiedOrphan is ever reachable, so none of them are at risk of
// killing an unverified stranger).
func TestReattachKillsAVerifiedOrphanThatStoppedAnsweringItsSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("real process spawn + socket reattach timing; covered by the untimed race/metrics CI jobs")
	}
	bin, sha := buildFixture(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	spec := fixtureUnit("stuck", bin, sha, "FIXTURE_TAG=orig")

	// A REAL, live, uid-owned process standing in for the orphan holding
	// whatever exclusive resource this unit serves — exec'd directly (not
	// through go-plugin) since its only job here is to stay alive, exactly as
	// a hung memory unit would. This test, not go-plugin, is its parent, so it
	// reaps it itself the instant it exits (a zombie still answers kill(pid,0)
	// as "alive").
	orphan := exec.Command(bin)
	orphan.Env = FilterEnv(spec.EnvAllow, spec.EnvGrant)
	must(t, orphan.Start())
	pid := orphan.Process.Pid
	reaped := make(chan struct{})
	go func() { _ = orphan.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = orphan.Process.Kill(); <-reaped })

	// A REAL unix socket file the orphan does NOT own the listening end of
	// anymore: bound, then closed WITHOUT unlinking, so the path still Lstat's
	// as a socket owned by our uid (verifyReattachTarget passes) but dialing
	// it fails — the same ErrProcessNotFound go-plugin's own reattach path
	// gets connecting to any dead listener.
	sock := filepath.Join(dir, "stuck.sock")
	l, err := net.Listen("unix", sock)
	must(t, err)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	must(t, l.Close())
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatal("test setup: the stale socket must refuse new connections")
	}

	must(t, SaveReattach(state, spec, &goplugin.ReattachConfig{
		Pid: pid, Protocol: goplugin.ProtocolNetRPC, ProtocolVersion: plugin.ProtocolVersion,
		Addr: &net.UnixAddr{Name: sock, Net: "unix"},
	}, plugin.ProtocolVersion))

	tr := testTree(t, func(c *Config) { c.StateDir = state })
	h, err := tr.Add(spec, fixtureHealth)
	must(t, err)

	// The unit still comes up — freshly spawned, never reattached.
	if got := describe(t, h); got.WatcherModel != "orig" || got.CaptureReason == strconv.Itoa(pid) {
		t.Errorf("unexpected reattach to the stuck orphan: %+v", got)
	}
	if st, _ := tr.Unit("stuck"); st.Reattached {
		t.Error("status claims a reattach to a socket that refuses connections")
	}
	if !sawEvent(tr, "stuck", EventReattachRejected) {
		t.Errorf("no typed reattach-rejected event: %+v", tr.Events())
	}

	// The VERIFIED orphan must actually be dead — not left running to hold the
	// flock forever.
	waitFor(t, "the verified orphan to be killed", 3*time.Second, func() bool { return !alive(pid) })
	if !sawEvent(tr, "stuck", EventOrphanKilled) {
		t.Errorf("no typed orphan-killed event: %+v", tr.Events())
	}
}

// L1: the admission fingerprint covers the ENV SURFACE — order-independent for
// the allowlist, value-sensitive for grants, collision-free against argv — and
// the reattach state on disk never carries a grant's VALUE, hashed or plain.
func TestIdentityCoversEnvSurfaceWithoutLeakingValues(t *testing.T) {
	base := UnitSpec{Name: "u", Kind: "memory", SelfExec: true,
		EnvAllow: []string{"PATH", "HOME"}, EnvGrant: []string{"TOKEN=hunter2-secret-value"}}
	perm := base
	perm.EnvAllow = []string{"HOME", "PATH"}
	if base.identity() != perm.identity() {
		t.Error("a reordered allowlist is the same grant; identity must not change")
	}
	widened := base
	widened.EnvAllow = []string{"PATH", "HOME", "AWS_SECRET_ACCESS_KEY"}
	if base.identity() == widened.identity() {
		t.Error("widening EnvAllow must change the identity")
	}
	regrant := base
	regrant.EnvGrant = []string{"TOKEN=rotated"}
	if base.identity() == regrant.identity() {
		t.Error("rotating a grant value must change the identity")
	}
	crossed := UnitSpec{Name: "u", Kind: "memory", SelfExec: true, Argv: []string{"PATH"}}
	uncrossed := UnitSpec{Name: "u", Kind: "memory", SelfExec: true, EnvAllow: []string{"PATH"}}
	if crossed.identity() == uncrossed.identity() {
		t.Error("an argv element must not collide with an allow name")
	}
	// The persisted state carries the fingerprint, never the secret.
	state := t.TempDir()
	must(t, SaveReattach(state, base, &goplugin.ReattachConfig{Pid: os.Getpid(),
		Addr: &net.UnixAddr{Name: "/tmp/x.sock", Net: "unix"}, Protocol: goplugin.ProtocolNetRPC,
		ProtocolVersion: plugin.ProtocolVersion}, plugin.ProtocolVersion))
	raw, err := os.ReadFile(reattachPath(state, "u"))
	must(t, err)
	for _, leak := range []string{"hunter2-secret-value", "TOKEN="} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("reattach state leaked %q", leak)
		}
	}
}

// L1 fixture: a surviving child whose spec's grants have since ROTATED is
// refused (its identity no longer matches) and replaced fresh.
func TestReattachRefusesRotatedEnvIdentity(t *testing.T) {
	bin, sha := buildFixture(t)
	state := filepath.Join(t.TempDir(), "state")
	old := fixtureUnit("envrot", bin, sha, "FIXTURE_TAG=old")
	orphan := startOrphan(t, bin, old)
	must(t, SaveReattach(state, old, orphan.ReattachConfig(), plugin.ProtocolVersion))
	tr := testTree(t, func(c *Config) { c.StateDir = state })
	rotated := fixtureUnit("envrot", bin, sha, "FIXTURE_TAG=new")
	h, err := tr.Add(rotated, fixtureHealth)
	must(t, err)
	if got := describe(t, h).WatcherModel; got != "new" {
		t.Errorf("reattached across an env-grant rotation (tag %q)", got)
	}
	if st, _ := tr.Unit("envrot"); st.Reattached {
		t.Error("status claims a reattach across a rotated grant")
	}
	if !sawEvent(tr, "envrot", EventReattachRejected) {
		t.Errorf("no typed reattach-rejected event: %+v", tr.Events())
	}
}

// Revocation: a clean stop revokes the persisted reattach state, so the NEXT
// supervisor spawns fresh instead of hunting a child that was deliberately stopped.
func TestCleanStopRevokesReattachState(t *testing.T) {
	bin, sha := buildFixture(t)
	state := filepath.Join(t.TempDir(), "state")
	tr := testTree(t, func(c *Config) { c.StateDir = state })
	_, err := tr.Add(fixtureUnit("revoked", bin, sha), fixtureHealth)
	must(t, err)
	path := reattachPath(state, "revoked")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no reattach state while running: %v", err)
	}
	tr.Stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("reattach state survived a clean stop (err=%v)", err)
	}
}

// L2: staging a NEW pin sweeps the superseded copy and this unit's orphaned
// temp files — and nothing else: the fresh copy and every sibling survive.
func TestStageSweepsSupersededCopies(t *testing.T) {
	bin, sha := buildFixture(t)
	stage := t.TempDir()
	sibling, err := StageExecutable(stage, "other", bin, sha)
	must(t, err)
	extending, err := StageExecutable(stage, "unit-b", bin, sha)
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(stage, "unit"), 0o700))
	orphan := filepath.Join(stage, "unit", "stage-orphan")
	must(t, os.WriteFile(orphan, []byte("tmp"), 0o600))

	first, err := StageExecutable(stage, "unit", bin, sha)
	must(t, err)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned temp file survived the sweep (err=%v)", err)
	}
	raw, err := os.ReadFile(bin)
	must(t, err)
	v2 := filepath.Join(t.TempDir(), "fixture2")
	must(t, os.WriteFile(v2, append(raw, '\n'), 0o755))
	sha2, err := FileSHA256(v2)
	must(t, err)
	second, err := StageExecutable(stage, "unit", v2, sha2)
	must(t, err)
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("superseded staged copy survived (err=%v)", err)
	}
	for name, path := range map[string]string{"current": second, "sibling": sibling, "name-extending sibling": extending} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("sweep removed the %s copy: %v", name, err)
		}
	}
}
