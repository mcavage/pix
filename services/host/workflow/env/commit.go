// commit.go — Wave C security L1: the ONE locked commit path every `pix
// env` registry/default mutation (add.go, use.go, forget.go) persists
// through. Without it, two pix processes each load config.toml, mutate
// their own in-memory copy, and Save() whole-file — whichever lands second
// silently reverts the first (a classic lost update, with the review
// prompt an arbitrarily long interleaving window).
//
// The shape is hosttrust's own fresh-load -> mutate -> save discipline
// applied to config.toml: take the launcher-owned env-registry flock
// (config.EnvRegistryLockPath — STATE dir, so a config-dir rename can
// never strand it), reload the LIVE file under the lock, apply the
// mutation (which enforces its own expected-state preconditions against
// that fresh copy), save the fresh copy, then synchronize the caller's
// in-memory cfg so an in-process reader never observes pre-commit state.
//
// Lock ordering: the env-registry lock and hosttrust's environment-trust
// lock (review.go) are NEVER held together. `add` takes them strictly in
// sequence (trust lock inside Review, released; then this lock at commit);
// `use` reads the trust store lock-free while holding this lock; `review`
// never touches this lock at all. If a future caller must ever nest them,
// the canonical order is env-registry OUTER, environment-trust INNER —
// the order `add`'s sequence already implies — and never the reverse.
//
// `pix config set` (every non-env key) still saves without this lock; that
// whole-file race predates this fix and is EXPLICITLY out of scope here
// (config set refuses the `environment` key outright, so it can revert an
// env registration only the same way it can revert any other concurrent
// key write). What this file guarantees is narrower and load-bearing: no
// two ENV mutations can lost-update each other, and an env mutation never
// commits against state it has not re-read under the lock.
package env

import (
	"fmt"

	"pix/host/config"
	"pix/host/hosttrust"
)

// commitEnvRegistryMutation runs apply against a FRESH under-lock reload of
// the live config, saves that fresh copy, and synchronizes cfg's two
// env-owned fields with what was actually committed. An error from apply
// aborts before any save: a refused commit leaves config.toml
// byte-for-byte as the concurrent writer left it.
func commitEnvRegistryMutation(cfg *config.Config, apply func(fresh *config.Config) error) error {
	return hosttrust.WithLock(config.EnvRegistryLockPath(), func() error {
		fresh, err := config.Load()
		if err != nil {
			return err
		}
		if err := apply(fresh); err != nil {
			return err
		}
		if err := fresh.Save(); err != nil {
			return err
		}
		if cfg != nil && cfg != fresh {
			// Synchronize the caller's ENTIRE in-memory view with the
			// committed state, not merely the two env-owned fields: fresh
			// was just loaded under this same lock, so it already reflects
			// every field any concurrent `pix config set`/pack/etc. writer
			// committed while cfgA sat stale in memory. Copying only
			// Environments/Environment left every OTHER field frozen at
			// whatever cfgA held when it was first loaded — a lost update
			// one level up, sprung the moment this same caller later calls
			// cfg.Save() itself and reverts that concurrent write. *cfg =
			// *fresh is a value copy of the whole struct (maps/slices keep
			// pointing at fresh's underlying data, which is fine: fresh is
			// a throwaway local, never mutated or reused after this).
			*cfg = *fresh
		}
		return nil
	})
}

// ConcurrentRegistrationError is `add`'s deterministic commit-time refusal
// (exit 2) when NAME's registration in the LIVE config no longer matches
// the state this add observed when it started (typically: another pix
// process registered, repointed, or forgot the name while this add's
// review prompt was open). Committing anyway would silently overwrite (or
// resurrect) their mutation — the exact lost update the env-registry lock
// exists to prevent — and overriding a concurrent change must always be a
// deliberate, fresh `add`. Existing == "" means the name was UNREGISTERED
// while this add was running (a concurrent forget), the one shape with no
// "theirs" root to print.
type ConcurrentRegistrationError struct {
	Name      string
	Existing  string // the root the live config now registers (theirs); "" = unregistered
	Attempted string // the canonical root this add reviewed (yours)
}

func (e *ConcurrentRegistrationError) Error() string {
	if e.Existing == "" {
		return fmt.Sprintf(
			"pix: environment %q was unregistered by another process while this add was running.\n"+
			"     yours: %s\n"+
			"     re-run to register it deliberately: pix env add %s %s",
			e.Name, e.Attempted, e.Name, e.Attempted)
	}
	return fmt.Sprintf(
		"pix: environment %q now points at a different root: another process registered it while this add was running.\n"+
			"     theirs: %s\n"+
			"     yours:  %s\n"+
			"     re-run to repoint it deliberately: pix env add %s %s",
		e.Name, e.Existing, e.Attempted, e.Name, e.Attempted)
}
