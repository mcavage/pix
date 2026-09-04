package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/launcher"
	"pix/host/release"
	"pix/host/workflow/launch"
	"pix/host/workflow/provision"
)

// upgrade_auto_test.go — `pix run`'s automatic post-upgrade reconcile, at
// the level a user actually hits it: an upgraded binary whose bundle no
// longer matches what this PIX_HOME installed. The doubles are the SAME
// ones setup_release_test.go drives the real `pix setup` body with, so what
// is compared here is the two commands' shared composition (machineSetup),
// not a re-implementation of it.

func autoUpgradeDeps() (*cli.Deps, *bytes.Buffer) {
	var errb bytes.Buffer
	return &cli.Deps{Out: &bytes.Buffer{}, Err: &errb}, &errb
}

// recordInstalled puts a release record in place, standing in for whatever
// the last `pix setup` (or the last automatic reconcile) left behind.
func recordInstalled(t *testing.T, home string, m release.Manifest) {
	t.Helper()
	if err := release.SaveInstalled(home, m); err != nil {
		t.Fatal(err)
	}
}

// TestAutoReconcile_SameVersionHasNoEffects is the steady state, and the
// cost this feature adds to every ordinary run: a manifest read and a
// compare. No Docker, no Gateway, no output.
func TestAutoReconcile_SameVersionHasNoEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, m := fakeInstallDir(t, "2.0.0")
	recordInstalled(t, home, m)
	docker := &setupFakeDocker{}
	mcp := &setupFakeMCP{}
	d, errb := autoUpgradeDeps()

	if err := autoReconcileRelease(d, setupSeamsFor(t, dir, docker, mcp)); err != nil {
		t.Fatalf("autoReconcileRelease: %v", err)
	}
	if len(docker.calls) != 0 {
		t.Fatalf("a matching manifest must touch no Docker; calls: %v", docker.calls)
	}
	if mcp.url != "" {
		t.Fatal("a matching manifest must touch no Gateway registration")
	}
	if errb.Len() != 0 {
		t.Fatalf("a matching manifest must print nothing; got %q", errb.String())
	}
}

// TestAutoReconcile_NoInstalledManifestDoesNotBootstrap keeps first run
// explicit: a machine that never ran `pix setup` is not provisioned as a
// side effect of typing `pix`.
func TestAutoReconcile_NoInstalledManifestDoesNotBootstrap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, _ := fakeInstallDir(t, "2.0.0")
	docker := &setupFakeDocker{}
	mcp := &setupFakeMCP{}
	d, errb := autoUpgradeDeps()

	if err := autoReconcileRelease(d, setupSeamsFor(t, dir, docker, mcp)); err != nil {
		t.Fatalf("autoReconcileRelease: %v", err)
	}
	if len(docker.calls) != 0 || mcp.url != "" {
		t.Fatalf("first run must not auto-provision; docker=%v mcp=%q", docker.calls, mcp.url)
	}
	if got, _ := release.LoadInstalled(home); got != nil {
		t.Fatalf("first run must record no release manifest; got %+v", *got)
	}
	if errb.Len() != 0 {
		t.Fatalf("first run must print no upgrade line; got %q", errb.String())
	}
}

// TestAutoReconcile_UpgradeRunsOnceThenIsANoOp is the user's actual
// complaint: after an upgrade, the FIRST ordinary run reconciles the
// machine-owned artifacts, and every run after that does nothing.
func TestAutoReconcile_UpgradeRunsOnceThenIsANoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	_, old := fakeInstallDir(t, "1.0.0")
	recordInstalled(t, home, old)
	dir, want := fakeInstallDir(t, "2.0.0")
	docker := &setupFakeDocker{}
	mcp := &setupFakeMCP{}
	d, errb := autoUpgradeDeps()
	seams := setupSeamsFor(t, dir, docker, mcp)

	if err := autoReconcileRelease(d, seams); err != nil {
		t.Fatalf("autoReconcileRelease: %v\n%s", err, errb.String())
	}
	got, err := release.LoadInstalled(home)
	if err != nil || got == nil || *got != want {
		t.Fatalf("recorded manifest = %v (err %v), want %+v", got, err, want)
	}
	if !strings.Contains(strings.Join(docker.calls, "\n"), provision.MemoryImageRef(want)) {
		t.Fatalf("the upgrade never reconciled against the new pinned image; calls:\n%s", strings.Join(docker.calls, "\n"))
	}
	if _, serr := os.Stat(filepath.Join(release.RuntimeDir(home, "2.0.0"), "agents/deep.md")); serr != nil {
		t.Fatalf("the upgrade did not install the new runtime: %v", serr)
	}
	if !strings.Contains(errb.String(), "upgraded to 2.0.0") {
		t.Fatalf("stderr = %q, want one concise upgrade line", errb.String())
	}

	before := len(docker.calls)
	errb.Reset()
	if err := autoReconcileRelease(d, seams); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if len(docker.calls) != before {
		t.Fatalf("the second run reconciled again; extra calls: %v", docker.calls[before:])
	}
	if errb.Len() != 0 {
		t.Fatalf("the second run printed %q, want silence", errb.String())
	}
}

