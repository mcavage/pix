# Architecture

This is the contract. It is enforced by `services/host/arch_test.go`, which
fails the build on violation — so it cannot rot into aspirational prose.

## The rule

**Five layers. Imports point down, never up, never sideways within L1.**

```
L4  cmd/pix          argv -> a command. Owns os.Exit. Nothing imports it.
L3  workflow/*       doctor, setup, status, run, task. Orchestrate L1+L2.
L2  readiness        turns L1 probes into a Snapshot with verdicts.
L1  capability/*     inference, secret, mcp, service, pack, sandbox, agentdef.
                     One domain each. MAY NOT import each other.
L0  foundation       sys, config, routing, rpc, cli, hostenv.
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
    hostenv  hostenv/hostenvtest  workspace                          done
L1  inference  monitor  monitor/tui  okf  plugin  slackoauth
    service  memory  knowledge  secret  mcp                          partial
L2  readiness                                                        done
L3  workflow/man  workflow/backup  workflow/upgrade                  open
L4  cmd/pix                                                          draining
```

`cmd/pix` is **40,905 -> 27,836** production lines across 64 files, and the
layering is enforced from L0 through L4.

Order to drain in is **inbound count ascending**, because inbound is the real
blocker (an extracted package cannot import `package main`):

| next | domain              | LOC   | needs back | state |
|------|---------------------|-------|------------|-------|
| —    | profile + workspace | 292   | 3          | done  |
| —    | serve → `service`   | 1,961 | 11         | done  |
| —    | readiness machinery | 786   | 2          | done  |
| —    | knowledge           | 1,050 | 17         | done  |
| —    | secret              | 1,770 | 19 → 0     | done  |
| —    | man/backup/upgrade  | 982   | 1 each     | done  |
| —    | mcp                 | 1,373 | 23 → 0     | done  |
| 1    | onboard             | 423   | 6          |       |
| 2    | pack                | 5,434 | 8          |       |
| 3    | slack               | 1,893 | 9          |       |
| 4    | reset               | 977   | 9          |       |
| 5    | status              | 734   | 21         |       |
| 6    | gworkspace          | 466   | 23         |       |
| 7    | task                | 2,557 | 25         |       |
| 8    | run + sandbox       | 1,248 | 47         |       |
| —    | setup               | 2,546 | 58         | stays |
| —    | doctor              | 2,535 | 34         | stays |

**Re-measure before starting one of these.** Every extraction so far dropped the
next one's count, usually by more than the extraction itself was worth: nine
deletions of forwarding wrappers took `man` from 21 to 1 and `pack` from 21 to
13, and `mcp` alone took `slack` from 15 to 9.

Three counts in that table are noise, in every domain: `main` (the sizer seeing
`func main`), `version` (any parameter of that name), and `defaultShellEnv`.
Subtract them before deciding an extraction is expensive.

`doctor` and `setup` are last and may never move: **a high inbound count is
what a workflow looks like**. A composition root that consumes every capability
is correct, not tangled. Do not "fix" it.

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
