# Parallel task sandboxes (pix task)

Status: IMPLEMENTED (v1), then EXTENDED. This document is the original v1 localclone
design. The sandbox naming scheme and the `harvest` / `gc` verbs (plus auto-GC) were
revised and added in `docs/design/task-ux-decisions.md` (accepted) — read that for
the current behavior where the two differ. The reconciled naming and command surface
are folded in below.
Follows the Shape B CLI redesign (docs/design/cli-redesign.md) — same taste.

## The ask

Run several pix sandboxes at once, each on a different task on the SAME
repo, without the tasks colliding (branch, working tree, sandbox name,
memory/knowledge scope). The owner noted `sbx --clone` exists and left the
mechanism choice to us.

## Decision: host-side per-task local clone (NOT git worktree, NOT sbx --clone as default)

The first design proposed **git worktree** in direct mode. A cross-vendor design
review found that a linked worktree's working dir has a `.git` **file** pointing
to `<main-repo>/.git/worktrees/<id>`, which lives OUTSIDE the mounted worktree
dir. In direct mode the launcher bind-mounts only the workspace dir, so git
operations INSIDE the sandbox (`status`, `commit`, `remote`) would fail. That
makes worktree-in-direct-mode broken as a default, and unverifiable from inside
the sandbox. Rejected.

The review also rejected "mount the common gitdir" (Approach A): it bets on
identical absolute paths on both sides of the sbx mount plus a web of
cross-referencing absolute pointers. Brittle.

**Chosen: host-side per-task `git clone --local`, direct-mounted.**

```
git clone --local <mainroot> <taskdir>   # hardlinked objects, self-contained .git DIRECTORY
```

Empirically validated (temp repo, this environment): the clone's `.git` is a
real **directory** (mounts fine), commits work, `origin` wires to the main repo,
objects are hardlinked (near-zero disk, gc-safe unlike `--shared`). Commits land
in the task clone ON THE HOST, so they survive `sbx rm` and are pushable /
fetchable. This is effectively the clone-based isolation the owner asked about,
but host-side so work persists.

`sbx --clone` (in-sandbox clone) is DEFERRED to v2: its interaction with the
pinned v1 kit is unverifiable from inside the sandbox, and its commits-trapped-
in-the-VM profile needs a fetch-before-teardown recovery path. The host-side
clone gives the same parallel isolation without that trap.

## Command surface (Shape B taste, Occasional tier)

One new `task` grouping noun (verbatim dispatcher, mirrors `state.go`). No change
to `pix run`. Shown in `help --all`, not Core.

```
usage: pix task <new|ls|harvest|rm|gc> [args]

Run parallel tasks on one repo. Each task is a lightweight local clone of the
repo with its own branch and sandbox, so tasks never collide. Commits land in
the task's clone on this host and can be pushed or fetched back.

  new     <name> [--from REF] [-- pi-args]   clone + branch + launch a sandbox
  ls      [--json]                           tasks, their branch, sandbox + git state
  harvest <name> [--to DIR]                  copy out uncommitted docs before teardown
  rm      <name> [--force]                   tear down sandbox + clone (guarded)
  gc      [--dry-run]                        clean up merged / stale tasks (guarded)

Clones live under $XDG_STATE_HOME/pix/tasks/<repokey>/co/<name> (outside your
repo). `pix task rm` refuses to drop uncommitted or unpushed work; `task
harvest` gets untracked docs out first (see docs/design/task-ux-decisions.md).
```

### Golden path
```
pix task new fix-login       # clone + branch pix/fix-login + sandbox, you are in it
pix task new refactor-db      # a second, fully isolated, in parallel
pix task ls                   # both: branch, sandbox status, git state
pix task rm fix-login         # tear down when pushed/merged (guarded)
```

## Mechanics (exact sequence)

