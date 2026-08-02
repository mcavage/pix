# Rearchitecture: foundations for a 41k-line package

Status: **in progress.** Phases 0 and 1 are done and on `main`'s
`refactor/foundations` branch, gate green. Phases 2 and 3 are specified below
and not started. This doc is both the plan and the handoff — read "Where we
are" before picking anything up.

## Why

`services/host/cmd/pix` is 40,905 production lines in ONE Go package: 91 files,
2,855 top-level functions, 182 types, 955 package-level vars, all mutually
visible. There is no import graph, so there is no boundary anyone can violate,
because there is nothing to violate.

The code is not careless. The readiness model (`requirement` × `verdict` ×
evidence, `buildSnapshot`) is a genuinely good abstraction that `doctor`,
`status`, `setup` and launch-gating all share. Comments are 27% of production
lines and are overwhelmingly *why*. Invariants are defended by tests that state
their own rationale, plus meta-guards (context budget ratchet, rename guard,
open-core marker check).

The problem is narrower and more fixable: **the code knows things the structure
does not enforce.** Three bugs in one week had the same shape — a comment stated
the correct rule and the code did something else, with nothing in the type
system to notice:

1. The model router resolved against the shipped catalog instead of the host's
   bindings, because the binding-aware resolver was marooned in `package main`
   where `pix-host` could not reach it. (Fixed: `services/host/inference`.)
2. `verifyDirectInference`/`verifyOllamaInference` returned a clean-looking zero
   when handed a `shellEnv` with no probe, so "not checked" and "checked, found
   nothing" were the same value. (Fixed: `errNoProbeSeam`.)
3. `pix setup` exited non-zero when a user declined a model download, because
   the hard-error branch preceded the consent check. Invisible for its whole
   life because the covering test wired no probe, so the failing branch was
   never reached. (Fixed: consent-first ordering.)

All three are the same root cause in different clothes: **absence is
representable, and every call site invents its own meaning for it.**

### The measurement that settles it

`shellEnv` is a 22-field struct of nullable function pointers threaded through
254 functions, with **125 nil-guards**. For the single condition `env.run ==
nil`, the package contains **14 distinct behaviours**:

```
  7  return fmt.Errorf("internal: shellEnv.run not wired")
  2  return nil, false
  2  return false
  1  return true                       // localImageLoaded: fails OPEN, documented
  1  return "", sbxSecretsAbsent
  1  return "", fmt.Errorf("git not available")
  ...
```

Some are considered (`localImageLoaded` explains itself). That is precisely the
problem: a reader cannot tell considered from accidental without reading all
125, and the accidental ones are silent by construction.

## Target architecture

Three rules, in priority order:

1. **Absence is unrepresentable.** No production code path can observe a nil
   capability. A test that declines to wire something gets a loud refusal at
   call time, never a zero value.
2. **A signature states what it touches.** A function that reads a file takes a
   file reader, not the world. Go's implicit interfaces make this free.
3. **One definition per fact.** Anything two binaries or two commands both need
   is a package, not a copy or a re-derivation.

### Layers (bottom-up, each importable by everything above)

```
config/        on-disk config.toml            (exists, unchanged)
routing/       catalog + scorecard + resolver (exists, dependency-light)
inference/     "callable on THIS host"        (exists — catalog ∩ bindings)
sys/           OS seams: Exec, FS, Env, Net   (NEW — Phase 1)
sys/systest/   the only test double           (NEW — Phase 1)
cli/           command model + kong wiring    (NEW — Phase 2)
<domain>/      pack, slack, mcp, task, ...    (Phase 3)
cmd/pix/       argv -> command lookup only    (shrinks to ~200 lines)
```

### Phase 1 — `sys`: kill the god struct

Four interfaces, split by *what they touch* (which is what a reader needs from a
signature), not by who calls them:

```go
type Exec interface {
    LookPath(name string) (string, error)
    Run(name string, args ...string) (string, error)
    RunTimed(name string, args ...string) (out string, timedOut bool, err error)
    RunInteractive(name string, args ...string) error
    RunInteractiveQuiet(name string, args ...string) error
}
type FS interface {
    ReadFile(path string) (string, error)
    WriteFile(path string, data []byte, perm os.FileMode) error
    IsFile(path string) bool
    Mode(path string) (os.FileMode, bool)
    Lock(lockPath string, fn func() error) error
}
type Env interface {
    Getenv(name string) string
    HomeDir() string
    Getwd() (string, error)
    StateDir() (string, error)
    Executable() (string, error)
}
type Net interface{ DialLocal(port int) bool }

type System interface { Exec; FS; Env; Net }
```

