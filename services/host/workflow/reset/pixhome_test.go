package reset

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pix/host/container"
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
	res, err := ResetHome(HomeDeps{Home: home, ContainerRunner: runner, Now: fixedNow})
	if err != nil {
		t.Fatalf("ResetHome: %v", err)
	}
	if res.BackupPath != "" {
		t.Fatalf("BackupPath = %q, want empty", res.BackupPath)
	}
}

// container.Name sanity: default ContainerName resolves to it.
func TestResetHome_DefaultsContainerName(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "pixhome")
	os.MkdirAll(home, 0o700)
	var seenName string
	runner := &recordingRunner{fakeContainerRunner: fakeContainerRunner{inspectOut: "Error: No such object", inspectErr: errors.New("exit 1")}}
	_, err := ResetHome(HomeDeps{Home: home, ContainerRunner: runner, Now: fixedNow})
	if err != nil {
		t.Fatalf("ResetHome: %v", err)
	}
	seenName = runner.lastArg
	if seenName != container.Name {
		t.Fatalf("used container name %q, want default %q", seenName, container.Name)
	}
}

type recordingRunner struct {
	fakeContainerRunner
	lastArg string
}

func (r *recordingRunner) Run(args ...string) (string, error) {
	if len(args) > 1 {
		r.lastArg = args[1]
	}
	return r.fakeContainerRunner.Run(args...)
}
