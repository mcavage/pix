package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// Prefix is the namespace every name this package derives or plans against
// lives under. Both Name and PlanRemove key off it.
const Prefix = "pix-"

// DigestLen is the number of hex characters kept from the path digest —
// 8 hex chars (32 bits) is enough entropy that two different workspace
// directories sharing a basename practically never collide, while staying
// short enough to leave real room for a human-legible basename inside
// MaxNameLen.
const DigestLen = 8

// MaxNameLen is the hard cap on a composed name: 63 is the strictest common
// limit in play (RFC1123 DNS label), the same bound
// workflow/task.MaxSandboxNameLen already uses for its own sandbox names.
// Kept as this package's own constant (not an import) — see doc.go on why
// this package takes no intra-module dependency.
const MaxNameLen = 63

// fallbackBase is substituted when the workspace's basename is empty or a
// bare path separator (e.g. deriving a name for "/"), and again if
// sanitizing strips a basename down to nothing (e.g. a directory named only
// in punctuation).
const fallbackBase = "workspace"

// Name derives the deterministic, collision-free sandbox name for workspace:
// "pix-<basename>-<8-hex digest of the canonical path>". Calling it twice
// for the same workspace (even via a different relative spelling, or through
// a symlink) yields the SAME name; calling it for two different directories
// that happen to share a basename yields two DIFFERENT names, because the
// digest is computed over the full canonical path, not the basename alone.
//
// When the composed name would exceed MaxNameLen, ONLY the basename portion
// is truncated — the digest is never shortened, so truncation can never
// reintroduce the basename-collision Name exists to prevent.
func Name(workspace string) string {
	canon := canonicalPath(workspace)
	digest := pathDigest(canon)
	base := sanitizeBase(filepath.Base(canon))
	return compose(base, digest)
}

// canonicalPath resolves workspace to one canonical absolute form so
// "./proj", "/abs/proj", and "/abs/proj/../proj" (and a symlinked path, where
// it exists) all digest identically. A path that does not exist yet (or
// cannot be resolved) degrades to its cleaned absolute spelling rather than
// erroring — Name must never fail merely because the directory isn't there
// yet at naming time.
func canonicalPath(ws string) string {
	abs, err := filepath.Abs(ws)
	if err != nil {
		abs = ws
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

// pathDigest is the first DigestLen hex characters of sha256(canon). canon
// must already be canonicalized by the caller.
func pathDigest(canon string) string {
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:])[:DigestLen]
}

// sanitizeBase keeps a basename safe as a sandbox-name segment: only
// [A-Za-z0-9_-] survive, anything else becomes '-', runs of '-' at either
// edge are trimmed, and an empty/degenerate result (a bare separator, or a
// basename that was ALL punctuation) falls back to fallbackBase so Name
// never returns just "pix-" glued to a digest.
func sanitizeBase(base string) string {
	if base == "" || base == "." || base == string(filepath.Separator) {
		return fallbackBase
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fallbackBase
	}
	return out
}

// compose bounds "pix-<base>-<digest>" to MaxNameLen. If it already fits,
// nothing is trimmed. Otherwise ONLY base is truncated (digest is a fixed
// DigestLen and is never shortened): base is cut to whatever budget remains
// after the fixed prefix, separator, and digest, with any trailing '-' the
// cut exposes trimmed away.
func compose(base, digest string) string {
	full := Prefix + base + "-" + digest
	if len(full) <= MaxNameLen {
		return full
	}
	budget := MaxNameLen - len(Prefix) - len("-") - len(digest)
	if budget < 1 {
		budget = 1 // pathological MaxNameLen; still keep the digest intact
	}
	trimmed := base
	if len(trimmed) > budget {
		trimmed = trimmed[:budget]
	}
	trimmed = strings.TrimRight(trimmed, "-")
	if trimmed == "" {
		trimmed = fallbackBase
		if len(trimmed) > budget {
			trimmed = trimmed[:budget]
		}
		if trimmed == "" {
			trimmed = "w"
		}
	}
	return Prefix + trimmed + "-" + digest
}
