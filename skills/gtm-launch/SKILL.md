---
name: gtm-launch
description: Take a built thing to a launched thing with the crew — positioning, competitive intel, content, enablement, campaign plan, launch readiness, gated at positioning. Use for "launch this feature".
---
# gtm-launch

Turn a built feature into a launch you can run. One user gate (positioning),
then the machine runs. Output feeds into `one-pager`, `ship`, or `verify`.

The point: **lock positioning before spending on content.** Wrong positioning
means wrong content, wrong enablement, wrong campaign — so the one gate that
stops the pipeline is the positioning gate.

## Pipeline at a glance

```
1. Positioning + competitive intel (growth-marketing + research fan-out, concurrent)
2. USER GATE (go / redirect / kill)
3. Content + enablement + dev content (growth-marketing x2 + devrel, concurrent)
4. Copy review (ux-copywriter)
5. Campaign plan (growth-marketing)
6. Peer review (cross-model)
7. Launch readiness
```

## Disk discipline

All state on disk, not in context. Working dir: `.pi-agent/gtm/<launch-slug>/`.
On start, look for `status.json`: if present, resume from the current stage;
else create the dir and write `status.json` with every stage `pending`, updated
on each transition. Never tell a subagent to "read the positioning brief" —
paste the relevant section inline. Each subagent gets a fresh context with only
what its stage needs. This skill names **capabilities**, not vendors: resolve
each through `capability-routing` (reads `capabilities.json`, pulls from wired
providers or tells you it is `none`; `none` = degrade to web/files and flag the
gap out loud, never fabricate).

**status.json shape:**

```json
{
  "launch": "<launch-slug>",
  "source": "plan | standalone",
  "plan_dir": "<path if from plan>",
  "stages": {
    "positioning":      { "status": "pending", "artifact": "positioning.md" },
    "user-review":      { "status": "pending", "verdict": null },
    "content":          { "status": "pending", "artifacts": [] },
    "enablement":       { "status": "pending", "artifact": "enablement.md" },
    "dev-content":      { "status": "pending", "artifact": "dev-content.md" },
    "copy-review":      { "status": "pending", "verdict": null },
    "campaign-plan":    { "status": "pending", "artifact": "campaign-plan.md" },
    "peer-review":      { "status": "pending", "verdict": null, "retry": 0 },
    "launch-readiness": { "status": "pending", "artifact": "launch-checklist.md" }
  },
  "launch_ready": false
}
```

## Pre-flight

1. Check for `status.json`. If present, this is a resume.
2. **Gather inputs.** From `plan`: read the PR/FAQ, PRD, and discovery artifacts
   from `.pi-agent/plan/<feature-slug>/`. Standalone: gather context from the
   user's brief.
3. **Resolve data capabilities** through `capability-routing`. Pull **crm**
   (deal/customer context), **warehouse** (usage metrics, adoption trends), and
   **calls** (customer voice, recorded transcripts). For **calls**, fan out
   across every wired call source and merge. Any capability that resolves to
   `none`: say so plainly ("no calls wired, using public reviews and forums")
   and substitute web research plus user-provided files.
4. Write `status.json` with all stages pending.

## Stage 1: Positioning + competitive intel (concurrent)

Fan out in one turn; the orchestrator collects both results before merging.

**`growth-marketing`** produces the positioning half of `positioning.md`:

- Target audience: a specific segment, not "developers" or "enterprises".
- Positioning statement: "For [target] who [need], [product] is a [category]
  that [key benefit]. Unlike [alternative], we [differentiator]."
- Core message: one sentence.
- Messaging hierarchy: primary message plus 3 supporting messages, each with
  evidence.
- Tone guidance for this specific launch.
- Channel strategy: where does this audience actually live?

**Research** runs as `fanout` workers pulling in parallel — or `deep` for one
large-context pull (a long competitive corpus, a transcript archive). Multi-
capability joins (list, then per-item detail, across servers) prefer `code-mode`
over a dozen separate calls. There is no `research` agent; use `fanout`. Merge
into the same `positioning.md`:

- Competitive landscape for this specific capability (the `competitive` skill is
  the tool when a named competitor needs a full brief).
- Customer voice: quotes from **calls** (merge across every wired source) or
  **crm** notes — or public reviews and forums if those resolve to `none`.