- `sys.Real()` is the one production implementation. It has no nullable state.
- `systest.Fake` is the one test double. Its fields are nullable *by design* —
  a nil field means "this test did not wire it", and calling it returns a loud
  error naming the method and the test's own fixture. Nullability is confined
  to the test double, where it is explicit and safe.
- The nil-guards are deleted: **125 -> 11**, and the 11 survivors are all on the
  domain probes below, which are not `sys`'s business.

Domain probes (`slackAuthTest`, `identityProbe`, `directInferenceProbe`,
`ollamaInferenceProbe`) deliberately do NOT live in `sys` — `sys` is OS seams
with no domain knowledge. They stay on a shrinking `deps` struct in
`cmd/pix` and move out with their packages in Phase 3.

**Migration was compiler-verified**, which is what made a ~780-site change safe.
Nothing was hand-edited in bulk; the type checker and 1,501 tests were the proof.
See "Tools, and why they are committed" below.

`sys.Exec` also carries `RunWithin(timeout, ...)`, because one caller needs a
tighter bound than the default and the previous code got that by reaching past
the seam to the real runner — which silently un-faked a hermetic test.

### Phase 2 — `cli`: one command model

Zero files currently import `flag`. All 34 verbs hand-roll their own arg loop,
usage string and exit: **266 `os.Exit` calls across 25 files, 349 direct
`os.Stderr` writes.** That is why the help screens drift — there is no single
place that could keep them honest, and no way to assert a verb's exit code
without spawning a subprocess (several tests do).

**Library: `github.com/alecthomas/kong`** (decided; owner chose it over cobra
after seeing both spelled out).

Cobra is the default choice and deserves the rebuttal. Dependency weight is NOT
the reason: cobra pulls three small pure-Go modules (cobra, pflag, mousetrap),
kong pulls one with no transitive deps — a real but minor difference. The
deciding factors were the two things this refactor is actually about:

- **Boilerplate.** The same `models add` command is 8 lines in kong and ~20 in
  cobra, because a kong flag is a struct field with a tag while a cobra flag is
  a declaration plus a registration call plus a target variable. Across 34 verbs
  that is the difference between the CLI layer shrinking and growing.
- **Where dependencies live.** Cobra's `RunE` has no parameter for them, so they
  arrive via `cmd.Context()` (untyped) and flag targets become package-level
  vars — which is the global mutable state this refactor exists to remove. Kong
  binds a typed `*cli.Deps` and hands it to `Run`, so a command is testable by
  constructing the struct and calling the method: no parser, no globals, no
  subprocess.

What cobra would have bought — shell completions and man-page generation — this
repo already implements itself (`pix help --all`, `--man`), and those renderers
are staying either way.

```go
type ModelsAddCmd struct {
    Provider string `arg:"" help:"anthropic | google | openai | ollama"`
    Local    bool   `help:"Ollama: only models that run on this machine."`
    Cloud    bool   `help:"Ollama: only models on your ollama.com plan."`
}
func (c *ModelsAddCmd) Run(d *cli.Deps) error { ... }
```

Rules the model enforces:
- A command **returns an error**; it never calls `os.Exit`. `main` owns the
  single exit point and the single error renderer.
- A command **writes to `d.Out`/`d.Err`**, never `os.Stdout`/`os.Stderr`. This
  is what makes output assertable without a subprocess.
- Usage is **generated**, so a flag cannot exist without appearing in help.

Existing help/man behaviour (`pix help --all`, `pix help <verb>`, `--man`,
retired-verb aliases) is preserved by keeping the current renderers and feeding
them kong's model — the tiered help is a real feature, not incidental.

### Phase 3 — extract packages

The domains already exist as name prefixes doing a package's job: `slack` (34
funcs), `mcp` (29), `setup` (21), `ollama` (15), `task` (14), `pack` (13),
`knowledge` (13), `memory` (9). Each becomes a package once Phase 1 removes the
`shellEnv` coupling that currently makes every function reachable only from
`package main`.

Order: cheapest and most self-contained first — `readiness` (already a coherent
subsystem, 9 files), `pack`, `slack`, `mcp`, `task`, `knowledge`. `setup`
last: it orchestrates all of them and should end up depending on packages
rather than containing them.

## Where we are

- [x] **Phase 0** — `services/host/inference` extracted; the router tells the
      truth about this host; `pix models add ollama`; probe seams return errors.
