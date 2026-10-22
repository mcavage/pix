---
name: peer-review
description: Cross-model peer review for NON-code deliverables (docs, plans, decisions, proposals). Use for "review this doc" or "peer review". Distinct from code-review, which reviews diffs.
---

> **Runtime:** this Kit uses marsh, not the legacy Pix launcher or subagent
> API. Load `marsh-orchestration` for executable delegation/composition forms.
> Historical task/chain/repair-packet outputs describe work orders only; do not
> invoke old Pix lifecycle commands or assume memory/business MCP is installed.

# peer-review

Send the deliverable to a `review` child job (a `pix -p` run with the `review` role prompt
and an explicit `--model`, per `marsh-orchestration`) running a different model vendor than
the producer; prove the vendor from response metadata, not the requested model. Different vendor means different blind spots,
not a rubber stamp.

## When to use

Use for anything that leaves the project or drives a significant decision: docs, plans,
proposals, external communications, design specs, architecture decisions. Skip for code
diffs (use `code-review`), quick internal notes, exploratory drafts, or anything the user
will review directly.

## Steps

1. **Brief the reviewer.** Start the `review` child job with: the full document, the intended
   audience, and the verdict criteria below. Instruct it to score each dimension and list
   findings, not just impressions.
2. **Reviewer scores 5 dimensions** (each 1-5, except Slop which is 0-10):
   - **Accuracy** (1-5): claims match cited evidence, metrics are internally consistent,
     competitive statements are verifiable.
   - **Slop** (0-10, must be <=3 to pass): AI writing tells: "leverage", "utilize", "delve",
     "robust", "seamless", "comprehensive", em-dash overuse, filler paragraphs, conclusions
     that restate the intro, bullets all starting the same word.
   - **Completeness** (1-5): nothing the audience needs is missing; someone could act on
     this without a follow-up question.
   - **Actionability** (1-5): next steps are specific (who, what, when); the ask is clear;
     a busy reader can decide from this alone.
   - **Consistency** (1-5): sections tell the same story; numbers match across sections;
     recommendations follow from the evidence.
3. **Reviewer emits verdict** using this format:

```
# Review: [Document Title]

## Verdict: PASS | REVISE | REJECT

## Scores
- Accuracy: [1-5]
- Slop: [0-10]
- Completeness: [1-5]
- Actionability: [1-5]
- Consistency: [1-5]

## Findings
### Critical (must fix)
- **[Section]:** [problem] -> [fix]
### Important (should fix)
- **[Section]:** [problem] -> [fix]
### Minor (nice to have)
- **[Section]:** [problem] -> [fix]

## Slop
- "[flagged phrase]" -> [suggested replacement]
```

## Verdict rules

- **PASS:** All 1-5 scores >= 3, Slop <= 3, no critical findings. Done.
- **REVISE:** Critical findings present but the document is fundamentally sound. Pass
  findings back to the producing agent; agent revises; re-review. Max 2 loops.
- **REJECT:** Fundamental problems with approach, not just execution. The document needs
  rethinking, not polishing. Escalate to the user with: the original, the findings, what
  was tried, and a recommendation.

After 2 REVISE loops with no PASS, escalate as REJECT regardless.
