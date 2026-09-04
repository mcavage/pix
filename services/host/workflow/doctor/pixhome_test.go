package doctor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"pix/host/container"
	"pix/host/health"
	"pix/host/release"
)

type fakeExecOK struct{}

func (fakeExecOK) Check(name string, args ...string) (string, error) {
	return name + " ok", nil
}

type fakeContainerRunnerAbsent struct{}

func (fakeContainerRunnerAbsent) Run(args ...string) (string, error) {
	return "Error: No such object", errors.New("exit 1")
}

func TestCheckHome_AllGapsNamedWhenNothingSetUp(t *testing.T) {
	home := t.TempDir()
	snap := CheckHome(context.Background(), HomeDeps{
		Home:            home,
		Exec:            fakeExecOK{},
		ContainerRunner: fakeContainerRunnerAbsent{},
		ContainerSpec:   container.Spec{ContainerName: "pix-memory-test", HostPort: 18080},
		MCPLister:       nil,
		MCPServerName:   "pix-memory",
	}, 0)
	if snap.Ready() {
		t.Fatal("expected NOT ready on a completely unset-up home")
	}
	for _, name := range []string{"pix-release", "pix-memory"} {
		res, ok := snap.Find(name)
		if !ok {
			t.Fatalf("missing probe %q", name)
		}
		if res.Fix == "" {
			t.Errorf("probe %q has no fix", name)
		}
	}
	// docker/git are ready via fakeExecOK
	for _, name := range []string{"docker", "git"} {
		res, ok := snap.Find(name)
		if !ok || res.Status != health.StatusReady {
			t.Errorf("probe %q = %+v, want ready", name, res)
		}
	}
	// It must never mutate anything under home.
	entries := mustReadDir(t, home)
	if len(entries) != 0 {
		t.Fatalf("CheckHome must not write to PIX_HOME; found %v", entries)
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCheckHome_ReadyWhenFullyProvisioned(t *testing.T) {
	home := t.TempDir()
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := release.SaveInstalled(home, release.Manifest{Version: "2.0.0", PixAgentDigest: digest, PixMemoryDigest: digest, RuntimeDigest: digest, KitRevision: "k1"}); err != nil {
		t.Fatal(err)
	}
	spec := container.Spec{ContainerName: "pix-memory-test", Image: digest, HostPort: 18080, DataDir: home + "/state/memory"}
	runner := fakeContainerRunnerHealthy{spec: spec}
	snap := CheckHome(context.Background(), HomeDeps{
		Home: home, Exec: fakeExecOK{},
		ContainerRunner: runner, ContainerSpec: spec, Prober: fakeProberOK{},
		MCPLister:     fakeMCPListerOK{name: "pix-memory", url: "http://127.0.0.1:18080/mcp"},
		MCPServerName: "pix-memory", MCPExpectedURL: "http://127.0.0.1:18080/mcp",
	}, 0)
	if !snap.Ready() {
		for _, r := range snap.Gaps() {
			t.Logf("gap: %+v", r)
		}
		t.Fatal("expected Ready()")
	}
}

type fakeContainerRunnerHealthy struct{ spec container.Spec }

func (f fakeContainerRunnerHealthy) Run(args ...string) (string, error) {
	if len(args) > 0 && args[0] == "inspect" {
		return `[{"Id":"x","Config":{"Image":"` + f.spec.Image + `","Labels":{"pix.managed":"true","pix.fingerprint":"` + f.spec.Fingerprint() + `"}},"State":{"Running":true}}]`, nil
	}
	return "", nil
}

type fakeProberOK struct{}

func (fakeProberOK) Probe(string) error { return nil }

type fakeMCPListerOK struct{ name, url string }

func (f fakeMCPListerOK) ListMCP() (map[string]string, error) {
	return map[string]string{f.name: f.url}, nil
}
