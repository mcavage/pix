// workspacestate.go is the single symlink-safe writer AND remover for the
// launcher's per-workspace state files (<workspace>/.pi-stack/*: sandbox.pack,
// profile, ollama-bridge.model, knowledge.scope, knowledge, host-state.json,
// onboarding.json).
//
// Why this exists (class fix, not a one-off): the workspace is
// attacker-influenced — a user can `pi-stack run` inside a freshly cloned,
// untrusted repo. That repo can ship a TRACKED symlink at .pi-stack/<file>
// (or make .pi-stack itself a symlink), and os.WriteFile FOLLOWS symlinks, so
// a plain WriteFile would truncate/overwrite an arbitrary host file with
// pack/scope/state data. Same class as the pack.lock fix in
// writePackLockBytes (pack.go); this generalizes that pattern for every
// workspace state write.
//
// The removal side has a DIFFERENT vector than the write side: os.Remove
// never follows a symlinked *destination* file (unlink just removes the
// link), but it DOES traverse a symlinked *parent* directory to resolve the
// path. So a repo that commits .pi-stack ITSELF as a symlink to another
// repo's .pi-stack turns a plain
// os.Remove(filepath.Join(workspace, ".pi-stack", name)) into a delete of
// that OTHER repo's profile/knowledge.scope/sandbox.pack/etc.
// removeWorkspaceStateFile mirrors writeWorkspaceStateFile's Lstat-and-refuse
// check on the .pi-stack dir so every workspace-state removal gets the same
// guarantee.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeWorkspaceStateFile writes <workspace>/.pi-stack/<name> without ever
// following a symlink:
//
//   - If .pi-stack exists and is a SYMLINK, it REFUSES — state is never
//     written through a symlinked state dir (os.MkdirAll treats a
//     symlink-to-dir as "already a directory", so this is Lstat-verified
//     after the MkdirAll). Absent, it is created as a real directory.
//   - The destination file is never opened for write directly: the data goes
//     to a same-dir os.CreateTemp and is os.Rename'd over the destination.
//     rename REPLACES a symlink (it never follows one) and is atomic, so a
//     hostile tracked symlink at .pi-stack/<name> is swapped out for a real
//     file instead of having its target truncated — and there is no
//     Lstat-then-write TOCTOU window for an attacker to slip a symlink into.
//
// Callers that are best-effort by contract (host-state, pack marker, memory
// scope, bridge model) discard the error; callers with a hard contract
// (knowledge.scope, the knowledge pointer) propagate it.
func writeWorkspaceStateFile(workspace, name string, data []byte, perm os.FileMode) error {
	dir := filepath.Join(workspace, ".pi-stack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write workspace state through it", dir)
	}
	return atomicWriteInDir(dir, name, data, perm)
}

// removeWorkspaceStateFile removes <workspace>/.pi-stack/<name> without ever
// traversing a symlinked .pi-stack DIRECTORY:
//
//   - If .pi-stack is absent, this is a clean no-op.
//   - If .pi-stack exists and is a SYMLINK, it REFUSES (mirrors
//     writeWorkspaceStateFile's dir check exactly) — a plain
//     os.Remove(filepath.Join(workspace, ".pi-stack", name)) does NOT follow a
//     symlinked *destination* file (unlink never follows the final symlink),
//     but it DOES traverse a symlinked *parent* directory to resolve the
//     path. So a hostile/cloned repo that commits .pi-stack itself as a
//     symlink to another repo's .pi-stack turns every "best-effort cleanup"
//     removal into a delete of that OTHER repo's state files. Refusing here
//     closes that without needing every call site to Lstat first.
//   - Otherwise (a real directory), os.Remove the file, treating
//     already-absent as success.
//
// Best-effort callers ignore the returned error (same contract as before);
// they just no longer delete through a symlinked parent while doing so.
func removeWorkspaceStateFile(workspace, name string) error {
	dir := filepath.Join(workspace, ".pi-stack")
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to remove workspace state through it", dir)
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// atomicWriteInDir writes dir/name via a same-dir temp file + os.Rename.
// The destination is never opened, so an existing symlink there is replaced,
// never followed; an interrupted write can never truncate an existing file.
// Shared by writeWorkspaceStateFile and writePackLockBytes (pack.go).
func atomicWriteInDir(dir, name string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil { // CreateTemp makes 0600
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
