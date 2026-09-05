package launch

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"pix/host/config"
)

// TestPersonalContextIsMountedWritableFromColdStart pins the two properties that
// make "edit your own skills inside the sandbox, then commit that directory"
// actually work.
//
//  1. The personal tree is mounted UNCONDITIONALLY. It used to be gated on the
//     dir already having entries, which made the first skill impossible to write
//     from inside a sandbox: nothing was mounted, so there was nowhere to write
//     it, and the only fix was to go back to the host and relaunch.
//  2. The MOUNT is the context ROOT while pi is pointed at its skills/ subdir, so
//     the standing AGENTS.md beside the skills is editable and the whole
//     directory can live in git. Mounting only skills/ hid AGENTS.md entirely.
func TestPersonalContextIsMountedWritableFromColdStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_HOME", dir)
	root := config.ContextDir()

	cfg := &config.Config{}
	o := RunOpts{Workspace: "/work"}

	// Cold start: nothing exists yet.
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("precondition: context dir must not exist yet (%v)", err)
	}
	mounts := MountDirs(cfg, o)
	if !slices.Contains(mounts, root) {
		t.Errorf("mounts = %v, want the context ROOT %q even before it exists", mounts, root)
	}
	if skills := filepath.Join(root, "skills"); slices.Contains(mounts, skills) {
		t.Errorf("mounts include %q; the parent is mounted instead, so this would double-mount", skills)
	}
	if got := LiveSkillDirs(cfg, o); !slices.Contains(got, PersonalSkillsDir()) {
		t.Errorf("skill dirs = %v, want the personal tree unconditionally", got)
	}

	// EnsurePersonalContextDir makes the mount real, so the first skill can be
	// written from inside the sandbox.
	EnsurePersonalContextDir()
	fi, err := os.Stat(PersonalSkillsDir())
	if err != nil || !fi.IsDir() {
		t.Fatalf("EnsurePersonalContextDir did not create %q: %v", PersonalSkillsDir(), err)
	}
	// Idempotent: a second run on an existing tree is a no-op, not an error.
	EnsurePersonalContextDir()
}

// TestPersonalSkillsDirIsOneSpelling: the loader and the mount must derive from
// the same path, or pi is told to load a dir that was never mounted.
func TestPersonalSkillsDirIsOneSpelling(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if got, want := PersonalSkillsDir(), filepath.Join(config.ContextDir(), "skills"); got != want {
		t.Errorf("PersonalSkillsDir() = %q, want %q", got, want)
	}
}
