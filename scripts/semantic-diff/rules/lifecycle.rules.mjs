// Story04 extension point — DELIBERATELY EMPTY at W0.
//
// This shard is reserved for the sandbox/process LIFECYCLE contracts the
// U00c plan calls out for a later story:
//
//   - "keep polarity": the `--keep`/keep-set semantics in the rm path
//     (services/host/workflow/launch/sandbox.go's keepSet) — which direction
//     the flag inverts (remove-except-kept vs. remove-only-listed) is a
//     one-word-diff away from silently reversing what gets destroyed.
//   - "non-force rm": the AGENTS.md safety invariant "an existing sandbox is
//     never force-removed and never replayed into" — pin the absence of a
//     bare `-f`/force path outside the explicit `--replace` flow.
//   - "instance identity": the memoryIdentity()/knowledgeIdentity() shape
//     (services/host/identity.go) that proves a listener on a reserved port
//     is actually OUR process at OUR version over OUR db, not a foreign
//     listener that happened to bind first.
//   - "lease paths / CLOEXEC": the memory/knowledge store lock file paths and
//     the O_CLOEXEC (or Go equivalent) flag on them, so a plugin subprocess
//     launched from the daemon can never inherit a held lock fd across exec
//     and deadlock a future holder.
//
// A pin belongs here the same way every other domain shard works: a fixed
// `expected` literal/set, one or more file witnesses, and — per pin — an
// intended-change manifest entry required for any future deliberate change.
// Keeping this file present-but-empty (rather than deferring its creation)
// means engine.mjs's loadRules() already exercises the "domain with zero
// pins" path today, so Story04 only ever needs to ADD an array element here,
// never touch the engine or the CLI.
export default [];