- [x] **Phase 1** — `sys` + `systest` landed and migrated. **125 nil guards ->
      11**, and all 11 remaining are on the four DOMAIN probes that deliberately
      do not live in `sys` and leave with their packages in Phase 3. Net **-623
      lines**. Gate green.
- [~] **Phase 2** — `services/host/cli` landed: the command contract, the single
      exit-code table, `cli.Run[T]`, `cli.Usage[T]`, and the shared primitives
      (`WantsHelp`/`IsTTY`/`Plural`) that every domain depended on. **Three verbs
      migrated** — `models`, `agent`, `secret` — deleting three hand-written
      usage blocks, three private arg-parsing kits, and 27 `os.Exit` calls from
      `agent` alone. **31 verbs remain.**
- [~] **Phase 3** — `hostenv` extracted (the bundle move that unblocks every
      other extraction) and `monitor/tui` extracted (3,129 lines). `cmd/pix` is
      **40,905 -> 37,456**. **The plan for the rest was wrong; see below.**

### Phase 3 was mis-scoped, and here is the measurement

The original plan said "extract seven domain packages, cheapest first". That was
wrong, and `scripts/extract-pkg` (go/parser, not regex) is what showed it.

Measured OUTBOUND (symbols the rest of `cmd/pix` needs from a candidate — the
export cost) and INBOUND (symbols the candidate needs back — the real blocker,
because an extracted package cannot import `package main`):

| domain              | LOC    | outbound | inbound |
|---------------------|--------|----------|---------|
| monitor/tui         | 3,280  | 2        | 1       | ← extracted
| rpcclient           | 126    | 10       | 1       |
| memory              | 509    | 6        | 11      |
| serve               | 1,959  | 20       | 16      |
| knowledge           | 1,049  | 9        | 20      |
| secret + syncedrefs | 1,632  | 27       | 20      |
| slack               | 1,885  | 3        | 27      |
| readiness + doctor  | 4,821  | 105      | 68      |
| task                | 2,550  | 12       | 35      |
| pack                | 5,471  | 22       | 36      |
| gog + gworkspace    | 1,298  | 8        | 37      |

`monitor/tui` came out cleanly because its seam was already one-directional
(`RunTUI`, `TUIConfig`). Nothing else is like that.

Then the two-cluster hypothesis was tested and ALSO failed:

- launch cluster (run, task, pack, sbxargs, inference, bootstrap, …): 12,529
  LOC, **73 inbound**
- host-management cluster (doctor, readiness, setup, secret, mcp, slack, gog,
  serve): 15,585 LOC, **90 inbound**

The two clusters depend on each other as heavily as their members do. So
`cmd/pix` is not seven domains, and it is not two — **it is one dense web**, and
extracting from it is UNTANGLING, not moving files. `slack` is the sharpest
illustration: only 3 symbols leave it, but it needs 27 back, spanning readiness
types, secret's op-ref parsing, and MCP registration. Moving it requires those
three to move first.

**Revised approach for whoever continues.** Do not pick a domain and try to move
it. Instead, repeatedly extract the SHARED KERNEL — the symbols that appear as
an inbound dependency of many domains — until the domains fall apart on their
own. Ranked by how many domains need them today:

```
9  wantsHelp        -> cli.WantsHelp        (done)
8  shellEnv         -> hostenv.Env          (done)
8  defaultShellEnv  -> hostenv              (next: hostenv.Default)
4  withFlock        -> sys.Lock             (done)
4  atomicWriteInDir -> sys.AtomicWriteInDir (done)
4  loadResolvedConfig -> config
3  isTTY / plural   -> cli                  (done)
3  findHostBinary / version -> a launcher-identity package
2  verdict* / check -> readiness types package
2  rpcClient / str  -> an rpc package
```

Each kernel move is small, safe, and reduces the inbound count of several
domains at once. When a domain's inbound count reaches roughly zero, it moves in
an afternoon.

**This is verified, not theorised.** `rpc` was extracted on exactly this basis
(inbound 0), and `memory`'s inbound count fell **11 -> 5** as a direct result,
without touching a line of memory.go. Its five survivors are the next lesson:
`wantsHelp` is already in `cli`, `loadResolvedConfig` belongs in `config`, and
`ensureServeUp`/`ensureServeTimeout` should be INVERTED rather than moved —
`memory` has no business knowing how to start a daemon, and should be handed an
"ensure it is up" function by whoever composes it. Extraction is not only about
moving code down; sometimes the dependency is pointing the wrong way.

