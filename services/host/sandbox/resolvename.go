package sandbox

import (
	"fmt"
	"regexp"
	"strings"

	"pix/host/stack"
)

// explicitBodyRe is the safe-charset grammar a SHORT logical name (no
// "pix-" prefix — see ScopeExplicitName) must match before it is scoped:
// the same argv/shell-safe posture nameRe already requires of a full name's
// body, applied here to the part a caller actually types.
var explicitBodyRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]*[A-Za-z0-9])?$`)

// ScopeExplicitName resolves what a user typed for `pix run --name` (or any
// other caller accepting a user-chosen sandbox name) into the ONE full,
// stack-scoped name that request may target, and refuses everything else —
// there is no bypass of stack scoping through this seam:
//
//   - a SHORT logical name (does not start with Prefix, and is itself a
//     safe argv token) is SCOPED to stackID: "pix-<stackID>-<name>", and is
//     REFUSED outright if it does not fit MaxNameLen.
//   - a name that is ALREADY a full, safely-charactered pix-* name AND is
//     scoped to stackID (stack.IsScopedSandboxName) travels verbatim — the
//     round-trip case, e.g. re-typing a name `pix ls` just printed.
//   - a name that starts with Prefix but is scoped to a DIFFERENT stack (or
//     carries no stack-id segment at all, the pre-scoping legacy grammar)
//     is REFUSED: this seam never lets one stack's --name reach into or
//     collide with another stack's namespace.
//   - anything outside the safe argv charset (spaces, path separators,
//     shell metacharacters, empty) is REFUSED regardless of prefix.
func ScopeExplicitName(stackID, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("sandbox: empty sandbox name")
	}
	if err := stack.ValidID(stackID); err != nil {
		return "", err
	}

	if strings.HasPrefix(requested, Prefix) {
		if err := validateName(requested); err != nil {
			return "", fmt.Errorf("sandbox: %q is not a safe sandbox name: %w", requested, err)
		}
		if !stack.IsScopedSandboxName(stackID, requested) {
			return "", fmt.Errorf("sandbox: %q is not scoped to this pix stack; pass a short logical name to create a new sandbox here, or the exact name `pix ls` shows for this stack", requested)
		}
		return requested, nil
	}

	if !explicitBodyRe.MatchString(requested) {
		return "", fmt.Errorf("sandbox: %q is not a safe sandbox name (allowed: letters, digits, '-', '_', '.')", requested)
	}
	prefix, err := stack.SandboxPrefix(stackID)
	if err != nil {
		return "", err
	}
	return composeExplicit(prefix, requested)
}

// composeExplicit joins "<prefix><name>" and REFUSES anything longer than
// MaxNameLen. Unlike compose (name.go), there is no digest here to keep the
// result distinct: the caller's own name IS the identity, so cutting it to
// fit is how `--name <sixty-chars>-alpha` and `--name <sixty-chars>-beta`
// silently become one sandbox: the second run would attach to the first
// run's box. A refusal that states the budget is the only answer that cannot
// alias.
func composeExplicit(prefix, name string) (string, error) {
	full := prefix + name
	if len(full) <= MaxNameLen {
		return full, nil
	}
	return "", fmt.Errorf("sandbox: name %q is too long: this pix stack's names carry a %d-character prefix (%s), leaving %d characters for a name, and %q is %d",
		name, len(prefix), prefix, MaxNameLen-len(prefix), name, len(name))
}
