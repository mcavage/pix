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
//     safe argv token) is SCOPED to stackID: "pix-<stackID>-<name>",
//     truncated to fit MaxNameLen exactly like a derived name is.
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
	return composeExplicit(prefix, requested), nil
}

// composeExplicit bounds "<prefix><name>" to MaxNameLen. Unlike compose
// (name.go), there is no fixed-length digest suffix to preserve here — the
// caller's own name IS the identity — so truncation, when needed at all,
// simply cuts name to whatever budget remains after prefix.
func composeExplicit(prefix, name string) string {
	full := prefix + name
	if len(full) <= MaxNameLen {
		return full
	}
	budget := MaxNameLen - len(prefix)
	if budget < 1 {
		budget = 1
	}
	trimmed := name
	if len(trimmed) > budget {
		trimmed = trimmed[:budget]
	}
	trimmed = strings.TrimRight(trimmed, "-")
	if trimmed == "" {
		trimmed = "n"
	}
	return prefix + trimmed
}
