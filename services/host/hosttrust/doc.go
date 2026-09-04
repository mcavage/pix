// Package hosttrust is the L1 capability behind pix's Tier-1 host-exec trust
// gate (docs/design/packs.md §9): everything that is true of "does the
// operator consent to this running on THEIR machine" regardless of WHAT is
// asking. It was extracted, PURE mechanism only, from workflow/pack's
// trust.go/truststore.go — a pack is today's only subject kind, but a future
// native environment (Story E1) is another one, and neither this package nor
// its callers should have to duplicate canonical identity, fingerprinting,
// the acceptance-record shape, or the flock-serialized read-modify-write
// dance a second time.
//
// It owns five things, each in its own file:
//
//   - identity.go   — Subject{Kind, Root}: the opaque identity an acceptance
//     record is keyed by, plus CanonicalRoot, the filesystem-root
//     normalization every identity is built from.
//   - fingerprint.go — Canonicalize + CanonicalDoc + Fingerprint: the
//     canonical-JSON-then-sha256 engine behind every host-exec fingerprint.
//     The caller still owns what gets marshaled (field order, sorted slices,
//     omitempty) — that is domain knowledge — but Fingerprint takes only a
//     CanonicalDoc (producible solely via Canonicalize), so "canonicalize
//     before you hash" is a compile-time constraint, not a comment a caller
//     has to trust.
//   - hash.go       — IsSymlink and HashFile: the content-hashing half of a
//     fingerprint, symlink-refused.
//   - store.go      — Record (the ONE acceptance-record shape) and
//     AcceptanceStore (a Subject-keyed map of them), plus the launcher-owned
//     document primitives: WithLock (the cross-process flock), and
//     ReadDocumentBytes/SaveDocument (symlink-refused read/atomic write).
//   - mutate.go     — LoadMutateSave: the fresh-load -> mutate -> save shape
//     every trust-document write uses, kept in a file that imports NOTHING
//     lock-related (see its doc comment) so nesting a second lock acquisition
//     inside an already-locked mutation is impossible by construction, not
//     merely forbidden by convention.
//
// # What this package deliberately is NOT
//
// It has no knowledge of packs, environments, MCP servers, or anything else
// that HAS a host-exec surface — that is domain data the caller marshals and
// hands in. It never decides whether a surface is Tier-0 or Tier-1, never
// renders a consent screen, and never chooses a subject's Kind string. Per
// docs/design/architecture.md's L1 contract it MAY NOT import a capability
// sibling (packinfo, sandbox, ...): CanonicalRoot and IsSymlink duplicate a
// few lines packinfo already has for the identical reason — the two copies
// are the price of that boundary, not drift (see each function's doc
// comment).
package hosttrust
