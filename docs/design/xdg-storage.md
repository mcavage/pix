# XDG storage reconciliation

Status: proposal v2 (crew synthesis + cross-vendor design review folded in). APPROVED-to-build.
Third in the stack: CLI redesign (#11) <- task (#12) <- this.

## Design-review outcome (v1 -> v2)

The cross-vendor design review returned REVISE on the v1 migration protocol (12
findings, several P0 on the precious memory db). The **layout below and the single
paths module are approved unchanged.** The migration protocol was rewritten; the
authoritative implementation spec is `.scratch/xdg-arch-v2.md`. The decisive changes:

- **Migration is EXPLICIT and HOST-SIDE**, never auto-on-startup. `pi-stack migrate`
  (launcher) execs `pi-stack-host migrate` (where the sqlite driver lives). Launcher
  and `serve` never migrate; existing installs keep working in place via the read-
  fallback. This kills the hot-path/help/version/restore/wedge findings outright.
- **The knowledge index is REBUILT at the new location, never moved** (`reindex`).
  Eliminates the entire unlocked-WAL-sqlite-move hazard class.
- **Memory moves under the memory flock** (`config.MemoryLockPath`), refused if held
  (the flock, not a port probe, is the authority); staged copy + `integrity_check` +
  atomic rename; resumable via a `.staging-*` dir + a complete|resumable|CONFLICT
  classification that REFUSES ambiguous old+new states.
- **Single-authority handoff:** after moving, the legacy dir becomes a SYMLINK->new,
  so any old binary converges on the one file+lock; the read-fallback is read-only and
  self-disarms the instant the new path exists. No two writable authorities.
- **Nothing precious is ever deleted** (per-artifact retention matrix): symlink or a
  `.pre-xdg-<ts>` safety copy. The receipt's "nothing deleted" is literally true.
- **Config rewrite is ONE atomic transaction under a new `.config.lock`** covering base
  + every non-nil profile `knowledge_bundles`, rewriting the old default bundle path and
  old-cache-root descendants, skipping git URLs / outside paths.
- **Backups are merge-COPIED** (collision-safe names), never whole-dir renamed, so a
  `restore <legacy-path>` is never invalidated. **serve.pid** dual-read (STATE then
  legacy CONFIG) for one release. **reset** preserves every memory authority + sweeps
  STATE + uses the flock guard.

The sections below describe the approved layout + module. For the exact migration
algorithm, resolver precedence, consumer list, and test seams, follow `.scratch/xdg-arch-v2.md`.

## Problem

Host storage is scattered across three inconsistent bases with three
philosophies: the knowledge bundle (authored data) lives under `~/.config`
(config), memory lives in a bespoke `~/.pi-stack`, and `task` (just shipped) uses
`~/.local/state`. Incoherent, and "where is my stuff" has no answer. Fix to one
principled XDG layout with a migration that is invisible and cannot lose data.

## Target layout

Each base honors its XDG override, else the documented default:
- **CONFIG** = `$XDG_CONFIG_HOME/pi-stack` else `~/.config/pi-stack`
- **DATA** = `$XDG_DATA_HOME/pi-stack` else `~/.local/share/pi-stack`  (precious)
- **STATE** = `$XDG_STATE_HOME/pi-stack` else `~/.local/state/pi-stack`  (regenerable)

| artifact | base | path | why |
|---|---|---|---|
| config.toml | CONFIG | ~/.config/pi-stack/config.toml | user settings = config |
| op-refs.env | CONFIG | ~/.config/pi-stack/op-refs.env | 1Password refs (not secrets) = config |
| broker-token | CONFIG | ~/.config/pi-stack/broker-token | config-adjacent |
| serve.pid | STATE | ~/.local/state/pi-stack/serve.pid | runtime state, rewritten each serve |
| memory.db (+wal/shm/lock) | DATA | ~/.local/share/pi-stack/memory/memory.db | the precious artifact, irreplaceable |
| backups/ | DATA | ~/.local/share/pi-stack/backups/ | once source is gone, the backup IS the data |
| knowledge bundle (default) | DATA | ~/.local/share/pi-stack/knowledge/ | user-authored OKF content |
| knowledge index | STATE | ~/.local/state/pi-stack/knowledge/index.db | cache; reindex() rebuilds it |
| knowledge-cache/ | STATE | ~/.local/state/pi-stack/knowledge-cache/ | re-cloneable git bundles |
| tasks/ | STATE | ~/.local/state/pi-stack/tasks/ | already correct; route through module |

Untouched (correctly in-repo, NOT host storage): `<repo>/.pi-stack/{knowledge,
knowledge.scope,profile}`.

Debatable calls, decided: serve.pid→STATE (operational, not intent; XDG_RUNTIME_DIR
is purest but unreliable on macOS/CI). backups→DATA ("would the user be upset to
lose it?" yes). index→STATE (rebuildable). bundle→DATA (authored). index db file
renamed `knowledge.db`→`index.db` to signal "derived"; old name accepted on read
for one release.

## Single paths module (in `config`, no consumer hand-builds a path again)

```go
func ConfigDir() (string, error)   // export of configDir()
func DataDir()   (string, error)   // $XDG_DATA_HOME/pi-stack else ~/.local/share/pi-stack
func StateDir()  (string, error)   // $XDG_STATE_HOME/pi-stack else ~/.local/state/pi-stack

func Path() string                 // config.toml (unchanged; honors PI_STACK_CONFIG)
func OpRefsPath() string           // unchanged (CONFIG)
func BrokerTokenPath() string      // formalize (CONFIG)

func ServePidPath() string         // STATE/serve.pid   (was config-dir)
func KnowledgeIndexPath() string   // $KNOWLEDGE_DB else STATE/knowledge/index.db (+legacy fallback)
func KnowledgeCacheDir() string    // STATE/knowledge-cache
func TasksRoot() string            // STATE/tasks   (dedups task.go)

func MemoryDBPath() string         // $MEMORY_DB else DATA/memory/memory.db (+legacy fallback)
func MemoryLockPath() string       // dir(MemoryDBPath())/.memory.lock (shape unchanged)
func BackupsDir() (string, error)  // DATA/backups
func KnowledgeBundleDefault() string // DATA/knowledge
```

Env precedence per helper: explicit file override (MEMORY_DB / KNOWLEDGE_DB /
PI_STACK_CONFIG) > XDG_* > ~/… (unchanged).

**Legacy read-fallback** (makes migration non-urgent and lossless): `MemoryDBPath`
and `KnowledgeIndexPath` return the env override if set; else the new path if it
exists; else the OLD `~/.pi-stack/…` path if IT exists (pre-migration read); else
the new path. So any new-binary reader (serve, backup, restore) finds the db
whether or not migration has physically run. Branch is documented for removal next
major.

## Migration (the DX crux): a receipt, not a request

**Trigger: both.** `config.Migrate()` runs once at launcher startup (before
dispatch) and at `pi-stack-host serve` startup (before opening the db) —
idempotent, so double-fire is safe; plus an explicit `pi-stack migrate` verb
(verbose) for those who want to see/force it.

**Core is hermetic** (mirrors reset.go's env seam): injected getenv / homeDir /
stat / rename / copyTree / serveUp / now / out; `Migrate(env) (moved []string, err)`.

**Order: cheap+safe first, precious+guarded last.**
1. Per-artifact env-override short-circuit: if MEMORY_DB / KNOWLEDGE_DB /
   PI_STACK_CONFIG pins that artifact, skip it (the user owns that location).
2. Move knowledge **bundle** `~/.config/pi-stack/knowledge` → DATA/knowledge, and
   rewrite matching `knowledge_bundles` entries in config.toml (canonicalize both
   sides). Safe (git repo, no live writer).
3. Move **knowledge-cache** → STATE/knowledge-cache. Safe.
4. Move **backups** `~/.pi-stack/backups` → DATA/backups. Safe.
5. Move **knowledge index** `~/.pi-stack/knowledge/*.db` → STATE/knowledge/index.db.
   Guarded (sqlite): skip if serveUp().
6. Move **memory** `~/.pi-stack/memory/` → DATA/memory/ LAST and GUARDED:
   refuse (leave in place) when serveUp() (probe BOTH memory+knowledge ports, like
   reset.go's serveStillUp); move the WHOLE `memory/` dir in one rename so
   db+wal+shm never split; on EXDEV copy db+wal+shm, `PRAGMA integrity_check` the
   copy, THEN remove the source (never delete before verify). The precious
   artifact is never at risk.
7. serve.pid: not migrated (ephemeral; next serve rewrites it at the new path).

**v1 never deletes an old copy** — for the precious data (memory, bundle) we
copy-verify and leave the old copy as a safety net; regenerable state may be
renamed. Old empty dirs are left for the user (a later release can prune). This is
the zero-data-loss guarantee.

**Idempotent**: every step is `if exists(old) && !exists(new) { move }`. Blocked
steps (serve up) just retry next startup.

**User-facing output — a receipt (past tense, no prompt, no flag to discover):**
```
pi-stack: tidied storage into the standard layout.
  memory      ~/.pi-stack/memory            ->  ~/.local/share/pi-stack/memory
  knowledge   ~/.config/pi-stack/knowledge  ->  ~/.local/share/pi-stack/knowledge
  index+cache ~/.pi-stack/knowledge         ->  ~/.local/state/pi-stack/knowledge
  tasks/cache                                    ~/.local/state/pi-stack
  Nothing was deleted. Your config and keys did not move.
```
Deferred-because-serve-running (calm, not scary):
```
pi-stack: moved knowledge + caches to the XDG layout.
  Your memory db is in use by a running `pi-stack serve`, so it stays put for now
  and moves automatically the next time serve restarts. Nothing is wrong.
```
Fresh install: prints NOTHING — dirs are created lazily at the right paths.

## Where-is-my-stuff view

`pi-stack paths` (and a section in `doctor`) prints the three bases with a
one-word gloss and any active overrides, so the layout is self-documenting:
```
pi-stack paths
  config   ~/.config/pi-stack           config.toml, op-refs.env
  data     ~/.local/share/pi-stack      memory, knowledge, backups   (precious)
  state    ~/.local/state/pi-stack      index, caches, tasks         (regenerable)
  overrides  MEMORY_DB=…  (none set)
```

## Consumer change list (every one must agree)

config.go (export ConfigDir; add DataDir/StateDir/BrokerTokenPath/BackupsDir/
KnowledgeBundleDefault/KnowledgeCacheDir/KnowledgeIndexPath/TasksRoot; ServePidPath
+ MemoryDBPath bodies + legacy fallback); new config/migrate.go; knowledge.go
(buildKnowledgeStore -> KnowledgeIndexPath); memory.go (buildMemStore ->
MemoryDBPath); memory_backup.go + memory_restore.go (db + backups defaults);
cmd/knowledge.go (defaultKnowledgeDir/knowledgeCacheDir); task.go (taskStateRoot ->
TasksRoot, same value); reset.go (dataRoot -> DataDir; add old ~/.pi-stack +
old config knowledge*/backups as move-aside targets during the window); serve.go +
main.go (call Migrate; add `migrate` + `paths` verbs); doctor.go/status.go
(report new paths + pending-migration note); setup.go/firstrun.go (seed via
module); help.go + AGENTS.md + README doc strings.

## Scope

- **v1**: paths module, new layout, guarded auto-migration (both triggers) +
  `pi-stack migrate` + `pi-stack paths`, legacy read-fallback, all consumers
  routed through the module, reset/backup/restore/doctor updated, zero-delete of
  old precious copies. Full hermetic + real-file tests.
- **v2 (defer)**: prune the emptied old dirs; drop the legacy read-fallback +
  old-name index acceptance (next major).

## Branch

Stack on `task-worktrees` (the current tip): `task.go` only exists there, and this
routes it through the paths module. Keeps the linear stack #11 <- #12 <- #13; merge
in order. Diff is taken against `task-worktrees`.

## Invariants (non-negotiable)

Zero data loss; zero manual steps on the happy path; idempotent; every env
override always wins; the live memory sqlite db is NEVER moved (serveUp guard +
whole-dir/atomic); a single paths module (grep proves no hand-built default path
remains).

## Risks

- Splitting a live sqlite db from its -wal (P0 corruption) — mitigated by the
  serveUp guard + whole-dir rename + EXDEV copy-verify-then-delete.
- Consumer drift (a missed call site silently reads the wrong dir) — mitigated by
  routing 100% through the module and a grep test.
- Migrating an env-pinned path (trust betrayal) — mitigated by the per-artifact
  override short-circuit.
