# Rearchitecture: foundations for a 41k-line package

Status: **in progress.** Phase 0 done, Phase 1 in flight. This doc is both the
plan and the handoff — read "Where we are" before picking anything up.

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
- The 125 nil-guards are deleted. `errNoProbeSeam` becomes unnecessary and goes
  with them.

Domain probes (`slackAuthTest`, `identityProbe`, `directInferenceProbe`,
`ollamaInferenceProbe`) deliberately do NOT live in `sys` — `sys` is OS seams
with no domain knowledge. They stay on a shrinking `deps` struct in
`cmd/pix` and move out with their packages in Phase 3.

**Migration is compiler-verified**, which is what makes a 570-site change safe:
every field access is a 1:1 rename, and the 205 `shellEnv{...}` test literals
are rewritten by a brace-matching script (`scripts/migrate-shellenv.py`) into
`&systest.Fake{...}`. Nothing is hand-edited in bulk; the type checker and 1,501
tests are the proof.

### Phase 2 — `cli`: one command model

Zero files currently import `flag`. All 34 verbs hand-roll their own arg loop,
usage string and exit: **266 `os.Exit` calls across 25 files, 349 direct
`os.Stderr` writes.** That is why the help screens drift — there is no single
place that could keep them honest, and no way to assert a verb's exit code
without spawning a subprocess (several tests do).

**Library: `github.com/alecthomas/kong`.** Chosen over cobra because it has
**zero transitive dependencies** (verified), because commands are plain structs
with a `Run(deps) error` method — which is the testability requirement stated
directly — and because struct tags replace the hand-rolled parsing, so it
*removes* lines rather than adding a framework's worth.

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
- [ ] **Phase 1** — `sys` + `systest`, migration, 125 guards deleted.
- [ ] **Phase 2** — `cli` + kong; verbs migrated incrementally.
- [ ] **Phase 3** — domain packages.

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