Resolve the real repo even when cwd is itself a worktree:
```
MAINROOT = dirname(git -C <cwd> rev-parse --path-format=absolute --git-common-dir)
git -C MAINROOT rev-parse --verify --quiet "<REF|HEAD>^{commit}"   # start point exists?
ORIGIN   = git -C MAINROOT remote get-url origin   (may be empty)
```
`task new <name> [--from REF]`:
```
CO   = $STATE/pix/tasks/<repokey>/co/<name>
META = $STATE/pix/tasks/<repokey>/meta/<name>.json
git clone --local MAINROOT CO
git -C CO checkout -q -b pix/<name> "<REF|HEAD>"
[ -n ORIGIN ] && git -C CO remote set-url origin ORIGIN   # in-sandbox push uses the sbx cred proxy
write META {name, mode:"localclone", sandbox, mainroot, branch, base, origin, created}  # BEFORE launch
```
Then hand to the existing run path with `o.Workspace=CO`, `o.Name=<sbxname>`.
No new launch code; `deriveSandboxName` is BYPASSED (only used when Name=="").

## Collision-proof sandbox name + metadata

`deriveSandboxName` is basename-only (`pix-fix` collides across repos), so
tasks set an explicit name:
```
repokey      = first 8 hex of sha256(canonical MAINROOT abs path)
repolabel    = sanitized basename of the repo (human-readable; added in task-ux-decisions.md)
sandbox name = "pix-t-" + repolabel + "-" + repokey + "-" + sanitize(<name>) [ + "-" + profile ]
```
`sanitizeTaskName` caps the name segment so the full name stays within sbx's
bound. Metadata at `$STATE/pix/tasks/<repokey>/meta/<name>.json` is the
single source of truth `ls`/`rm` read (mode + branch detectable without guessing
from the sandbox name), written before launch so a mid-launch crash is still
discoverable.

## Lifecycle + teardown guard (executable spec)

- **new**: clone -> checkout -b -> write meta -> launch. If `CO` already exists,
  error ("task exists; `pix run <CO>` to reattach"). If launch fails after
  the clone, roll back with `rm -rf CO` + remove meta (no git-state to unwind).
- **ls**: read-only. Reads meta + joins `sandboxStatus`; per task shows branch,
  sandbox status, dirty flag, and unpushed/uncommitted markers so the human sees
  what `rm` would refuse.
- **rm** `<name>`: guarded. Refuses (exit 2, teachable, unless `--force`) when:
  1. sandbox still running (stop the session first);
  2. uncommitted changes: `git -C CO status --porcelain` non-empty;
  3. would-lose-work: `UNREC>0 && UNPUSHED>0`, where `UNREC` = commits on
     `pix/<name>` not reachable from any ref in MAINROOT (fetch into a temp
     ref, `rev-list --count ... --not --all`, delete temp ref), and `UNPUSHED` =
     `@{u}..HEAD` when an upstream exists, else `UNREC` (defined for no-upstream
     AND no-remote). A fully-pushed branch is safe even if `UNREC>0`.
  Teardown order (data-safe even with `--force`): ALWAYS snapshot first (`git -C
  MAINROOT fetch CO pix/<name>:refs/pix/recovered/<name>`), then `sbx
  rm -f` and ABORT if it fails (before deleting the checkout), then `rm -rf CO` +
  meta. The recovered ref makes even a forced teardown recoverable. We never
  touch the main repo's branches.

## Storage (does not touch the tracked .pix/knowledge contract)

Task clones live OUTSIDE the repo at `$XDG_STATE_HOME/pix/tasks/<repokey>/`
(`meta/<name>.json`, `co/<name>/`). So no file under the repo's `.pix/` is
created, the tracked `.pix/knowledge` pointer is untouched, no blanket
`.gitignore *` is needed, and nested-worktree hazards do not exist. Each clone
carries its own `.pix/` (knowledge.scope / profile), ignored through that
clone's `.git/info/exclude`, for per-task isolation without changing tracked
repository files.

## Implementation footprint (least churn; no bound symbol renamed)

