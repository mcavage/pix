// Package sandbox is the pure, dependency-free L1 sandbox domain: name a
// sandbox, read what `sbx`/docker say about it, plan what to run next. It owns
// four things, each in its own file:
//
//   - name.go       — a deterministic, collision-free sandbox name:
//     "pix-<basename>-<8-hex path digest>", truncated (basename only, digest
//     always intact) to fit the RFC1123 label cap. Two different directories
//     that share a basename get different names; the same directory always
//     gets the same name.
//   - list.go        — a JSON parser for an `sbx`-style sandbox listing,
//     against one of two pinned canonical profiles (legacy bare-array,
//     v0.38 `{"sandboxes": [...]}`): a tri-state State (Running/Stopped/
//     Unknown — never guessed) per row, and an optional (v0.38: required,
//     UUID-shaped), immutable InstanceID. See "Schema posture" below for
//     the two profiles and how a row flags identity it cannot vouch for.
//   - argv.go        — pure create-vs-exec argv planning: `-it` when a TTY is
//     wanted, `-i` otherwise, and a fail-closed PlanLaunch that refuses to
//     guess when List's parse could not verify identity or state.
//   - fingerprint.go — comparing a STORED fingerprint (recorded at creation)
//     against a freshly computed one and reporting which named keys diverged.
//   - remove.go      — planning a removal, scoped to names in this package's
//     own pix-* namespace: PlanRemove (`rm`, no `-f`) and PlanForceRemove
//     (`rm -f`) share the exact same scope/name-safety check, so neither argv
//     shape can be handed a name the other would refuse. PlanForceRemove's
//     `-f` is a transport detail (sbx v0.38 refuses a bare, non-interactive
//     `rm` outright), never a widened authority — see its doc comment for
//     the two proofs a caller must already hold before reaching for it.
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
// # Schema posture: two pinned profiles, everything else degrades honestly
//
// There is no single authoritative schema for what an installed `sbx` emits
// for a sandbox listing across versions, so list.go recognizes exactly two
// CANONICAL top-level/row profiles, selected once from the top-level shape
// (unwrapRows) and applied to every row in a listing:
//
//   - legacy: a bare JSON array, rows keyed name/state/instance_id (with a
//     documented set of ALIAS field names — nameKeys/stateKeys/idKeys — for
//     sbx versions or callers this package has seen use a different spelling).
//   - v0.38 (`sbx ls --json` on the pinned version — see
//     docs/upstream/sbx-0.38-noninteractive-rm.md for the other v0.38
//     behavior change this repo already accounts for): the OBJECT
//     `{"sandboxes": [...]}`, rows keyed EXACTLY name/id/agent/status/
//     workspaces plus optional workspace_missing (omitempty on v0.38) —
//     captured verbatim from a real v0.38
//     install (see list_test.go's testdata/list_v38_canonical.json), so this
//     shape is trusted as fully as the legacy bare array, not treated as a
//     lesser alias. It has NO key aliases of its own: a row using a legacy
//     alias under this wrapper (e.g. "instance_id" for "id") is a key
//     outside the SELECTED profile, handled the same as any other
//     undocumented key.
//
// Leniency does not imply trust. Under the legacy profile, a row is
// IdentityVerified only when every field it supplied used the CANONICAL key
// (not a fallback alias) and carried no key outside the fully documented
// set; a row that had to lean on an alias, or came with a key this package
// has never heard of, parses successfully but reports IdentityVerified=false.
// Under the v0.38 profile, the pinned evidence is stronger, so the bar is
// higher in one direction and lower in another: id/status/agent/workspaces
// are each REQUIRED with the documented type; workspace_missing may be absent
// but must be bool when present (missing required or mistyped fields fail the
// WHOLE parse, not just this row — see parseRowV38). id
// must be shaped like a UUID, and status must be a recognized value; but an
// undocumented EXTRA key or an unrecognized-but-well-typed status value still
// only downgrades IdentityVerified, exactly like the legacy profile. Either
// way, this is the same "fail closed on uncertainty, never guess" posture
// this package's own State four-state and workflow/launch's
// PlanSandboxLaunch already use for sandbox liveness. PlanLaunch (argv.go)
// enforces this: it refuses to plan create-vs-exec against an unverified row.
package sandbox
