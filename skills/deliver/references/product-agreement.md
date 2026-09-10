# Agree on the product before implementation

The main agent owns product decisions, architecture and integration. Keep one
coherent product context; specialists answer bounded questions, not mandatory
stages. Model selection belongs to the user's environment or explicit override,
not a model switch hidden in a skill.

For a new product or material feature, use this sequence before production code:

1. **PR/FAQ:** the target user and situation, promised experience, materially
   different approaches and tradeoffs, chosen release, non-goals and unresolved
   assumptions. Benefits are hypotheses unless evidenced; do not fabricate quotes,
   customer research or impact numbers. Ask for agreement on the product promise.
2. **PRD and architecture:** journeys and priorities; observable acceptance IDs;
   empty/loading/error/recovery states; visual direction for UI; data ownership,
   interfaces and failure boundaries; and a small implementation plan. Surface
   material unresolved scope or UX tradeoffs for agreement before building.
3. **Implement the agreed scope:** keep routine technical choices autonomous.
   Do not replace the agreed product with an easier implementation or expand its
   promise silently. Reopen only the materially changed decision.

Use `plan` to create missing artifacts. Keep one copy of each artifact and link
it from the delivery contract; do not duplicate a PRD in every work order.
Record agreement in status with the artifact identity, scope and the user message
that authorized it. Existing approval persists across turns and skill boundaries.
A supplied approved spec can satisfy both stages. A plan-only request ends with
that plan; it does not authorize implementation.

**Autonomy is explicit.** If the user authorizes autonomous product decisions,
record that instruction and decide within its scope without pausing. Merely
invoking `deliver` does not waive agreement. A later “cook” or “build it” approves
a concrete proposal already presented; it is not a fresh permission interview.
If agreement is required and absent, prepare the concrete reviewable proposal,
then ask the product question; continue only independent investigation meanwhile.

A bounded bug fix, refactor or maintenance task with an understood requested
outcome uses the short delivery contract instead of manufacturing PR/FAQ and PRD
ceremony. Classify by the product decision being made, not estimated lines or days.
