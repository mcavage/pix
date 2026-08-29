# Phase 1 architecture and sharding: native sandbox environments

Inputs read in full: `.pi-agent/deliver/native-environments/prd.md` (spec of
record), `docs/design/environments.md` (mechanism), `docs/design/architecture.md`
(the enforced layer contract), plus the live code: `services/host/` (uat,
uatmatrix, workflow/uat, workflow/pack, workflow/launch, packinfo, config,
sandbox, routing, inference, cmd/pix).

Methods used: **C4 component level** for the target decomposition, **ADRs
(Nygard)** for the four structural calls this phase forces, **evolutionary
architecture fitness functions** for the properties that must not regress under
36 separate merges, **lightweight ATAM** for the two real tradeoffs (trust-code
extraction vs. fork; one env capability vs. two), and **YAGNI** to keep the
sidecar and the UAT vocabulary from growing a generic escape hatch.

---

## 0. Verdict

The work shards into **47 units across 8 waves**. The plan is executable, but
three structural facts dominate the sharding and were not visible from the PRD
alone:

1. **`arch_test.go` enforces "L1 may not import L1."** Every new leaf package is
   constrained by it. In particular the Story 0 UAT check package **cannot** import
   the Story 1 environment parser — which is the right answer anyway (Story 0 must
   prove the *upstream* contract against literal fixture bytes, not against a Pix
   adapter that does not exist yet). This makes Wave A genuinely independent of
   everything downstream.
2. **There must be exactly one new L1 capability for environments**, not two.
   `packinfo` (read-only pack facts, L1) vs. `workflow/pack` (trust + adoption, L3)
   is the existing, proven split, and it exists because `doctor`, `launch` and
   `provision` all needed the same facts and the sideways edges had to be deleted.
   Environments have the identical three consumers. Split the native parser and the
   sidecar parser into two L1 packages and composition gets duplicated in three L3
   workflows — the exact web `docs/design/architecture.md` was written to prevent.
3. **The trust primitives must be *extracted*, not copied.** A forked copy in
   `workflow/env` beside the original in `workflow/pack` is a second trust store
   for the entire duration of Stories 1–5, and PRD §8's stop condition ("a trust
   test deleted without a named replacement") becomes unenforceable because both
   implementations look like the replacement.

Highest-risk single merge is **E2.5** (the `pix run` cutover). It is the only unit
that cannot be split further without creating a temporarily selectable second
launch path, which PRD §8 names as the one outcome worse than today. Everything
around it is split hard so that E2.5 arrives as a small diff over pre-landed,
independently tested parts.

Story 0 is confirmed as a **capability-check** extension, not a vocabulary
extension: the live closed vocabulary is
`browser_check, candidate_smoke, mcp_add, mcp_auth, mcp_remove, mcp_status`
(`services/host/uat/schema.go:34`) and `named_checks` is literally `[]string{}`
(`services/host/workflow/uat/runner.go:236`). The six environment checks land as
a **named-check matrix executed inside `candidate_smoke`**, exactly mirroring
`uatmatrix` (the memory matrix) — which is already wired at
`workflow/uat/execute.go:269` and reported at `runner.go:245`. AC-8 is therefore
satisfied structurally, not by discipline.

---

## 1. Target component view (C4 level 3)

```
L4  cmd/pix
      env_cmd.go        env ls|add|use|show|edit|review|forget  + `env rm` pointer error
      run_cmd.go        --env NAME, --model (no --intent)
      doctor_cmd.go     --recreates
        |
L3  workflow/
      env/        the seven verbs: register, scaffold, select, edit, review-gate,
                  forget. Composes envinfo + hosttrust + sys. Writes config.
      launch/     effective render -> sbx env create -> name-based sbx exec.
                  create intent, receipt, attach fingerprint check, teardown.
      doctor/     env rows + --recreates read
      reset/      invalidate every acceptance; delete no source
      uat/        runner: candidate_smoke calls memoryMatrix, then envMatrix
        |
L2  health        sbx >= 0.39.0 requirement + verdict (fails closed on unparsable)
        |
L1  envinfo       THE environment capability. Owns .sbxenv.yaml grammar AND
                  pix.toml sidecar grammar, canonical root identity, documented
                  merge semantics, the PRE-COMPOSITION semantic tree, and both
                  fingerprint inputs. Read-only facts; no adoption, no prompts.
    hosttrust     canonical host-exec fingerprint, acceptance store, flock,
                  atomic write, symlink refusal. Extracted from workflow/pack,
                  keyed by an opaque subject id (pack root today, env root next).
    recreatelog   bounded (100) local diagnostic counter. No network. No config key.
    uatenvmatrix  the six named environment checks. Literal fixture bytes only;
                  MUST NOT import envinfo (L1 rule + Story 0 independence).
    sandbox       Name(), PlanRemove, PlanForceRemove, + new PlanEnvRemove
    inference     inference.json v1 + roster (no scores, no intents)
    uatmatrix     unchanged memory matrix
        |
L0  config        environment = "NAME"; [environments] name -> canonical path
    sys, cli, hostenv, launcher, lease, workspace, rpc
    routing       DELETED in Wave F (removed from arch_test.go's layer table)
```

