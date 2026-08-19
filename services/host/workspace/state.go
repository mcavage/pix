// workspacestate.go is the single symlink-safe writer AND remover for the
// launcher's per-Workspace state files (<Workspace>/.pix/*: sandbox.pack,
// profile, ollama-bridge.model, memory-capture, knowledge.scope, knowledge,
// onboarding.json).
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
// The directory-level check stays Lstat-then-open (there is no O_NOFOLLOW-
// shaped open(2) for a directory lookup here); the state FILE itself is
// opened by readStateFileNoFollow, whose unix build refuses the symlink
// ATOMICALLY at open(2) — see state_unix.go — instead of the separate
// Lstat-then-ReadFile this used to do, which left a TOCTOU window between the
// check and the read for something to swap the symlink in.
func ReadStateFile(Workspace, name string) ([]byte, error) {
	dir := filepath.Join(Workspace, ".pix")
	di, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if di.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read workspace state through it", dir)
	}
	return readStateFileNoFollow(filepath.Join(dir, name))
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
