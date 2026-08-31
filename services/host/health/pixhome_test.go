package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/container"
	"pix/host/release"
)

func validManifest() release.Manifest {
	digest := "sha256:" + repeat("a", 64)
	return release.Manifest{Version: "2.0.0", PixAgentDigest: digest, PixMemoryDigest: digest, RuntimeDigest: digest, KitRevision: "k1"}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func TestReleaseInstalledProbe_AbsentWhenNoManifest(t *testing.T) {
	home := t.TempDir()
	res := ReleaseInstalledProbe{Home: home}.Check(context.Background())
	if res.Status != StatusAbsent || res.Fix == "" {
		t.Fatalf("res = %+v", res)
	}
}

func TestReleaseInstalledProbe_ReadyWhenInstalled(t *testing.T) {
	home := t.TempDir()
	if err := release.SaveInstalled(home, validManifest()); err != nil {
		t.Fatal(err)
	}
	res := ReleaseInstalledProbe{Home: home}.Check(context.Background())
	if res.Status != StatusReady {
		t.Fatalf("res = %+v", res)
	}
}

func TestEnvironmentDefaultProbe_ReadyWhenSelected(t *testing.T) {
	res := EnvironmentDefaultProbe{Home: t.TempDir(), DefaultEnvironment: "default"}.Check(context.Background())
	if res.Status != StatusReady {
		t.Fatalf("res = %+v", res)
	}
}

func TestEnvironmentDefaultProbe_OffWhenNoEnvironmentExistsYet(t *testing.T) {
	res := EnvironmentDefaultProbe{Home: t.TempDir()}.Check(context.Background())
	if res.Status != StatusOff {
		t.Fatalf("res = %+v, want StatusOff (nothing to point a fix at yet)", res)
	}
	if res.Fix != "" {
		t.Fatalf("res.Fix = %q, want empty: StatusOff never carries a fix", res.Fix)
	}
}

func TestEnvironmentDefaultProbe_AbsentNamesEnvDefaultWhenEnvironmentsExistButNoneSelected(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "envs", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := EnvironmentDefaultProbe{Home: home}.Check(context.Background())
	if res.Status != StatusAbsent {
		t.Fatalf("res = %+v, want StatusAbsent", res)
	}
	if res.Fix != "pix env default NAME" {
		t.Fatalf("res.Fix = %q, want the exact named remedy, never a guess", res.Fix)
	}
}

type fakeExec struct {
	out map[string]string
	err map[string]error
}

func (f fakeExec) Check(name string, args ...string) (string, error) {
	return f.out[name], f.err[name]
}

func TestDockerAvailableProbe(t *testing.T) {
	ready := DockerAvailableProbe{Exec: fakeExec{out: map[string]string{"docker": "27.0.0"}}}.Check(context.Background())
	if ready.Status != StatusReady {
		t.Fatalf("ready case: %+v", ready)
	}
	absent := DockerAvailableProbe{Exec: fakeExec{err: map[string]error{"docker": errors.New("not found")}}}.Check(context.Background())
	if absent.Status != StatusAbsent || absent.Fix == "" {
		t.Fatalf("absent case: %+v", absent)
	}
}

func TestGitAvailableProbe(t *testing.T) {
	ready := GitAvailableProbe{Exec: fakeExec{out: map[string]string{"git": "git version 2.45.0"}}}.Check(context.Background())
	if ready.Status != StatusReady {
		t.Fatalf("ready case: %+v", ready)
	}
}

type fakeContainerRunner struct {
	inspectOut string
	inspectErr error
}

func (f fakeContainerRunner) Run(args ...string) (string, error) {
	if len(args) > 0 && args[0] == "inspect" {
		return f.inspectOut, f.inspectErr
	}
	return "", nil
}

type fakeProber struct{ err error }

func (f fakeProber) Probe(string) error { return f.err }

func testSpec() container.Spec {
	return container.Spec{ContainerName: "pix-memory-test", Image: "img@sha256:" + repeat("a", 64), HostPort: 18080, DataDir: "/data"}
}

func TestMemoryContainerProbe_AbsentWhenNotCreated(t *testing.T) {
	r := fakeContainerRunner{inspectErr: errors.New("exit 1"), inspectOut: "Error: No such object: x"}
	res := MemoryContainerProbe{Runner: r, Spec: testSpec()}.Check(context.Background())
	if res.Status != StatusAbsent || res.Fix != Setup2Fix {
		t.Fatalf("res = %+v", res)
	}
}

func TestMemoryContainerProbe_ReadyWhenHealthy(t *testing.T) {
	spec := testSpec()
	out := `[{"Id":"x","Config":{"Image":"` + spec.Image + `","Labels":{"pix.managed":"true","pix.fingerprint":"` + spec.Fingerprint() + `"}},"State":{"Running":true}}]`
	r := fakeContainerRunner{inspectOut: out}
	res := MemoryContainerProbe{Runner: r, Spec: spec, Prober: fakeProber{}}.Check(context.Background())
	if res.Status != StatusReady {
		t.Fatalf("res = %+v", res)
	}
}

func TestMemoryContainerProbe_UnhealthyNamesDockerRestart(t *testing.T) {
	spec := testSpec()
	out := `[{"Id":"x","Config":{"Image":"` + spec.Image + `","Labels":{"pix.managed":"true","pix.fingerprint":"` + spec.Fingerprint() + `"}},"State":{"Running":true}}]`
	r := fakeContainerRunner{inspectOut: out}
	res := MemoryContainerProbe{Runner: r, Spec: spec, Prober: fakeProber{err: errors.New("healthz refused")}}.Check(context.Background())
	if res.Status != StatusAbsent || res.Fix != "docker restart pix-memory-test" {
		t.Fatalf("res = %+v", res)
	}
}

func TestMemoryContainerProbe_DriftIsAbsentWithSetupFix(t *testing.T) {
	spec := testSpec()
	out := `[{"Id":"x","Config":{"Image":"stale@sha256:` + repeat("b", 64) + `","Labels":{"pix.managed":"true","pix.fingerprint":"sha256:stale"}},"State":{"Running":true}}]`
	r := fakeContainerRunner{inspectOut: out}
	res := MemoryContainerProbe{Runner: r, Spec: spec}.Check(context.Background())
	if res.Status != StatusAbsent || res.Fix != Setup2Fix {
		t.Fatalf("res = %+v", res)
	}
}

type fakeMCPLister struct {
	m   map[string]string
	err error
}

func (f fakeMCPLister) ListMCP() (map[string]string, error) { return f.m, f.err }

func TestMemoryMCPRegistrationProbe_ReadyOnMatch(t *testing.T) {
	res := MemoryMCPRegistrationProbe{
		Lister:      fakeMCPLister{m: map[string]string{"pix-memory": "http://127.0.0.1:18080/mcp"}},
		ServerName:  "pix-memory",
		ExpectedURL: "http://127.0.0.1:18080/mcp",
	}.Check(context.Background())
	if res.Status != StatusReady {
		t.Fatalf("res = %+v", res)
	}
}

func TestMemoryMCPRegistrationProbe_MismatchNamesSetup(t *testing.T) {
	res := MemoryMCPRegistrationProbe{
		Lister:      fakeMCPLister{m: map[string]string{"pix-memory": "http://127.0.0.1:9999/mcp"}},
		ServerName:  "pix-memory",
		ExpectedURL: "http://127.0.0.1:18080/mcp",
	}.Check(context.Background())
	if res.Status != StatusAbsent || res.Fix != Setup2Fix {
		t.Fatalf("res = %+v", res)
	}
}

func TestMemoryMCPRegistrationProbe_AbsentWhenUnregistered(t *testing.T) {
	res := MemoryMCPRegistrationProbe{Lister: fakeMCPLister{m: map[string]string{}}, ServerName: "pix-memory"}.Check(context.Background())
	if res.Status != StatusAbsent || res.Fix != Setup2Fix {
		t.Fatalf("res = %+v", res)
	}
}
