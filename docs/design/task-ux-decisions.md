# pix `task`: three UX decisions

Status: **accepted** (user decisions folded in 2025). Scope: `services/host/cmd/pix/task.go`
(+ `status.go`, `config/config.go`). Audience: the engineer who implements this. All three
recommendations are approved; open cross-cutting calls are now resolved (see the decisions log at
the end).

## The job

A `task` is a durable parallel workspace: local git clone + branch + sandbox, one per line
of work on a repo. The user runs several at once, across several repos, over days. Three
things fail that job today:

1. Every task's identity is stamped with an opaque 8-hex repo hash, so the user cannot name a
   task `fix` and still tell foo's `fix` from bar's in `sbx ls` or on disk.
2. Nothing ever cleans a task up. The clone + branch + metadata live forever unless the user
   runs `task rm` by hand, one at a time. Over weeks this piles up.
3. Docs a task produces but never commits (a PRD draft, investigation notes, a design doc) have
   no way out. The removal guard snapshots committed refs; it does nothing for untracked files,
   so `task rm --force` silently vaporizes them.

Ground truth read: `task.go` (`taskRepoKey`, `taskSandboxName`, `taskPaths`, `taskMeta`,
`hardenTaskMeta`, `gatherTaskState`, `taskRemoveGuard`, `executeTaskTeardown`, `runTaskRm`,
`runTaskLs`), `task_test.go`, the knowledge/OKF flow in `knowledge.go`, `run.go:deriveSandboxName`.

Note on artifact location: the task clone (`co`) lives on the HOST at
`$XDG_STATE_HOME/pix/tasks/<key>/co/<name>`. The sandbox mounts it, so files written in
the sandbox already land on the host clone. "Getting artifacts out" is therefore not about
crossing the sandbox boundary; it is about surviving `task rm` when the file is not in git.

---

## Problem 1 — Naming: surface a human repo label

### Recommendation

Introduce a human repo label alongside the existing hash, and put it in the sandbox name, the
on-disk path, and `task ls`. Keep the hash; it is the collision guarantee.

- `taskRepoLabel(mainroot)` = sanitized basename of the repo (the dir that contains the
  git-common-dir, or the bare repo name minus `.git`), capped at **12 chars**. Overflow is a
  plain truncation (no hash tag needed — the label is a hint, the repokey already guarantees
  uniqueness).
- Sandbox name becomes `pix-t-<repolabel>-<repokey>-<name>[-<profile>]`. The hash stays: two
  repos that both basename to `api` (`~/work/api`, `~/personal/api`) still differ by their
  repokey, so they never collide. The label is legibility; the hash is correctness.
- Per-repo state dir becomes `<repolabel>-<repokey>` instead of bare `<repokey>`, so browsing
  `$STATE/pix/tasks/` is readable.
- `task ls` prints a header line naming the repo (`Tasks for api (~/work/api):`) and the sandbox
  column already shows the full name, now legible.

Length: today only the `<name>` segment is bounded (`maxTaskNameLen = 40`); nothing enforces a
total. Replace the per-segment cap with one `boundSandboxName(...)` that composes the full name
and, if it exceeds `maxSandboxNameLen`, trims in priority order: **name first** (hash-tagged so it
stays unique), then label (cosmetic), never the repokey (correctness).

**Bound resolved: `maxSandboxNameLen = 63`** (RFC1123 label, the strictest common limit; the code
never enforced a total, so 63 is the safe conservative constant). Budget with a 12-char label:

```
pix-t-   11
<repolabel>-  13   (12 cap + dash, cosmetic — trimmed second)
<repokey>-     9   (8 hex + dash — NEVER trimmed)
= subtotal    33   → 30 left for <name>, or ~17 once a profile is appended
```

So the old `maxTaskNameLen = 40` drops: `<name>` now bounds to whatever `boundSandboxName` leaves
after the fixed segments (≈30, less with a profile). 12 chars of label is plenty for a human
hint; longer repo names just truncate.

### Back-compat / migration

- `taskMeta` gains a `Repo string` field. It is display/path only and is **re-derived, never
  trusted**: `hardenTaskMeta` sets `m.Repo = taskRepoLabel(mainroot)` and rebuilds `m.Sandbox`
  from it, exactly as it already re-derives `Branch`/`Sandbox`/`Mainroot`. An old meta with no
  `Repo` field just works.