// TestAutoReconcile_NeverSolicitsCredentialsOrRunsHooks pins the boundary
// between what an upgrade may do unasked (machine-owned artifacts) and what
// only an explicit `pix setup` may do: credentials, environment trust, and
// `[[setup]]` hooks.
func TestAutoReconcile_NeverSolicitsCredentialsOrRunsHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_CONFIG", filepath.Join(home, "config.toml"))
	// An environment carrying a setup hook whose execution would be
	// observable, plus a never-reviewed host-exec surface.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	envDir := filepath.Join(home, "envs", "hooked")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := "[[setup]]\nkind = \"install\"\ncommand = \"/bin/sh\"\nargs = [\"-c\", \"touch " + marker + "\"]\n"
	if err := os.WriteFile(filepath.Join(envDir, "pix.toml"), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	_, old := fakeInstallDir(t, "1.0.0")
	recordInstalled(t, home, old)
	dir, _ := fakeInstallDir(t, "2.0.0")
	d, _ := autoUpgradeDeps()
	d.Interactive = true
	d.In = &countingReader{s: "y\n"}

	if err := autoReconcileRelease(d, setupSeamsFor(t, dir, &setupFakeDocker{}, &setupFakeMCP{})); err != nil {
		t.Fatalf("autoReconcileRelease: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an automatic upgrade executed an environment setup hook")
	}
	if _, err := os.Stat(filepath.Join(home, "secrets.env")); err == nil {
		t.Fatal("an automatic upgrade wrote the credentials file; that is `pix setup`'s own step")
	}
	if _, err := os.Stat(filepath.Join(home, "state", "trust", "environments", "hooked.json")); err == nil {
		t.Fatal("an automatic upgrade accepted environment trust")
	}
	if r := d.In.(*countingReader); r.reads > 0 {
		t.Fatalf("an automatic upgrade read stdin %d times; it must ask nothing", r.reads)
	}
}

// foreignContainerDocker reports a pix-memory container owned by ANOTHER
// stack, the one case container.Reconcile refuses before any confirmation
// is consulted.
type foreignContainerDocker struct{ calls []string }

func (f *foreignContainerDocker) Run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case args[0] == "inspect":
		return `[{"Id":"abc","State":{"Running":true},"Config":{"Image":"pix-memory:old","Labels":{"` +
			container.StackLabel + `":"0000000000000000","` + container.HomeLabel + `":"/somewhere/else"}}}]`, nil
	case args[0] == "image":
		return "[]", nil
	default:
		return "id123", nil
	}
}

// TestAutoReconcile_ForeignContainerRefusesAndRollsBack proves the
// auto-confirmation is not a widening: a container this stack does not own
// is refused inside container.Reconcile, the upgrade fails, and the
// previous release record is restored so the next run retries.
func TestAutoReconcile_ForeignContainerRefusesAndRollsBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	_, old := fakeInstallDir(t, "1.0.0")
	recordInstalled(t, home, old)
	dir, _ := fakeInstallDir(t, "2.0.0")
	docker := &foreignContainerDocker{}
	d, errb := autoUpgradeDeps()

	err := autoReconcileRelease(d, setupSeams{
		DiscoverBundle: func() (*release.Bundle, error) {
			return release.DiscoverBundle(func() (string, error) { return filepath.Join(dir, "pix"), nil })
		},
		Prereqs:         setupFakePrereqs{},
		ContainerRunner: docker,
		Prober:          setupFakeProber{},
		MCP:             &setupFakeMCP{},
	})
	if err == nil {
		t.Fatalf("a foreign-owned pix-memory container must refuse the automatic upgrade; stderr=%q", errb.String())
	}
	if strings.Contains(strings.Join(docker.calls, "\n"), "rm ") {
		t.Fatalf("a foreign container must never be removed; calls:\n%s", strings.Join(docker.calls, "\n"))
	}
	got, lerr := release.LoadInstalled(home)
	if lerr != nil || got == nil || *got != old {
		t.Fatalf("installed manifest = %v (err %v), want the previous %s restored", got, lerr, old.Version)
	}
}

