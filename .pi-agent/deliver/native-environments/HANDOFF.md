# Native environments delivery handoff

Continue the native-environments delivery from this durable handoff.

**Repository:** `/Users/mcavage/dev/pix`
**Branch:** `design/simplify-pack-rigs`
**Current main HEAD (before this handoff commit):** `7275e5fe`

## Lean execution mode (user-approved at this handoff)

The user approved a leaner workflow for the remaining waves. This supersedes
the prior over-granular per-unit review cadence but **preserves every
acceptance requirement** (full-migration bar, E2.8's live-conversion gate,
C9/C10/C11, host-gate proof). `status.json.lean_mode` is now `true`.

- **One engineer pass per remaining wave**, not one subagent round-trip per
  unit with a full review after each.
- **No per-unit reviews.** Only the mandatory gates below still get a review.
- **Focused tests during development**; `make gate` only after each wave's
  merges land, plus once at the very end.
- **Findings batched**, not triaged one at a time.
- **Cheap static checks** preferred over a full gate run mid-wave.
- **No broken-Ollama `qa-lead` retries** — if the `qa-lead` preset can't
  start (as it repeatedly couldn't in Wave C), substitute a QA executor
  directly instead of retrying the broken preset.
- **Mandatory, never skipped:** final security review, final QA, and exactly
  **2** cross-vendor reviews before this delivery closes.

Two units already in flight predate this approval and are being finished
under the **old** discipline (one more concise final review each) rather
than restarted; every wave after them runs lean from the start.

## Current state

Story 0 / Wave A, Story 1 foundations / Wave B, and Story 1 verbs / Wave C
remain complete and unchanged (see prior handoff history in `status.json`
for that evidence; not repeated here).

**Wave D (launch cutover):** E2.1-E2.4 are merged and done. **E2.5 is
`review_pending`**, in worktree `/tmp/pix-native-e2-5`, branch
`unit/native-e2-5`: initial head `a3ac29e1` came back **BLOCK**, fixes
landed in `7b2ff399`. Key fixes: env root vs primary workspace semantics;
`Ensure`'d HMAC on both create and load/attach paths; the legacy `sbx run`
fallback deleted entirely (no dual path left); create and attach now derive
the *same* fingerprints; MCP/kits/mounts preserved end-to-end; a promote
intent receipt; a bounded, single-shot `sbx` JSON listing (no unbounded
polling loop). The unit agent claims a full green gate, but **that claim is
not yet independently reverified at the top level** — do that after merge,
not instead of it. E2.5 needs exactly **one** concise final review, then
merge. E2.6-E2.8 remain pending.

**Wave E (literal roster):** E3.1-E3.3 are merged and done. **E3.4 is
`review_pending`**, in worktree `/tmp/pix-native-e3-4`, branch
`unit/native-e3-4`: initial head `03d4ddba` came back with concerns, fixes
landed in `94a9c908`. Every shipped intent/fallback/model was removed;
parsers no longer retain intent; docs were corrected. E3.4 needs exactly
**one** concise final review, then merge — it is the last unit of Wave E.

**Explicit non-claim, carried forward, not resolved by either fix commit
above:** neither E2.5 nor anything merged so far proves **host live
behavior of the native MCP name/render** — that proof is owned by the host
gate ahead of E2.8 (see below), not by E2.5's unit-level fixes.

Waves F, G, H remain pending, unopened, in that order.

## Immediate next steps (in order)

1. **Review E2.5** (worktree `/tmp/pix-native-e2-5`, fix head `7b2ff399`):
   one concise final pass. If clean, merge `unit/native-e2-5` into
   `design/simplify-pack-rigs` (stash the user's 8-file cleanup first — see
   below — then pop and reverify after).
2. **Review E3.4** (worktree `/tmp/pix-native-e3-4`, fix head `94a9c908`):
   one concise final pass. If clean, merge `unit/native-e3-4` the same way.
   This closes Wave E.
3. **Wave D E2.6/E2.7**: run as **one parallel/batched lean pass** (both are
   small, independent of each other per `units.json`'s file-conflict data).
   Focused tests only during development; do not run a full gate per unit.
4. **HOST GATE** (see requirements below) — must pass before E2.8 opens.
   E2.8 (delete every selectable pack launch path) **cannot land** until the
   private work environment has a recorded, live, host-proven conversion
   (architect correction C11, still open — a verbal claim never satisfies
   this).
5. **Wave F**, one engineer pass, strict order **E4.1 → E4.3 → E4.2 →
   E4.4** (note: not numeric order — E4.3 is a prerequisite for E4.2 per the
   architecture's dependency graph; do not reorder).
6. **Wave G**, one engineer pass, order **E5.1 → E5.4** — and re-derive
   E5.1's pack-trust test enumeration from the live tree first (architect
   correction C9, still open; the list in `units.json` is a stale snapshot,
   not an authoritative input). E5.1 may overlap Wave F's tail per the
   architecture's cross-wave parallelism, but lean mode may choose to
   serialize F then G instead if that's simpler to execute correctly — both
   are acceptable under this approval.
7. **Wave H**, one engineer pass, order **E6.1 → E6.4**. **E6.1 must not
   resurrect any of the user's deleted legal/community files** (see the
   user-cleanup section below) — it lands the rest of the doc cut
   (README, reference, getting-started, AGENTS.md, onboarding/healthcheck
   skills) without touching `.claude/settings.local.json`,
   `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, `NOTICE.md`, `SECURITY.md`, or
   `THIRD_PARTY_NOTICES.md`.
8. **Host Gate 2**: a final concurrent home/work UAT pass, after Wave H, as
   the last host-backed proof before closeout.
9. Mandatory final security review, final QA, and 2 cross-vendor reviews —
   run once, at the very end, per the lean-mode approval above. Do not skip
   these; they are the one place lean mode does not cut scope.

## Host gate requirements (before E2.8)

The host gate needs, and an agent cannot fake any of these:

- **`sbx` >= 0.39** on the host.
- **Home environment create/attach/teardown**, proven live.
- **Private work environment conversion + a live run**, proving PRD
  acceptance criteria **A8** and **R6**.
- **Removal name/argv proof** — the exact `sbx env rm` invocation and target
  name actually used, not a description of what it should be.

**Host Gate 2** (after Wave H, before closeout) is a final **concurrent**
home + work environment UAT pass, run together, not sequentially.

## User-owned unstaged cleanup — preserve exactly, never stage

The working tree carries **8** unstaged, user-owned changes. This is the
user's own cleanup, entirely outside this delivery's scope, and must remain
unstaged and uncommitted by any work here, now or in any remaining wave:

- deleted: `.claude/settings.local.json`
- deleted: `CODE_OF_CONDUCT.md`
- deleted: `CONTRIBUTING.md`
- deleted: `NOTICE.md`
- deleted: `SECURITY.md`
- deleted: `THIRD_PARTY_NOTICES.md`
- modified: `LICENSE`
- modified: `Dockerfile`

Every gate run (past and future) treats this as fixed, external state:
capture it byte-exact with `git diff | sha256sum`, `git stash push -u`, run
the gate on a clean HEAD, `git stash pop`, then reverify the same hash and
an empty `git status --porcelain` diff against it before declaring the gate
result. The last-verified patch hash is:

```
2dd22c878cb7723c929bf6c5f21c0a8567bdc3fc5d75dc057f30e8fcf4656691
```

Reverify this hash unchanged at every stash/pop cycle **unless and until the
user commits this cleanup themselves** — at that point it stops being
unstaged state to preserve and this whole section becomes moot; do not
re-derive or "fix" a mismatch without first checking whether the user
committed it.

## External cleanup debt (unchanged, still open)

Pre-fix run `run-20260824-092338-d4c384f5` leaked
`pix-uatenv-fixture-image`. On the host, first verify the exact name and
workspace with `sbx ls --json`; only if they still match, remove it, then
probe again before calling the debt cleared. This debt predates Wave D/E and
is unrelated to either wave's work; do not fold its cleanup into either
review.

## Resume instructions (exact commands)

```bash
# 1. Orient: confirm main HEAD and the 8 preserved unstaged files
git status --short
git log -1 --format='%H %D'
git worktree list

# 2. Review the two waiting units at their fix heads
cd /tmp/pix-native-e2-5 && git log --oneline -5   # expect 7b2ff399 on unit/native-e2-5
cd /tmp/pix-native-e3-4 && git log --oneline -5   # expect 94a9c908 on unit/native-e3-4

# 3. Merge each after its one concise final review (stash the user's
#    8-file cleanup around each merge + gate, pop and reverify after)
cd /Users/mcavage/dev/pix
git stash push -u -m "user-legal-cleanup-preserve"
git merge --no-ff unit/native-e2-5 -m "Merge unit/native-e2-5 (E2.5 cutover)"
git merge --no-ff unit/native-e3-4 -m "Merge unit/native-e3-4 (E3.4 roster)"
make gate
git stash pop
git diff | sha256sum   # must equal 2dd22c878cb7723c929bf6c5f21c0a8567bdc3fc5d75dc057f30e8fcf4656691
git status --porcelain # must be empty aside from the 8 known files

# 4. Remove the finished worktrees once merged
git worktree remove /tmp/pix-native-e2-5
git worktree remove /tmp/pix-native-e3-4
git branch -d unit/native-e2-5 unit/native-e3-4

# 5. Continue with E2.6/E2.7 (one batched pass), then the host gate,
#    then Waves F/G/H per the ordering above, gating the whole delivery
#    once at the end per lean_mode.
```

## Resume checklist

1. Read this file, `status.json` (`lean_mode`, `wave_d`, `wave_e`, and the
   `E2.5`/`E3.4` unit entries), `prd.md`, `architecture.md`, and
   `units.json`.
2. Confirm main HEAD is `7275e5fe` before this handoff commit, and that the
   8 unstaged user files above are present, unstaged, exactly as listed.
3. Review E2.5, then E3.4, each exactly once, then merge both.
4. Run E2.6/E2.7 as one lean batched pass.
5. Do not open E2.8 before the host gate (`sbx` >= 0.39, home
   create/attach/teardown, private-work-env conversion + live run proving
   A8/R6, removal name/argv proof) passes live on the host.
6. Run Waves F, G, H each as one lean engineer pass in the stated unit
   order; re-derive E5.1's test list from the live tree first (C9); never
   touch `SECURITY.md` in E6.1 (C10) or any of the other 7 preserved files.
7. Run Host Gate 2 (concurrent home + work UAT) after Wave H.
8. Run the mandatory final security review, final QA, and 2 cross-vendor
   reviews once, at the very end — these are never cut by lean mode.
9. Clear the external `pix-uatenv-fixture-image` debt on the host,
   independently of the wave work, once the name/workspace is reverified.
