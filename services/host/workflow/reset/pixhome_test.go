package reset

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeContainerRunner struct {
	inspectOut string
	inspectErr error
	stopped    bool
	removed    bool
	failStop   error
}

func (f *fakeContainerRunner) Run(args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("no args")
	}
	switch args[0] {
	case "inspect":
		return f.inspectOut, f.inspectErr
	case "stop":
		if f.failStop != nil {
			return "boom", f.failStop
		}
		f.stopped = true
		return "", nil
	case "rm":
		f.removed = true
		return "", nil
	}
	return "", nil
}

func fixedNow() time.Time { return time.Unix(1700000000, 0) }

func TestResetHome_HappyPath(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}
	sweptCalled := false
	var out, errOut bytes.Buffer
	res, err := ResetHome(HomeDeps{
		Home:            home,
		ContainerRunner: runner,
		ContainerName:   "pix-memory-test",
		Sweep:           func(o, e io.Writer) error { sweptCalled = true; return nil },
		Out:             &out, ErrOut: &errOut,
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("ResetHome: %v", err)
	}
	if !sweptCalled {
		t.Error("Sweep was never called")
	}
	if !res.SweptSandboxes || !res.ContainerRemoved {
		t.Fatalf("res = %+v", res)
	}
	wantBackup := home + ".bak-1700000000"
	if res.BackupPath != wantBackup {
		t.Fatalf("BackupPath = %q, want %q", res.BackupPath, wantBackup)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("original home should be gone (renamed), stat err = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(wantBackup, "AGENTS.md"))
	if err != nil || string(data) != "hi" {
		t.Fatalf("backup content missing: %v %q", err, data)
	}
	if !runner.stopped || !runner.removed {
		t.Fatalf("container was not stopped/removed: %+v", runner)
	}
}

func TestResetHome_RefusesWhenContainerStillPresent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	os.MkdirAll(home, 0o700)

	// inspect always reports present, even after stop/rm ran (simulating a
	// container that would not die).
	runner := &fakeContainerRunner{inspectOut: `[{"Id":"x","Config":{"Image":"i"},"State":{"Running":true}}]`}
	_, err := ResetHome(HomeDeps{Home: home, ContainerRunner: runner, ContainerName: "pix-memory-test", Now: fixedNow})
	if err == nil {
		t.Fatal("expected an error when the container cannot be proven absent")
	}
	if _, statErr := os.Stat(home); statErr != nil {
		t.Fatal("PIX_HOME must not be renamed when the container proof fails")
	}
}

func TestResetHome_SweepFailurePreventsAnyDockerCall(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	os.MkdirAll(home, 0o700)

	runner := &fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}
	_, err := ResetHome(HomeDeps{
		Home: home, ContainerRunner: runner, ContainerName: "pix-memory-test",
		Sweep: func(o, e io.Writer) error { return errors.New("sandbox still holds a reference") },
		Now:   fixedNow,
	})
	if err == nil {
		t.Fatal("expected an error when Sweep fails")
	}
	if runner.stopped || runner.removed {
		t.Fatal("a failed sweep must never reach the container step")
	}
	if _, statErr := os.Stat(home); statErr != nil {
		t.Fatal("PIX_HOME must not be renamed when sweep fails")
	}
}

func TestResetHome_RefusesSymlinkedHome(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-home")
	os.MkdirAll(real, 0o700)
	link := filepath.Join(root, "pixhome-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks not supported here: %v", err)
	}

	runner := &fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}
	_, err := ResetHome(HomeDeps{Home: link, ContainerRunner: runner, ContainerName: "pix-memory-test", Now: fixedNow})
	if err == nil {
		t.Fatal("expected an error for a symlinked PIX_HOME")
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Fatal("the symlink itself must survive a refused reset")
	}
	if _, statErr := os.Stat(real); statErr != nil {
		t.Fatal("the real target directory must survive a refused reset")
	}
}

func TestResetHome_MissingHomeIsNotAnError(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "never-existed")
	runner := &fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}
	res, err := ResetHome(HomeDeps{Home: home, ContainerRunner: runner, ContainerName: "pix-memory-test", Now: fixedNow})
	if err != nil {
		t.Fatalf("ResetHome: %v", err)
	}
	if res.BackupPath != "" {
		t.Fatalf("BackupPath = %q, want empty", res.BackupPath)
	}
}

// TestResetHome_RefusesWithoutExplicitContainerName is the TDD anchor for
// Wave B U4's "reset cannot default to bare pix-memory": omitting
// ContainerName must refuse outright, with zero Docker calls and PIX_HOME
// completely untouched — never a silent fall back to container.Name, which
// could reach a totally different PIX_HOME's own container on a host
// running more than one stack.
func TestResetHome_RefusesWithoutExplicitContainerName(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	os.MkdirAll(home, 0o700)
	runner := &recordingRunner{fakeContainerRunner: fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}}

	_, err := ResetHome(HomeDeps{Home: home, ContainerRunner: runner, Now: fixedNow})
	if err == nil {
		t.Fatal("expected an error when ContainerName is empty")
	}
	if !strings.Contains(err.Error(), "pix-memory") {
		t.Fatalf("error should name the bare legacy name it refuses to guess, got: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected zero docker calls, got %v", runner.calls)
	}
	if _, statErr := os.Stat(home); statErr != nil {
		t.Fatal("PIX_HOME must be untouched when the container name is missing")
	}
}

