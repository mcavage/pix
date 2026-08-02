package pack

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPackUse_ForgedDirectorySymlinkLockScrubbedNotFollowed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	victim := filepath.Join(dir, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "evil")
	mustWritePack(t, root, Manifest{Name: "evil", Schema: 1})
	if err := os.Symlink(victim, PackLockPath(root)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var out bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &out, []string{root}, registerOK)
	if fi, err := os.Lstat(PackLockPath(root)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("pack.lock must be a fresh regular file after adoption, got %v (err=%v)", fi, err)
	}
}