- On-disk path: migrate lazily. `taskPaths` computes the new `<repolabel>-<repokey>` dir; `ls`
  and `rm` try it first and fall back to a legacy bare-`<repokey>` dir when the new one is
  absent. `task new` always writes the new layout. No destructive move, no flag day.

### Rejected

- **Drop the hash, use only the label.** Two same-basename repos collide on sandbox name and
  path. The hash exists precisely for this; dropping it reintroduces the bug it fixed.
- **Relabel `sbx ls` output only, leave paths and metadata alone.** Cheaper, but the user listed
  paths as a surface, and a half-legible system (readable names, opaque dirs) is worse to reason
  about than a consistent one. The lazy path migration is cheap enough to just do.

### Sketch

`taskRepoKey` (add sibling `taskRepoLabel`) · `taskSandboxName` (insert label, route through
`boundSandboxName`) · `maxTaskNameLen` → `maxSandboxNameLen` + `boundSandboxName` · `taskPaths` +
its callers in `runTaskLs`/`runTaskRm` (new dir + legacy fallback) · `taskMeta.Repo` ·
`hardenTaskMeta` (re-derive `Repo`) · `runTaskLs` (header line).

---

## Problem 2 — Lifecycle: an explicit, guarded `task gc`, plus passive surfacing

### Recommendation

No auto-clean on exit. Add `task gc`, run from inside a repo, that removes only over-age tasks
that fully pass the existing removal guard. Surface buildup passively in `task ls` and
`pix status`. No cron, no serve hook.

- `task gc [--days N] [--dry-run] [--no-harvest]`, default `--days 7`. For each task in the
  current repo: compute age, run the exact `gatherTaskState` + `taskRemoveGuard` used by `rm`, and:
  - guard passes AND age ≥ N → **harvest first** (the same always-on harvest `rm` runs — see
    Problem 3), then tear down via the same `executeTaskTeardown` + `removeTaskArtifacts` path.
    Same snapshots, same atomic sbx rm, same safety. `--no-harvest` opts out of the pre-gc
    harvest for the whole sweep.
  - guard fails (dirty / unpushed / clone-only commits / sandbox running / unknown) → **skip and
    report the reason**. Never quarantine-by-deletion; the task is left exactly as is.
  - age < N → skip.
- No `--force` on `gc`. Force is a deliberate per-task act on `rm`; a bulk forced sweep is a
  footgun. If you want a specific task gone despite the guard, `task rm <name> --force` it.
- Age = `now - max(meta.Created, mtime(co))`. `Created` alone is a bad proxy: a three-week-old
  task you are actively using looks stale. Stat- timestamping the checkout is schema-light and
  reflects real activity.
- Passive surfacing: `task ls` appends a nudge when candidates exist ("2 tasks are clean and
  older than 7d; `task gc` to prune"). `pix status` gains a one-line tasks summary (total,
  how many GC-eligible) so the user SEES the pile without any automatic deletion.

### Why not the other models

- **Auto-clean on sandbox exit — rejected.** It destroys the feature's value. Task workspaces
  are meant to outlive a session so you can reattach (`pix run <co>`), push later, or review.
  "Exit" is also ambiguous (done vs crash vs disconnect). Deleting a clone because a pi process
  ended is the opposite of durable parallel workspaces.
- **Automatic serve/cron GC — rejected (with a caveat below).** The safety model re-derives
  `mainroot` from the caller's cwd; that is how `ls`/`rm` refuse to trust a tampered
  `meta.Mainroot`. A background sweep has no cwd and would have to trust stored `meta.Mainroot`
  to know which repo to probe, weakening the guarantee. `serve` is also neither repo- nor
  profile-scoped. Keeping GC a cwd-rooted verb preserves the trust model for free.

> Cross-cutting: if the user truly wants zero-touch pruning, the only safe automatic form is a
> serve sweep that (a) trusts `meta.Mainroot`, (b) touches only guard-passing, over-age,
> sandbox-absent tasks, and (c) routes through `executeTaskTeardown` unchanged. That is a
> deliberate weakening of the "never trust stored mainroot" invariant. Recommend NOT doing it;
> surface counts in `status` instead and let the human pull the trigger. Flagging it as the
> user's call.

### Back-compat

Purely additive. Old metas (no `Created`) fall back to `mtime(co)` for age, so they are still
GC-eligible. No format change.

### Sketch