### Migrating the next verb (Phase 2)

Copy `models_cmd.go`. The whole recipe:

1. Declare the verb tree as structs with `cmd:`/`arg:`/`help:` tags. One type
   per subcommand even when two share flags — kong dispatches on the type, so a
   shared type cannot tell which subcommand was selected. Embed the shared flags
   instead (see `hostQuery`).
2. Each leaf gets `Run(*cli.Deps) error`. Return errors; never `os.Exit`. Write
   to `d.Out`/`d.Err`; never `os.Stdout`.
3. `cli.Usagef` for a bad invocation (exit 2, usage printed). `cli.SilentError`
   when the command has already reported the problem in its own words.
4. Point main's `case` at `cli.Run[YourCmd]`, and point any `pix help <verb>`
   entry at `cli.Usage[YourCmd]` so the hand-written usage string can be deleted.
5. Delete the hand-rolled arg loop and its usage constant. If the verb's tests
   re-exec the test binary to observe an exit code, they no longer need to.

### Choosing the next package (Phase 3)

Rank by OUTBOUND coupling — how many symbols the rest of `cmd/pix` needs FROM
the candidate — not by size. Measured today:

| domain     | files | LOC   | symbols the rest of cmd/pix needs |
|------------|-------|-------|-----------------------------------|
| monitor    | 2     | 3,280 | 2   ← done                        |
| memory     | 1     | 509   | 13                                |
| serve      | 11    | 1,983 | 21                                |
| readiness  | 9     | 1,937 | 28 (mostly to/from doctor_*)      |
| knowledge  | 1     | 1,050 | 38                                |
| slack      | 2     | 1,885 | 40                                |
| mcp        | 4     | 2,012 | 56                                |
| task       | 1     | 2,559 | 61                                |
| pack       | 5     | 5,476 | 65                                |

`monitor` went first because its seam was already one-directional (`RunTUI`,
`TUIConfig`). `readiness` is the most valuable next move but should be taken
TOGETHER with `doctor_*`: doctor is readiness's renderer, and most of the 28
references cross that line. `pack` is last for a reason.

Beware the measurement: a naive identifier regex reports ~25 external symbols
for monitor when the true answer is 2, because common local names (`state`,
`result`, `check`, `parse`) collide with declarations elsewhere. Confirm with
the compiler before believing a number.

### What Phase 1 actually found

Every one of these was a fixture gap that the nullable seam had been rendering
as a result. They are listed because they are the argument for the whole
exercise, and because the same shape will keep surfacing in Phases 2-3:

- `ollama pull` always preferred `RunInteractive`; the plain-runner fallback
  existed only for fixtures that left it nil, so setup's pull tests spent their
  entire lives exercising a path no user ever took.
- `gogAuthed` reached PAST its own seam to the real bounded runner whenever a
  nil check happened to pass — a "hermetic" test shelled out for real.
- `localMCPNames` bailed on `env.run == nil`: production behaviour keyed off
  "am I running inside a test".
- An undeclared file in the doctor fixture returned an I/O error rather than
  `os.ErrNotExist`. "I did not declare this file" means absent.

### Tools, and why they are committed

- `scripts/migrate-shellenv/` (Go, go/parser) — rewrote 207 composite literals.
  A hand-rolled brace matcher was written first and abandoned: Go source has
  braces inside line comments, rune literals and raw strings, and a scanner that
  does not know the grammar mis-pairs them.
- `scripts/migrate-shellenv.py` — 576 field-access renames. Textual, but every
  false positive is a compile error (it caught `manEnv`, `resetFS`, `installFS`,
  `serveStarter` and `fakeProber`, all of which share field names by accident).
- `scripts/drop-nil-guards/` (Go, go/parser) — deleted 94 dead guards, skipping
  nested edits so an outer guard's stale offsets cannot splice into text an
  inner edit already moved.

## Rules for whoever picks this up

- **The gate stays green at every commit.** `make gate` (needs node ≥25 and
  `npm install`). The 12s budget line already fails on `main` and is not yours
  to fix.
- **Behaviour changes are separate commits from moves.** A refactor commit that
  also fixes a bug hides the bug.
- **Do not hand-edit in bulk.** If a change touches >20 sites, write the script,
  commit the script, and let the compiler check it.
- **Delete as you go.** This work should be net-negative on lines. If a phase
  adds lines, say why in the commit.
- **A comment that states a rule the types could enforce is a TODO.** That is
  the failure mode this whole document exists to end.
