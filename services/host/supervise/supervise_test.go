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
		dir, err := os.MkdirTemp("", "supervise-fixture")
		if err != nil {
			fixtureErr = err
			return
		}
		src, err := os.ReadFile("testdata/fixture/main.go.txt")
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
		t.Fatalf("build fixture plugin: %v", fixtureErr)
	}
	sha, err := FileSHA256(fixtureBin)
	must(t, err)
	return fixtureBin, sha
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
	if filepath.Dir(staged) != filepath.Clean(stage) {
		t.Errorf("staged copy %q is not inside the stage dir %q", staged, stage)
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
	ents, _ := os.ReadDir(stage)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "bad-") {
			t.Errorf("a refused binary was left staged: %s", e.Name())
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
// next supervisor reattaches to it — identity verified — instead of orphaning
// it and spawning a duplicate.
func TestReattachAfterHardSupervisorDeath(t *testing.T) {
	bin, sha := buildFixture(t)
	state := filepath.Join(t.TempDir(), "state")

	spec := fixtureUnit("survivor", bin, sha, "FIXTURE_TAG=gen1")
	rc := startOrphan(t, bin, spec).ReattachConfig()
	if err := SaveReattach(state, spec, rc, plugin.ProtocolVersion); err != nil {
		t.Fatalf("SaveReattach: %v", err)
	}

	tr := testTree(t, func(c *Config) { c.StateDir = state })

	// A FRESH spawn would answer "gen2"; a reattach answers "gen1".
	spec2 := fixtureUnit("survivor", bin, sha, "FIXTURE_TAG=gen2")
	h, err := tr.Add(spec2, fixtureHealth)
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
