//go:build unix

// Package lease implements the hardened per-sandbox lifecycle/ref-lock
// primitives for U04a (Story04, PRD AC-LIFE): an immutable creation record, an
// identity-bound "keep alive" marker, and an flock-backed reference lock that
// lets many holders share liveness while giving a reaper a kernel-verified,
// non-blocking proof that zero holders remain.
//
// # U04c1: lifecycle serialization resharded off live refs
//
// U04a's single lease.lock file served two different jobs at once: many
// concurrent SHARED holders declaring liveness, and the EXCLUSIVE
// zero-holder proof a reaper needs before destroying state. U04c1 splits
// those into two files with two different concurrency shapes:
//
//   - refs.lock (RefLease, OpenRefLease) — unchanged job, many concurrent
//     SHARED holders, non-blocking EXCLUSIVE zero-holder proof.
//   - lifecycle.lock (LifecycleLock, OpenLifecycleLock) — EXCLUSIVE only,
//     one lifecycle transition (create/destroy/state change) at a time,
//     deadline-bounded, meant to be held BRIEFLY.
//
// Lease/Open remain as compatibility aliases for RefLease/OpenRefLease.
//
// AttachRef, WithLifecycle, and TryReapProof (ordering.go) compose the two
// locks correctly so callers never have to get the ordering right by hand:
// AttachRef takes lifecycle EX, then refs SH, then releases lifecycle —
// NEVER lifecycle-EX-forever and NEVER refs EX at all — so a new live
// reference can never deadlock against another process's already-held refs
// SH (shared never blocks shared) while still being serialized against a
// concurrent lifecycle transition. TryReapProof is the reaper's whole safety
// contract in one non-blocking call: it only runs its fn when BOTH the
// lifecycle lock and the refs lock can be proven uncontended right now.
//
// U04c2 wired create/attach onto these (launch.RunSession) and U04d wired the
// LAST-SHELL TEARDOWN onto TryReapProof + ClearState (launch.TeardownSandbox):
// the policy for WHEN to reap lives there, in the launcher, while this package
// still only provides locks and state a caller cannot get wrong.
//
// # Foundation, unix-only
//
// This is L0 foundation work (no domain knowledge, unix syscalls + stdlib
// only — see arch_test.go's pkgLayer map: "lease": layerFoundation), folded
// into the pix/host module: zero non-stdlib dependencies, no separate
// go.mod. Every file here carries //go:build unix — the product compiles on
// a macOS host and a Linux dev/CI box (the latter needed for Linux sandbox
// dev builds, not as a shipped host platform), and Windows support is
// dropped entirely — pix's host lifecycle is macOS-only, so there is no
// Windows degrade variant left anywhere in the module (including
// services/host/lock.go) to contrast against any more.
//
// # What is NOT here (by design — no behavior wiring yet)
//
// No supervisor loop, no policy for WHEN to reap, no wiring to sbx/pi-kit,
// no CLI. Those are integration decisions for the caller (see
// workflow/launch's reap.go, which makes them); this package only gives them
// a lock, a record, a keep and a proven clear they cannot get wrong.
//
// # Threat model / hardening summary
//
//   - Traversal & symlink refusal: instance IDs are allowlist-validated
//     (ValidateInstanceID) and every leaf file this package opens goes through
//     openNoFollow, which ORs in O_NOFOLLOW at the open(2) syscall — a
//     pre-existing symlink at a lease path is refused (ELOOP), never followed.
//   - Directories are created 0700, files 0600 — never group/other readable,
//     let alone writable.
//   - The lease lock fd is opened with O_CLOEXEC set explicitly at open(2)
//     time (not left to a runtime default): this package uses raw
//     syscall.Open (for O_NOFOLLOW), which bypasses os.OpenFile's own
//     automatic O_CLOEXEC, so the flag must be — and is — set by hand. See
//     lock_test.go's TestCLOEXEC_ChildDoesNotInheritLeaseFd for why this
//     matters: a real child process DOES inherit a non-cloexec fd across
//     fork+exec, fully functional, at the same fd number.
//   - PIDs are advisory only. Record.CreatedPID exists for a human reading
//     the record on disk; nothing in this package treats a PID as proof of
//     liveness or identity, because PIDs are reused the instant their owner
//     exits. The only correctness primitive is the kernel flock, which the
//     kernel releases atomically on the owning fd's close — including an
//     abrupt SIGKILL, with no cleanup handler required and no window for a
//     leaked reference. See TestSIGKILL_ReleasesLock.
package lease
