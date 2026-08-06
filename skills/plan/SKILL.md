---
name: plan
description: Take a concrete idea to an eng-ready plan with the crew (discovery, PR/FAQ, PRD, design, architecture, DX, peer review, gated at PR/FAQ). Use for "plan this" or "write a PRD".
---
# plan

Turn a concrete idea into a plan you can hand to `build`. The point: **force
clarity before investment.** Shape fuzzy ideas with `brainstorm` first; `plan`
is for when you know roughly what you want and need it vetted and specified.
Output feeds directly into `build`.

Two modes, same skill. Pick by the size of the work; when unsure, run full.

- **Full (default).** The multi-role product process: discovery → PR/FAQ →
  user gate → PRD → design + architecture + DX → copy → peer review → eng
  handoff. For a real feature that deserves the crew.
- **Quick.** A single-agent auto-gated review of a rough idea: shape → scope →
  architecture → product → DX → peer gate → one approval. For a change where a
  full product process is overkill but you still want it pressure-tested. Skip
  straight here when the user says "quick plan" or the work is under ~1 day.

## Decision principles (auto-answer intermediate questions in either mode)

1. **Completeness first.** Prefer the approach that covers more edge cases.
2. **Fix the blast radius.** Adjacent breakage under ~a day of effort: approve
   the expansion.
3. **Pragmatic.** Two options fix the same thing? Pick the cleaner one.
4. **DRY.** Duplicates existing functionality? Reject; reuse what exists.
5. **Explicit over clever.** A 10-line obvious fix beats a 200-line abstraction.
6. **Bias toward action.** Progress over endless review cycles.

**Decision types:** *Mechanical* (one right answer) → auto-decide silently.
*Taste* (reasonable people disagree) → auto-decide with a note, surface at the
gate. *User challenge* (the crew recommends changing the user's stated
direction) → NEVER auto-decided; held for the gate with what the user said, what
the crew recommends, why, missing context, and the cost if we're wrong.

## Disk discipline (both modes)

All state on disk, not in context. Working dir: `.pi-agent/plan/<feature-slug>/`.
On start, look for `status.json`: if present, resume from the current stage;
else create the dir and write `status.json` with every stage `pending`, updated
on each transition. Never tell a subagent to "read the PR/FAQ" — paste the
relevant section inline. Each subagent gets a fresh context with only what its
stage needs. This skill names **capabilities**, not vendors: resolve each
through `capability-routing` (reads `capabilities.json`, pulls from wired
providers or tells you it is `none`; `none` = degrade to web/files and flag the
gap).

---

## Full mode

```
1. Discovery (PM + fanout research + applicable specialists, concurrent)
2. PR/FAQ (PM writes)
3. PR/FAQ peer review  →  USER GATE (go / redirect / kill)
4. PRD (PM, from approved PR/FAQ)
5. Design + arch review + DX review (concurrent)
6. Copy review
7. Full peer review
8. Eng handoff → build
```

### Stage 1: Discovery (concurrent subagents)
**Skip if** the user gives a clear brief with evidence, or it's a small
enhancement with an obvious job to be done. Fan out in one turn. The crew is
chosen by what the work TOUCHES, not by habit: always the first two, plus every
specialist whose surface the work hits:

- **PM** (always): JTBD ("When [situation], I want [motivation], so I can
  [outcome]"), top 3-5 assumptions (confidence / evidence / validation / impact
  if wrong), competitive landscape, opportunity score, cost of delay (H/M/L).
- **Research** (always): `fanout` workers (or the orchestrator directly) pull
  usage/adoption signals and customer feedback via `capability-routing` and
  prior internal docs from **docs** / **meeting-notes**. There is no `research`
  agent; use `fanout`. Multi-capability join: prefer `code-mode` for the fan-out.
- **Architect** (when it touches infrastructure, security boundaries, or
  cross-system integration): feasibility, constraints/blockers, complexity
  (trivial / moderate / hard / research-needed).
- **growth-marketing** (when the work is user-facing or has a launch/adoption
  surface): positioning, ICP, the GTM angle the PR/FAQ must support.
- **finance-analyst** (when it touches pricing, cost, spend, or budgets): unit
  economics and the cost tradeoff the design must respect.
- **designer** (a visual UI surface), **ux-copywriter** (any user-facing copy),
  **dx-consultant** (a dev-facing API/CLI/SDK), **legal** (licensing, privacy,
  or regulatory obligations), **enterprise-admin** (SSO/RBAC/procurement).

Product-facing roles feed the PR/FAQ + PRD; engineering-facing ones feed design
+ arch. Skipping a relevant specialist is a choice you note, not a default.

Merge into `discovery.md`. If the recommendation is "deprioritize," stop and
present findings before writing anything. (Under `deliver` this stop is
suppressed: a deprioritize recommendation is escalated per Override plan, not
returned to the user.)

