package sandbox

import "sort"

// Fingerprint is a named set of identity components describing how a
// sandbox was created (e.g. "image", "kit", "workspace_digest",
// "static_mcp"). The CALLER decides what goes in it and how each value is
// computed — mirroring workflow/pack's ComputeHostExecFingerprint, which
// hashes a pack's own host-exec surface for its own trust store. This
// package only COMPARES two fingerprints and reports which keys diverged;
// deciding what a divergence should DO (warn, block, force a replace) is an
// integration decision left to the caller.
type Fingerprint map[string]string

// Diff compares stored (recorded at creation) against current (freshly
// computed) and returns the keys whose values differ, sorted for stable
// output. A key present on only one side counts as differing too — an
// added or removed key is drift, not something to silently ignore.
func Diff(stored, current Fingerprint) []string {
	seen := map[string]bool{}
	var out []string
	for k, sv := range stored {
		seen[k] = true
		if cv, ok := current[k]; !ok || cv != sv {
			out = append(out, k)
		}
	}
	for k := range current {
		if seen[k] {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Equal reports whether stored and current have no differing keys.
func Equal(stored, current Fingerprint) bool {
	return len(Diff(stored, current)) == 0
}

// FromFacetMap adapts any facet-keyed map[string]string — e.g. envinfo's
// ComputeFingerprint (E2.2), whose composed-facet key space ("env.FOO",
// "mcp.servers[github].url", "kits[2]") is richer than SessionFingerprint's
// small "static_mcp"/"template" set — into a Fingerprint, so Diff/Equal
// work unchanged over it. This package never derives, resolves, or hashes
// a facet value itself (this file's own doc comment: "The CALLER decides
// what goes in it"); FromFacetMap is a straight type conversion, not a
// second comparison engine, and it never imports envinfo or any other L1
// sibling to do it — the caller hands over a plain map, already computed.
func FromFacetMap(facets map[string]string) Fingerprint {
	out := make(Fingerprint, len(facets))
	for k, v := range facets {
		out[k] = v
	}
	return out
}
