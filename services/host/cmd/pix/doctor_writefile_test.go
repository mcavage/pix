// doctor_writefile_test.go: defaultShellEnv().writeFile is the real os-backed
// writer every op-refs.env/hostmode.env/config write goes through. It must be
// LEAF-symlink-safe (item B): a tracked symlink at the destination path is
// REPLACED by an atomic same-directory temp file + rename, never followed —
// so writing "over" a symlinked op-refs.env/hostmode.env can never truncate
// whatever file it points at. Parent-directory symlinks are a separate,
// narrower concern (see atomicWriteInDir's doc comment) and are out of scope
// here; these tests cover the LEAF only, matching the writer's documented
// scope.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A plain, non-existent destination is created as a regular 0600 file (perm
// argument honored) under a 0700 parent.
func TestDefaultShellEnvWriteFile_CreatesRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "op-refs.env")
	env := defaultShellEnv()
	if err := env.WriteFile(path, []byte("ANTHROPIC_API_KEY=op://v/a/k\n"), 0o600); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ANTHROPIC_API_KEY") {
		t.Errorf("content not written, got: %q", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
	}
}

// A LEAF symlink at the destination (op-refs.env or hostmode.env pointing
// somewhere else) is REPLACED, never followed: the symlink's target is left
// byte-for-byte untouched, and the destination path ends up a regular 0600
// file carrying the new content.
func TestDefaultShellEnvWriteFile_ReplacesLeafSymlinkTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim.env")
	const victimContent = "SOME_OTHER_SECRET=op://v/x/y\n"
	if err := os.WriteFile(target, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "op-refs.env")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	env := defaultShellEnv()
	if err := env.WriteFile(link, []byte("ANTHROPIC_API_KEY=op://v/a/k\n"), 0o600); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	victim, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(victim) != victimContent {
		t.Errorf("symlink target must be untouched, got:\n%s", victim)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("destination must no longer be a symlink after the write")
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("replaced file perm = %v, want 0600", fi.Mode().Perm())
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "ANTHROPIC_API_KEY") {
		t.Errorf("replaced file must carry the new content, got: %q", got)
	}
}
