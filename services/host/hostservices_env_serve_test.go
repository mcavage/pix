// hostservices_env_serve_test.go — E2.7's serve-side integration (P0-11,
// AC-40): environmentServiceSnapshots' real sidecar-read + liveness-probe
// composition, environmentHostServiceViews' Probe->Health conversion, and
// the architecture sentinel proving this union lives in
// workflow/launch/hostservices_env.go + serve.go, never pack_units.go.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/packinfo"
	"pix/host/sys/systest"
	"pix/host/workflow/launch"
)

// runningSbxFor reports exactly one RUNNING, schema-verified row for name —
// the same minimal fixture workflow/launch's own EnvironmentHolders tests
// use (envlaunch_test.go), reproduced here because this file's package
// cannot import that _test.go helper.
func runningSbxFor(name string) hostenv.Env {
	row := `[{"name":"` + name + `","state":"running","instance_id":"inst-1"}]`
	return hostenv.Env{System: &systest.Fake{
		RunFn: func(bin string, args ...string) (string, error) { return row, nil },
		RunWithinFn: func(_ time.Duration, bin string, args ...string) (string, bool, error) {
			return row, false, nil
		},
	}}
}

func unreadableSbxFor() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		RunFn: func(string, ...string) (string, error) { return "", os.ErrPermission },
		RunWithinFn: func(time.Duration, string, ...string) (string, bool, error) {
			return "", false, os.ErrPermission
		},
	}}
}

// environmentServiceSnapshots reads a registered environment's OPTIONAL
// pix.toml for its declared [[host.services]] (none declared for an
// environment with no sidecar, never an error) and probes real liveness
// through launch.EnvironmentHolders.
func TestEnvironmentServiceSnapshots_ReadsSidecarAndLiveness(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	liveRoot := t.TempDir()
	writePixToml(t, liveRoot, `schema = 1

[[host.services]]
name = "work-proxy"
command = "work-proxy"
port = 19002
probe = "http://127.0.0.1:19002/health"
`)
	quietRoot := t.TempDir() // no pix.toml at all

	sessionKey := "pix-work-abc123"
	if err := launch.RecordSessionEnvironment(sessionKey, launch.SessionEnvironment{
		Name: "work", Root: liveRoot, SandboxName: sessionKey,
	}); err != nil {
		t.Fatalf("RecordSessionEnvironment: %v", err)
	}

	cfg := &config.Config{Environments: map[string]string{"work": liveRoot, "home": quietRoot}}
	snaps, err := environmentServiceSnapshots(cfg, runningSbxFor(sessionKey))
	if err != nil {
		t.Fatalf("environmentServiceSnapshots: %v", err)
	}
	byName := map[string]launch.EnvironmentServiceSnapshot{}
	for _, s := range snaps {
		byName[s.Name] = s
	}
	work, ok := byName["work"]
	if !ok {
		t.Fatalf("no snapshot for work, got %+v", snaps)
	}
	if !work.Live || work.Unknown {
		t.Errorf("work snapshot = %+v, want Live=true Unknown=false", work)
	}
	if len(work.Services) != 1 || work.Services[0].Name != "work-proxy" {
		t.Errorf("work services = %+v, want one work-proxy entry", work.Services)
	}
	home, ok := byName["home"]
	if !ok {
		t.Fatalf("no snapshot for home, got %+v", snaps)
	}
	if len(home.Services) != 0 {
		t.Errorf("home (no pix.toml) services = %+v, want none", home.Services)
	}
}

// An untrusted `sbx ls` marks the environment Unknown, never a guessed
// Live=false — the fail-closed contract EnvironmentHolders already has,
// carried through this file's own composition.
func TestEnvironmentServiceSnapshots_UnreadableListingIsUnknown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	writePixToml(t, root, `schema = 1

[[host.services]]
name = "work-proxy"
command = "work-proxy"
port = 19002
probe = "tcp"
`)
	// A recorded session referencing this root is what makes EnvironmentHolders
	// consult the (here, unreadable) sbx listing at all: with no recorded
	// session, it answers unheld without ever probing sbx.
	sessionKey := "pix-work-unreadable"
	if err := launch.RecordSessionEnvironment(sessionKey, launch.SessionEnvironment{
		Name: "work", Root: root, SandboxName: sessionKey,
	}); err != nil {
		t.Fatalf("RecordSessionEnvironment: %v", err)
	}
	cfg := &config.Config{Environments: map[string]string{"work": root}}
	snaps, err := environmentServiceSnapshots(cfg, unreadableSbxFor())
	if err != nil {
		t.Fatalf("environmentServiceSnapshots: %v", err)
	}
	if len(snaps) != 1 || !snaps[0].Unknown || snaps[0].Live {
		t.Fatalf("got %+v, want exactly one Unknown=true Live=false snapshot", snaps)
	}
}

