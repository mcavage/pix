package hosttrust

import (
	"os"
	"path/filepath"
	"strings"
)

// Subject is the opaque identity an acceptance record is keyed by: a KIND
// namespace (a pack today; a future environment kind tomorrow) plus a
// CANONICAL ROOT. Two subjects sharing a Root but differing in Kind are
// DIFFERENT keys (see Key) — so a pack record and an environment record can
// never collide merely because they happen to point at the same directory.
type Subject struct {
	Kind string
	Root string
}

// Key returns the opaque string an AcceptanceStore indexes by: kind-namespaced
// so two subject kinds addressing the same Root never collide. It is
// intentionally not human-legible (a raw NUL separator, not a delimiter a
// Kind or Root could plausibly contain) — nothing round-trips through it,
// only equality matters.
func (s Subject) Key() string { return s.Kind + "\x00" + s.Root }

// CanonicalRoot normalizes a filesystem root path for identity comparison:
// expand a leading ~, then Abs + Clean (falling back to Clean alone if Abs
// fails). This is the SAME algorithm packinfo.CanonicalizePackRoot has always
// used for pack identity, duplicated here rather than imported: packinfo is
// itself an L1 capability, and L1 may not import L1 (architecture.md; see
// arch_test.go). The two copies are the price of that boundary — every
// caller of either one gets a byte-identical answer for the same input.
func CanonicalRoot(p string) string {
	p = expandUser(strings.TrimSpace(p))
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// expandUser expands a leading ~ to $HOME, mirroring packinfo.ExpandUser for
// the same reason CanonicalRoot mirrors packinfo.CanonicalizePackRoot.
func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