Two edges that the layer rule forbids and that the sharding therefore designs
around:

- `envinfo` (L1) may not import `sandbox` (L1). The `pix-*` name is computed at
  L3/L4 and **passed into** the renderer as a parameter. This matches
  `docs/design/environments.md` §5.1 ("Pix computes the workspace-specific `pix-*`
  sandbox name and writes it only to the generated effective file") and it is what
  makes AC-23 checkable as a pure equality on a value the caller supplied.
- `uatenvmatrix` (L1) may not import `envinfo` (L1). Story 0 writes its own
  fixture YAML bytes. That is a feature: if Story 0's fixtures and Story 1's
  renderer agree, the agreement is evidence; if the renderer generated the fixture,
  it would be a tautology.

---

## 2. ADRs

### ADR-ENV-001 — One L1 capability (`envinfo`) owns both authored grammars

**Status:** accepted. **Date:** 2026-07 phase 1.

**Context.** AC-38 requires exactly one package to parse native sbx env grammar.
The sidecar `pix.toml` is a different grammar, so a naive reading allows two
packages. But `workflow/env`, `workflow/launch` and `workflow/doctor` all need
"what does this environment declare, composed" — the identical three-consumer
shape that produced the four sideways L3 edges `packinfo` was created to delete
(`docs/design/architecture.md`, "the worked example").

**Decision.** One L1 capability `services/host/envinfo/` owns `.sbxenv.yaml`
parsing/merging/rendering **and** strict `pix.toml` parsing, in separate files
(`sbxenv.go`, `sidecar.go`, `tree.go`, `fingerprint.go`, `render.go`). It has no
adoption, no prompts, no writes outside an atomic render, and returns facts.
`workflow/env` owns every decision and every prompt.

**Consequences.** AC-38's fitness function becomes "only `envinfo` imports a YAML
decoder for the native schema" and is greppable. The sidecar's ownership rule
("if a `pix.toml` field duplicates an sbx field, the Pix field is wrong") is
enforceable inside one package with one test. Cost: `envinfo` will be the largest
new package; mitigated by keeping every prompt, editor spawn, and store write out
of it.

**Rejected.** `sbxenv` + `envspec` as two L1 packages (forces composition into
three L3 workflows, or an illegal L1→L1 edge). Putting the parser in
`workflow/env` (blocks `doctor` and `launch` from reading facts without a sibling
workflow import, which `arch_test.go` fails).

### ADR-ENV-002 — Extract the trust primitives; do not fork them

**Status:** accepted.

**Context.** Story 1 needs canonical identity, host-exec fingerprinting, an
acceptance store outside the payload, flock-serialized read-modify-write, atomic
write and symlink refusal. All of it exists in `workflow/pack/truststore.go`
(327 lines), `trust.go` (669), `trustsummary.go` (402). Packs survive until Story
5.

**Options.** (a) Copy into `workflow/env`, delete the pack copy in Story 5.
(b) Extract to a new L1 `hosttrust`, have `workflow/pack` import it, and Story 5
deletes only pack-specific glue.

**Decision.** (b).

**Tradeoff (ATAM axes).** (a) is faster to ship (≈1 day) and has zero blast
radius on pack behavior; (b) costs a careful refactor of a security-critical,
flock-serialized store with 2,500+ lines of existing tests
(`pack_trust_rounds_test.go`, `pack_v2_trust_host_state_test.go`,
`truststore_legacy_migration_test.go`). But (a) means **two acceptance stores
exist simultaneously for four stories**, and PRD §8's stop condition — "a trust
test is deleted in Story 5 without a named environment replacement" — cannot be
adjudicated when both a fork and an original claim to be the replacement. Trust
divergence is the highest-severity failure available in this change. (b) wins on
correctness; the schedule cost is one unit (E1.4) that is pure refactor with the
existing suite as its oracle.

**Consequences.** E1.4 is a no-behavior-change unit whose acceptance is "every
existing pack trust test passes unmodified." The store gains an opaque
`subject` key (kind + canonical root) so pack and environment records coexist in
one file with no ambiguity, which also makes AC-70 ("`pix reset` invalidates
*every* acceptance") a one-line sweep rather than two.

### ADR-ENV-003 — Story 0 proves upstream with literal fixtures, not the Pix adapter

**Status:** accepted.

**Context.** `docs/design/environments.md` §12 Story 0 says "generate authored and
stable effective environment fixtures inside the UAT run." Story 1 builds the
renderer. Sequencing them the other way would let Story 0 slip behind Story 1.

**Decision.** `uatenvmatrix` writes fixture bytes it owns, as string constants in
`uatenvmatrix/fixtures.go`. It is an L1 package and the layer rule already forbids
it from importing `envinfo`, so this is enforced by the build, not by review.

**Consequences.** Wave A runs to completion with no Story 1 code in the tree, so
the migration's stop conditions (PRD §8: A1/A2/A3 disproved) fire before any
production architecture depends on them. When E2.1 lands the real renderer, a
golden test asserts the renderer's output is semantically equal to the Story 0
fixture; a divergence is then real evidence, not a self-comparison. Cost: the
fixture bytes are maintained in two places for the life of the project — accepted,
because that duplication *is* the check.

### ADR-ENV-004 — The launch cutover is one unit; everything around it is split

**Status:** accepted.

**Context.** PRD §8 and §12 both name a parallel launch path as the worst possible
outcome. `pix run`'s launch path today spans `cmd/pix/run_cmd.go` (599 lines),
`workflow/launch/{run,sandbox,sbxargs,session,kits,launchpack,hoststate}.go`, and
resolves pack contribution at `run_cmd.go:362`.

**Decision.** E2.5 cuts `pix run` from the pack/legacy path to
`sbx env create` + name-based `sbx exec` in a **single commit**, and every
computation it needs — effective render (E2.1), creation fingerprint and
attribution (E2.2), create intent and recovery (E2.3), `PlanEnvRemove` and the
name-based fallback (E2.4) — lands **before** it as separately reviewed,
separately tested pure units with no call site.

**Consequences.** E2.5's diff is wiring, not logic; its review is about the
switch, not the algorithms. The pre-landed units are dead code for a short window
— explicitly allowed by `docs/design/environments.md` §12 ("dead pack-builder code
may survive until Story 5 … neither is user-selectable beside its replacement").
E2.5 must serialize against E1.1, E1.15, E2.8 and E4.1, which all edit
`run_cmd.go`.

### ADR-ENV-005 — `docs/design/environments.md` is corrected in Wave B, not Wave H

**Status:** accepted.

**Context.** PRD §preamble: "Where this doc and the design doc disagree … this doc
wins and the design doc is updated in Story 1." The design doc currently ships
`pix env rm NAME [--force]` (§8) and `pix env edit [NAME] [--sbxenv]`, which
contradict D2 and D4, and an error example with `--name pix-repo-work`, which
contradicts D11.

**Decision.** E1.0 is a docs-only unit landing at the head of Wave B that
reconciles the design doc to PRD D1–D24. It carries no code and no AC, but every
downstream unit cites the design doc, so leaving it wrong for six waves guarantees
someone implements `--sbxenv`.

---

## 3. Fitness functions (added to `make gate`, per PRD §14)

Each is an automated check that fails the build when the property regresses. They
are assigned to the unit that must introduce them, so no unit "will add it later."

| # | Property | Mechanism | Introduced by |
| --- | --- | --- | --- |
| F1 | UAT action vocabulary is unchanged; environment coverage is named checks | assert `LegalVocabulary()` actions equal the six existing strings; assert `named_checks` equals `uatenvmatrix.CheckNames()` | E0.1 (AC-8) |
| F2 | Advertised named checks == executed named checks | capability list derived from the registry, same as `uatmatrix.CheckNames()` | E0.1 |
| F3 | Exactly one package parses native sbx env grammar | import-graph test: only `envinfo` imports `yaml` for `.sbxenv.yaml` | E1.2 (AC-38) |
| F4 | `envinfo` imports no L1 sibling | existing `arch_test.go` layer table | E1.2 |
| F5 | `uatenvmatrix` never imports `envinfo` | `arch_test.go` + explicit deny test | E0.1 (ADR-ENV-003) |
| F6 | One acceptance store; no second trust implementation | grep sentinel: only `hosttrust` defines an acceptance record type | E1.4 |
| F7 | Launch performs no config save | call-graph test over the `--env` path | E2.5 (AC-37) |
| F8 | Composed effective name == recorded `pix-*` name before create and remove | equality assertion in the lifecycle test | E2.4/E2.5 (AC-23) |
| F9 | Attribution comes from the pre-composition tree | list-growth table test: adding an unrelated entry never changes another facet's key path | E2.2 (AC-72) |
| F10 | Recreate log makes no network call | grep for net/http/dial in `recreatelog` + record-shape test (no facet values) | E1.6 (AC-68) |
| F11 | No scored-routing symbols in live code | sentinel rejecting `scorecard`, `prefer_providers`, `max_cost_usd`, `run_intent`, `CompiledRouting` | E4.4 (AC-34) |
| F12 | Ollama hardware table has no price/accuracy/routing fields | shape test | E4.3 (AC-35) |
| F13 | No live `pack` command, type, or config key | grep/AST sentinel | E5.4 (AC-36) |
| F14 | `pix help --all` and docs enumerate the same surface | generated comparison test | E6.3 (AC-39) |
| F15 | No env-path string uses an unearned success word, em dash, or filler | string lint over env command output | E1.13 (AC-67) |
| F16 | No `pix env` verb blocks on stdin when stdin is not a TTY | non-TTY run of every verb | E1.13 (AC-65) |
| F17 | `envinfo`'s effective renderer is a pure function over a caller-supplied `RuntimeFacts` value; `envinfo` imports neither `mcp` nor `sandbox`; exactly one producer of the Pi mixin kit | import-graph test (no `mcp`/`sandbox` import in `envinfo`) + grep sentinel for a second mixin-materialization call site outside `envinfo/mixin.go` | E2.1 (AC-54) |

---

## 4. Unit sharding

Full machine-readable detail (scope, ACs, files, red-first tests, focused
command, commit boundary, dependencies, conflicts) is in
`.pi-agent/deliver/native-environments/units.json`. This section is the shape and
the reasoning.

### Wave A — Story 0, contract proof (7 units) — **blocks everything**

`E0.1` builds the seam and the first check in one vertical slice: new L1
`uatenvmatrix` (registry + typed inputs + `fixtures.go` + `check_create_exec.go`),
capability wiring in `workflow/uat/runner.go` (`NamedChecks` from the registry,
`candidateSmokeCoverage` gains `environment_checks`), execution wiring in
`workflow/uat/execute.go` right after the memory matrix, and the layer-table entry
in `arch_test.go`. F1/F2/F5 land here.

`E0.2`–`E0.6` each add exactly one check file plus one registry line:
image digest (AC-2), recreate boundary (AC-3), failed-create cleanup (AC-4), rm
scope refusal (AC-5), custom-agent Ollama as a non-failing capability result
(AC-6). **These five run in parallel worktrees.** Their only conflicts are one
line in `uatenvmatrix/checks.go` and one entry in the capability golden list in
`workflow/uat/server_test.go`.

`E0.7` is the gate: submit the committed candidate through the existing UAT MCP,
capture artifacts, write `docs/upstream/sbx-0.39-environments.md` (observed argv,
sbx version, rendered fixtures, listings, undefined-`${VAR}` behavior). **If A1,
A2 or A3 fails here, stop — PRD §8.**

Why the checks are separate units: each is a real host-backed interaction with
its own failure mode and its own artifact. Reviewing five of them in one diff
means reviewing none of them.

### Wave B — Story 1 foundations (7 units: E1.0–E1.6) — mostly parallel

`E1.1` **lands first** (PRD §6: "P0-12 lands first inside Story 1"): the
sbx ≥ 0.39.0 gate in `health/probes.go` (which already parses `sbx --version` at
lines 143–209), surfaced in `pix run` and `pix doctor`, unparsable fails closed
(AC-20). On a 0.38 host every other failure mode is misdiagnosed.

`E1.0` (docs, ADR-ENV-005), `E1.2` (native grammar + merge + pre-composition
tree), `E1.3` (strict sidecar), `E1.4` (trust extraction, ADR-ENV-002), `E1.5`
(config keys + `config set` refusal), `E1.6` (`recreatelog`) are all independent
of each other. **Six parallel worktrees.** Only `E1.2`, `E1.4` and `E1.6` touch
`arch_test.go`'s layer table (one line each).

### Wave C — Story 1 verbs (9 units: E1.7–E1.15) — partially serialized

`E1.7` (registry + exact-name resolution + containment/symlink refusals) is the
spine and must land before every verb. `E1.8` (host BoM + `env review`) must land
before `add`, `use` and `forget`, because all three consult acceptance.

`E1.9` creates `cmd/pix/env_cmd.go` **and owns the shared struct**: it lands
`ls` + `show` and declares the dispatch skeleton. `E1.10` (`add`), `E1.11`
(`use|forget|rm` pointer) and `E1.12` (`edit`) each add their own
`workflow/env/<verb>.go` and one field line to the `envCmd` struct. They can be
*developed* in parallel but must be *landed* in ID order with a rebase.

`E1.13` (error-form enumeration, copy lint, non-TTY sweep) is deliberately last
in the wave: it is the unit that proves the family is consistent, and it cannot
do that before the family exists.

`E1.14` (`pix reset`: invalidate all acceptance, delete no source) and `E1.15`
(quiet setup/status/run surfaces) run in parallel with the verb units — different
files (`workflow/reset/reset.go`, `workflow/provision/setup.go`) — except that
`E1.15` touches `run_cmd.go` and therefore serializes against Wave D.

### Wave D — Story 2, launch (8 units) — one serialized spine

`E2.1` (effective render + Pi mixin kit, AC-54), `E2.2` (creation fingerprint +
attribution map, AC-72), `E2.3` (create intent + recovery), `E2.4`
(`sandbox.PlanEnvRemove` + name-based fallback, AC-28) are pure and **parallel**.
`E2.5` is the cutover (ADR-ENV-004) and serializes. `E2.6` (`doctor --recreates`),
`E2.7` (host-service desired-set union), `E2.8` (pack launch-path deletion,
AC-27) follow it in order; `E2.6` and `E2.7` can be parallel with each other.

**Architect corrections applied at full-send (findings ledger C1-C12, see
`status.json`).** `E2.1`'s effective renderer is a pure function over one
caller-supplied `RuntimeFacts` value; it imports neither `mcp` nor `sandbox`
(F17), it is the single producer of the Pi mixin kit (no duplicate
materialization in `cmd/pix/env_cmd.go` or `workflow/launch`), and its file
list includes `services/host/cmd/pix/env_cmd.go`. `E2.2` `depends_on` `E2.1`
directly (it consumes the effective render) and its launcher-keyed HMAC key is
exactly one stored `hosttrust` record, invalidated/rotated by `pix reset`
alongside every acceptance record (F6 extension). `E2.5` also wires the `env
forget`/`env show` seams to the new live-launch facts. `E2.7`'s desired-set
union lives solely in `workflow/launch/hostservices_env.go` + `serve.go`, never
in `services/host/pack_units.go` (E5.2 deletes that file outright). `E2.8`'s
private-work-environment gate is enforced, not descriptive: it refuses to land
without a recorded live-conversion evidence artifact. Downstream, `E5.1`'s
named pack-trust test files are a snapshot that must be re-derived from the
live tree at execution time, and `E6.1` excludes `SECURITY.md` (one of the
user's seven pending unstaged legal-file cleanup items) until the user
resolves its disposition.

### Wave E — Story 3, literal roster (4 units) — parallel with D

`E3.1` (roster in `inference.json` v1 + reference validation) is Go and depends
only on `E1.3`. `E3.2` is **TypeScript only** (`extensions/inference.ts`,
`subagents.ts`, `ollama-bridge.ts` + node tests) and has zero Go conflicts — the
single best candidate for a fully independent worktree running concurrently with
the entire Wave D spine. `E3.3` (facts-only `models`/`agent ls`, `exclusive`),
`E3.4` (strip `intent:`/`model:` from `agents/*.md`).

### Wave F — Story 4, delete the router (4 units) — serialized

Order matters for a non-obvious reason: `E4.3` **moves the local-Ollama hardware
table under `inference` before** `E4.2` deletes `routing/defaults/`. Reversing it
leaves a window where `pix setup`'s local-model flow has no RAM/download/context
facts. Then `E4.1` (CLI surface: `--intent`, `run_intent`), `E4.2` (`routing/`,
`route.go`, `routing.json`, `make routing`, catalog verbs), `E4.4` (sentinel +
`skills/model-refresh` + every reference).

### Wave G — Story 5, delete packs (4 units) — serialized, gated

`E5.1` is a **hard gate**: every pack trust test gets a named environment-invariant
replacement *before* any pack code is deleted (PRD §6, §8, §15). A Wave G diff that
removes a trust test with no named replacement is rejected at review. ADR-ENV-002
is what makes this adjudicable — there is one store, so "the replacement" is
unambiguous.

### Wave H — Story 6, docs and acceptance (4 units)

`E6.1` docs cut, `E6.2` `docs/how-to/environments.md` + supersede the historical
design docs, `E6.3` invariant tests + help/doc parity (AC-39) + AGENTS.md
context-budget ratchet, `E6.4` concurrent home+work acceptance (AC-40, AC-63).

**AGENTS.md rule for the whole project:** only Wave H edits `AGENTS.md` and
`tests/agents-md-invariants.test.mjs`. Earlier units that invalidate a documented
invariant record the required edit in their commit message under a
`AGENTS-DELTA:` line. Otherwise 47 units serialize on one prose file.

---

## 5. Unavoidable file conflicts

These are the files more than one unit must touch. Each has a named owner and a
landing order; everything else is genuinely parallel.

| File | Units | Resolution |
| --- | --- | --- |
| `services/host/cmd/pix/run_cmd.go` | E1.1, E1.15, E2.5, E2.8, E4.1 | **Strict serialization.** Owner: E2.5. E1.1 and E1.15 land before Wave D; E2.8 and E4.1 after. No two open at once. |
| `services/host/cmd/pix/env_cmd.go` | E1.9 (creates), E1.10–E1.13 | E1.9 owns the struct; each verb unit adds one field line + its own file. Land in ID order, rebase. |
| `services/host/uatenvmatrix/checks.go` | E0.1 (creates), E0.2–E0.6 | One registry line each. Land in ID order; conflicts are one-line and mechanical. |
| `services/host/workflow/uat/server_test.go` (capability golden) | E0.1–E0.6 | Same as above; the golden list grows by one string per unit. |
| `services/host/arch_test.go` (layer table) | E0.1, E1.2, E1.4, E1.6, E4.2, E5.2 | One line each; additions in A/B, removals in F/G. No overlap in time. |
| `services/host/config/config.go` | E1.5 (add env keys), E4.1 (drop `run_intent`), E5.3 (drop pack keys) | Separated by wave; never concurrent. |
| `services/host/workflow/launch/sbxargs.go` | E2.5, E2.8, E5.3 | Owner E2.5. Sequential within and across waves. |
| `services/host/workflow/pack/{trust,truststore,trustsummary}.go` | E1.4 (extract), E5.2 (delete residue) | E1.4 is pure refactor with the existing suite as oracle; E5.2 five waves later. |
| `Makefile` | E4.2 (`routing` target), E5.2 (pack targets), E6.1 | Sequential by wave. |
| `AGENTS.md`, `tests/agents-md-invariants.test.mjs` | E6.1, E6.3 | Wave H only; `AGENTS-DELTA:` commit trailers everywhere else. |
| `docs/design/environments.md` | E1.0, E6.2 | E1.0 corrects it to the PRD; E6.2 marks superseded docs. |

---

## 6. Parallel vs. serialized, at a glance

**Fully parallel worktrees (safe to run concurrently):**

- Wave A: E0.2, E0.3, E0.4, E0.5, E0.6 (5-way, after E0.1)
- Wave B: E1.0, E1.2, E1.3, E1.4, E1.5, E1.6 (6-way, after E1.1)
- Wave C: {E1.10, E1.11, E1.12} develop in parallel, land serially; E1.14 and
  E1.15 are fully parallel with them
- Wave D: E2.1, E2.2, E2.3, E2.4 (4-way); later E2.6 ∥ E2.7
- Wave E: E3.1 ∥ (E3.3 after E3.1); **E3.2 is TypeScript-only and parallel with
  all of Wave D**
- Wave H: E6.1 ∥ E6.2, then E6.3, then E6.4

**Must serialize:**

- E0.1 before all of Wave A; E0.7 after all of Wave A (stop-condition gate)
- E1.1 before the rest of Wave B (PRD mandate)
- E1.7 → E1.8 → E1.9 → verbs → E1.13
- E2.5 alone (nothing else may hold `run_cmd.go` or `workflow/launch/`)
- E2.5 → E2.8 (pack launch deletion needs the replacement live)
- E4.3 → E4.2 (hardware table moves before the defaults are deleted)
- E4.1 → E4.2 → E4.4
- E5.1 → E5.2 → E5.3 → E5.4 (trust translation gates pack deletion)
- E6.4 last (it is the acceptance run)

Wave-level parallelism: **B ∥ nothing** (A gates it), **D ∥ E** is the big win —
the roster work and the TS extension work proceed while the launch spine
serializes.

---

## 7. File-level retain / adapt / delete map

### Delete

| Path | Unit | Note |
| --- | --- | --- |
| `services/host/workflow/pack/` (10,621 lines) | E5.2 | after trust primitives are extracted (E1.4) and trust tests translated (E5.1) |
| `services/host/cmd/pix/pack_cmd.go` | E5.2 | no alias, no deprecation shim (N4) |
| `services/host/cmd/pix/pack_*_test.go`, `launchpack_*_test.go` | E5.2 | only after E5.1 names a replacement for each trust assertion |
| `services/host/pack_units.go`, `pack_units_test.go` | E5.2 | pack units in the Suture tree |
| pack-shaped parts of `services/host/packinfo/` (`pack.go` 996 lines, `state.go`) | E5.3 | `service.go` facets partially retained → `envinfo` |
| `services/host/routing/` (2,031 lines, L0) | E4.2 | minus the Ollama hardware table (E4.3) |
| `services/host/route.go`, `route_test.go` | E4.2 | |
| `routing.json` (repo root + baked) | E4.2 | |
| `make routing` target + compile-oriented model targets | E4.2 | |
| `skills/model-refresh/` | E4.4 | |
| `services/host/workflow/launch/launchpack.go` + pack paths in `sbxargs.go` | E2.8 / E5.3 | `RunOpts.Pack`, `PackKits`, `Intent` fields |
| `config.toml` keys: `pack`, `packs`, `kits.stack`, `run_intent`, `mcp` attachment list | E4.1 / E5.3 | `pix mcp add|ls|auth` verbs survive |
| `tests/pack-authoring-docs-parity.test.mjs`, `tests/kit-subagent-roster-intents.test.mjs` | E5.2 / E3.4 | replaced, not dropped |

### Adapt

| Path | Unit | Change |
| --- | --- | --- |
| `services/host/workflow/pack/{truststore,trust,trustsummary}.go` | E1.4 | **extract** to L1 `hosttrust`, subject-keyed; pack imports it unchanged |
| `services/host/workflow/uat/{runner,execute}.go` | E0.1 | `named_checks` from the registry; env matrix call after the memory matrix |
| `services/host/uat/schema.go` | E0.1 | **no vocabulary change** — only a test asserting it did not change |
| `services/host/health/probes.go` | E1.1 | sbx version requirement ≥ 0.39.0, unparsable fails closed |
| `services/host/config/config.go` | E1.5 | `environment`, `[environments]`; canonical absolute paths only |
| `services/host/cmd/pix/config_cmd.go` | E1.5 | refuse `environment` / `environments.*`, name the env verb |
| `services/host/sandbox/remove.go` | E2.4 | add `PlanEnvRemove` beside `PlanRemove`/`PlanForceRemove`; same scope proofs |
| `services/host/sandbox/fingerprint.go` | E2.2 | creation fingerprint over the composed effective doc |
| `services/host/workflow/launch/{run,session,sandbox,kits}.go` | E2.5 | create → poll → bind lease → name-based exec, on the effective path |
| `services/host/workflow/reset/reset.go` | E1.14 | sweep sandboxes, invalidate every acceptance, delete no env source |
| `services/host/inference/live.go` | E3.1 | `inference.json` v1 gains `roster` (additive) |
| `services/host/inference/hardware.go` | E4.3 | becomes the sole home of the local-Ollama table |
| `services/host/cmd/pix/{models,agent}*.go` | E3.3 / E3.4 | facts + source; no WHY, no status taxonomy |
| `extensions/{inference,subagents,ollama-bridge}.ts` | E3.2 | read `roster` from `inference.json`; no `routing.json` |
| `agents/*.md` (19 files) | E3.4 | drop `intent:` / `fallback_intent:` / `model:` |
| `docs/design/environments.md` | E1.0 | reconcile to PRD D1–D24 (`forget`, `edit NAME pix\|sbxenv`, no `--name`) |

### Retain untouched

`services/host/uat/`, `uatmatrix/`, `workflow/uat/` structure (extended, not
reshaped) · `services/host/lease/` · `services/host/sandbox/{name,list,argv}.go`
· `services/host/memory/`, `supervise/`, `plugin/`, `service/` · `workflow/task/`
· `services/host/mcp/` and `pix mcp add|ls|auth` · `extensions/ollama-bridge.ts`
as transport (P1-3 / N5) · `scripts/check-open-core.sh` · the `uat-mcp` closed
vocabulary.

---

## 8. Risk-based integration order

Ordered by "what, if wrong, invalidates the most already-merged work."

1. **Wave A (E0.1 → E0.2–E0.6 → E0.7).** Highest uncertainty: an unproven
   upstream contract. A1/A2/A3 failing here costs seven units; failing in Wave D
   costs twenty-two. This is also why E0.7 is a hard gate rather than a checkpoint.
2. **E1.1, the sbx version gate.** Cheapest possible unit, and it removes an entire
   class of misdiagnosis from every subsequent host-backed run.
3. **E1.4, the trust extraction.** Security-critical refactor with a complete
   existing test suite as its oracle. Do it while packs are still alive and the
   tests still run against both consumers; doing it after Wave G means doing it
   without the oracle.
4. **Wave B parsers (E1.2, E1.3) and E1.6.** Pure, high-coverage, zero blast
   radius. They de-risk Wave D by making E2.1/E2.2 wiring over tested primitives.
5. **Wave C verbs.** User-visible but non-destructive: nothing here can lose a
   sandbox or a credential. Ship the read-only ones first (`ls`, `show`), then the
   ones that write (`add`, `use`), then the ones that refuse (`forget`, `rm`
   pointer), then the consistency sweep.
6. **Wave D pure units (E2.1–E2.4), then E2.5.** E2.5 is the single highest-risk
   merge in the project; it arrives last in its wave, small, over pre-tested parts.
7. **E2.8, pack launch deletion.** Only after E2.5 has been exercised — deleting
   the old path before the new one has run live is how you lose your fallback.
8. **Wave E roster.** Independent of D and safe to interleave; land E3.2 (TS) early
   to bank the parallelism.
9. **Wave F router deletion.** Mechanical, but E4.3 before E4.2 or setup breaks.
10. **Wave G pack deletion.** Highest line count, lowest uncertainty — provided
    E5.1's trust translation is complete. Gate, do not trust the diff.
11. **Wave H docs and concurrent acceptance.** Last, because AC-39/AC-40 assert on
    the finished surface, and because AGENTS.md serialization is only tolerable
    once.

**Rollback posture.** Every unit through Wave C is additive and revertible in
isolation. From E2.5 onward, revert granularity is the wave: reverting E2.5 alone
after E2.8 has landed leaves no launch path at all. Operationally, that means
E2.5 → E2.8 should land within one working session, and the private work
environment must be converted and run live **before** E2.8 (PRD §6, A8/R6).

---

## 9. What I could not settle from the code

- **`sbx env` argv is asserted, not observed.** `services/host/health/probes.go`
  parses `sbx --version` but nothing in the tree invokes `sbx env`. Every argv in
  this plan is from `docs/design/environments.md` §4 and is exactly what E0.7 must
  replace with observed evidence in `docs/upstream/sbx-0.39-environments.md`.
- **`--dev` live-skill interaction with the effective file.** `execute.go`'s
  candidate smoke passes `--dev` for two named scenarios; how Mode B skill mounts
  compose with an environment's own workspaces is a Story 0 observation (matrix
  item 2), and E2.1's renderer contract depends on the answer.
- **Whether `agent: pix` is satisfied by a `kits:` entry** or needs a host-side
  install (design §5.1 restriction 6). This changes E2.1's render and possibly adds
  a unit; E0.1's create-then-exec check is where it surfaces.

---

## Artifacts

- `.pi-agent/deliver/native-environments/architecture.md` (this file)
- `.pi-agent/deliver/native-environments/units.json` (47 units, DAG, conflicts,
  integration order, file map)
