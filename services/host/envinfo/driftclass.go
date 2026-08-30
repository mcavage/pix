package envinfo

import "strings"

// DriftClass is how a set of attributed creation-fingerprint Drifts may be
// handled by a launch that finds an existing sandbox.
//
// The v1 launcher had exactly one answer for every drift: refuse, print
// `pix rm ... && pix run ...`, and make the user run it by hand. That is
// correct for a drift whose recreation could destroy work or widen host
// exposure, and it is pure friction for the common case — a Pix version
// bump changing the pinned template and the pinned kit references, which
// is a construction-time pin and nothing else. Every ordinary image
// upgrade turned into a manual remove/recreate loop.
//
// So drift is classified rather than uniformly refused:
//
//   - DriftRecreationSafe: every changed facet is a Pix-owned,
//     construction-time pin. Nothing inside the sandbox and no host
//     exposure changes meaning; the sandbox merely has to be rebuilt to
//     pick the pin up. A launch MAY recreate automatically, but only
//     after the separate liveness/ownership proofs in
//     workflow/launch.DecideEnvAttach (fresh listing, exact pix-owned
//     instance, zero holders, no keep marker, direct host-mounted
//     workspace).
//   - DriftSubstantive: at least one changed facet is authored
//     configuration or host exposure (a mount, a secret, a binding, an
//     MCP server, an env var, a port), or the whole-environment reset
//     record whose scope is unknown. Recreation is never automatic.
//
// Classification is a property of the drift set alone. It authorizes
// nothing by itself.
type DriftClass int

const (
	// DriftNone means there was nothing to classify.
	DriftNone DriftClass = iota
	// DriftRecreationSafe means every changed facet is a Pix-owned
	// construction-time pin.
	DriftRecreationSafe
	// DriftSubstantive means at least one changed facet is authored
	// configuration or host exposure, or has unknown scope.
	DriftSubstantive
)

func (c DriftClass) String() string {
	switch c {
	case DriftNone:
		return "none"
	case DriftRecreationSafe:
		return "recreation-safe"
	default:
		return "substantive"
	}
}

// recreationSafeComposedKeys is the exact allowlist of composed
// fingerprint keys a recreation-safe drift may touch. It is an allowlist,
// never a denylist: a composed key this package has not classified is
// substantive by construction, so a facet added later fails closed instead
// of silently becoming auto-recreatable.
//
//   - sandboxOptions.template  — the pinned Pix agent image
//   - sandboxOptions.pullPolicy — the pinned pull policy for that image
//
// Kit entries are handled separately (see classifyKey): they are index
// addressed and can also collapse to the "kits[]" group key.
var recreationSafeComposedKeys = map[string]bool{
	"sandboxOptions.template":   true,
	"sandboxOptions.pullPolicy": true,
}

// classifyKey classifies ONE composed fingerprint key.
//
// Kit references are recreation-safe: they are either Pix's own pinned
// mixin/base kits (which move with the Pix version, exactly like the
// template) or an authored kit whose executable content identity is a
// separate host-trust fact. An authored kit change that mattered has
// already invalidated host trust, and DecideEnvAttach requires a STILL
// reviewed environment before it will consider recreating at all, so kit
// drift cannot smuggle unreviewed host execution through this path.
func classifyKey(key string) DriftClass {
	if recreationSafeComposedKeys[key] {
		return DriftRecreationSafe
	}
	if key == "kits[]" || (strings.HasPrefix(key, "kits[") && strings.HasSuffix(key, "]")) {
		return DriftRecreationSafe
	}
	return DriftSubstantive
}

// ClassifyDrifts classifies a whole attributed drift set. The result is the
// WEAKEST classification present: one substantive facet makes the whole set
// substantive, because a launch handles a drift set as one unit.
func ClassifyDrifts(drifts []Drift) DriftClass {
	if len(drifts) == 0 {
		return DriftNone
	}
	class := DriftRecreationSafe
	for _, d := range drifts {
		if classifyKey(d.ComposedKey) == DriftSubstantive {
			class = DriftSubstantive
		}
	}
	return class
}

// RecreationSafe reports whether every drift in the set is a Pix-owned
// construction-time pin. An empty set is NOT recreation-safe: there is
// nothing to recreate for, and a caller that recreated on it would be
// destroying a sandbox for no reason at all.
func RecreationSafe(drifts []Drift) bool {
	return ClassifyDrifts(drifts) == DriftRecreationSafe
}
