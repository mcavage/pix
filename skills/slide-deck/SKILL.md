---
name: slide-deck
description: Build a Marp-compatible presentation deck with intentional messaging strategy. Use for "make a deck", "create slides", "put together a presentation", or any request to turn a doc, spec, or idea into a slide deck.
---
# slide-deck

Turn content into a structured Marp deck. Slides are decisions, not transcripts.

## Audience-first framing

Before writing a single slide, identify the audience and lead accordingly:
- Exec: open with the ask, then the evidence.
- Engineering: open with the architecture or technical decision.
- Customer: open with their problem, not your solution.
- Board: open with financials or key metrics, then context.

If the audience is mixed, write for the decision-maker in the room.

## Iron laws

- One idea per slide. If you need two sentences to explain the title, split the slide.
- Max 3 bullets per slide. Max 15 words per bullet.
- Data slide titles state the insight, not the metric: "Conversion dropped 15%" not "Conversion Rate".
- No agenda slide. No "Questions?" slide. No "Thank you." slide.
- End with the ask or the next action, stated plainly.
- No nested bullets. Ever.

## Structure

1. **Frame**: one slide: the situation, the decision or question, the stakes.
2. **Evidence**: 2-5 slides: data, proof, or demonstration. Each slide = one supporting point.
3. **Options** (if a decision deck): one slide per option, same schema: what it is, tradeoff, recommendation.
4. **Recommendation**: one slide: your call, stated as a sentence.
5. **Ask**: one slide: what you need from this audience, by when.

For a status update, drop Options; for a proposal, lead with the Recommendation. Scale the evidence section to the room's prior context.

## Marp format rules

- Separate slides with `---`.
- Use `<!-- _class: lead -->` on the opening slide only.
- Inline code for technical terms; code fences for any block of code or config.
- If you include a chart or diagram, use Mermaid inside a fenced block.
- Consistent heading level per slide type: H1 for slide title, H2 for section dividers.

## Workflow

1. Run `brainstorm` or `plan` first if the underlying content does not exist yet.
2. Ask for: audience, goal (inform / decide / sell / align), and any hard constraints (time limit, slide count, tone).
3. Draft the narrative arc in plain sentences before opening a slide file. The arc is the real deliverable.
4. Write the deck to `docs/slides/<name>.md`.
5. If the deck will be reviewed before presenting, hand it to `design-review`.