// failingMCP is the changed-endpoint / unregistrable case: the registrar
// refuses rather than overwriting a name no receipt proves we own.
type failingMCP struct{ calls int }

func (m *failingMCP) EnsureMemoryRemote(name, url string) (provision.MCPRegistrationState, error) {
	m.calls++
	return provision.MCPRegistrationNone, errors.New("a different endpoint is already registered under this name")
}

// TestAutoReconcile_FailedMCPRollsBackAndRetries is the durability rule: a
// failure after the manifest was recorded must not leave the home claiming
// the new release, or the next run would skip the reconcile that never
// finished.
func TestAutoReconcile_FailedMCPRollsBackAndRetries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	_, old := fakeInstallDir(t, "1.0.0")
	recordInstalled(t, home, old)
	dir, want := fakeInstallDir(t, "2.0.0")
	mcp := &failingMCP{}
	docker := &setupFakeDocker{}
	seams := setupSeamsFor(t, dir, docker, nil)
	seams.MCP = mcp
	d, _ := autoUpgradeDeps()

	if err := autoReconcileRelease(d, seams); err == nil {
		t.Fatal("a failed MCP registration must fail the automatic upgrade")
	}
	got, err := release.LoadInstalled(home)
	if err != nil || got == nil || *got != old {
		t.Fatalf("installed manifest = %v (err %v), want the previous %s restored", got, err, old.Version)
	}
	// The runtime that DID install is left alone: re-installing over it is
	// idempotent, and deleting it on a failure path is how a retry becomes
	// a loss.
	if _, serr := os.Stat(release.RuntimeDir(home, "2.0.0")); serr != nil {
		t.Fatalf("the newly installed runtime was deleted on the failure path: %v", serr)
	}

	// The next run retries the SAME reconcile rather than believing the
	// upgrade landed.
	if err := autoReconcileRelease(d, seams); err == nil {
		t.Fatal("the retry must attempt the upgrade again")
	}
	if mcp.calls != 2 {
		t.Fatalf("registrar calls = %d, want 2 (the failure retried on the next run)", mcp.calls)
	}
	_ = want
}

// TestRunLaunch_InvokesTheAutomaticReconcile is the CALLER test: the
// reconcile is wired into `pix run` itself, before the environment is
// resolved and before any sandbox side effect, not merely available as a
// function.
func TestRunLaunch_InvokesTheAutomaticReconcile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	t.Setenv("PATH", t.TempDir()) // sbx absent: the launch fails after the reconcile
	called := 0
	orig := autoUpgradeSeamsFor
	autoUpgradeSeamsFor = func() setupSeams {
		s := orig()
		s.DiscoverBundle = func() (*release.Bundle, error) {
			called++
			return nil, errors.New("no bundle beside this test binary")
		}
		return s
	}
	t.Cleanup(func() { autoUpgradeSeamsFor = orig })

	recordInstalled(t, home, release.Manifest{
		Version: "1.0.0", PixAgentDigest: "sha256:" + strings.Repeat("a", 64),
		PixMemoryDigest: "sha256:" + strings.Repeat("b", 64), RuntimeDigest: "sha256:" + strings.Repeat("c", 64),
		KitRevision: strings.Repeat("0", 40),
	})
	d, _ := autoUpgradeDeps()
	_ = runLaunch(d, launch.RunOpts{Workspace: t.TempDir(), LauncherVersion: launcher.Version})
	if called != 1 {
		t.Fatalf("pix run consulted the release bundle %d times, want exactly 1", called)
	}
}
