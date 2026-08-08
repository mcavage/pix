# Handoff: the drain is finished

`services/host/cmd/pix` was 40,905 production lines in one Go package — 91
files, 2,855 functions, all mutually visible, no import graph, so no boundary
anyone could violate. It is 3,503 lines in 21 files, and the layering is
enforced by `services/host/arch_test.go` for every package with no exemptions.

Read `architecture.md` for the contract and the reasoning. This file is what to
do next and what not to break.

## State

```
cmd/pix production LOC    40,905 -> 3,503   (91 files -> 21)
packages                       6 -> 27
drainingPackages exemptions      1 -> 0
nil-seam guards              125 -> 11      (all on domain probes; see hostenv)
gate                       red -> green     (30.1s against a 34s ceiling)
```

What is left in cmd/pix is what L4 is for: thirteen `*_cmd.go` argv seams, the
composition root (`env.go`, `mcp_credentials.go`, `main.go`), the verb table
(`help.go`), and `agent.go` + `models.go`, whose handlers take kong command
structs directly and are therefore the command layer by definition.

## How to work

```sh
export PATH="/opt/homebrew/bin:$PATH"      # node 26, for the gate
cd /Users/mcavage/dev/pix
bash scripts/gate.sh                       # build, vet, test, node, tsc, guards
```

The gate is GREEN and must stay green. Wall time is now REPORTED locally, not
enforced: `scripts/gate.sh` defaults `GATE_BUDGET_MS` to 0 because the old 34s
ceiling had started failing correct suites (the suite is ~54s since the
lifecycle rearchitecture; see the rationale block in the script header). CI is
the one place the ceiling is enforced, at 75s. `tests/ci-gate.test.mjs` pins
both literals on purpose, so changing either is a deliberate, reviewed edit.
Run with `GATE_BUDGET_MS=<ms>` if you want a local hard fail back.

Rules, in priority order:

1. **Gate green at every commit.** Commit early — an uncommitted refactor that
   goes wrong costs the whole session (it did, once).
2. **>20 sites means write a script.** Commit the script. The compiler and the
   1,500 tests are the proof, not your eyes.
3. **Moves and behaviour changes are separate commits.** A refactor commit that
   also fixes a bug hides the bug.
4. **Net-negative on lines**, or say why in the message.

## Adding a package

`arch_test.go` fails on any package not placed in `pkgLayer`, which is the
prompt to decide where it belongs rather than discovering later. The layers and
the one load-bearing clause (**L1 may not import L1**) are in
`architecture.md`.

If a new capability needs something from a sibling, do not move either package:
define a struct of the facts it needs and let the workflow fill it. There are
worked examples — `mcp.Credentials`, `pack.RegisterFn` — and each made the tests
simpler, not just the graph. (Do it for a SIBLING's facts, not for composition a
package already holds: `onboard.Deps` was a second copy of provision's own
HostBinary/Register wiring, and folding onboarding into provision deleted it.)

`drainingPackages` is empty and ratcheted at zero. An entry is legitimate for a
package genuinely mid-extraction; it needs an argument in the commit message for
why the extraction cannot land in one step.

## The tools

| tool | what it is for |
|---|---|
| `scripts/extract-pkg` | Size an extraction. go/parser, NOT regex — a regex was off by 12× here. Run from the REPO ROOT with bare basename prefixes (`extract-pkg pack kitref`), not paths. |
| `scripts/movesym` | Move declarations between files in one package, carrying doc comments. The most useful tool in the set: most extractions were unblocked by moving symbols home, not files. |
| `scripts/migrate-shellenv/` | AST literal rewriter. Pattern to copy for composite-literal surgery. |
| `scripts/drop-nil-guards/` | AST dead-guard removal, skipping nested edits. |

Three counts are noise in EVERY `extract-pkg` reading: `main` (the sizer seeing
`func main`), `version` (any parameter of that name), and `defaultShellEnv`.
Subtract them before believing a number.

## Traps, all of which bit

- **Renames leak into string literals AND comments.** Six times, three of them
  reaching a sentence a user reads: `pix config set mcp` became `pix config set
  group`; `gog` became `Gog` in 28 strings; `provenance` became
  `upgrade.Provenance` in three sentences doctor/status/pack print. Diff both,
  every time:
  ```sh
  git diff -- services/host | grep -E '^\+' | grep -oE '"[^"]{5,}"' | sort -u > /tmp/n
  git diff -- services/host | grep -E '^-' | grep -oE '"[^"]{5,}"' | sort -u > /tmp/o
  comm -23 /tmp/n /tmp/o
  ```
  Repair literals and comment tails SEPARATELY — one pass over the whole file
  re-breaks the code you just renamed.
- **A bulk rename hits PARAMETERS of the same name.** Replacing a package-level
  `version` with `launcher.Version` silently made four functions read the global
  instead of their argument. Grep for the global inside any function that has a
  parameter of that name.
- **Exporting erases a case distinction Go allows.** The type `ModelReadiness`
  and its constructor `modelReadiness` became the same symbol.
- **A bulk export pass cannot tell scaffolding from API.** It exported
  `BareGog`, `ReceiptClock`, `DefaultCfg`, `Phase2HostPack` — all test-only.
  Copy them to the side that needs them instead.
- **Renames leak into `package` clauses.** One pass produced `package Workspace`;
  the error (`undefined: workspace` at a correct-looking import) is deeply
  unhelpful.
- **A local named after a package shadows it.** Rename the local; the package
  has the better claim.
- **`-X main.version` is silently ignored** on a variable with a non-constant
  initializer. `main` owns the stamp and pushes it into `launcher.Version` in
  `init()`. Do not "simplify" that.
- **Tests follow their SUBJECT, not their filename.** Hundreds moved both
  directions during this work. When a moved test reaches back, ask what it is
  really testing.
- **Guards that hand-list files rot.** All six in this repo had. See the table in
  `architecture.md`; derive the input set instead.

## Phase 2 (the CLI) is the remaining work

`services/host/cli` is the command contract: a command returns an error (never
`os.Exit`), writes to `d.Out` (never `os.Stdout`), and declares flags as struct
tags so usage is generated. Library is kong — chosen over cobra because cobra's
`RunE` has no parameter for dependencies, so they arrive via untyped
`cmd.Context()` and flag targets become package-level vars.

Migrated: `models`, `agent`, `secret`. 31 verbs remain, and they are now much
easier than when this started: each verb's logic already lives in a workflow
package, so migrating one means writing a kong tree in its `*_cmd.go` seam and
deleting hand-rolled flag parsing. `cli/flagset.go` is the transitional parser
three verbs still use; when the last one migrates, delete it.

One quirk to know: `cli.Run` recovers a panic to intercept kong's `os.Exit` on
`--help`. Kong offers no other hook. It is confined to one function and
commented.

## Known cosmetic debt

- Commit `fe5aaf1`'s message has stray backslash escaping. Interactive rebase is
  unavailable in this environment, so it was left alone.
- `workflow/setup/setup.go` prints its own `✗` step-progress rather than going
  through `readiness`'s vocabulary; it carries a documented, shrink-only
  exemption in `TestRendererPurity`. Converting it is a real improvement and a
  separate change — a first attempt corrupted a doc comment and a usage string,
  because the glyph appears in both.
- cmd/pix still runs 639 tests sequentially (~15s) because 29 of its 91 test
  files call `t.Setenv` and so cannot call `t.Parallel`. Adding `t.Parallel()` to
  the 62 files that can take it is the remaining lever on gate time, and it is
  named in `scripts/gate.sh`'s header as such.
