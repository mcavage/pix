// workspacestate.go is the single symlink-safe writer AND remover for the
// launcher's per-Workspace state files (<Workspace>/.pix/*: sandbox.pack,
// profile, ollama-bridge.model, knowledge.scope, knowledge, onboarding.json).
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"pix/host/sys"
)

// WriteStateFile writes <Workspace>/.pix/<name> without ever
// following a symlink:
func WriteStateFile(Workspace, name string, data []byte, perm os.FileMode) error {
	dir := filepath.Join(Workspace, ".pix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write Workspace state through it", dir)
	}
	return sys.AtomicWriteInDir(dir, name, data, perm)
}

// RemoveStateFile removes <Workspace>/.pix/<name> without ever
// traversing a symlinked .pix DIRECTORY:
func RemoveStateFile(Workspace, name string) error {
	dir := filepath.Join(Workspace, ".pix")
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to remove Workspace state through it", dir)
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
