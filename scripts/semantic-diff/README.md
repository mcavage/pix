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

An entry that never matches an actual mismatch is reported as
`unusedManifestEntries`. That alone is not fatal — an entry can be "unused"
by the waiver mechanism above and STILL be earning its keep as the thing
`checkRuleDrift()` needs to explain a real fingerprint change this run (see
below); the CLI's `staleManifestEntries()` combines both signals and only
fails the guard on an entry that neither waives a currently-failing check NOR
explains any real drift vs. the base — that is a genuinely dead entry, and
the guard now treats it as one, not a `note:` nobody has to act on.

## The second layer: rule-drift-vs-git

`checkRuleDrift()` compares each `rules/*.rules.mjs` file's checkable content
(id + checks, never free-text `description`) against its content at a git
base ref. If a pin's `expected`/`values` changed with no manifest entry for
that `id`, the guard fails — this is what stops someone from "fixing" a
broken pin (or a hostile edit "fixing" it to match corrupted production
code) by just editing the rules file itself, unreviewed. It is
defense-in-depth: it no-ops cleanly (not a failure) on a fresh checkout or
when a rules file/ref has no prior committed version yet.

The base ref is **never** a hardcoded `HEAD`: comparing the current rules
against `HEAD` compares them against themselves in exactly the case that
matters (a clean, already-committed checkout — every CI run), so it could
never see a PR's own rule-file edits. `check-semantic-diff.mjs`'s default (no
`--base` flag) resolves through `resolveDefaultBase()`: the CI-provided
`SEMANTIC_DIFF_BASE_SHA` (the PR's actual base sha, or the pre-push sha on a
direct push — see `.github/workflows/test.yml`'s `gate` job), else
`merge-base(HEAD, origin/main)`, else `HEAD~1`, else a literal `HEAD` only for
a brand-new single-commit repo with nothing else to compare against.
`checkRuleDrift()` itself still just takes whatever `base` its caller passes
(explicit `--base` always wins, and every test in
`tests/check-semantic-diff.test.mjs` is free to pass its own fixture base).

## Landing a rule change: same commit vs. a subsequent commit

`checkRuleDrift()` diffs the CURRENT rules directory against the resolved git
base ref, so the manifest entry that waives a pin's fingerprint change must
exist **in the same
commit** that changes the pin's `expected`/`values` — that diff is exactly
what gets compared against the base, and if the fingerprint moved with no
matching manifest `id` present at that point, the guard fails, full stop.
Concretely:

1. **Rule change + intended-change waiver land together, in the same
   commit (or PR).** Editing `rules/<domain>.rules.mjs` to a new literal
   without a matching `intended-changes.json` entry in that SAME commit
   fails `checkRuleDrift()` — the drift check has no way to tell an
   intentional, reviewed contract change from a hostile or accidental edit
   to the pin itself, so it always requires the waiver to be present at the
   moment the fingerprint changes.
2. **Stale waiver removal happens in a SUBSEQUENT commit/PR, never the
   same one.** Once the rule-change commit lands, its new `expected` value
   becomes the base ref for every later diff. A follow-up commit that only
   deletes the now-unused manifest entry (flagged by `unusedManifestEntries`)
   sees NO fingerprint change between base and current — the rule already
   reads the new value on both sides — so `checkRuleDrift()` no-ops on it
   regardless of the manifest. That is what makes the removal safe.
3. **Removing the waiver in the SAME commit as the rule change fails.** If
   a commit changes the pin's literal AND strips (or never adds) the
   manifest entry, the final state `checkRuleDrift()` sees for that commit
   has a changed fingerprint with no corresponding `id` in the manifest —
   this is indistinguishable from an unreviewed pin edit, so it is rejected
   the same way. The waiver is only unnecessary AFTER its commit is the new
   base, never within it.

So the sequence is always: **(a)** ship the rule change with its waiver
together, **(b)** merge, **(c)** delete the now-stale waiver entry in a
later, separate commit or PR.

## Story04 / staged pins (U04g)

`rules/lifecycle.rules.mjs` is the sandbox/process lifecycle shard (keep
polarity, non-force `rm`, instance identity, lease paths/CLOEXEC) — it
shipped W0 present-but-empty and now (U04g) carries real pins, both kinds:

- **ACTIVE** pins on contracts already true today: the `pix rm` seam's
  non-force scoping and keep polarity, and the U04a lease foundation's
  write-once instance records, state file paths/modes, and CLOEXEC.
- **STAGED** pins (`activation: "<key>"`) on contracts describing behavior
  that does not exist in production yet — the orphan reaper, bare non-TTY
  `pix rm` refusal, and the `-k`/`--keep` flag. Evaluating these for real
  today would permanently redden the gate for unlanded work, so a pin whose
  `activation` key is not present in `scripts/semantic-diff/activation.json`
  is **skipped entirely** by `evaluatePins` (no file I/O, reported
  `pending: true`, never counted as a failure) instead of evaluated.

### The activation manifest

`activation.json` is `intended-changes.json`'s sibling, same discipline,
inverted purpose: instead of waiving an already-true pin's changed value, it
**turns a staged pin on**. Shape:

```json
[{ "key": "story04", "rationale": "landed the orphan reaper + -k/--keep", "evidence": "PR #123" }]
```

`rationale`/`evidence` must be non-empty (same hard-error-on-malformed rule
as the manifest). It ships **empty** until a story lands the real behavior.
A pin's `activation` field is part of its fingerprint for `checkRuleDrift()`
too — silently flipping a pin from staged to active (or vice versa) outside
a reviewed commit is caught exactly like any other pin-content drift.

**Landing sequence, mirroring the manifest's:** (a) the commit that lands the
real behavior ALSO adds the matching `activation.json` entry, in the same
commit — an unlanded feature can never accidentally get evaluated, and a
landed one can never silently stay pending; (b) the STAGED pin itself
typically stays as-is (or graduates to no `activation` field at all in a
later, separate commit, the same subsequent-commit pattern
`checkRuleDrift()` already supports for manifest waivers).

Use `node scripts/check-semantic-diff.mjs --activate <key>` to preview what a
staged pin would check locally without editing `activation.json` — this is
how `tests/check-semantic-diff.test.mjs` proves every staged lifecycle pin is
a real, currently-failing TODO and not a vacuous placeholder.
