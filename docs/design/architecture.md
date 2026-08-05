# Architecture

This is the contract. It is enforced by `services/host/arch_test.go`, which
fails the build on violation — so it cannot rot into aspirational prose.

## The rule

**Five layers. Imports point down, never up, never sideways within L1.**

```
L4  cmd/pix          argv -> a command. Owns os.Exit. Nothing imports it.
L3  workflow/*       pack, doctor, launch, models, provision. Orchestrate L1+L2.
L2  health           turns L1 probes into a Snapshot with verdicts (plus
                     supervise, the process lifecycle that RUNS a capability).
L1  capability/*     inference, secret, mcp, service, memory, monitor, plugin,
                     sandbox, workflow/task. One domain each. MAY NOT import
                     each other.
L0  foundation       sys, config, routing, rpc, cli, hostenv, launcher, lease,
                     workspace.
```

The load-bearing clause is **L1 may not import L1**. Everything else follows
from it.

## Why that clause

The old `cmd/pix` was 40,905 lines in one package: 91 files, 2,855 functions,
all mutually visible, no import graph, so no boundary anyone could violate.
Measured with `scripts/extract-pkg` (go/parser — a regex is off by 12× here),
every domain was dominated not by what it exported but by what it needed BACK:

| domain | LOC   | exports | needs back |
|--------|-------|---------|------------|
| slack  | 1,885 | 3       | **27**     |
| pack   | 5,471 | 22      | **36**     |
| task   | 2,550 | 12      | **35**     |

`slack` is the diagnosis in one line: three symbols leave it, twenty-seven come
back — spanning readiness types, secret's op-ref parsing, and MCP registration.
Splitting it into a package would have created an import cycle.

A two-cluster hypothesis was tested and failed too: the launch cluster needed 73
symbols back, host-management 90, from each other. So this was never seven
domains, or two. It was **one web**, and the web existed because *capabilities
called each other*.

The fix is not smaller files. It is that **`slack` must not call `secret` and
`mcp`** — the *workflow* (`gworkspace setup`) calls secret, then mcp, then
slack. Capabilities become leaves; only workflows compose. That single
inversion turns a web into a DAG.

## What each layer may do

**L0 foundation.** No domain knowledge. `sys` is OS seams and cannot know what
a model is; `config` is the file format and cannot know what a probe is. A
change here is felt everywhere, so the bar is: would this be true of any CLI?

**L1 capability.** One domain, one package, no siblings. A capability owns its
own types, its own probe, and its own errors. It takes what it needs as
parameters — including from other capabilities. If `mcp` needs a resolved
credential, the caller passes the credential; `mcp` does not import `secret`.

**L2 readiness.** The one shared model of "is this working": `requirement` ×
`verdict` × evidence, assembled by `buildSnapshot`. It is its own layer because
`doctor`, `status`, `setup` and launch-gating all derive from it, and the worst
outcome in this codebase would be two of them disagreeing about whether the
stack is healthy. It depends on L1 for probes and on nothing else.

**L3 workflow.** A user-facing verb's logic. Workflows are where cross-domain
sequencing lives, and they are ALLOWED to be big — `setup` legitimately touches
eight capabilities. What they may not do is contain a capability.

**L4 cmd/pix.** argv, dependency construction, and the single `os.Exit`. It
shrinks toward a lookup table.

## Testability follows from the layering

- L0/L1 take `sys.System`; tests pass `systest.Fake`, whose unwired methods fail
  loudly rather than returning a zero that reads like an answer.
- L3 workflows take capability interfaces; tests pass in-memory fakes.
- L4 commands are structs with `Run(*cli.Deps) error`; tests construct one and
  call it. No subprocess, no globals — the three verbs migrated so far
  (`models`, `agent`, `secret`) demonstrate it.

## Migration state

Layering is enforced from day one for the packages that exist. `cmd/pix` is
exempt while it is still being drained, and that exemption is the ONLY one —
`arch_test.go` names it explicitly so it cannot quietly grow to cover a new
violation.

```
L0  sys  sys/systest  config  routing  rpc  cli  launcher
    lease  hostenv  workspace                                        done
L1  inference  mcp  secret  memory  service  monitor  plugin
    sandbox  workflow/task                                           done
L2  health  supervise                                                done
L3  workflow/{pack, doctor, launch, models, provision}                done
L4  cmd/pix                                                          done
```

`knowledge`, `okf`, `slackoauth`, `slack`, `gworkspace`, `onboard`, `reset`,
`upgrade`, `man`, and `backup` — all present in an earlier revision of this
map — no longer exist. `slack` and `gworkspace` were externalized (see
`docs/design/slack-setup.md`, `docs/design/gworkspace-externalization.md`);
`onboard`/`reset`/`upgrade`/`man`/`backup` were retired outright (their verbs
answer `PIX_RETIRED`); `knowledge` and `okf` collapsed once the public stack
shipped no built-in corpus — `knowledge` is a capability a pack wires
(`files`/`http`), not a host package. `lease` (U04a: per-sandbox
lifecycle/ref-lock primitives), `sandbox` (U04b: naming, listing, argv
planning, fingerprinting), `workflow/task` (Story06: task-checkout, filed
under `workflow/` for discoverability but an L1 capability, not a workflow),
`supervise` (the Suture-based process lifecycle that runs a capability, filed
at L2 because it composes `plugin` rather than owning a domain), and
`workflow/provision` (the current name for what used to be called `setup`)
are net-new since the table above was first written.

