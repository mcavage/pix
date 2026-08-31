package provision

import (
	"errors"
	"strings"
	"testing"

	"pix/host/container"
	"pix/host/envinfo"
	"pix/host/pixhome"
	"pix/host/release"
)

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
	mcp := &fakeMCP{registered: map[string]string{envinfo.MCPMemoryName: "http://127.0.0.1:9999/mcp"}}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + strings.Repeat("b", 64), HostPort: 18080, DataDir: home.StateMemory}

	res, err := Setup(Deps{Home: home, ContainerRunner: docker, Prober: fakeProber{}, ContainerSpec: spec, MCP: mcp})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !res.MCPRegistered || res.MCPState != MCPRegistrationPresent {
		t.Fatalf("expected registered=true state=present, got %+v", res)
	}
	if mcp.registered[envinfo.MCPMemoryName] != "http://127.0.0.1:9999/mcp" {
		t.Fatal("Setup must never overwrite an existing mismatched registration")
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
	if got := mcp.registered[envinfo.MCPMemoryName]; got != want {
		t.Fatalf("registered MCP URL = %q, want %q", got, want)
	}
}

func TestReservedMCPNameMatchesContainerName(t *testing.T) {
	// container.Name is the docker adapter's name; envinfo.MCPMemoryName is
	// the reserved MCP server name Setup registers it under. They must be
	// the same literal string, or setup's registration and doctor's probe
	// would silently drift apart.
	if envinfo.MCPMemoryName != container.Name {
		t.Fatalf("envinfo.MCPMemoryName = %q, container.Name = %q", envinfo.MCPMemoryName, container.Name)
	}
}