### Stage 2: PR/FAQ (PM subagent)
Input: `discovery.md` (or the user's brief). Structure: **press release**
(target customer, quantified benefit, how it works, a realistic quote,
availability); **customer FAQ** (what it does, who for, why over the
alternative, cost, what it does NOT do); **internal FAQ** (strategic fit, eng
cost S/M/L/XL, GTM motion, top risks, cost of not building). Self-check: would a
customer actually care? Write to `pr-faq.md`.

### Stage 3: PR/FAQ peer review → USER GATE
Cross-model `peer-review` on `pr-faq.md` + `discovery.md`: accuracy, slop 0-10
(<=3 to pass), consistency, clarity (can the user make a go/no-go from this
alone?). Verdict PASS / REVISE / REJECT, max 2 revision loops. **Present to the
user and wait:** aligned PR/FAQ, verdict + scores, recommendation. Outcomes:
**Go** → Stage 4; **Redirect** → update PR/FAQ, proceed; **Kill** → archive, done.

**Auto-gate under an autonomous wrapper.** When `plan` runs inside `deliver`
("cook and deliver": don't return mid-flight), this user gate is AUTO-GATED:
apply the decision principles, decide every mechanical and taste question
yourself (record taste calls), and proceed to Stage 4 without stopping. Halt for
the user ONLY on a genuine user-only decision (no safe reversible default, and
it blocks everything downstream). The crew still runs in full; only the pause
is removed.

### Stages 4-8: The machine (auto after go)
No further user involvement unless something is infeasible or peer review rejects.

- **4, PRD (PM).** From approved PR/FAQ + discovery: refined JTBD, problem
  statement, scope (building / NOT building), requirements with P0/P1/P2 and
  testable acceptance criteria, >=5 edge cases, success metrics with
  baselines/targets, dependencies/risks, timeline with a 1.5x buffer.
  Self-check: implementable without questions? Write `prd.md`.
- **5, Concurrent design + arch + DX** (fire together, collect all):
  *Designer* — top 3 interactions, all states (empty/loading/error/overflow/
  permission), responsive, realistic data, `microcopy` → `design.md`.
  *Architect* — feasibility, hidden deps, complexity S/M/L/XL, testable criteria,
  perf/security, specific PRD changes; verdict READY / NEEDS REVISION (loop to
  PM, max 2) → `arch-review.md`. *DX* (skip if no dev-facing surface) — mental
  model, API/CLI surface, composability, progressive disclosure, errors, naming,
  defaults; advisory → `dx-review.md`.
- **6, Copy review.** From `prd.md` + `design.md`: anti-slop, voice, `microcopy`
  compliance; apply copy-only fixes to `design.md` directly → `copy-review.md`.
- **7, Full peer review.** Full package: accuracy, slop <=3, completeness,
  actionability, consistency. Verdict PASS / REVISE / REJECT, max 2 loops → `peer-review.md`.
- **8, Eng handoff.** Set `status.json: eng_ready: true`. Notify in one
  paragraph (feature, complexity estimate, peer verdict). This dir is the input
  to `build`; the complexity estimate informs story sizing.

### Skip logic (full mode)
| Situation | Skip |
|---|---|
| Small enhancement (< 1 day eng) | Stages 1-3; start at PRD — or just use Quick mode |
| User provides the PR/FAQ | Stages 1-2; start at peer review |
| User provides the spec | Stages 1-4; start at design + arch review |
| API-only / no UI | Design (5a) and copy review (6) |
| No dev-facing surface | DX review (5c) |
| Urgent / time-boxed | Peer reviews (3 and 7); note in status.json |

---

## Quick mode

One command, rough idea in, reviewed plan out. Sequential; never parallel. Print
a progress marker between phases (expect 20-60 min).

1. **Shape** — run `brainstorm`. Auto-decide with the principles above (never
   auto-decide context selection or premise agreement). Output: `DESIGN.md`.
2. **Scope** — scope the design: in/out and why; auto-approve in-blast-radius,
   under-a-day expansions.
3. **Architecture** — data flow, dependencies, failure modes, stack; flag hidden
   infra assumptions and anything harder than it looks.
4. **Product** — JTBD clarity, scope appropriateness, measurable success
   criteria, what to cut.
5. **DX** (dev-facing only, else skip) — naming, mental models, error surfaces,
   onboarding, composability.
6. **Peer gate** — `peer-review` over phases 1-5. PASS / REVISE (fix, max 2) /
   REJECT (surface to user).
7. **Approval gate** — present: one-paragraph summary + verdict; each taste
   decision (overridable); any user challenges; artifact paths; recommended next
   step (usually `build`).

---

## Both modes
- If any phase/stage fails, report what completed and what did not. Never
  silently skip.
- The user can interrupt at any boundary; save progress to disk first.
- The output is the input to `build`.
