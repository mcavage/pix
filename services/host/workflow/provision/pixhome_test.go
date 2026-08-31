package provision

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/stack"
)

// wantMemoryMCPName returns the exact scoped pix-memory MCP name Setup must
// register home under — the same derivation Setup itself uses (stack.ID,
// then stack.MCPMemoryName), so a test never independently guesses at a
// name Setup could legitimately compute differently.
func wantMemoryMCPName(t *testing.T, home pixhome.Paths) string {
	t.Helper()
	id, err := stack.ID(home.Home)
	if err != nil {
		t.Fatalf("stack.ID: %v", err)
	}
	name, err := stack.MCPMemoryName(id)
	if err != nil {
		t.Fatalf("stack.MCPMemoryName: %v", err)
	}
	return name
}

type fakeGitRunner struct{ ran bool }

func (f *fakeGitRunner) Run(dir, name string, args ...string) (string, error) {
	f.ran = true
	return "", nil
}

type fakeDockerRunner struct {
	inspectOut string
	inspectErr error
	calls      []string
}

func (f *fakeDockerRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "inspect":
		return f.inspectOut, f.inspectErr
	default:
		return "id123", nil
	}
}

type fakeProber struct{}

func (fakeProber) Probe(string) error { return nil }

type fakeMCP struct {
	registered map[string]string
	calls      int
}

func (f *fakeMCP) EnsureMemoryRemote(name, url string) (MCPRegistrationState, error) {
	f.calls++
	if f.registered == nil {
		f.registered = map[string]string{}
	}
	if _, ok := f.registered[name]; !ok {
		f.registered[name] = url
		return MCPRegistrationAdded, nil
	}
	// Already registered: left untouched, and reported as an unverifiable
	// PRESENCE — never as a match (round-4 review).
	return MCPRegistrationPresent, nil
}

func testManifest() release.Manifest {
	digest := "sha256:" + strings.Repeat("a", 64)
	return release.Manifest{Version: "2.0.0", PixAgentDigest: digest, PixMemoryDigest: digest, RuntimeDigest: digest, KitRevision: "k1"}
}

func TestSetup_FromScratch(t *testing.T) {
	home := pixhome.New(t.TempDir() + "/pixhome")
	git := &fakeGitRunner{}
	docker := &fakeDockerRunner{inspectErr: errors.New("exit 1"), inspectOut: "Error: No such object"}
	mcp := &fakeMCP{}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), HostPort: 18080, DataDir: home.StateMemory}

	res, err := Setup(Deps{
		Home:            home,
		GitRunner:       git,
		ContainerRunner: docker,
		Prober:          fakeProber{},
		Manifest:        testManifest(),
		ContainerSpec:   spec,
		MCP:             mcp,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !res.ReleaseInstalled {
		t.Errorf("expected ReleaseInstalled=true")
	}
	if res.Container.Action != container.ActionCreated {
		t.Errorf("Container.Action = %s, want created", res.Container.Action)
	}
	if !res.MCPRegistered || res.MCPState != MCPRegistrationAdded {
		t.Errorf("MCPRegistered/MCPState = %v/%v, want true/added", res.MCPRegistered, res.MCPState)
	}
	if !res.Ready() {
		t.Errorf("Ready() = false, want true")
	}

	// Idempotent rerun.
	res2, err := Setup(Deps{
		Home: home, GitRunner: git, ContainerRunner: docker, Prober: fakeProber{},
		Manifest: testManifest(), ContainerSpec: spec, MCP: mcp,
	})
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if res2.Init.CreatedHome {
		t.Errorf("second run should not recreate the home")
	}

	// Recorded manifest is readable back.
	m, err := release.LoadInstalled(home.Home)
	if err != nil || m == nil {
		t.Fatalf("LoadInstalled: %v, %v", m, err)
	}
}

func TestSetup_MCPMismatchIsReportedNotOverwritten(t *testing.T) {
	home := pixhome.New(t.TempDir())
	docker := &fakeDockerRunner{inspectErr: errors.New("exit 1"), inspectOut: "Error: No such object"}
	memoryName := wantMemoryMCPName(t, home)
	mcp := &fakeMCP{registered: map[string]string{memoryName: "http://127.0.0.1:9999/mcp"}}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), HostPort: 18080, DataDir: home.StateMemory}

	res, err := Setup(Deps{Home: home, ContainerRunner: docker, Prober: fakeProber{}, ContainerSpec: spec, MCP: mcp})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !res.MCPRegistered || res.MCPState != MCPRegistrationPresent {
		t.Fatalf("expected registered=true state=present, got %+v", res)
	}
	if mcp.registered[memoryName] != "http://127.0.0.1:9999/mcp" {
		t.Fatal("Setup must never overwrite an existing mismatched registration")
	}
}

