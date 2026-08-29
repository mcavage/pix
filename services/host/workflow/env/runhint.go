// runhint.go — the ONE quiet, workspace-scoped nudge `pix run` may print
// about a native environment it did not select (docs/design/environments.md
// §6.1, D13; Wave C E1.15, AC-59). A workspace's `.sbxenv.yaml` is never
// selected automatically — RunHint is the single, negative-first sentence
// that says so, at most once per canonical workspace, ever, on this host.
//
// This is display-only. RunHint reads cfg.Environments and the workspace's
// own directory listing; it never mutates or saves cfg, never parses the
// file it found (that is Load's job, reached only after an explicit `pix env
// add`), and never prompts. The durable "already told this workspace" fact
// lives in launcher-owned state — never a file inside the workspace, which a
// cloned repo could plant or clear on its own.
package env

import (
	"encoding/json"
	"os"
	"path/filepath"

	"pix/host/config"
	"pix/host/hosttrust"
)

// SbxEnvFileName is the native environment file whose mere PRESENCE in a
// workspace can trigger RunHint. Its contents are never read here.
const SbxEnvFileName = ".sbxenv.yaml"

// sbxenvHintMessage leads with the negative (Pix did not select this), says
// so is not automatic, and names exactly one next step — `pix env add` — and
// nothing else: no `pix env review`, no `pix help env`, no implication that
// an environment is required to keep running.
const sbxenvHintMessage = "pix: did not select the .sbxenv.yaml found in this workspace; pix run\n" +
	"never picks one up on its own. Register it if you want it: pix env add <name> [path]\n"

// RunHint returns the D13/AC-59 hint text, or "" when any of these hold:
//
//   - cfg already has at least one registered environment (cfg.Environments
//     is non-empty): a host that already knows the feature exists gets no
//     more nudging, registered or not for THIS workspace;
//   - workspace holds no `.sbxenv.yaml`;
//   - this exact canonical workspace already showed the hint on a prior
//     `pix run`, per the durable state-dir marker; or
//   - the marker could not be read or written.
//
// The last case fails OPEN toward silence, never toward blocking or
// repeating: a hint is a display-only nudge, so a marker pix could not
// record must not become either a launch failure or a nag on every run.
func RunHint(cfg *config.Config, workspace string) string {
	if cfg != nil && len(cfg.Environments) > 0 {
		return ""
	}
	root := hosttrust.CanonicalRoot(workspace)
	if root == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, SbxEnvFileName)); err != nil {
		return ""
	}
	already, err := markRunHintShown(root)
	if err != nil || already {
		return ""
	}
	return sbxenvHintMessage
}

// runHintStoreName is the durable once-per-workspace marker's file, in
// launcher-owned STATE (config.StateDir): runtime bookkeeping about what a
// user has already been told, not a configuration or trust decision.
const runHintStoreName = "sbxenv-hint.json"

// runHintLockName is a lock file distinct from the data file it guards
// (never the data file's own path): the data file gets replaced wholesale by
// SaveDocument's temp-plus-rename, and locking a path that can be renamed out
// from under an open handle is the exact TOCTOU a separate, never-replaced
// lock path avoids.
const runHintLockName = runHintStoreName + ".lock"

// runHintStore is the entire on-disk shape: which canonical workspace roots
// already saw the hint. It is a SET, not a log — no cap, no timestamp,
// because the only question this ever answers is "have we told this
// workspace once", never "when" or "how many times".
type runHintStore struct {
	Version int             `json:"version"`
	Shown   map[string]bool `json:"shown,omitempty"`
}

// loadRunHintStore reads the marker document from dir. Absent -> a fresh,
// empty store (nothing shown yet, ever); anything else unreadable is an
// error, so a caller fails toward silence rather than trusting a half-read
// set as empty.
func loadRunHintStore(dir string) (*runHintStore, error) {
	b, err := hosttrust.ReadDocumentBytes(filepath.Join(dir, runHintStoreName))
	if err != nil {
		if os.IsNotExist(err) {
			return &runHintStore{Version: 1, Shown: map[string]bool{}}, nil
		}
		return nil, err
	}
	var s runHintStore
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Shown == nil {
		s.Shown = map[string]bool{}
	}
	return &s, nil
}

// save writes the store to dir symlink-safe + atomic (hosttrust.SaveDocument:
// Lstat-refuse a symlinked destination, then a same-dir temp file + rename).
func (s *runHintStore) save(dir string) error {
	s.Version = 1
	return hosttrust.SaveDocument(dir, runHintStoreName, s)
}

// markRunHintShown reports whether root already had the hint recorded and,
// if not, atomically records it now: one cross-process-locked
// (hosttrust.WithLock) fresh-load -> mutate -> save (hosttrust.LoadMutateSave)
// so two concurrent `pix run` invocations in the same workspace can never
// both observe "not shown yet" and both print the hint.
func markRunHintShown(root string) (alreadyShown bool, err error) {
	dir, derr := config.StateDir()
	if derr != nil {
		return false, derr
	}
	lockPath := filepath.Join(dir, runHintLockName)
	err = hosttrust.WithLock(lockPath, func() error {
		_, e := hosttrust.LoadMutateSave(
			func() (*runHintStore, error) { return loadRunHintStore(dir) },
			func(s *runHintStore) error {
				if s.Shown[root] {
					alreadyShown = true
				} else {
					s.Shown[root] = true
				}
				return nil
			},
			func(s *runHintStore) error {
				if alreadyShown {
					return nil // nothing changed; no write needed
				}
				return s.save(dir)
			},
		)
		return e
	})
	return alreadyShown, err
}
