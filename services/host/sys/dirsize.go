package sys

import (
	"io/fs"
	"path/filepath"
)

// DirSize counts the regular files under root and their total bytes. Both task
// (reporting what it harvested) and reset (deciding whether there is anything
// to move aside) need it, so it sits below both.

// DirSize sums the file count + byte size under root (best-effort; an
// unreadable tree or a missing root contributes 0). Shared by the status summary
// and uninstall's "keeping artifacts" notice.
func DirSize(root string) (files int, bytes int64) {
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files++
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes
}