// TestSetup_RegistersScopedMemoryMCPName is the TDD anchor for Wave B U4:
// two DIFFERENT PIX_HOME homes must register two DIFFERENT scoped names,
// never the bare legacy "pix-memory".
func TestSetup_RegistersScopedMemoryMCPName(t *testing.T) {
	homeA := pixhome.New(t.TempDir())
	homeB := pixhome.New(t.TempDir())
	nameA := wantMemoryMCPName(t, homeA)
	nameB := wantMemoryMCPName(t, homeB)
	if nameA == nameB {
		t.Fatalf("two different PIX_HOME homes must derive different scoped MCP names, both got %q", nameA)
	}
	if nameA == "pix-memory" || nameB == "pix-memory" {
		t.Fatalf("a scoped name must never equal the bare legacy name: %q, %q", nameA, nameB)
	}

	docker := &fakeDockerRunner{inspectErr: errors.New("exit 1"), inspectOut: "Error: No such object"}
	mcp := &fakeMCP{}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), HostPort: 18080, DataDir: homeA.StateMemory}
	res, err := Setup(Deps{Home: homeA, ContainerRunner: docker, Prober: fakeProber{}, ContainerSpec: spec, MCP: mcp})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !res.MCPRegistered || res.MCPState != MCPRegistrationAdded {
		t.Fatalf("expected a fresh registration, got %+v", res)
	}
	if _, ok := mcp.registered[nameA]; !ok {
		t.Fatalf("Setup did not register under the scoped name %q; registered = %v", nameA, mcp.registered)
	}
	if _, ok := mcp.registered["pix-memory"]; ok {
		t.Fatal("Setup must never register under the bare legacy name")
	}
}

// TestSetup_LegacyBareRegistrationDoesNotSatisfyReadiness proves a stale
// registration under the OLD bare "pix-memory" name (from before per-stack
// scoping) is irrelevant to this run's readiness: Setup must still register
// (or find registered) its OWN scoped name, never treat an unrelated bare
// name as already covering it.
func TestSetup_LegacyBareRegistrationDoesNotSatisfyReadiness(t *testing.T) {
	home := pixhome.New(t.TempDir())
	docker := &fakeDockerRunner{inspectErr: errors.New("exit 1"), inspectOut: "Error: No such object"}
	mcp := &fakeMCP{registered: map[string]string{"pix-memory": "http://127.0.0.1:9999/mcp"}}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), HostPort: 18080, DataDir: home.StateMemory}

	res, err := Setup(Deps{Home: home, ContainerRunner: docker, Prober: fakeProber{}, ContainerSpec: spec, MCP: mcp})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if res.MCPState != MCPRegistrationAdded {
		t.Fatalf("a legacy bare registration must not be mistaken for this stack's own; MCPState = %v, want Added", res.MCPState)
	}
	if !res.Ready() {
		t.Fatal("Ready() = false after this stack's own scoped name was freshly registered")
	}
}

func TestSetup_SkipsReleaseWhenManifestEmpty(t *testing.T) {
	home := pixhome.New(t.TempDir())
	docker := &fakeDockerRunner{inspectErr: errors.New("exit 1"), inspectOut: "Error: No such object"}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), HostPort: 18080, DataDir: home.StateMemory}

	res, err := Setup(Deps{Home: home, ContainerRunner: docker, Prober: fakeProber{}, ContainerSpec: spec})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if res.ReleaseInstalled {
		t.Fatal("ReleaseInstalled must be false when no manifest was given")
	}
	m, err := release.LoadInstalled(home.Home)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatal("no manifest should have been written")
	}
}

func TestMemoryMCPURL(t *testing.T) {
	got := container.MemoryMCPURL(container.Spec{HostPort: 18080}, "")
	if got != "http://127.0.0.1:18080/mcp" {
		t.Fatalf("got %q", got)
	}
}

