---
name: analyze
description: Strategic analysis (financial model, legal review, market sizing, build/buy/partner, pricing, or decision one-pager). Use for "model the economics of X", "review this contract", or "build vs buy X".
---
# analyze

Produce decision-ready documents. The user reads the output and makes the call.
Every artifact ends the same way: here's what we know, here's what we recommend,
here's the ask.

Orchestrate directly for 1-2 roles in sequence. For Task 4 (build/buy/partner),
three roles run at once: fire them in a single turn and collect before synthesizing.

## Data sources and fallback

This skill names **capabilities**, not vendors. Resolve each through
`capability-routing`, which reads `capabilities.json` and either pulls from the
wired provider(s) or tells you it is `none`. `warehouse` (usage, pricing, and
revenue metrics), `crm` (deal and pipeline history), and `calls` (call
transcripts) are private-pack capabilities: in the default public profile they
resolve to `none`, so most runs take the degraded path unless a pack wires them.

**Full path** (capabilities wired): pull `warehouse`, `crm`, and `calls` before
analysis. When a pull spans 2+ capabilities (deals, then per-deal usage and
transcripts), prefer `code-mode` for the fan-out over many separate tool calls.
When a capability lists several providers, fan out across all wired providers via
`capability-routing` and merge/dedupe; the same record often lands in more than
one system.

**Degraded path** (a capability resolves to `none`): say "no [capability] wired,
using [fallback]" once, then substitute public web research (fan out `fanout`
workers; hand a genuinely hard synthesis to `deep`), the user's own files or
repo, and assumptions flagged explicitly. Never pretend to have data you don't.

## Disk discipline

All artifacts live under `.pi-agent/strategy/<topic-slug>/`. On start, check for
`status.json`: if it exists, resume from the current stage; update it on every
transition. Single-pass tasks (1-3, 5-6) need no tracker: write the output file
and return.

## Task 1: Financial Model

**Triggers:** "Model the economics of X", "What does X cost us?", "What's the ROI on X?"

**Roles:** finance-analyst + deep (large-context data pulls).

Stage 1 (deep, concurrent with any other research): pull usage, revenue, and
customer counts from `warehouse` if wired; otherwise use public benchmarks,
flagged as such.

Stage 2 (finance-analyst): the `finance-analyst` agent owns the modeling
discipline. It builds driver-based, with named assumptions carrying source and
sensitivity flag, base/bull/bear scenarios, sensitivity analysis on the flagged
inputs, and break-even. Break the model out by the dimensions the data supports
(segment, region, cohort). Show the math.

Output: `financial-model.md`

## Task 2: Legal Review

**Triggers:** "Review this contract", "What are the legal risks of X?", "DPA review for X"

**Role:** legal.

Input: contract text (pasted or from a file the user provides) plus context. The
`legal` agent produces:

- Risk assessment: HIGH / MEDIUM / LOW overall, with findings per level.
- Specific redline recommendations. Give the exact change, not "consider revising".
- For DPAs: GDPR/CCPA compliance review inline.
- For open source: license compatibility analysis.
- For M&A: due-diligence framework.
- Comparison to the user's standard terms if provided.
- Verdict: SIGN / SIGN WITH CHANGES / DO NOT SIGN.

Escalate the moment a HIGH risk surfaces. Do not wait for the full document.

Output: `legal-review.md`

## Task 3: Market Sizing / Competitive Analysis

**Triggers:** "How big is the X market?", "What's the TAM for X?", "Competitive analysis on X"

**Roles:** finance-analyst + a competitive worker (concurrent at Stage 2).

Stage 1 (deep): pull usage data that sizes the addressable market from
`warehouse`; pull category mentions from `calls` if wired (fan out across every
wired provider via `capability-routing` and merge/dedupe). Fallback: public
analyst reports and web research via `fanout`.

Stage 2 (concurrent): finance-analyst produces TAM/SAM/SOM with methodology; a
second subagent runs the `competitive` skill for the landscape.

Synthesize into `market-analysis.md`: TAM/SAM/SOM with assumptions, current
position and share, competitive landscape table (only where there are real
differentiators to compare), customer evidence, and opportunity assessment.

## Task 4: Build / Buy / Partner Decision

**Triggers:** "Should we build or buy X?", "Evaluate acquiring X", "Build vs partner on X?"

**Roles:** finance-analyst + legal + deep (all parallel).

This is the one case where three interdependent roles run at once. Fire all three
subagents in a single turn, collect every output, then synthesize.

- **finance-analyst:** build cost (headcount, timeline, opportunity cost), buy
  cost (price, integration, retention), partner cost (revenue share, engineering
  investment, dependency risk), 3-year NPV for each option.
- **legal:** IP implications, regulatory exposure, contract and licensing risk,
  M&A structure considerations if it's an acquisition.
- **deep:** `warehouse` usage signalling demand, competitive timing, internal
  capability assessment.

finance-analyst folds the three outputs into `investment-decision.md`: TL;DR,
context, option comparison (Build / Buy / Partner) with cost/time/pros/cons/risk,
3-year NPV table, customer evidence, recommendation with deciding factors, and ask.

Peer review is required before delivery: high stakes need a cross-model check.
Run the `review` agent (adversarial, different vendor) or the `peer-review` skill
over the doc.

Output: `investment-decision.md`

## Task 5: Pricing / Packaging Decision

**Triggers:** "How should we price X?", "Packaging options for X", "Should we bundle X with Y?"

**Roles:** finance-analyst + growth-marketing (concurrent at Stage 2).

Stage 1 (deep): pull current pricing, seat counts, utilization, and plan
distribution from `warehouse`; willingness-to-pay signals and pricing objections
from `calls`; deal history, discount patterns, and win/loss from `crm`. This join
spans 2+ capabilities, so prefer `code-mode` for the fan-out. Fallback:
user-provided data and web research for competitive pricing.

Stage 2 (concurrent): finance-analyst produces unit economics, margin, and
revenue impact per option; the `growth-marketing` agent covers willingness to
pay, competitive pricing benchmarks, and deal-motion impact.

Output: `pricing-decision.md`: current state, customer evidence, 2-3 options with
revenue/margin/customer impact each, recommendation, implementation plan
(timeline, grandfathering, enablement), and ask.

Peer review required if the change touches a large number of customers.

## Task 6: Decision One-Pager

**Triggers:** "Write a one-pager on X", "Decision doc for X", "I need to present X to [audience]"

**Role:** varies by topic. Route to the right specialist for what the decision is about.

Use the `one-pager` skill for the format: one page, one ask, every sentence
survives "so what?". Structure: TL;DR, Context (data not vibes), Proposal
(specific actions with dates), Evidence (numbers and quotes), Risks and
Mitigations (2-3 max), Ask (specific: budget, headcount, approval, by date).

Peer review required before delivery if the audience is a leadership team or board.

Output: `one-pager.md`

## Escalation policy

Escalate immediately, without waiting for the full analysis:

- Legal review finds HIGH risk.
- Financial model shows negative NPV in every scenario.
- Investment decision has no clear winner: present all options honestly.

Max 2 revision loops per role. On the third failure, escalate with what was
tried, what failed, and a recommendation. Never loop silently.

## Connections

- **From `plan`:** pricing and decision work often follows a product process.
- **To `one-pager`:** Task 6 delegates the format there.
- **To `competitive`:** Task 3's landscape delegates there.
- **To the `finance-analyst` agent:** Tasks 1, 3, 4, 5 lean on its modeling discipline.
- **To `brainstorm` / `challenge`:** run before committing to a direction.
