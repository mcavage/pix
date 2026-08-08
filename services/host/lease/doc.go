//go:build unix

// # Threat model
//
//   - Traversal & symlink refusal: instance IDs are allowlist-validated
//     (ValidateInstanceID) and every leaf file goes through openNoFollow, which
//     ORs in O_NOFOLLOW at open(2) — a symlink at a lease path is refused
//     (ELOOP), never followed. Directories are 0700, files 0600.
//   - O_CLOEXEC is set BY HAND at open(2), because this package uses raw
//     syscall.Open (for O_NOFOLLOW) and so bypasses os.OpenFile's automatic
//     flag. A real child DOES inherit a non-cloexec fd across fork+exec, fully
//     functional, at the same fd number.
//   - PIDs are advisory only. Record.CreatedPID is for a human reading the file;
//     nothing may treat it as proof of liveness or ownership, because a PID is
//     reused the instant its owner exits. The kernel flock is the only
//     correctness primitive, and it is released atomically on the owning fd's
//     close — including on SIGKILL.
package lease
