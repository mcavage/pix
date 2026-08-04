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
// BEFORE the caller-scope (Prefix) check in PlanRemove ever gets to decide
// whether it's in scope — an unsafe name is rejected regardless of prefix.
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
func PlanRemove(name string) ([]string, error) {
	if !strings.HasPrefix(name, Prefix) {
		return nil, fmt.Errorf("sandbox: refusing to plan removal of %q: outside the %s* namespace", name, Prefix)
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	return []string{"rm", name}, nil
}