**`readiness` and `readiness/axis` are gone (W5/U11r).** The Requirement ×
Verdict model, its lazy axis registry, its four exit codes and its second
renderer were replaced by `health` (Probe → Result → Snapshot). The utilities
that lived in `axis` only by history moved to the domains that own them:
session-model resolution, Ollama endpoint resolution and machine sizing to
`inference`; "is a model key present" to `secret`; the launch gate and its
warning rows to `workflow/launch`.

`cmd/pix` is **40,905 -> 3,207** production lines across 20 files (measured at
this writing; the count keeps shrinking as unrelated tidy passes trim prose
and dead branches — treat the ratio, not the exact figure, as the point):
thirteen argv seams, the composition root, the verb table, and the two kong
verbs whose handlers take command structs directly (`agent`, `models`) and are
therefore L4 by definition.

**drainingPackages is empty.** It held one entry, `cmd/pix`, for the whole of
this work; every package in the module now satisfies the layering with no
exemption. `TestArchitecture_DrainingListIsShrinking` ratchets it at zero.

Order to drain in is **inbound count ascending**, because inbound is the real
blocker (an extracted package cannot import `package main`):

**Historical record.** The table below is the plan for the original
40,905-line extraction, kept for what it teaches about measuring before you
move code. Several of its domains (`knowledge`, `man`/`backup`, `upgrade`,
`slack`, `reset`, `onboard`, `gworkspace`) were later externalized or retired
outright and no longer exist in the tree; the counts are not current LOC.

| domain               | LOC   | inbound at plan | at extraction |
|----------------------|-------|-----------------|---------------|
| profile + workspace  | 292   | 3               | 0             |
| serve → `service`    | 1,961 | 11              | 0             |
| readiness model      | 786   | 17              | 2             |
| readiness/axis       | 1,426 | —               | 2             |
| knowledge            | 1,050 | 17              | 0             |
| secret               | 1,770 | 19              | 0             |
| man / backup         | 340   | 21              | 1             |
| upgrade              | 642   | 8               | 2             |
| mcp                  | 1,373 | 39              | 0             |
| slack                | 1,798 | **27**          | 2             |
| pack                 | 5,223 | 36              | 0             |
| reset                | 1,049 | 9               | 1             |
| onboard              | 422   | 10              | 1             |
| gworkspace           | 1,227 | 23              | 1             |
| doctor + status      | 2,791 | **110**         | 3             |
| launch (run + task)  | 5,714 | 63              | 3             |
| setup                | 4,624 | **110**         | 2             |

Every count in the middle column was measured before starting; every count in
the right column was measured immediately before the `git mv`. **The gap between
them is the whole method.** Nothing was extracted at its planned cost, because
the cost was never the code — it was symbols living in the wrong file.

**Re-measure before starting one of these.** Every extraction so far dropped the
next one's count, usually by more than the extraction itself was worth: nine
deletions of forwarding wrappers took `man` from 21 to 1 and `pack` from 21 to
13, and `mcp` alone took `slack` from 15 to 9.

Three counts in that table are noise, in every domain: `main` (the sizer seeing
`func main`), `version` (any parameter of that name), and `defaultShellEnv`.
Subtract them before deciding an extraction is expensive.

**`doctor` and `setup` were both declared immovable in this document, twice,
and both moved.** "A high inbound count is what a workflow looks like" was the
wrong inference from a true observation. Their counts were 110 each; the real
figures were 3 and 2, and the difference was builders and helpers filed under
whichever flow first displayed them.

The rule that survives is narrower and testable: a symbol belongs to the package
that owns the CONCEPT, not to the first caller written. If a count looks like a
composition root, check whether it is one before believing it.

When a domain's inbound count is stubborn, the cause is almost always a
capability calling a sibling. Invert it into the workflow rather than moving
both.

## Three lessons from doing it

**Cut the machinery, not the folder.** `readiness_*.go` measured 17 inbound as a
unit and 2 as just types+snapshot+render. The domain axis BUILDERS were the
entanglement; the model was always independent. Before moving a directory, check
whether a smaller thing inside it is the real package.

**Go forces some moves; inject rather than surrender.** A method must live with
its type, so `Report.Render` had to follow `Report` into `readiness` — carrying
doctor's hard-coded advice strings with it. Making them `readiness.Hints`,
passed in, kept the layer clean. When the compiler forces a move, look for what
the moved code knew that it should not have.

**Mechanical renames leak into prose.** Exporting `outstanding` → `Outstanding`
silently rewrote the doctor headline a user reads. Twice a rename also landed
inside a string literal (`"google-workspace"` → `"google-ws"`, `-test.run` →
`-test.Run`). The gate caught all of them, which is the argument for running it
on every commit rather than at the end.