func writePixToml(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "pix.toml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write pix.toml: %v", err)
	}
}

// environmentServiceHealth: "tcp" passes through; a loopback http(s) URL
// reduces to its path+query; a non-loopback host, an unparsable URL, or an
// empty probe all refuse.
func TestEnvironmentServiceHealth(t *testing.T) {
	cases := []struct {
		probe  string
		want   string
		wantOK bool
	}{
		{"tcp", "tcp", true},
		{"http://127.0.0.1:19002/health", "/health", true},
		{"http://localhost:19002/health?x=1", "/health?x=1", true},
		{"http://127.0.0.1:19002", "/", true},
		{"http://example.com/health", "", false},
		{"not a url \x7f", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := environmentServiceHealth(tc.probe)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("environmentServiceHealth(%q) = (%q, %v), want (%q, %v)", tc.probe, got, ok, tc.want, tc.wantOK)
		}
	}
}

// environmentHostServiceViews converts a desired want into the daemon-shaped
// pack.AcceptedService reconcileDaemons consumes, and skips (reporting,
// never silently) a want whose probe cannot be read as a health check.
func TestEnvironmentHostServiceViews(t *testing.T) {
	wanted := []launch.HostServiceWant{
		{Environment: "work", Service: hostServiceFixture("work-proxy", "work-proxy", []string{"serve"}, 19002, "http://127.0.0.1:19002/health")},
		{Environment: "home", Service: hostServiceFixture("bad-proxy", "bad-proxy", nil, 19003, "http://example.com/health")},
	}
	views, err := environmentHostServiceViews(wanted)
	if err == nil {
		t.Fatal("want an error naming the unsupervisable probe, got nil")
	}
	if !strings.Contains(err.Error(), "bad-proxy") {
		t.Errorf("error %q does not name the skipped service", err.Error())
	}
	if len(views) != 1 {
		t.Fatalf("views = %+v, want exactly the one convertible entry", views)
	}
	v := views[0]
	if v.Name != "work-proxy" || v.Command != "work-proxy" || v.Runtime != packinfo.ServiceRuntimeDaemon ||
		v.Activation != "always" || v.Port != 19002 || v.Health != "/health" {
		t.Errorf("view = %+v, want the work-proxy daemon shape", v)
	}
}

func hostServiceFixture(name, command string, args []string, port int64, probe string) envinfo.HostService {
	return envinfo.HostService{Name: name, Command: command, Args: args, Port: port, Probe: probe}
}

// TestHostServicesEnv_ArchitectureSentinel is architect correction C8, made
// executable: pack_units.go (deleted outright by E5.2) must carry NONE of
// E2.7's desired-set union, and serve.go must actually CALL it — not carry
// a dead helper nobody wires in.
func TestHostServicesEnv_ArchitectureSentinel(t *testing.T) {
	packUnits, err := os.ReadFile("pack_units.go")
	if err != nil {
		t.Fatalf("read pack_units.go: %v", err)
	}
	for _, forbidden := range []string{"DesiredHostServices", "hostservices_env", "EnvironmentServiceSnapshot", `"pix/host/workflow/launch"`} {
		if strings.Contains(string(packUnits), forbidden) {
			t.Errorf("pack_units.go references %q; E2.7's desired-set union must live only in "+
				"workflow/launch/hostservices_env.go + serve.go (architect correction C8), "+
				"never in the file E5.2 deletes outright", forbidden)
		}
	}

	serveSrc, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatalf("read serve.go: %v", err)
	}
	for _, want := range []string{"reconcileEnvironmentServices", "launch.DesiredHostServices"} {
		if !strings.Contains(string(serveSrc), want) {
			t.Errorf("serve.go does not reference %q; E2.7's desired-set union must be INTEGRATED into "+
				"real serve supervision, not a dead helper", want)
		}
	}
	if !strings.Contains(string(serveSrc), "reconcileEnvironmentServices(cfg, sup, selfPath)") {
		t.Error("serve.go's runServe does not call reconcileEnvironmentServices; the union is computed but never wired into the startup sequence")
	}
}
