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
L0  sys  sys/systest  config  routing  rpc  cli  hostenv        done
L1  inference  monitor  monitor/tui  okf  plugin  slackoauth    partial
L2  readiness                                                   pending
L3  —                                                           pending
L4  cmd/pix                                                     draining
```

Order to drain in is **inbound count ascending**, because inbound is the real
blocker (an extracted package cannot import `package main`):

| next | domain              | LOC   | needs back |
|------|---------------------|-------|------------|
| 1    | profile + workspace | 292   | 7          |
| 2    | serve → `service`   | 1,961 | 14         |
| 3    | readiness           | 1,926 | 17         |
| 4    | knowledge           | 1,050 | 17         |
| 5    | secret              | 1,441 | 19         |
| 6    | pack                | 5,471 | 36         |
| 7    | mcp                 | 1,479 | 39         |
| 8    | task                | 2,550 | 35         |
| 9    | run + sandbox       | 2,464 | 63         |
| —    | doctor              | 2,892 | 110        |

`doctor` is last and may never move: **110 inbound is what a workflow looks
like**. A composition root that consumes every capability is correct, not
tangled. Do not "fix" it.

When a domain's inbound count is stubborn, the cause is almost always a
capability calling a sibling. Invert it into the workflow rather than moving
both.