New `runTaskGc` + `parseTaskGcArgs`, dispatched from `runTask`'s switch. Reuses
`gatherTaskState`, `taskRemoveGuard`, `executeTaskTeardown`, `removeTaskArtifacts` per candidate
(loop over the meta dir the way `runTaskLs` already scans it). New `taskAge(meta, co)` helper.
`runTaskLs` nudge line. `status.go` tasks summary (walk `taskStateRoot()`, count metas and
guard-eligible-over-age). Optional config key `task_gc_days` only if you want the status
threshold configurable; the verb default is fine without it.

---

## Problem 3 — Artifacts: harvest untracked docs, not "everything to the knowledge bundle"

### The hunch, evaluated

The user's hunch — "maybe artifacts always go to the knowledge bundle" — is **wrong**, and the
code shows why. The OKF bundle (`knowledge.go`, the `enrich` skill) is for shared, durable
DOMAIN concepts, written back through a gated PR flow. A task's scratch docs are usually the
opposite: feature-scoped, not-yet-shared, often deliberately uncommitted. There are three
distinct destinations, not one:

1. **Docs that belong with the code** → commit them to the task branch. Git already carries
   them out via push/fetch and the recovery snapshot. Not an extraction problem.
2. **Reusable domain knowledge** → `enrich` into the OKF bundle. Already exists, already gated.
3. **Uncommitted / gitignored scratch docs you want to keep** → nothing handles these today.
   This is the actual gap.

Auto-dumping every task's scratch docs into the shared bundle would pollute shared truth with
half-formed feature notes. Keep them separate.

### Recommendation

A `task harvest` verb for the deliberate case, plus an always-on safety-net harvest inside
`task rm` (and `task gc`, unless `--no-harvest`) so `--force` and bulk prunes can never silently
destroy an un-git'd doc. Destination is a host artifacts dir, NOT the knowledge bundle.

**Harvest is idempotent and re-runnable any time.** `task harvest <name>` can be run mid-task,
repeatedly, without a live sandbox mattering — it copies from the host `co` clone. Each run writes
a fresh timestamped dir, so re-running never clobbers a prior capture; identical content under the
same timestamp is a no-op.

