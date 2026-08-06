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

// ReadStateFile reads <Workspace>/.pix/<name> without ever following a
// symlinked .pix DIRECTORY or a symlinked state FILE — the read-side mirror of
// WriteStateFile's fail-safe symlink handling. A missing .pix dir or a missing
// file both return os.ErrNotExist-wrapping errors (ordinary os.IsNotExist
// works on the result); a symlink at either level is refused, never followed.
func ReadStateFile(Workspace, name string) ([]byte, error) {
	dir := filepath.Join(Workspace, ".pix")
	di, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if di.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read workspace state through it", dir)
	}
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read workspace state through it", path)
	}
	return os.ReadFile(path)
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
