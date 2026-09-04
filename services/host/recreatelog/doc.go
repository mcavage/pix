// Package recreatelog is a local-only, bounded diagnostic log of environment
// recreate-boundary drift (docs/design/environments.md section 10.2: Pix P0
// treats every effective declaration change as recreate-only). It answers
// exactly one question later, for a human: "what canonical keys changed the
// last time this environment needed a recreate" — never "to what", and never
// anything about how the environment authenticates or runs commands.
//
// # Shape is the whole safety story
//
// Record carries exactly three fields — a timestamp, an environment name, and
// the canonical KEY PATHS that changed (e.g. "mcp.local.command",
// "host.mcp.probe") — because sandbox.Fingerprint.Diff already produces
// exactly that vocabulary: the set of keys whose value differed, never the
// values themselves (see sandbox/fingerprint.go). A facet VALUE, a credential
// NAME, an argv, or a filesystem path outside the environment root has no
// field to land in: the API accepts only (dir, environment, changedKeyPaths
// []string), so there is nothing else a caller could pass even by accident,
// and Read parses with DisallowUnknownFields so a hand-edited or foreign-
// written file cannot smuggle a fourth field back in past this package's own
// writer.
//
// # Local-only, bounded, and never configuration
//
// The log lives under a caller-supplied state DIRECTORY (never a name this
// package invents on its own), holds at most MaxRecords entries — a compile-
// time constant, deliberately never read from config.toml or an env var, so
// no host can be talked into retaining more diagnostic history than this
// package ever intended — and is written with an flock-guarded, atomic
// temp-file-then-rename swap so two host commands recreating overlapping
// environments at once can never corrupt each other's write or silently lose
// one appender's record (see TestAppend_ConcurrentAppendsAllPersist). A
// missing or deleted log file reads as zero records, never an error: this is
// diagnostic history, not a durability contract, and its absence is not a
// bug report. Malformed content is the opposite case and is NOT tolerated —
// Read fails closed on it, because silently discarding an unreadable log
// would hide the exact drift a human is trying to inspect.
//
// # No L1 siblings, no network
//
// recreatelog holds zero imports from this module (see guard_test.go's F10)
// and none from net/net-http/crypto-tls: it is placed L1 in ../arch_test.go
// with no capability sibling importing it and none it imports in turn. It
// never wires into `pix doctor` or any other workflow — that composition, if
// it ever happens, belongs to whichever L3 workflow calls sandbox.Diff and
// decides to log the result, not to this package.
//
// # Cross-platform: unix and windows both get a real implementation
//
// The API, the record shape, the flock-guarded atomicity, and the
// symlink-refusing reads/writes are IDENTICAL on every platform: only the two
// primitives an operating system does not standardize — the advisory
// exclusive lock and the "refuse to open through a symlink" check — are
// split into platform files (lock_unix.go/lock_windows.go for the lock,
// alongside each platform's own openNoFollow). Windows has no flock, so
// lock_windows.go uses LockFileEx/UnlockFileEx over the same maximal byte
// range gofrs/flock and other cross-platform Go file-lockers use for "the
// whole file, however large it grows" — polled under the SAME
// appendLockTimeout deadline as unix, so a wedged holder times out
// identically on both platforms rather than hanging forever on one of them.
// Windows also has no O_NOFOLLOW at open(2), so its openNoFollow falls back
// to the Lstat-then-open sequence this codebase already uses for the same
// gap elsewhere (hosttrust/nofollow_other.go, workspace/state_other.go):
// this narrows, but does not fully close, the TOCTOU window a symlink
// swapped in between the Lstat and the Open could exploit. No build in this
// tree currently exercises Windows end to end, so that fallback is a
// good-faith implementation of the same primitive, not a hardened guarantee
// — never an unimplemented stub that skips the check outright.
package recreatelog
