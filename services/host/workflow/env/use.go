// use.go — E1.11's `pix env use NAME`: docs/design/environments.md §8.1
// ("`use NAME` only changes the machine default. It performs no adoption,
// host registration, or probing. An unreviewed environment is refused."),
// PRD §5.5, AC-13/14.
//
// Use is deliberately thin: it reloads and re-validates NAME exactly as
// `env show`/`env review` always do (so a broken or drifted environment is
// refused here identically, never a softer check), requires a Tier1
// environment's CURRENT host-exec fingerprint to already be accepted, and
// then performs the ONE mutation the verb exists for — cfg.Environment —
// through config.UseEnvironment, which touches no other key. It never
// registers, scaffolds, prompts, or launches anything.
package env

import (
	"fmt"

	"pix/host/cli"
	"pix/host/config"
)

// UseNotReviewedError is Use's refusal (AC-14) when NAME's host-exec
// fingerprint, recomputed right now, is not the fingerprint already on
// record for its root. Two distinct causes collapse into one type with a
// Changed discriminant, because both demand the exact same fix: Changed
// == false means nothing was ever accepted for this root at all (a fresh
// registration, or a repoint — AC-16 already guarantees a repoint never
// inherits a record); Changed == true means a record exists but no longer
// matches (the environment's host-exec surface moved since it was
// reviewed). Either way the only honest next step is the same command.
type UseNotReviewedError struct {
	Name    string
	Changed bool
}

// Error follows the design doc's own "changed" refusal shape (docs/design/
// environments.md §8's example errors) for the Changed case, and its
// unreviewed-registration counterpart otherwise — both self-prefixed
// "pix: " (cmd/pix/env_cmd.go's envRun is what keeps that from ever
// doubling), and both naming exactly one runnable fix: `pix env review
// NAME`, never an inline prompt (`use` is not `review` — D5's "no inline
// [y/N]" reasoning applies here too: a default-selection command is not
// the place to ask someone to accept a host-execution bill).
func (e *UseNotReviewedError) Error() string {
	if e.Changed {
		return fmt.Sprintf(
			"pix: environment %q changed what it runs on your host.\n     review it: pix env review %s",
			e.Name, e.Name)
	}
	return fmt.Sprintf(
		"pix: environment %q has not been reviewed.\n     review it: pix env review %s",
		e.Name, e.Name)
}

// Use resolves name to a trustworthy, freshly loaded *Environment (Load —
// the SAME exact-name resolution, location refusals, and strict parse
// every other verb in this package shares), and — only for a Tier1
// (host-executing) environment — requires its CURRENT bill-of-materials
// fingerprint to match an already-accepted record for its canonical root.
// A Tier0 environment has nothing to review (Review's own short-circuit:
// "Tier0 ... return accepted with NO output"), so Use never gates one.
//
// The WHOLE of Use — resolve, validate, gate, set, save — runs under the
// env-registry lock (commit.go), against a FRESH under-lock reload of the
// live config, never the caller's possibly stale cfg (Wave C security L1):
// a name another process just forgot refuses as unknown, a name another
// process just repointed is validated as what it points at NOW, and the
// save is of the fresh document, so a registration committed by a
// concurrent process is never reverted. Holding the lock across the whole
// verb is the simpler of the two safe shapes and is safe HERE because Use
// never prompts: its longest step is a bounded filesystem walk, nowhere
// near the flock's 30-second budget (contrast add.go's
// commitAddRegistration, which must go optimistic because of its prompt).
// On success the caller's cfg is synchronized with the committed state;
// on any refusal both cfg and the file are left completely untouched.
//
// lookPath is threaded straight to Load/ComputeBoM, exactly as
// env_cmd.go's `env review`/`env show` already do; nil defaults to the
// real exec.LookPath (ResolveLocalCommand's own contract).
func Use(cfg *config.Config, name string, lookPath func(string) (string, error)) error {
	return commitEnvRegistryMutation(cfg, func(fresh *config.Config) error {
		ts, err := loadEnvironmentTrustStore()
		if err != nil {
			return err
		}
		loaded, err := Load(fresh, &ts.AcceptanceStore, name, nil, lookPath)
		if err != nil {
			return err
		}
		bom, err := ComputeBoM(loaded, nil, lookPath)
		if err != nil {
			return err
		}
		if bom.Tier1() {
			fp, err := Fingerprint(bom)
			if err != nil {
				return err
			}
			rec, ok := ts.Get(loaded.Subject)
			if !ok {
				return cli.UsageError{Err: &UseNotReviewedError{Name: name}}
			}
			if rec.Fingerprint != fp {
				return cli.UsageError{Err: &UseNotReviewedError{Name: name, Changed: true}}
			}
		}
		return fresh.UseEnvironment(name)
	})
}
