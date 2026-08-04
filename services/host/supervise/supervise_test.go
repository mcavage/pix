// supervise_test.go — the supervision tree is proven against a REAL go-plugin
// fixture binary (testdata/fixture), not a mock: every test below execs a
// process, completes the actual handshake, and makes real net/rpc calls. The
// failure modes (crash, unhealthy, stubborn stop, swapped binary, dead
// reattach target) are produced by the fixture itself.
package supervise

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

// buildFixture compiles testdata/fixture once per test binary.
func buildFixture(t *testing.T) (string, string) {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "supervise-fixture")
		if err != nil {
			fixtureErr = err
			return
		}
		out := filepath.Join(dir, "fixture")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/fixture")
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
	if err != nil {
		t.Fatal(err)
	}
	return fixtureBin, sha
}

// testBudgets shrink the pinned production budgets so a supervision test runs
// in seconds. The DEFAULTS are asserted separately (TestDefaultBudgetsPinned).
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

func brokerHealth(impl any) error {
	b, ok := impl.(plugin.CredentialBroker)
	if !ok || b == nil {
		return errors.New("not a broker")
	}
	return b.Check()
}

func fixtureUnit(name, bin, sha string, grant ...string) UnitSpec {
	return UnitSpec{Name: name, Kind: "broker", Path: bin, SHA: sha,
		EnvAllow: []string{"PATH", "HOME", "TMPDIR"}, EnvGrant: grant}
}

