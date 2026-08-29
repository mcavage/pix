// forget.go — E1.11's `pix env forget NAME`: docs/design/environments.md
// §8.1 ("`forget NAME` unregisters the registration and never deletes the
// environment directory: the source is untouched. It refuses the current
// default and refuses a live holder, with no override of either
// refusal."), PRD §5.5, AC-15/42/44.
//
// Forget is UNREGISTER, never delete: its only possible mutation is the
// same one Unregister (registry.go) already performs — cfg.Environments
// and, if name was it, cfg.Environment — and it never touches the
// environment's own directory, trust store, or any other file on disk. It
// adds exactly two refusals on top of Unregister's own "does this name
// exist" check, both fail-closed and neither overridable by a flag: NAME
// is the current machine default, or a live sandbox still references it.
package env

import (
	"fmt"

	"pix/host/cli"
	"pix/host/config"
)

// HolderProbe is Forget's injectable, fail-closed live-holder check: given
// name, report whether any live sandbox currently references it as its
// selected environment, or an error when the probe could not determine an
// answer at all. Forget treats a probe ERROR identically to `held == true`
// — "cannot prove nothing holds it" refuses exactly as "something holds
// it" does; there is no in-between reading of an inconclusive probe.
//
// NoLiveHolders below is the correct answer for what exists TODAY, not a
// stand-in for a feature that has not been written yet: no `pix env`
// launch cutover exists (Wave D; see show.go's own "sandbox: unknown ...
// lands with a later wave" line), so no live sandbox anywhere associates
// itself with an environment NAME yet, and "not held" is the only thing a
// probe can honestly report. The type is the seam a future Wave D launch
// path slots a real, sandbox-aware probe into without Forget itself
// changing.
type HolderProbe func(name string) (held bool, err error)

// NoLiveHolders is the default HolderProbe (see its doc comment on this
// package's current, launch-cutover-free state).
func NoLiveHolders(name string) (bool, error) { return false, nil }

// ForgetCurrentDefaultError is Forget's refusal when name is the machine's
// current default (cfg.Environment): unregistering it would leave the
// default naming a registration that no longer exists, exactly the
// dangling state config.RemoveEnvironment's own doc comment says a default
// "may never" reach — Forget refuses OUTRIGHT rather than silently
// clearing the default out from under the caller.
type ForgetCurrentDefaultError struct{ Name string }

func (e *ForgetCurrentDefaultError) Error() string {
	return fmt.Sprintf(
		"pix: environment %q is the current default; forget refuses to leave it dangling.\n     default: %s\n     pick a different default first: pix env use <name>",
		e.Name, e.Name)
}

// ForgetLiveHolderError is Forget's refusal when a HolderProbe reports name
// is still referenced by a live sandbox (Held == true), or could not prove
// otherwise (Unknown == true) — both fail closed to the identical outcome,
// distinguished here only so the two messages can each name their own
// ground truth honestly.
type ForgetLiveHolderError struct {
	Name    string
	Unknown bool
}

func (e *ForgetLiveHolderError) Error() string {
	if e.Unknown {
		return fmt.Sprintf(
			"pix: could not confirm no live sandbox still references environment %q; forget refuses without a fresh, positive probe.\n     probe: inconclusive\n     retry: pix env forget %s",
			e.Name, e.Name)
	}
	return fmt.Sprintf(
		"pix: environment %q is still held by a live sandbox; forget refuses to unregister it while one is running.\n     holder: a live sandbox\n     remove the sandbox first: pix rm <sandbox>",
		e.Name)
}

// Forget resolves name, then refuses (in order) an unknown name, the
// current-default case, and a positive-or-unknown live-holder probe,
// before delegating to Unregister for the ONE actual mutation. On success
// it returns the canonical root that survives untouched — Unregister
// deletes nothing, moves nothing, and this function never reads or writes
// anything under it — so a caller can name that surviving path in its own
// success line.
//
// probe is threaded straight from the caller (cmd/pix's `env forget`); a
// nil probe defaults to NoLiveHolders.
func Forget(cfg *config.Config, name string, probe HolderProbe) (root string, err error) {
	root, ok := Root(cfg, name)
	if !ok {
		return "", cli.UsageError{Err: &config.UnknownEnvironmentError{Name: name, Known: Known(cfg)}}
	}
	if cfg.Environment == name {
		return "", cli.UsageError{Err: &ForgetCurrentDefaultError{Name: name}}
	}
	if probe == nil {
		probe = NoLiveHolders
	}
	held, perr := probe(name)
	if perr != nil {
		return "", cli.UsageError{Err: &ForgetLiveHolderError{Name: name, Unknown: true}}
	}
	if held {
		return "", cli.UsageError{Err: &ForgetLiveHolderError{Name: name}}
	}
	Unregister(cfg, name)
	return root, nil
}