## Three more, from the second stretch

**The blocker is usually a symbol, not a file.** `secret` went 13 inbound to 0,
and `mcp` 23 to 5, before either changed directory — by moving declarations to
the package that owns the CONCEPT. "How do you read `sbx mcp auth status`
output" is MCP knowledge; it was in doctor_mcp.go because doctor was the first
caller. Measure, then look at WHERE each inbound symbol is declared: if it is
named after your domain and lives somewhere else, that is the whole extraction.
`scripts/movesym` does this via go/parser.

**Count how many domains need each symbol before extracting any of them.**
Nine symbols blocked four to eight domains each — six were one-line forwarding
wrappers around functions that already existed in L0. Deleting them bought more
coupling reduction than an entire extraction did, and it was net-negative lines.
A wrapper is not free: it is a second name for one thing, and every caller of
the wrapper counts as needing the wrapping package back.

**A type two capabilities exchange belongs below both.** `packContainer` was
pack's, and mcp consumed it. Moving it either way creates a sibling edge;
moving it DOWN to config removes it. Same for `classifyProbeFailure`, which
seven files needed and which is now `sys.ClassifyProbeFailure`. When two
capabilities look coupled, check whether what they share is data rather than
behaviour — data has a lower home.

## What the rule caught, unprompted

`arch_test` failed twice on real violations that looked fine in review:
`memory` and `knowledge` importing `service` and `workspace`, and `mcp`
importing `secret`. The last one is the exact shape the architecture exists to
prevent, and it appeared on the first run after `mcp` became a package.

The fix in each case was an inversion, not a rewrite, and it improved the code
independently of the rule. `mcp.Credentials` is four booleans-and-strings the
workflow resolves and passes in; the gog-wrapper test that used to write real
op-refs.env files into a temp dir and fake two OS seams is now three struct
literals. **The layering paid for itself in testability the same day it cost
something in indirection.**

## The guards all rotted, every one

Six source-scanning guards in this repo hand-listed the files they checked, and
every single one silently stopped checking something as packages moved:

| guard | what it stopped seeing |
|---|---|
| `assertOnlyCalledFrom` | globbed `*.go` in its own directory; lost the mcp.go call site it exists to guard |
| `TestRendererPurity` | named eleven renderer files; three had moved |
| `TestPrimaryHelpAndStatusAvoidEmDashes` | read `status.go` by bare name |
| `TestSetupReport_NeverReadsInventory` | read `setup_models.go` by bare name |
| `tests/pi-patches.test.mjs` (Node) | read `cmd/pix/hostrun.go` and grepped for a since-renamed symbol |
| `TestTaskLaunchUsesSharedCreateLifecycle` | read `task.go` by bare name |

Not one of them failed loudly at the moment it went blind. Two failed only
because a path stopped resolving; the rest were found by running them after a
move and noticing they still passed when they should not have.

**A guard that hand-lists its inputs has a half-life.** The fix in each case was
to DERIVE the input set: walk the module, or select by a property that travels
with the code (imports readiness, mentions `readiness.Group`, is a non-test .go
file). Deriving it is also how the guard gets stronger — `assertOnlyCalledFrom`
now covers 27 packages instead of one directory.

Two failure modes to avoid when deriving:

- **Too broad is a different kind of broken.** Scoping the glyph guard to
  "imports readiness" caught 68 glyphs in setup/reset/secret step-progress
  output, which it never guarded. In-scope is now "mentions readiness.Group /
  Report / Snapshot", and the one file that legitimately does both carries a
  documented, shrink-only exemption.
- **Assert the derivation found something.** Both walks now fail if they scanned
  implausibly few files, because a walk that silently matches nothing is exactly
  the state these guards were already in.

## On driving the compiler in a loop

Most of this work was done by scripts that read a compile error, edit the
source, and recompile — 900-iteration loops. It is the only way to move 37,000
lines without hand-editing, and it is genuinely safe here because the compiler
and 1,500 tests run after every single step.

It is not safe in the way it feels safe. Things it did wrong, all caught:

- Wrote `unexported:` as a literal field name, twice, having parsed the
  compiler's phrase *"but does have unexported field name"* as a suggestion.
- Renamed a package-level `version` to `launcher.Version` and hit four
  **parameters** of the same name, so four functions silently began reading the
  global instead of their argument. Three tests caught it; the tell was a syntax
  error in an unrelated signature.
- Renamed `gog`→`Gog` inside 28 strings and 42 comments, `provenance`→
  `upgrade.Provenance` inside 23 strings and 57 comments, and turned the doctor's
  advice `pix config set mcp <server>` into `pix config set group <server>`.
- Exported test-only fixtures (`BareGog`, `ReceiptClock`, `DefaultCfg`,
  `Phase2HostPack`) into packages' public APIs, because a bulk pass cannot tell
  scaffolding from API.

Every one of those was caught by the compiler, a test, or the string-and-comment
diff — which is the argument for running all three after every step, not at the
end.