type recordingRunner struct {
	fakeContainerRunner
	lastArg string
	calls   [][]string
}

func (r *recordingRunner) Run(args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 1 {
		r.lastArg = args[1]
	}
	return r.fakeContainerRunner.Run(args...)
}

// fakeDeregistrar is a scripted MCPDeregistrar: each call consumes the next
// scripted outcome for its name, and records every call so a test can
// assert exactly which names were (and were not) targeted.
type fakeDeregistrar struct {
	outcomes map[string]MCPRemovalState
	errs     map[string]error
	calls    []string
}

func (f *fakeDeregistrar) RemoveIfRegistered(name string) (MCPRemovalState, error) {
	f.calls = append(f.calls, name)
	if err, ok := f.errs[name]; ok {
		return MCPRemovalRetained, err
	}
	return f.outcomes[name], nil
}

// TestResetHome_RemovesOnlyThisStacksOwnMCPRegistrations proves the
// best-effort MCP removal targets EXACTLY the two scoped names handed in,
// never anything else, and reports the proven outcome per name.
func TestResetHome_RemovesOnlyThisStacksOwnMCPRegistrations(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	os.MkdirAll(home, 0o700)
	runner := &fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}
	dereg := &fakeDeregistrar{outcomes: map[string]MCPRemovalState{
		"pix-memory-aaaaaaaaaaaaaaaa":  MCPRemovalRemoved,
		"pix-session-aaaaaaaaaaaaaaaa": MCPRemovalAbsent,
	}}

	res, err := ResetHome(HomeDeps{
		Home: home, ContainerRunner: runner, ContainerName: "pix-memory-aaaaaaaaaaaaaaaa",
		MCP: dereg, MemoryMCPName: "pix-memory-aaaaaaaaaaaaaaaa", SessionMCPName: "pix-session-aaaaaaaaaaaaaaaa",
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("ResetHome: %v", err)
	}
	if res.MemoryMCPState != MCPRemovalRemoved {
		t.Fatalf("MemoryMCPState = %v, want Removed", res.MemoryMCPState)
	}
	if res.SessionMCPState != MCPRemovalAbsent {
		t.Fatalf("SessionMCPState = %v, want Absent", res.SessionMCPState)
	}
	if len(dereg.calls) != 2 {
		t.Fatalf("expected exactly 2 targeted removal calls, got %v", dereg.calls)
	}
}

// TestResetHome_NoDeregistrarWiredIsHonestlyRetained proves the default
// (no MCPDeregistrar wired) never fabricates a "removed" result.
func TestResetHome_NoDeregistrarWiredIsHonestlyRetained(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	os.MkdirAll(home, 0o700)
	runner := &fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}

	res, err := ResetHome(HomeDeps{
		Home: home, ContainerRunner: runner, ContainerName: "pix-memory-aaaaaaaaaaaaaaaa",
		MemoryMCPName: "pix-memory-aaaaaaaaaaaaaaaa", SessionMCPName: "pix-session-aaaaaaaaaaaaaaaa",
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("ResetHome: %v", err)
	}
	if res.MemoryMCPState != MCPRemovalRetained || res.SessionMCPState != MCPRemovalRetained {
		t.Fatalf("expected both retained with no deregistrar wired, got memory=%v session=%v", res.MemoryMCPState, res.SessionMCPState)
	}
}

// TestResetHome_MCPRemovalFailureNeverBlocksRename proves best-effort really
// means best-effort: a deregistrar error must not stop PIX_HOME from being
// renamed once the container is proven absent.
func TestResetHome_MCPRemovalFailureNeverBlocksRename(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	os.MkdirAll(home, 0o700)
	runner := &fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}
	dereg := &fakeDeregistrar{errs: map[string]error{"pix-memory-aaaaaaaaaaaaaaaa": errors.New("sbx unreachable")}}

	res, err := ResetHome(HomeDeps{
		Home: home, ContainerRunner: runner, ContainerName: "pix-memory-aaaaaaaaaaaaaaaa",
		MCP: dereg, MemoryMCPName: "pix-memory-aaaaaaaaaaaaaaaa",
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("ResetHome: %v", err)
	}
	if res.MemoryMCPState != MCPRemovalRetained {
		t.Fatalf("MemoryMCPState = %v, want Retained on an operational failure", res.MemoryMCPState)
	}
	if res.BackupPath == "" {
		t.Fatal("a failed MCP removal must never block the PIX_HOME rename")
	}
}
