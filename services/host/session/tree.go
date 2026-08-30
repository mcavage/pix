// tree.go resolves, for one sandbox lease directory, WHICH session tree its
// interactive root belongs to: a fresh sandbox starts a fresh tree, and a
// re-attached sandbox resumes the SAME tree it was created with (architecture
// §7.1: "Every `pix run` creates or resumes a session tree"). The pointer
// lives beside (never inside) the reference files Hold/CountHolders already
// own in the same sandbox directory, so a torn-down sandbox's directory
// removal (owned elsewhere, by workflow/launch's teardown) takes the pointer
// with it — a fresh sandbox reusing the same name later gets a fresh tree,
// never a stale resumed one.
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// treePointerFileName is the sandbox-directory-local pointer to this
// sandbox's current session tree id. It is deliberately a bare id, not a
// JSON envelope: the only thing worth persisting here is "which tree", and
// the tree's own record (Store.ReadTree) is the schema-checked source of
// truth for everything else.
const treePointerFileName = "tree-id"

// EnsureTree resolves the session tree an interactive root joins for
// sandboxDir: a previously recorded, still-readable tree id is resumed;
// anything else (no pointer, an unreadable pointer, or a pointer naming a
// tree this store can no longer read — e.g. a newer schema) starts a FRESH
// tree rather than guessing, because resuming the wrong tree would silently
// misattribute every node recorded under it.
func EnsureTree(store Store, sandboxDir, environment, workspace string) (string, error) {
	path := filepath.Join(sandboxDir, treePointerFileName)
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			if _, rerr := store.ReadTree(id); rerr == nil {
				return id, nil
			}
		}
	}
	t, err := store.CreateTree(environment, workspace)
	if err != nil {
		return "", fmt.Errorf("pix: could not create a session tree for %s: %w", sandboxDir, err)
	}
	if err := os.MkdirAll(sandboxDir, 0o700); err != nil {
		return "", fmt.Errorf("pix: could not create %s: %w", sandboxDir, err)
	}
	if err := os.WriteFile(path, []byte(t.ID+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("pix: could not record %s's session tree pointer: %w", sandboxDir, err)
	}
	return t.ID, nil
}
