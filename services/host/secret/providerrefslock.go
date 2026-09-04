// providerrefslock.go — the ONE advisory cross-process transaction lock over
// secrets.env, the single provider-ref credential file.
//
// Why a lock at all: every provider-key operation is a read-modify-write
// (`secret set` upserts, `secret rm` removes, sync reads a snapshot then
// reconciles sbx against it). An in-memory snapshot alone can't stop a
// concurrent `pix secret set/rm` in another PROCESS from interleaving and
// leaving sbx and the file sourcing a provider from different refs, so every
// such operation holds this single exclusive lock for its whole transaction.
//
// The lock file lives ADJACENT to secrets.env (same config dir), host-owned,
// created mode 0600 by withFlock. Deriving the path from DefaultOpRefsPath
// keeps it test-injectable: a test that fakes the config path (PIX_CONFIG /
// XDG_CONFIG_HOME) isolates the lock with it.
//
// DEADLOCK RULE: flock is per-open-file-description, so re-acquiring the same
// lock file from inside a held section blocks forever — even in one process.
// Public write helpers (WriteOpRefQuiet, RunSecretSet/Rm) acquire the lock
// themselves; code already inside a locked transaction must call the *Locked
// variants instead. Never call a public wrapper from inside
// WithProviderRefsLock.
package secret

import (
	"path/filepath"
	"pix/host/hostenv"
)

// providerRefsLockName is the ONE advisory transaction lock file, a sibling
// of secrets.env in the config dir. Named .secrets.lock (not provider-refs.
// lock): round 5 unified every secrets.env transaction — CRUD, sync, setup
// seeding — onto this single lock file so there is exactly one lock
// protecting the one accepted secrets file, never a second legacy path.
const providerRefsLockName = ".secrets.lock"

// ProviderRefsLockPath is <config-dir>/.secrets.lock, adjacent to the
// refs file it serializes (derived from the same injected env as
// DefaultOpRefsPath so it stays hermetic under test).
func ProviderRefsLockPath(env hostenv.Env) string {
	return filepath.Join(filepath.Dir(DefaultOpRefsPath()), providerRefsLockName)
}

// WithProviderRefsLock runs fn holding the exclusive provider-refs
// transaction lock (blocking — credential transactions are short except
// setup's strict flow, where waiting is exactly the point). A nil env.Lock
// (hermetic unit tests) runs fn directly with no lock file; defaultShellEnv
// wires the real withFlock. A lock-acquisition error is returned to the
// caller, which must fail its operation honestly — never proceed unlocked.
func WithProviderRefsLock(env hostenv.Env, fn func() error) error {

	return env.Lock(ProviderRefsLockPath(env), fn)
}