- Usage data supporting the positioning (from **warehouse** if wired; else
  public benchmarks or the user's own analytics).
- Existing positioning docs or battle cards from the repo or user files.

Merge both halves into `positioning.md`. Update `status.json`.

## Stage 2: User gate (pipeline stops)

**This is the gate.** Present: the positioning brief, competitive context,
recommended channels, and — if from `plan` — a consistency check against the
PR/FAQ. Then wait for the user. Three outcomes:

- **Go:** proceed. Record verdict → "go".
- **Redirect:** update the brief with the user's direction, then proceed. Record
  verdict → "redirect".
- **Kill:** stop. Record verdict → "kill".

## Stage 3: Content + enablement + dev content (concurrent)

Three subagents in one turn; each inherits the positioning brief pasted inline.
The orchestrator collects all three before Stage 4.

- **`growth-marketing` (content):** blog post, landing page copy, 3-5 platform-
  specific social posts, email announcement. Lead with the problem; no marketing
  fluff. Write `blog-post.md`, `landing-page.md`, `social-posts.md`, `email.md`.
- **`growth-marketing` (enablement):** the sellability lens — battle card (what
  it is, who it's for, objections and responses, competitive positioning,
  pricing guidance), talk track (60-second pitch, 5-minute demo script,
  discovery questions), and a customer-facing one-pager for an enterprise
  launch (hand that to `one-pager`). Write `enablement.md`. For pricing or
  unit-economics claims in the battle card, defer to the `finance-analyst` agent
  rather than inventing numbers.
- **`devrel`:** getting-started tutorial (per `docs-standards`), copy-pasteable
  sample code that runs, migration guide if this replaces something, demo
  script. Write `dev-content.md`.

Update `status.json` after all three complete.

## Stage 4: Copy review

Input: all content from Stage 3. `ux-copywriter` checks:

- Anti-slop across every piece (per `anti-slop`).
- Voice consistency with the positioning brief.
- Cross-content consistency: blog and landing page tell the same story;
  enablement matches positioning.
- Landing-page microcopy per `microcopy`.

Write `copy-review.md`. On CHANGES NEEDED, apply fixes directly to the content
files; no round-trip to the original subagents for copy-only edits. Update
`status.json`.

## Stage 5: Campaign plan

`growth-marketing` produces `campaign-plan.md`:

- Launch timeline: day-by-day for launch week, week-by-week for the first month.
- Channel plan with specific dates and content mapped to each channel.
- Metrics: what to measure, baselines (from **warehouse** if wired), targets,
  measurement method.
- Dependencies: what must be live before launch (docs, pricing page, feature
  flag).
- A/B test opportunities.

Update `status.json`.

## Stage 6: Peer review (cross-model)

Input: the full package (`positioning.md` + all content + `enablement.md` +
`dev-content.md` + `campaign-plan.md` + `copy-review.md`). Run the `review`
agent (or the `peer-review` skill) — a different model vendor than the one that
wrote most of the content, so its blind spots differ. Evaluate:

1. Accuracy: claims match evidence; competitive claims are defensible.
2. Slop score 0-10 (must be <=3 to pass).
3. Completeness: anything missing that content, enablement, or devrel needs to
   execute?
4. Actionability: can someone run the campaign plan without asking questions?
5. GTM-specific: does the positioning actually differentiate? Would a developer
   try the product after reading the blog post? Could a seller use the battle
   card in a live call? Is the tutorial actually runnable?

Verdict: PASS / REVISE / REJECT. Max 2 revision loops. On REJECT, escalate to
the user; positioning may need rethinking. Write `peer-review.md`. Update
`status.json`.

## Stage 7: Launch readiness

The orchestrator produces `launch-checklist.md` from all artifacts:

```
# Launch Readiness: [Launch Name]
Target Date: [date] | Status: GREEN / YELLOW / RED

## Content:      [ ] blog [ ] landing [ ] social [ ] email
## Enablement:   [ ] battle card [ ] talk track [ ] one-pager
## Developer:    [ ] tutorial [ ] sample code [ ] docs updated
## Technical:    [ ] feature deployed [ ] feature flag [ ] monitoring
## Dependencies: [ ] [item]: owner | status | blocks
## Risks:        [risk]: impact | mitigation | owner

Go / No-Go: [GO / NOT READY, what's blocking]
```

Update `status.json`: `launch_ready: true/false`.

## Post-pipeline

Present to the user: launch name, target date, readiness status, any blockers.
All artifacts live in `.pi-agent/gtm/<launch-slug>/`. Two user touchpoints
total — the positioning gate (Stage 2) and this summary. Everything between is
the machine.

## Skip logic

| Situation | Skip |
|---|---|
| Internal launch (no external content) | Marketing content in Stage 3 (blog, landing, social) |
| Enablement-only launch (new pricing, new SKU) | Dev content in Stage 3 |
| Developer-only launch (OSS, CLI tool) | Enablement in Stage 3 |
| Urgent / time-boxed | Peer review (Stage 6); note it in `status.json` |
| From `plan` with a strong PR/FAQ | Positioning is lighter — inherit and validate, don't redo |

## Retry and escalation

Max 2 revision loops per stage. On the third failure, escalate to the user with
both positions and a recommendation. Never loop silently; note each revision in
`status.json`. If any stage fails, report what completed and what did not.

## Connection to other workflows

- **From `plan`:** the PR/FAQ and PRD feed directly into Stage 1. Positioning is
  lighter when a PR/FAQ already exists — inherit and validate.
- **Standalone:** for a campaign, event, or announcement with no `plan` upstream,
  Stage 1 does the full positioning work from scratch.
- **Output to:** `one-pager` (a customer-facing one-pager), `ship` (deploy the
  content and doc changes), `verify` (confirm the tutorial actually runs).
