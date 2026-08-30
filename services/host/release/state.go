package release

import (
	"fmt"
	"os"
	"path/filepath"
)

// InstallStateFile is release.json's file name under a Pix home's state
// directory.
const InstallStateFile = "release.json"

// InstallStatePath is where the currently-installed release manifest is
// recorded: <home>/state/release.json. home is a caller-resolved Pix home
// root (e.g. pixhome.Dir()'s result) — this package takes no dependency on
// pixhome so it stays usable from anything that already has a home path in
// hand. Setup writes this file after installing a release; doctor reads it
// back to compare against the artifacts actually running, so there is one
// authoritative "what is installed" record, never a second version map
// (architecture §3, §12).
func InstallStatePath(home string) string {
	return filepath.Join(home, "state", InstallStateFile)
}

// LoadInstalled reads and validates the manifest recorded at
// InstallStatePath(home). A missing file returns (nil, nil): "nothing
// installed yet" is not an error, it is the state of a machine before its
// first `pix setup`.
func LoadInstalled(home string) (*Manifest, error) {
	path := InstallStatePath(home)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// SaveInstalled validates m and durably records it at
// InstallStatePath(home): a same-directory temp file, fsync, then an atomic
// rename — never a truncate in place. Unlike pixhome's README/.gitignore,
// this file IS meant to be replaced on every install/upgrade, so a rename
// (not a no-clobber link) is the right publish here.
//
// The file is written mode 0600: it is machine state
// (docs/design/pix-v2-architecture.md §5), never something a user edits or a
// Git commit should ever see (it lives under state/, which pixhome's
// .gitignore excludes outright).
func SaveInstalled(home string, m Manifest) error {
	data, err := m.Encode()
	if err != nil {
		return err
	}
	path := InstallStatePath(home)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return atomicWrite(dir, path, data, 0o600)
}

// atomicWrite writes data to dir/path (path already inside dir) via a
// same-directory temp file, fsync, then rename — mirroring the launcher's
// existing durable-write pattern (config.writeFileAtomic) without importing
// it, since this package takes no intra-module dependency.
func atomicWrite(dir, path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
