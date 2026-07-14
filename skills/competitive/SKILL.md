---
name: competitive
description: Structured competitive brief on a named competitor or category. Use for "analyze competitor X", "how do we compare to Y", "what's the competitive landscape for Z", or "should we worry about X".
---
# competitive

Produce a tight, opinionated brief, not a Wikipedia summary. Every section
should answer a question a decision-maker actually has.

## Flow

1. **Scope.** Confirm the competitor (or category) and the angle: are we looking
   at a specific product area, a pricing comparison, a strategic threat, or all
   three? One clarifying question if it's ambiguous; proceed otherwise.

2. **Research.** Fan out web searches in parallel: company overview, recent news
   (last 6 months), pricing page, job postings, changelog/blog, and any analyst
   coverage. Use `debug` discipline when claims conflict: trace to the
   primary source, not the summary.

3. **Profile.** What they do, who they target, funding and headcount (if public),
   and their 1-sentence positioning pitch.

4. **Feature matrix.** Honest four-bucket comparison for the capabilities that
   matter to the user's context:

   | Capability | Us | Them | Notes |
   |---|---|---|---|
   | ... | Leading / Parity / Behind / N/A | ... | ... |

   "Leading" and "Behind" require evidence, not opinion.

5. **Pricing model.** What they charge at small/medium/large scale, and how the
   unit economics shift as usage grows. Flag hidden costs (egress, seats,
   overages).

6. **Where they win (honest).** 2-4 concrete scenarios where a customer would
   pick them over us right now. No hedging.

7. **Where we win.** Same format. Only claims you can back up.

8. **Investment signals.** Hiring patterns, recent acquisitions, conference
   themes, patent filings. These reveal roadmap intent before the roadmap ships.

9. **Recommendation.** One of: **Compete** (match or beat them directly),
   **Differentiate** (stop competing on their terms), **Partner** (their strength
   is ours if combined), or **Ignore** (not our customer segment). Add one
   sentence: "if we do nothing for 12 months, X happens."

## Output format

Deliver the brief as a markdown document. If the user has an existing doc to
update, patch it in place. Offer to turn the brief into a `build` story if a
feature response is implied.

## Iron laws

- No speculation passed off as fact. If you don't know, say so and note how to
  find out.
- If sources conflict, show both and say which you trust and why.
- Do not soften the "where they win" section. A brief that only flatters is
  useless.
