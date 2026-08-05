// Package sandbox is the pure, dependency-free L1 sandbox domain: name a
// sandbox, read what `sbx`/docker say about it, plan what to run next. It owns
// four things, each in its own file:
//
//   - name.go       — a deterministic, collision-free sandbox name:
//     "pix-<basename>-<8-hex path digest>", truncated (basename only, digest
//     always intact) to fit the RFC1123 label cap. Two different directories
//     that share a basename get different names; the same directory always
//     gets the same name.
//   - list.go        — a tolerant JSON parser for an `sbx`-style sandbox
//     listing: a tri-state State (Running/Stopped/Unknown — never guessed)
//     per row, and an optional, immutable InstanceID. See "Schema posture"
//     below for why it accepts documented ALIASES rather than one pinned
//     shape, and how it flags a row whose identity it cannot vouch for.
//   - argv.go        — pure create-vs-exec argv planning: `-it` when a TTY is
//     wanted, `-i` otherwise, and a fail-closed PlanLaunch that refuses to
//     guess when List's parse could not verify identity or state.
//   - fingerprint.go — comparing a STORED fingerprint (recorded at creation)
//     against a freshly computed one and reporting which named keys diverged.
//   - remove.go      — planning a NON-FORCE removal (`rm`, no `-f`), scoped to
//     names in this package's own pix-* namespace.
//
// # What this package deliberately is NOT
//
// It never execs sbx, docker, or anything else. It never deletes, force-
// removes, or auto-schedules a removal — every "plan" function returns argv
// (or an error) and stops there; running it, confirming it with a user, and
// deciding WHEN a divergent fingerprint or a stopped sandbox warrants action
// are integration decisions for a caller, not this package's job. It has no
// CLI and no config (see docs/design/architecture.md's L1-capability
// contract: "one domain each; MAY NOT import each other").
//
// # Schema posture: no ground truth, so degrade honestly
//
// There is no authoritative schema for what an installed `sbx` emits for a
// sandbox listing, and different sbx versions plausibly differ, so list.go
// accepts a documented set of ALIAS field names (nameKeys/stateKeys/idKeys)
// instead of refusing anything that isn't one exact shape. But leniency does
// not imply trust: a row is IdentityVerified only when every field it supplied
// used the CANONICAL key (not a fallback alias) and carried no key outside the
// fully documented set. A row that had to lean on an alias, or came with a key
// this package has never heard of, parses successfully but reports
// IdentityVerified=false — the same "fail closed on uncertainty, never guess"
// posture this package's own State four-state and workflow/launch's
// PlanSandboxLaunch already use for sandbox liveness. PlanLaunch (argv.go)
// enforces this: it refuses to plan create-vs-exec against an unverified row.
package sandbox