**Home: `XDG_DATA_HOME`, NOT `XDG_STATE_HOME`** (see decision #5). Harvested docs are the only
surviving copy of un-git'd user work, so they must sit OUTSIDE the tree `state reset`/`uninstall`
wipe. Destination: `$XDG_DATA_HOME/pix/artifacts/<repolabel>-<repokey>/<name>/<timestamp>/`
(defaults to `~/.local/share/pix/artifacts/...`). This is a deliberate split from the task
CLONES, which stay in `XDG_STATE_HOME` (expendable, `rm`/`gc`/`reset` may delete them).

**Naming is load-bearing** — the whole point is finding a doc "god knows when, 6 months down the
line." The path is self-describing: `<repolabel>-<repokey>/<name>/<ts>/` puts the repo, the task
name, and the date right in the path. Alongside the copied files, write a
`manifest.json` (repo path, branch, base ref, task name, profile, harvest time, source git status
of each file) so a stray artifacts dir is always traceable back to what produced it.

- "Artifact" = a file matching a narrow, configurable doc globset (`*.md`, `docs/**`, `notes/**`,
  `*.prd`, and a convention dir `.pix/artifacts/**`) that is **untracked or ignored** in the
  clone. Tracked+committed files are excluded; they already leave via git, and the ref snapshot
  covers them. This deliberately mirrors the existing split: the guard snapshots committed refs;
  harvest rescues the uncommitted file half.
- `task harvest <name> [--to DIR]` copies matching files to
  `$XDG_DATA_HOME/pix/artifacts/<repolabel>-<repokey>/<name>/<timestamp>/` (host, durable,
  outside both the `co` dir that `rm` deletes AND the `XDG_STATE_HOME` tree `reset` wipes), writes
  `manifest.json` beside them, and prints the path.
- Inside `runTaskRm`, after a successful teardown and **before** `removeTaskArtifacts` deletes the
  clone, run the same harvest automatically on both the guarded and `--force` paths. Report what
  was saved and where. This makes `--force` non-destructive of real work in the same spirit as
  the always-on ref snapshot.
- Not automatic into OKF. Promoting a harvested doc into the bundle stays a separate, deliberate
  act (`enrich`, or a later `task harvest --to-knowledge` that stages into the bundle for review).
  Default keeps shared truth clean.
- **Retention (required, per SRE).** An unbounded artifacts dir grows forever. `task gc` prunes
  artifact snapshots older than `--artifact-days` (default **30**, distinct from the 7-day clone
  age), honoring `--dry-run`. `pix status` reports total artifacts-dir size so growth is
  visible without digging. Pruning artifacts is pure deletion (they were already the rescue copy),
  so it is gated only by age, never by the clone guard.
- **`uninstall` (per SRE + PM).** Lists the artifacts path and size and SKIPS it by default. The
  only deletion path is an explicit `--purge-data`, which prints a second confirmation naming the
  artifacts before proceeding. `reset` never touches artifacts at all.

### Rejected

- **Auto-capture everything into the knowledge bundle (the hunch).** Pollutes the shared,
  PR-gated OKF corpus with per-feature scratch. Wrong altitude and wrong scope; see above.
- **Require `git add` before rm.** Loses the deliberately-uncommitted case entirely, and a
  gitignored doc can never be added, so the exact files most at risk stay unprotected.

### Back-compat

Additive. New artifacts dir is created on demand. Existing tasks gain the safety net on their
next `rm` with no metadata change.

> Cross-cutting: two calls for the user. (a) Is the auto-harvest-on-rm always-on (recommended) or
> opt-in behind a flag? Recommend always-on: it is cheap and it closes the `--force` data-loss
> hole. (b) Default destination host artifacts dir (recommended) vs staging into OKF. Recommend
> host dir; keep OKF a deliberate promotion.

### Sketch

New `taskArtifactRoot()` (mirrors `taskStateRoot()` but rooted at `XDG_DATA_HOME`, falling back to
`~/.local/share/pix/artifacts`). New `runTaskHarvest` + `parseTaskHarvestArgs`, dispatched
from `runTask`. New `harvestArtifacts(co, dest, globs) ([]string, error)` (enumerate
untracked/ignored matches via `git -C co status --porcelain --ignored` + globset filter, copy out,
write `manifest.json`). Call it in `runTaskRm` between the `rc == 0` teardown success and
`removeTaskArtifacts`, and in `runTaskGc` before each teardown (unless `--no-harvest`). Globset in
config (reuse the config surface) with a sane built-in default. Retention: extend `runTaskGc` with
`--artifact-days` (default 30) that prunes over-age dirs under `taskArtifactRoot()`. `status.go`
adds the artifacts-dir size line. `reset.go` stays untouched (it never reached `XDG_DATA_HOME`);
`uninstall` gains `--purge-data` + the skip-and-report default.

---

## Decisions log (resolved)

1. **sbx name bound → 63.** `maxSandboxNameLen = 63`. Repo label capped at **12** chars;
   `boundSandboxName` trims name first, then label, never the repokey (Problem 1).
2. **Path migration → lazy fallback.** New `<repolabel>-<repokey>` layout on `task new`; `ls`/`rm`
   fall back to legacy bare-`<repokey>` dirs. No flag day (Problem 1).
3. **GC → explicit `task gc` verb only.** No serve/cron sweep (rejected: would trust stored
   `meta.Mainroot`). `task gc` runs harvest before each teardown, `--no-harvest` to skip
   (Problem 2).
4. **Harvest → always-on rm/gc safety net.** Idempotent + re-runnable any time; timestamped,
   self-describing path + `manifest.json` for long-term findability. OKF promotion stays a
   deliberate `enrich` act (Problem 3).

5. **Harvest home → `XDG_DATA_HOME` (`~/.local/share/pix/artifacts/`), not `XDG_STATE_HOME`.**
   Harvested docs are user work product, not stack state, so they must survive `state reset` and
   `uninstall`. `reset` never touches them (pure operational reset). `uninstall` leaves them by
   default and prints the path; add an explicit `--purge-data` opt-in for the rare "burn it all"
   case. Rationale: putting the data-loss safety net inside the thing `reset` wipes defeats the
   safety net (Problem 3).
6. **Retention → `task gc --artifact-days` (default 30) + size in `pix status`.** The
   artifacts dir is unbounded otherwise; pruning is age-gated pure deletion (the rescue copy has
   already served its purpose), and `status` surfaces total size so growth stays visible
   (Problem 3, per SRE).

All three share one shape: keep the existing guard/snapshot machinery as the single choke point,
and extend it — legible names on top, a guarded bulk prune beside it, and an uncommitted-file
rescue that mirrors the committed-ref rescue already there.
