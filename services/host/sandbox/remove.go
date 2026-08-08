package sandbox

import (
	"fmt"
	"regexp"
	"strings"
)

// nameRe allowlists what a name PlanRemove will accept once the Prefix
// check passes: printable, shell-metacharacter-free, no path separators —
// the same defensive posture lease.ValidateInstanceID uses for its own
// identifiers, applied here to a name this package is about to hand to an
// external `rm` invocation's argv.
var nameRe = regexp.MustCompile(`^` + Prefix + `[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$`)

// validateName rejects an empty, oversized, or unsafely-charactered name
// BEFORE the caller-scope (Prefix) check ever gets to decide whether it's in
// scope — an unsafe name is rejected regardless of prefix.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("sandbox: empty name")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("sandbox: name %q exceeds %d bytes", name, MaxNameLen)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("sandbox: name %q must match %s", name, nameRe.String())
	}
	return nil
}

// validateScopedName is the ONE check both PlanRemove and PlanForceRemove run
// before composing any argv: a name outside this domain's own Prefix
// namespace is refused regardless of unsafe characters, and an unsafe name is
// refused regardless of prefix. Neither planner may diverge from this check —
// that would reopen exactly the gap PlanForceRemove exists to close (see its
// doc comment): a name-safety or scope rule that only one of the two argv
// shapes enforces is a rule an integrator can bypass just by picking the
// other shape.
func validateScopedName(name string) error {
	if !strings.HasPrefix(name, Prefix) {
		return fmt.Errorf("sandbox: refusing to plan removal of %q: outside the %s* namespace", name, Prefix)
	}
	return validateName(name)
}

// PlanRemove composes the argv to remove name WITHOUT force (no `-f`/
// `--force`): deleting a sandbox that might still be doing something is an
// integrator's call, gated behind whatever confirmation or lease-check it
// wants layered on top — this package only ever plans a PLAIN remove. It
// also refuses any name outside this domain's own Prefix namespace: a
// planner that manages what pix created must never be handed an arbitrary
// sbx/docker name to remove.
//
// PlanRemove never executes anything and never schedules auto-removal; it
// returns argv (or an error) and stops there.
//
// NOTE on sbx v0.38: a bare `rm` now prompts for confirmation and refuses
// outright with no TTY attached — which every caller in this codebase is,
// since pix-host's own subprocess calls never attach one. PlanRemove is kept
// as the pure "what does a plain removal look like" argv (and stays exactly
// what its own tests pin), but no automated pix caller can actually use it
// against a live sbx anymore; see PlanForceRemove for the shape they use
// instead, and why that is not a widening of what may be removed.
func PlanRemove(name string) ([]string, error) {
	if err := validateScopedName(name); err != nil {
		return nil, err
	}
	return []string{"rm", name}, nil
}

// PlanForceRemove composes the argv for a removal that passes `-f`/`--force`
// to sbx, through the EXACT SAME pix-* scope and name-safety validation as
// PlanRemove — there is no second, looser check anywhere in this package.
//
// The `-f` it plans is a TRANSPORT detail, not a widened authority. On sbx
// v0.38, a bare `rm` prompts for confirmation and refuses outright with no
// TTY attached; `-f` only tells sbx to skip a question that a non-interactive
// caller could never answer anyway. It is NOT permission to remove something
// pix itself has not already decided is safe to remove — that decision is
// made entirely by the CALLER, before this function is ever reached, and by
// exactly one of two proofs:
//
//   - a kernel-verified, zero-holder reference proof (no shell still
//     references the sandbox, no lifecycle transition in flight) — the
//     automatic last-shell teardown and the orphan sweep's authorization; or
//   - an explicitly, individually named removal intent with nothing left to
//     prove a reference against (no lease state at all) — never a wildcard
//     (`--all`/`--orphans`), never a name this caller merely guessed.
//
// A caller reaching for PlanForceRemove to skip EITHER of those proofs, or to
// widen scope past pix-*, is misusing it: this function only ever composes
// argv, it does not and cannot re-derive or check either proof itself.
func PlanForceRemove(name string) ([]string, error) {
	if err := validateScopedName(name); err != nil {
		return nil, err
	}
	return []string{"rm", "-f", name}, nil
}
