# U00c semantic-diff guard

Pins real, currently-true contracts (RPC method/param names, reserved ports,
`pix-*` sandbox scope, permission modes, stdout/stderr conventions, critical
subprocess argv, config.toml keys) as fixed literals independent of the
production code, so a mechanically-introduced change (bulk rename, codemod,
generated edit) that moves BOTH the production value AND whatever else in the
tree asserted it — "lockstep corruption" — still gets caught. See
`skills/architecture-audit/SKILL.md`'s "Phase 3: semantic-diff pass".

Run it: `node scripts/check-semantic-diff.mjs` (wired into `node --test` via
`tests/check-semantic-diff.test.mjs`; NOT wired into `scripts/gate.sh` at W0).

## Layout

- `lib/engine.mjs` — pure, testable core (no I/O side effects beyond reading
  files/git). `tests/check-semantic-diff.test.mjs` exercises it directly.
- `rules/<domain>.rules.mjs` — one file per domain, each `export default` an
  array of **pins**. The file basename (minus `.rules.mjs`) is the domain.
- `intended-changes.json` — the manifest. Empty array at W0.

## Pin shape

```js
{
  id: "domain.unique-id",              // stable; referenced by manifest entries
  description: "why this is pinned",
  checks: [
    {
      file: "services/host/whatever.go",     // repo-root-relative
      kind: "contains" | "notContains" | "set" | "equals",
      region: { start: "literal anchor", end: "literal anchor (optional)" },
      // contains/notContains:
      values: ["literal substring", ...],
      // set: every capture-group-1 match across `pattern`/`patterns`, compared
      // as a SET (order-independent, catches additions AND removals):
      pattern: "regex source (capture group 1 = the value)",
      patterns: ["regex source", ...],   // union of several patterns
      expected: ["a", "b", ...],
      // equals: the first capture-group-1 match must equal `expected` exactly
      pattern: "regex source",
      expected: "single-value",
    },
  ],
}
```

A pin can (and for the strongest contracts, should) carry **multiple witness
checks across different files** that are each independently pinned to the
same fixed `expected`. That is the actual lockstep-corruption defense: if a
codemod renames a JSON-RPC method consistently in both the server's method
table and a client's call sites, both witnesses now disagree with the SAME
fixed pin and the guard fails on both — even though the two files still agree
with each other.

A missing `region` anchor (the surrounding code moved) is a hard failure with
an `ANCHOR ERROR`, not a silent skip — a pin whose anchor rotted is not
verifying anything anymore, which is worse than a false positive.

## The manifest

Any pin change that is an ACTUAL, intended contract change (not a bug) needs
an entry in `intended-changes.json`:

```json
{
  "id": "domain.unique-id",
  "rationale": "why this changed",
  "evidence": "PR/commit/doc link",
  "changes": [
    { "file": "services/host/whatever.go", "kind": "contains", "from": "old literal", "to": "new literal" }
  ]
}
```

The engine only treats a mismatch as "waived" when a manifest entry's
`changes[].to` **exactly matches what the code now actually says** — a
manifest that merely gestures at a change without stating the real
destination value does not waive anything. `rationale` and `evidence` must be
non-empty; a malformed manifest is a hard error, not a silent no-op.

An entry that never matches an actual mismatch is reported (non-fatally) as
`unusedManifestEntries` — stale waivers should be deleted once the pin itself
is updated to the new literal.

## The second layer: rule-drift-vs-git

`checkRuleDrift()` compares each `rules/*.rules.mjs` file's checkable content
(id + checks, never free-text `description`) against its content at a git
base ref (default `HEAD`). If a pin's `expected`/`values` changed with no
manifest entry for that `id`, the guard fails — this is what stops someone
from "fixing" a broken pin (or a hostile edit "fixing" it to match corrupted
production code) by just editing the rules file itself, unreviewed. It is
defense-in-depth: it no-ops cleanly (not a failure) on a fresh checkout or
when a rules file/ref has no prior committed version yet.

## Story04

`rules/lifecycle.rules.mjs` is the reserved, present-but-empty shard for
sandbox/process lifecycle contracts (keep polarity, non-force `rm`, instance
identity, lease paths/CLOEXEC) — see that file's header comment. Adding pins
there requires no engine or CLI change.
