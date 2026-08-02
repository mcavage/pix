// providerrefslock.go — the ONE advisory cross-process transaction lock for
// the provider-ref credential files, op-refs.env AND hostmode.env together.
//
// Why one lock over both files: a provider key is a two-file transaction
// (`secret set` upserts op-refs.env then mirrors to hostmode.env; `secret rm`
// removes from both; setup's strict flow validates refs, canonical-writes both
// files, verifies hostmode membership, and reconciles sbx + the synced-refs
// record against the SAME snapshot it validated). An in-memory snapshot alone
// can't stop a concurrent `pix secret set/rm` in another PROCESS from
// interleaving between those steps and leaving the two files (or sbx and the
// files) sourcing a provider from different refs. So every both-file operation
// holds this single exclusive lock for its whole transaction.
//
// The lock file lives ADJACENT to the refs files themselves (same config dir
// as op-refs.env), host-owned, created mode 0600 by withFlock. Deriving the
// path from DefaultOpRefsPath keeps it test-injectable: a test that fakes the
// config path (PIX_CONFIG / XDG_CONFIG_HOME) isolates the lock with it.
//
// DEADLOCK RULE: flock is per-open-file-description, so re-acquiring the same
// lock file from inside a held section blocks forever — even in one process.
// Public write helpers (WriteOpRefQuiet, writeOpRefFileQuiet,
// mirrorProviderRefsToHostMode, RunSecretSet/Rm) acquire the lock themselves;
// code already inside a locked transaction must call the *Locked variants
// instead. Never call a public wrapper from inside WithProviderRefsLock.
package secret

import (
	"path/filepath"
	"pix/host/hostenv"
)

// providerRefsLockName is the advisory transaction lock file, a sibling of
// op-refs.env and hostmode.env in the config dir.
const providerRefsLockName = "provider-refs.lock"

// ProviderRefsLockPath is <config-dir>/provider-refs.lock, adjacent to the
// two refs files it serializes (derived from the same injected env as
// DefaultOpRefsPath so it stays hermetic under test).
func ProviderRefsLockPath(env hostenv.Env) string {
	return filepath.Join(filepath.Dir(DefaultOpRefsPath(env)), providerRefsLockName)
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
