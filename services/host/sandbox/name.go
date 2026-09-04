package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"

	"pix/host/stack"
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
// this package takes no intra-module dependency beyond the L0 stack package.
const MaxNameLen = 63

// fallbackBase is substituted when the workspace's basename is empty or a
// bare path separator (e.g. deriving a name for "/"), and again if
// sanitizing strips a basename down to nothing (e.g. a directory named only
// in punctuation).
const fallbackBase = "workspace"

// Name derives the deterministic, collision-free, STACK-SCOPED sandbox name
// for workspace under THIS process's current stack (stack.Current(), i.e.
// this PIX_HOME): "pix-<stack16>-<basename>-<8-hex digest of the canonical
// path>". Two different PIX_HOMEs naming the SAME workspace derive two
// DIFFERENT names — the coexistence property this package exists to give —
// while calling it twice within one PIX_HOME (even via a different relative
// spelling of workspace, or through a symlink) yields the SAME name.
//
// An error here means this process's own stack identity could not be
// resolved (see stack.Current) — a caller must never fall back to composing
// an unscoped name in that case, the same posture stack's own naming
// helpers hold.
func Name(workspace string) (string, error) {
	return NameFor(workspace, "")
}

// NameFor derives the deterministic sandbox name for one (workspace,
// environment) pair under this process's current stack — the v2 identity
// rule (PRD: "Sandbox names are deterministic from canonical workspace and
// environment and remain `pix-*` scoped").
//
// Two runs of the same project under two different environments are two
// different credential and host-execution contexts, so they must be two
// different sandboxes; keying the name on the workspace alone would make
// the second `pix run --env other` silently attach to the first one's
// sandbox. An empty env name reproduces Name(workspace) byte for byte, so
// the no-environment case keeps its existing identity and no already
// running sandbox is renamed out from under its records.
func NameFor(workspace, env string) (string, error) {
	id, err := stack.Current()
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve this stack's identity: %w", err)
	}
	return NameForStack(id, workspace, env)
}

// NameForStack is Name/NameFor's pure core: it derives the name for
// workspace (and, when env is non-empty, the (workspace, env) pair) SCOPED
// TO stackID, taking no dependency on the current process's own PIX_HOME.
// Name/NameFor are thin convenience wrappers over this that resolve
// stackID from stack.Current(); a caller that already holds a resolved
// stack ID (e.g. one derived from an explicit PIX_HOME path, or in a test
// exercising two stacks side by side) calls this directly instead of
// re-resolving stack.Current() a second time.
//
// stackID is validated (stack.ValidID) before anything is composed: a
// malformed id is refused rather than silently producing an unscoped or
// wrongly-scoped name.
func NameForStack(stackID, workspace, env string) (string, error) {
	prefix, err := stack.SandboxPrefix(stackID)
	if err != nil {
		return "", err
	}
	canon := canonicalPath(workspace)
	seed := canon
	env = strings.TrimSpace(env)
	if env != "" {
		seed = canon + "\x00" + env
	}
	digest := pathDigest(seed)
	base := sanitizeBase(filepath.Base(canon))
	return compose(prefix, base, digest), nil
}

// canonicalPath resolves workspace to one canonical absolute form so
// "./proj", "/abs/proj", and "/abs/proj/../proj" (and a symlinked path,
// where it exists) all digest identically — stack.CanonicalPath's own rule,
// reused here rather than duplicated. A path that does not exist yet (or
// cannot be resolved) degrades to its cleaned absolute spelling rather than
// erroring — Name must never fail merely because the directory isn't there
// yet at naming time.
func canonicalPath(ws string) string {
	if canon, err := stack.CanonicalPath(ws); err == nil {
		return canon
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		abs = ws
	}
	return filepath.Clean(abs)
}

// pathDigest is the first DigestLen hex characters of sha256(seed), via
// stack's own HashPrefix — the ONE hashing routine this module uses for a
// collision-resistant name component, rather than a second, duplicate
// crypto/sha256 call kept here.
func pathDigest(seed string) string {
	return stack.HashPrefix(seed, DigestLen)
}

// sanitizeBase keeps a basename safe as a sandbox-name segment: only
// [A-Za-z0-9_-] survive, anything else becomes '-', runs of '-' at either
// edge are trimmed, and an empty/degenerate result (a bare separator, or a
// basename that was ALL punctuation) falls back to fallbackBase so Name
// never returns just the prefix glued to a digest.
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

// compose bounds "<prefix><base>-<digest>" to MaxNameLen, where prefix is
// already a full stack-scoped namespace (e.g. "pix-<stack16>-", including
// its trailing separator). If it already fits, nothing is trimmed.
// Otherwise ONLY base is truncated (digest is a fixed length and is never
// shortened): base is cut to whatever budget remains after prefix, the
// separator, and digest, with any trailing '-' the cut exposes trimmed
// away.
func compose(prefix, base, digest string) string {
	full := prefix + base + "-" + digest
	if len(full) <= MaxNameLen {
		return full
	}
	budget := MaxNameLen - len(prefix) - len("-") - len(digest)
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
	return prefix + trimmed + "-" + digest
}