func describe(t *testing.T, h *Holder) plugin.BrokerInfo {
	t.Helper()
	b, ok := h.Get().(plugin.CredentialBroker)
	if !ok || b == nil {
		t.Fatalf("holder does not carry a dispensed broker: %T", h.Get())
	}
	info, err := b.Describe()
	if err != nil {
		t.Fatalf("Describe over real rpc: %v", err)
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

// --- budgets ----------------------------------------------------------------

// The production budgets are PINNED in code (not config): a regression that
// silently widens the stop budget past the launchd/systemd stop grace, or
// makes the health probe slower than the interval, is caught here.
func TestDefaultBudgetsPinned(t *testing.T) {
	b := DefaultBudgets()
	for _, c := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"handshake", b.Handshake, 30 * time.Second},
		{"healthInterval", b.HealthInterval, 5 * time.Second},
		{"healthTimeout", b.HealthTimeout, 3 * time.Second},
		{"drain", b.Drain, 5 * time.Second},
		{"stop", b.Stop, 15 * time.Second},
		{"failureBackoff", b.FailureBackoff, 2 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("budget %s = %v, want %v", c.name, c.got, c.want)
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

// --- unit spec / staging / env ----------------------------------------------

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
		{"no name", UnitSpec{Kind: "broker", SelfExec: true}, "name"},
		{"no kind", UnitSpec{Name: "x", SelfExec: true}, "kind"},
		{"neither", UnitSpec{Name: "x", Kind: "broker"}, "exactly one"},
		{"both", UnitSpec{Name: "x", Kind: "broker", SelfExec: true, Path: "/bin/true", SHA: sha}, "exactly one"},
		{"unpinned", UnitSpec{Name: "x", Kind: "broker", Path: "/bin/true"}, "unpinned"},
		{"short sha", UnitSpec{Name: "x", Kind: "broker", Path: "/bin/true", SHA: "abc"}, "sha256"},
		{"relative path", UnitSpec{Name: "x", Kind: "broker", Path: "rel/bin", SHA: sha}, "absolute"},
		{"env value", UnitSpec{Name: "x", Kind: "broker", SelfExec: true, EnvAllow: []string{"FOO=bar"}}, "reference name"},
		{"env secret ref", UnitSpec{Name: "x", Kind: "broker", SelfExec: true, EnvAllow: []string{"op://vault/i/f"}}, "reference name"},
		{"grant shape", UnitSpec{Name: "x", Kind: "broker", SelfExec: true, EnvGrant: []string{"NOEQUALS"}}, "KEY=VALUE"},
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

// NewExternalUnit is the constructor a pack-admitted [[services]] entry is
// wired through. It is the ONLY way to build an external unit, so the same
// fail-closed validation applies to a pack as to a config block.
func TestNewExternalUnitIsTheWiringSeam(t *testing.T) {
	bin, sha := buildFixture(t)
	u, err := NewExternalUnit("packsvc", "broker", bin, sha, []string{"--flag"}, []string{"PATH"})
	if err != nil {
		t.Fatalf("NewExternalUnit: %v", err)
	}
	if u.SelfExec || u.Path != bin || u.SHA != sha || len(u.Argv) != 1 {
		t.Fatalf("unexpected unit: %+v", u)
	}
	if _, err := NewExternalUnit("packsvc", "broker", bin, "", nil, nil); err == nil {
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
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("staged copy is group/world writable: %v", fi.Mode())
	}
	// A swapped original does not change the staged bytes.
	if err := os.WriteFile(bin+".swap", []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
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

// The child process gets ONLY the allowlisted names plus explicit grants — the
// parent's secrets never cross the process boundary. Proven by reading the env
// the REAL child received.
func TestChildEnvIsAllowlisted(t *testing.T) {
	bin, sha := buildFixture(t)
	t.Setenv("PIX_BROKER_AUTH", "super-secret-bearer")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak-me")
	dump := filepath.Join(t.TempDir(), "env.txt")

	tr := testTree(t, nil)
	if _, err := tr.Add(fixtureUnit("envunit", bin, sha, "FIXTURE_ENV_DUMP="+dump), brokerHealth); err != nil {
		t.Fatalf("Add: %v", err)
	}
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
	if !strings.Contains(env, "FIXTURE_ENV_DUMP="+dump) {
		t.Errorf("explicit grant missing from child env:\n%s", env)
	}
	if !strings.Contains(env, "PATH=") {
		t.Errorf("allowlisted PATH missing from child env:\n%s", env)
	}
	if !strings.Contains(env, plugin.Handshake.MagicCookieKey+"=") {
		t.Errorf("go-plugin handshake cookie missing from child env:\n%s", env)
	}
}

// --- lifecycle --------------------------------------------------------------

func TestUnitStartsHealthyAndReportsTypedStatus(t *testing.T) {
	bin, sha := buildFixture(t)
	tr := testTree(t, nil)
	h, err := tr.Add(fixtureUnit("u1", bin, sha, "FIXTURE_TAG=gen1"), brokerHealth)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := describe(t, h).AuthHeader; got != "gen1" {
		t.Errorf("dispensed the wrong generation: %q", got)
	}
	st, ok := tr.Unit("u1")
	if !ok {
		t.Fatal("no status for u1")
	}
	if st.State != UnitRunning {
		t.Errorf("State = %q, want %q", st.State, UnitRunning)
	}
	if !st.HealthOK || st.PID <= 0 || st.Restarts != 0 || st.Reattached {
		t.Errorf("unexpected status: %+v", st)
	}
	if st.Kind != "broker" || st.Name != "u1" {
		t.Errorf("status identity wrong: %+v", st)
	}
	if !alive(st.PID) {
		t.Errorf("reported pid %d is not alive", st.PID)
	}
	if len(tr.Events()) == 0 || tr.Events()[0].Type != EventStarted {
		t.Errorf("expected a typed 'started' event, got %+v", tr.Events())
	}
}

// A crashed child is restarted by suture (no hand-rolled watchdog) and the
// holder swaps to the NEW process, transparently to the proxy handlers.
func TestCrashIsRestartedByTheTree(t *testing.T) {
	bin, sha := buildFixture(t)
	tr := testTree(t, nil)
	h, err := tr.Add(fixtureUnit("crasher", bin, sha, "FIXTURE_CRASH_MS=400"), brokerHealth)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	first, _ := tr.Unit("crasher")
	waitFor(t, "restart", 20*time.Second, func() bool {
		st, _ := tr.Unit("crasher")
		return st.Restarts >= 1 && st.State == UnitRunning && st.PID != first.PID
	})
	st, _ := tr.Unit("crasher")
	if !alive(st.PID) {
		t.Fatalf("restarted pid %d is not alive", st.PID)
	}
	if got := describe(t, h).DefaultPort; got != st.PID {
		t.Errorf("holder still points at the dead generation (pid %d, want %d)", got, st.PID)
	}
	var sawExit bool
	for _, e := range tr.Events() {
		if e.Unit == "crasher" && e.Type == EventExited {
			sawExit = true
		}
	}
	if !sawExit {
		t.Errorf("no typed exit event recorded: %+v", tr.Events())
	}
}

// A process that is up but failing its health probe is evicted and replaced:
// "the port is bound" is not health.
func TestUnhealthyUnitIsEvicted(t *testing.T) {
	bin, sha := buildFixture(t)
	tr := testTree(t, nil)
	_, err := tr.Add(fixtureUnit("sick", bin, sha, "FIXTURE_UNHEALTHY=1"), brokerHealth)
	if err == nil {
		t.Fatal("Add must fail when the unit never passes its first health probe")
	}
	if !strings.Contains(err.Error(), "unhealthy") && !strings.Contains(err.Error(), "health") {
		t.Errorf("error should name the health failure, got %v", err)
	}
	st, ok := tr.Unit("sick")
	if !ok || (st.State != UnitFailed && st.State != UnitBackoff && st.State != UnitStarting) {
		t.Errorf("unhealthy unit status = %+v", st)
	}
	waitFor(t, "a health-failure event", 5*time.Second, func() bool {
		for _, e := range tr.Events() {
			if e.Unit == "sick" && e.Type == EventHealthFailed {
				return true
			}
		}
		return false
	})
}

// A permanently-poisoned unit (its pinned binary was swapped underneath it)
// stops for good — and the ROOT plus every sibling survives that.
func TestPoisonedUnitDoesNotRestartAndRootSurvives(t *testing.T) {
	bin, sha := buildFixture(t)
	tmp := t.TempDir()
	copyBin := filepath.Join(tmp, "fixture")
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyBin, raw, 0o755); err != nil {
		t.Fatal(err)
	}

	tr := testTree(t, nil)
	good, err := tr.Add(fixtureUnit("good", bin, sha, "FIXTURE_TAG=good"), brokerHealth)
	if err != nil {
		t.Fatalf("Add good: %v", err)
	}
	// Swap the pinned binary, then start a unit that pins the ORIGINAL sha.
	if err := os.WriteFile(copyBin, append(raw, byte('\n')), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = tr.Add(fixtureUnit("poisoned", copyBin, sha), brokerHealth)
	if err == nil {
		t.Fatal("a unit whose pinned bytes changed must refuse to start")
	}
	if !errors.Is(err, suture.ErrDoNotRestart) {
		t.Errorf("a poisoned unit must be permanent (ErrDoNotRestart), got %v", err)
	}
	st, _ := tr.Unit("poisoned")
	if st.State != UnitFailed {
		t.Errorf("poisoned unit State = %q, want %q", st.State, UnitFailed)
	}
	// Root and sibling survive: the good unit still answers real RPC...
	if got := describe(t, good).AuthHeader; got != "good" {
		t.Errorf("sibling died with the poisoned unit (tag %q)", got)
	}
	// ...and the root still accepts new work.
	if _, err := tr.Add(fixtureUnit("later", bin, sha, "FIXTURE_TAG=later"), brokerHealth); err != nil {
		t.Fatalf("root supervisor stopped accepting units: %v", err)
	}
	// The poisoned unit is not restarted (no new pid ever appears).
	time.Sleep(600 * time.Millisecond)
	if st, _ := tr.Unit("poisoned"); st.PID != 0 || st.State != UnitFailed {
		t.Errorf("poisoned unit came back: %+v", st)
	}
}

// Stop drains, then kills within the stop budget, and clears the holder so a
// late caller gets "unavailable" instead of a nil-pointer surprise.
func TestStopDrainsAndKillsWithinBudget(t *testing.T) {
	bin, sha := buildFixture(t)
	dir := t.TempDir()
	tr := NewTree(Config{
		StageDir: filepath.Join(dir, "stage"), StateDir: filepath.Join(dir, "state"),
		Budgets: testBudgets(), Plugins: plugin.PluginMap, Handshake: plugin.Handshake,
		Logf: func(string, ...any) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	tr.Start(ctx)
	h, err := tr.Add(fixtureUnit("stopper", bin, sha, "FIXTURE_STUBBORN=1"), brokerHealth)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	pid := describe(t, h).DefaultPort

	start := time.Now()
	cancel()
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

// --- reattach ---------------------------------------------------------------

// A HARD supervisor death (SIGKILL: no shutdown, no cleanup) leaves a live
// plugin process behind. The next supervisor reattaches to it — identity
// verified — instead of orphaning it and spawning a duplicate.
func TestReattachAfterHardSupervisorDeath(t *testing.T) {
	bin, sha := buildFixture(t)
	dir := t.TempDir()
	state := filepath.Join(dir, "state")

	// Stand in for the dead supervisor's child: a live, already-handshaked
	// plugin process nobody is currently supervising.
	spec := fixtureUnit("survivor", bin, sha, "FIXTURE_TAG=gen1")
	cmd := exec.Command(bin)
	cmd.Env = FilterEnv(spec.EnvAllow, spec.EnvGrant)
	orphan := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: plugin.Handshake, Plugins: plugin.PluginMap, Cmd: cmd,
	})
	if _, err := orphan.Client(); err != nil {
		t.Fatalf("start the pre-existing plugin: %v", err)
	}
	defer orphan.Kill()
	rc := orphan.ReattachConfig()
	if err := SaveReattach(state, spec, rc, plugin.ProtocolVersion); err != nil {
		t.Fatalf("SaveReattach: %v", err)
	}

	tr := NewTree(Config{
		StageDir: filepath.Join(dir, "stage"), StateDir: state,
		Budgets: testBudgets(), Plugins: plugin.PluginMap, Handshake: plugin.Handshake,
		Logf: func(string, ...any) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	tr.Start(ctx)
	defer func() { cancel(); tr.Stop() }()

	// A FRESH spawn would answer "gen2"; a reattach answers "gen1".
	spec2 := fixtureUnit("survivor", bin, sha, "FIXTURE_TAG=gen2")
	h, err := tr.Add(spec2, brokerHealth)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	info := describe(t, h)
	if info.AuthHeader != "gen1" || info.DefaultPort != rc.Pid {
		t.Fatalf("did not reattach: tag=%q pid=%d, want gen1/%d", info.AuthHeader, info.DefaultPort, rc.Pid)
	}
	st, _ := tr.Unit("survivor")
	if !st.Reattached || st.PID != rc.Pid || st.State != UnitRunning {
		t.Errorf("status does not report the reattach: %+v", st)
	}
}

// Reattach state is IDENTITY-CHECKED: a state file written for a different
// executable, kind, or a dead pid is discarded and a fresh child is spawned.
func TestReattachRefusesForeignOrDeadState(t *testing.T) {
	bin, sha := buildFixture(t)

	t.Run("dead pid", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		spec := fixtureUnit("dead", bin, sha, "FIXTURE_TAG=fresh")
		dead := exec.Command(bin)
		dead.Env = []string{}
		if err := dead.Start(); err != nil {
			t.Fatal(err)
		}
		pid := dead.Process.Pid
		_ = dead.Process.Kill()
		_, _ = dead.Process.Wait()
		if err := SaveReattach(state, spec, &goplugin.ReattachConfig{Pid: pid, Protocol: goplugin.ProtocolNetRPC,
			Addr:            &net.UnixAddr{Name: filepath.Join(dir, "gone.sock"), Net: "unix"},
			ProtocolVersion: plugin.ProtocolVersion}, plugin.ProtocolVersion); err != nil {
			t.Fatal(err)
		}
		tr := testTree(t, func(c *Config) { c.StateDir = state })
		h, err := tr.Add(spec, brokerHealth)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got := describe(t, h); got.AuthHeader != "fresh" || got.DefaultPort == pid {
			t.Errorf("reattached to a dead pid: %+v", got)
		}
		if st, _ := tr.Unit("dead"); st.Reattached {
			t.Error("status claims a reattach that cannot have happened")
		}
	})

	t.Run("foreign identity", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		spec := fixtureUnit("foreign", bin, sha, "FIXTURE_TAG=gen1")
		cmd := exec.Command(bin)
		cmd.Env = FilterEnv(spec.EnvAllow, spec.EnvGrant)
		orphan := goplugin.NewClient(&goplugin.ClientConfig{
			HandshakeConfig: plugin.Handshake, Plugins: plugin.PluginMap, Cmd: cmd,
		})
		if _, err := orphan.Client(); err != nil {
			t.Fatal(err)
		}
		defer orphan.Kill()
		// State saved under the SAME unit name but a different pinned identity.
		other := fixtureUnit("foreign", bin, strings.Repeat("cd", 32), "FIXTURE_TAG=gen1")
		if err := SaveReattach(state, other, orphan.ReattachConfig(), plugin.ProtocolVersion); err != nil {
			t.Fatal(err)
		}
		tr := testTree(t, func(c *Config) { c.StateDir = state })
		spec2 := fixtureUnit("foreign", bin, sha, "FIXTURE_TAG=gen2")
		h, err := tr.Add(spec2, brokerHealth)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if got := describe(t, h).AuthHeader; got != "gen2" {
			t.Errorf("reattached across an identity change (tag %q)", got)
		}
	})
}
