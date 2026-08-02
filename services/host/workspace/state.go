// workspacestate.go is the single symlink-safe writer AND remover for the
// launcher's per-Workspace state files (<Workspace>/.pix/*: sandbox.pack,
// profile, ollama-bridge.model, knowledge.scope, knowledge, onboarding.json).
//
// Trusted host-state facts (keys/services/knowledge/gog/mcp/models/pack/
// identity) are deliberately NOT among these: they are built in memory and
// injected directly into the launcher-generated initial prompt (see
// hoststate.go's injectTrustedHostState), never written as a Workspace file.
// A Workspace is attacker-influenced (a cloned repo), so a file there can
// never be the trust boundary for facts the fenced agent treats as ground
// truth — see the top of hoststate.go for the full rationale.
//
// Why this exists (class fix, not a one-off): the Workspace is
// attacker-influenced — a user can `pix run` inside a freshly cloned,
// untrusted repo. That repo can ship a TRACKED symlink at .pix/<file>
// (or make .pix itself a symlink), and os.WriteFile FOLLOWS symlinks, so
// a plain WriteFile would truncate/overwrite an arbitrary host file with
// pack/scope/state data. Same class as the pack.lock fix in
// writePackLockBytes (pack.go); this generalizes that pattern for every
// Workspace state write.
//
// The removal side has a DIFFERENT vector than the write side: os.Remove
// never follows a symlinked *destination* file (unlink just removes the
// link), but it DOES traverse a symlinked *parent* directory to resolve the
// path. So a repo that commits .pix ITSELF as a symlink to another
// repo's .pix turns a plain
// os.Remove(filepath.Join(Workspace, ".pix", name)) into a delete of
// that OTHER repo's profile/knowledge.scope/sandbox.pack/etc.
// RemoveStateFile mirrors WriteStateFile's Lstat-and-refuse
// check on the .pix dir so every Workspace-state removal gets the same
// guarantee.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"pix/host/sys"
)

// WriteStateFile writes <Workspace>/.pix/<name> without ever
// following a symlink:
//
//   - If .pix exists and is a SYMLINK, it REFUSES — state is never
//     written through a symlinked state dir (os.MkdirAll treats a
//     symlink-to-dir as "already a directory", so this is Lstat-verified
//     after the MkdirAll). Absent, it is created as a real directory.
//   - The destination file is never opened for write directly: the data goes
//     to a same-dir os.CreateTemp and is os.Rename'd over the destination.
//     rename REPLACES a symlink (it never follows one) and is atomic, so a
//     hostile tracked symlink at .pix/<name> is swapped out for a real
//     file instead of having its target truncated — and there is no
//     Lstat-then-write TOCTOU window for an attacker to slip a symlink into.
//
// Callers that are best-effort by contract (host-state, pack marker, memory
// scope, bridge model) discard the error; callers with a hard contract
// (knowledge.scope, the knowledge pointer) propagate it.
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
//
//   - If .pix is absent, this is a clean no-op.
//   - If .pix exists and is a SYMLINK, it REFUSES (mirrors
//     WriteStateFile's dir check exactly) — a plain
//     os.Remove(filepath.Join(Workspace, ".pix", name)) does NOT follow a
//     symlinked *destination* file (unlink never follows the final symlink),
//     but it DOES traverse a symlinked *parent* directory to resolve the
//     path. So a hostile/cloned repo that commits .pix itself as a
//     symlink to another repo's .pix turns every "best-effort cleanup"
//     removal into a delete of that OTHER repo's state files. Refusing here
//     closes that without needing every call site to Lstat first.
//   - Otherwise (a real directory), os.Remove the file, treating
//     already-absent as success.
//
// Best-effort callers ignore the returned error (same contract as before);
// they just no longer delete through a symlinked parent while doing so.
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
