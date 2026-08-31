// Package stack is the single producer of a PIX_HOME's stable stack
// identity and every resource name derived from it. "One PIX_HOME = one
// stack" (the Wave A coexistence design): a stack ID lets more than one
// PIX_HOME cleanly coexist on the same host — its own sandboxes, its own
// pix-memory container, its own reserved MCP names, its own locally-loaded
// image tags — without colliding with another PIX_HOME's copies of the
// same resources.
//
// It owns two things:
//
//   - id.go    — ID derives the stack ID from a canonicalized PIX_HOME path:
//     the first IDLen lowercase hex characters of sha256(canonical home).
//     Current() derives it from pixhome.Dir() so a caller never has to
//     canonicalize a home path by hand. Canonicalization is filepath.Abs,
//     then filepath.EvalSymlinks when the path resolves, then
//     filepath.Clean — the same shape sandbox.Name already uses for a
//     workspace path, applied here to PIX_HOME itself.
//   - names.go — every scoped resource name this stack's later callers need
//     (sandbox name prefix, the pix-memory container name, the two reserved
//     MCP names, and the locally-loaded image tag grammar), plus the two
//     predicates that tell a scoped name apart from an unscoped or
//     differently-scoped one.
//
// Every naming helper VALIDATES the id it is given and returns an error on
// anything malformed, rather than composing an unsafe name or silently
// falling back to the old, unscoped global name (bare "pix-memory",
// "pix-session", "pix-<basename>-<digest>"): a malformed id here is a bug in
// the caller, and papering over it with a global name is exactly the
// collision this package exists to prevent.
//
// This package is dependency-light like pixhome itself: it imports ONLY
// pixhome (for Current's PIX_HOME resolution), no domain-capability
// package, so a later capability (sandbox, container, mcp, envinfo, ...) is
// free to depend on it without creating a cycle.
package stack