- New `task.go`: `runTask` dispatcher (mirrors `runState`); pure helpers
  `taskRepoKey`, `taskPaths`, `sanitizeTaskName`, `taskSandboxName`,
  `parseTaskArgs`; pure `taskRemoveGuard(state) (blockMsg string, ok bool)`;
  git/sbx behind the injected `shellEnv` seam.
- `main.go`: `case "task": runTask(args[1:])`. `help.go`: `knownVerbs["task"]` +
  `verbUsage` case + `taskUsage`. `pix.1`: one `.SS task` block (quoted
  `"pix task new"` etc; man 1:1 invariant held; new prose examples UNQUOTED
  in .nf/.RS).
- `run.go` / `sbxargs.go`: UNCHANGED in v1 (no `--clone`; deferred to v2).

### Test seams (real evidence achievable in-sandbox)
- Pure helpers unit-tested directly (repokey, paths, name, sanitize).
- `taskRemoveGuard` table test over (running, dirty, unpushed x {upstream,
  no-upstream, no-remote}) x force.
- REAL git integration tests: create temp repos, `git clone --local`, commit,
  and assert the guard's `UNREC`/`UNPUSHED` computation against real `git
  rev-list` output (git is available in this environment). The only slice NOT
  testable here is launching a real Docker sandbox (needs host sbx/docker).

## Scope

- **v1 (this PR)**: `task new` / `task ls` / `task rm` on the localclone model;
  repo-qualified names + metadata; teardown guard (executable spec above);
  state-dir storage; launch-failure rollback; repo-matrix handling (worktree
  source via `--git-common-dir`, bare repo, submodule note). Full pure + real-git
  tests.
- **v2 (defer)**: sbx `--clone` flag with fetch-before-rm recovery; recursive
  submodule init; `task pull` to fast-forward the recovered ref into a host
  branch; auto-cleanup of merged tasks; TUI `ls`; `task enter` reattach; per-task
  memory tagging beyond the free per-clone `.pix/` split.

## Host verification (cannot run from inside the sandbox — do before relying on it)

The Go logic is fully unit + real-git tested here. Launching an actual Docker
sandbox is the one slice that needs the host. Run on the host with sbx + docker:
```
R=$(mktemp -d)/r; git init "$R" && (cd "$R"; echo x>f; git add .; git commit -m init)
pix task new probe --from HEAD -- -p 'run: git status; git commit --allow-empty -m t; git log --oneline -1'
#   MUST show clean git status + a successful commit INSIDE the sandbox
pix task ls        # probe present, sandbox status shown
pix task rm probe  # guarded teardown; clean case removes clone + sandbox
```
A failure of the in-sandbox `git status`/`commit` step is a hard block (it would
mean the mount does not preserve the clone's .git dir — highly unlikely for a
plain directory, but this is the one unverified assumption).

The non-force teardown's atomicity rests on an sbx-CLI assumption not verifiable
from inside the sandbox: `sbx rm <name>` (without `-f`) must REFUSE a running
sandbox (docker-style). Confirm on the host that `sbx rm` on a running sandbox
fails, and only `sbx rm -f` kills it — that is what makes `task rm` safe against
the probe-then-remove race.

Teardown also classifies the `sbx rm` RESULT, not the pre-rm probe: success =
removed (proceed to delete the clone); failure + a re-probe that still reads
absent = the sandbox was not present (proceed); failure + running = refuse and
leave the clone intact. That classification relies on `sbx rm` semantics —
success only on an actual stopped-sandbox removal, and a distinguishable failure
when the name is nonexistent vs. running — which must be confirmed on the host.

## Risks

- **In-sandbox mount of the clone's .git dir**: validated for git-on-the-host;
  the sandbox-mount slice is the host-checklist item above. Low risk (plain dir).
- **Name length**: `pix-t-<repolabel>-<8>-<name>-<profile>`; `sanitizeTaskName`
  caps the name segment (see the length budget in task-ux-decisions.md).
- **Submodules**: not auto-initialized in v1 (note printed if `.gitmodules`
  exists). v2.
