---
name: plan
description: Agree on a product through PR/FAQ, PRD and architecture before implementation. Use for "plan this" or "write a PRD".
---
# plan

Make the product promise concrete before investing in implementation. The main
agent owns this process and writes the plan. Specialists are optional, focused
second opinions for an unresolved question; there is no default document crew.

Read [product agreement](../deliver/references/product-agreement.md). That rule
also applies under `deliver` and `build`: they do not silently waive user agreement.
Honor existing approvals and explicit authorization for autonomous product choices.

## Shape and agree

Read the user's context and relevant existing product/code. For a fuzzy idea,
compare materially different experiences and tradeoffs; ask only questions whose
answers affect the product. Keep research proportional and label assumptions.
Do not auto-expand scope because an adjacent change seems inexpensive.

Write `.pi-agent/plan/<slug>/pr-faq.md`: user and situation, promised experience,
why this approach, how it works, non-goals, likely user questions, constraints,
risks and assumptions. Do not invent customer quotes, validation or impact figures.
Present the product choice and unresolved tradeoffs for agreement. If the user
already approved a supplied PR/FAQ, reuse it instead of rewriting or reapproving it.

## Specify the agreed release

Write `prd.md` beside the PR/FAQ: primary journey and priorities, criterion IDs,
observable acceptance including failures/recovery, concrete UI states and visual
intent where relevant, scope boundaries and success evidence. Write a concise
`architecture.md`: existing stack/callers, data ownership, interfaces, dependencies,
security boundaries, test strategy and implementation units. A small plan may
combine these sections in one file; role count and document count are not goals.

Agree material unresolved product tradeoffs before implementation. Routine technical
choices within the approved promise do not need another approval. Use a read-only
specialist only for a named uncertainty; record its evidence and decision. Parallel
investigations are useful when independent, not because a checklist names roles.

## Handoff

Keep `status.json` small: artifact paths/identities, agreement scope and authorizing
user message (or explicit autonomy instruction), unresolved questions and next step.
`eng_ready` means the product promise is agreed and acceptance is implementable;
it does not mean code, tests or release review have run. Preserve prior artifacts
and approvals when resuming. Changing the product promise reopens that decision.

A plan-only request stops with the reviewable plan. With implementation already
authorized, continue through `build`/`deliver` using these same artifacts, without
another document crew, redundant gate or invented duration estimate.