func TestSetup_RegistersMemoryURLWithToken(t *testing.T) {
	home := pixhome.New(t.TempDir())
	docker := &fakeDockerRunner{inspectErr: errors.New("exit 1"), inspectOut: "Error: No such object"}
	mcp := &fakeMCP{}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), HostPort: 18080, DataDir: home.StateMemory}

	_, err := Setup(Deps{
		Home: home, ContainerRunner: docker, Prober: fakeProber{},
		ContainerSpec: spec, MCP: mcp,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// Setup generates the token itself, INSIDE the setup lock, so the URL it
	// registers must carry exactly the token now persisted under PIX_HOME —
	// never one the caller passed in (there is no such parameter any more).
	tok, terr := container.ReadMemoryAuthToken(home)
	if terr != nil || tok == "" {
		t.Fatalf("ReadMemoryAuthToken = (%q, %v), want a generated token", tok, terr)
	}
	want := "http://127.0.0.1:18080/mcp?token=" + tok
	if got := mcp.registered[wantMemoryMCPName(t, home)]; got != want {
		t.Fatalf("registered MCP URL = %q, want %q", got, want)
	}
}

// TestReservedMCPNameMatchesContainerName pins the invariant
// TestReservedMCPNameMatchesContainerName used to check on the bare legacy
// names directly: stack.MCPMemoryName and stack.MemoryContainerName MUST
// always agree on the exact literal for the SAME stack id, or Setup's own
// registration and doctor's probe of the container would silently drift
// apart the moment either one's naming grammar changed independently.
func TestReservedMCPNameMatchesContainerName(t *testing.T) {
	const id = "0123456789abcdef"
	mcpName, err := stack.MCPMemoryName(id)
	if err != nil {
		t.Fatalf("stack.MCPMemoryName: %v", err)
	}
	containerName, err := stack.MemoryContainerName(id)
	if err != nil {
		t.Fatalf("stack.MemoryContainerName: %v", err)
	}
	if mcpName != containerName {
		t.Fatalf("stack.MCPMemoryName(%s) = %q, stack.MemoryContainerName(%s) = %q", id, mcpName, id, containerName)
	}
}

// flakyPortDockerRunner simulates the EXACT race QA F4/round-5 target:
// `docker create` succeeds, but the FIRST `docker start` loses a bind race
// on failPort (something else grabbed it between PortAvailable's pre-check
// and the real bind) — every subsequent start succeeds, as it would once
// Setup's retry loop has reallocated onto a genuinely free port.
type flakyPortDockerRunner struct {
	failPort   int
	startCalls int
	calls      [][]string
}

func (f *flakyPortDockerRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return "", errors.New("no subcommand")
	}
	switch args[0] {
	case "inspect":
		return "Error: No such object", errors.New("exit 1")
	case "create":
		return "flaky-container-id", nil
	case "start":
		f.startCalls++
		if f.startCalls == 1 {
			return "Error response from daemon: driver failed programming external " +
				"connectivity on endpoint pix-memory: Bind for 0.0.0.0:" +
				strconvItoa(f.failPort) + " failed: port is already allocated", errors.New("exit 1")
		}
		return "", nil
	case "rm":
		return "", nil
	default:
		return "", nil
	}
}

func strconvItoa(n int) string {
	// Tiny local copy so this test file needs no extra import beyond what
	// it already has; avoids pulling strconv in just for one call site.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// TestSetup_PortConflictFirstFailsSecondSucceeds_PersistsPortAndMCPURL pins
// round 5's provision-level retry proof: when the FIRST reconcile attempt
// loses a bind race on the persisted port, Setup must reallocate under the
// config lock, retry, and land on a container/registration/persisted-config
// triple that all name the SAME final port — never the stale conflicting
// one, and never a mismatch between what Docker is running, what
// config.toml records, and what the Gateway URL says.
func TestSetup_PortConflictFirstFailsSecondSucceeds_PersistsPortAndMCPURL(t *testing.T) {
	home := pixhome.New(t.TempDir())
	const conflictingPort = 18099
	if err := config.SaveTo(config.PathAt(home.Home), &config.Config{MemoryPort: conflictingPort}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	docker := &flakyPortDockerRunner{failPort: conflictingPort}
	mcp := &fakeMCP{}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), DataDir: home.StateMemory}

	res, err := Setup(Deps{
		Home: home, ContainerRunner: docker, Prober: fakeProber{},
		ContainerSpec: spec, MCP: mcp,
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if res.MemoryPort == conflictingPort {
		t.Fatalf("expected reallocation away from the conflicting port %d, got the same value", conflictingPort)
	}
	if res.Container.Action != container.ActionCreated {
		t.Errorf("Container.Action = %s, want created", res.Container.Action)
	}
	if docker.startCalls < 2 {
		t.Fatalf("expected first-fails/second-succeeds (>=2 start attempts), got %d", docker.startCalls)
	}

	// Persisted config.toml must agree with what Setup reports resolved.
	c, err := config.LoadFrom(config.PathAt(home.Home))
	if err != nil {
		t.Fatal(err)
	}
	if c.MemoryPort != res.MemoryPort {
		t.Errorf("persisted MemoryPort = %d, want %d (config.toml must never disagree with the container that actually started)", c.MemoryPort, res.MemoryPort)
	}

	// The registered Gateway URL must name the SAME final port — never the
	// stale conflicting one, and never a different port than what actually
	// started (the token query param varies run to run, so this checks the
	// host:port/path prefix rather than an exact string).
	memoryName := wantMemoryMCPName(t, home)
	wantPrefix := fmt.Sprintf("http://127.0.0.1:%d/mcp", res.MemoryPort)
	if got := mcp.registered[memoryName]; !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("registered MCP URL = %q, want prefix %q", got, wantPrefix)
	}
	if strings.Contains(mcp.registered[memoryName], fmt.Sprintf(":%d/", conflictingPort)) {
		t.Errorf("registered MCP URL still names the conflicting port %d: %q", conflictingPort, mcp.registered[memoryName])
	}

	// The failed first create must have been cleaned up (by ID) rather than
	// left as an orphaned, never-started container.
	var sawRemove bool
	for _, call := range docker.calls {
		if len(call) > 0 && call[0] == "rm" {
			sawRemove = true
		}
	}
	if !sawRemove {
		t.Errorf("expected the failed create to be removed before retrying, calls: %v", docker.calls)
	}
}
