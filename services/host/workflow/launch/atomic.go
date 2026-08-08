//go:build unix

// atomic.go — the one same-dir temp-file-plus-rename primitive every write in
// this package that must never leave a reader observing a partial file goes
// through. A FIXED ".tmp" name (what session-state and the teardown journal
// both used before this) is fine when a single writer is proven exclusive
// (e.g. lease's keep.json, guarded by keep.lock end to end) but is a real
// collision surface anywhere it is not: two writers racing the SAME fixed
// name can interleave their writes into one file, or one writer's rename can
// commit the OTHER writer's half-written temp. A unique same-directory temp
// name removes that surface outright; it does not, by itself, serialize a
// read-modify-write cycle spanning MULTIPLE files or one file's own prior
// contents — see reap.go's teardown-journal guard for that.
package launch

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path via a unique temp file in the SAME
// directory (so the final rename(2) is on one filesystem, atomic) plus
// rename, then best-effort cleans up the temp file on any failure. Never
// leaves path missing or containing a mix of two writers' data: the loser of
// a race always contends on the temp name's own O_EXCL (from CreateTemp),
// never on path itself, until the single atomic rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
