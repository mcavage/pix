---
name: one-pager
description: Write an executive one-pager or decision document, a single page that drives a specific ask (approval, budget, priority call). Use for "write a one-pager", "draft a decision doc", "make the case for X", or "I need to get buy-in on Y".
---
# one-pager

One page. One ask. Every sentence survives "so what?"

**Iron law: no ask without a number and a date.** A one-pager that ends in
"we should explore this" is a blog post, not a decision doc.

## Structure (in order, no exceptions)

1. **Title.** The decision or proposal in plain English. Not a project codename.
2. **TL;DR.** 2-3 sentences: the situation, the proposed action, the expected
   outcome. A reader who stops here should know what you want and why.
3. **Context.** Data, not vibes. What is true today? A metric, a user quote, a
   cost figure, a trend. If the context is a problem, quantify it. If it is an
   opportunity, scope it. No background paragraphs that don't earn their place.
4. **Proposal.** The specific action you are asking for, written as imperatives:
   "Approve X", "Allocate Y FTEs to Z", "Deprioritize A in favor of B." One
   proposal per doc. If you have two proposals, you have two docs.
5. **Evidence.** 3-5 bullets: numbers, comparisons, user signal, precedent. Each
   bullet answers a predictable objection. If you are comparing to a competitor
   or alternative, be specific and cite a source.
6. **Risks and mitigations.** Exactly 2-3. Name the real risks, not the safe
   ones. Each risk gets one mitigation sentence. If a risk has no mitigation,
   say so and explain why you are proceeding anyway.
7. **Ask.** One sentence: what you need, from whom, by when. Example:
   "Approve the Q3 budget increase of $X by [date] so we can start hiring."

## Hard limits

- 500-700 words total. If you are over, cut context first, then evidence bullets.
- No hedging language: "might", "could potentially", "it seems". Assert or
  qualify with data.
- Tables only if you are comparing 3 or more discrete options on 3 or more
  dimensions. Otherwise, prose.
- Tone is direct and opinionated. You are advocating, not surveying.

## Flow

1. If the proposal is still fuzzy, run `brainstorm` first to sharpen the idea
   before writing the case for it.
2. If you need to gather data or validate assumptions before writing, run
   `debug` on the open questions.
3. Draft in order. Do not wordsmith section 5 before section 3 is locked.
4. Self-check: read only the TL;DR and the Ask. If those two sections alone do
   not convey the decision, rewrite them.
5. Save to `docs/<slug>-one-pager.md` and return the path.
