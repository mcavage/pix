---
name: promote
description: Inspect a capped recent slice of memory (facts/learnings mixed, no recurrence ranking) and graduate a recurring one into a skill or convention edit. Use for "review learnings" or "promote learnings".
---
# promote

The watcher captures corrections as you work. When the same lesson recurs, it
should stop being a note and become changed behavior. This is the gated step that
does that: it closes the loop from a repeated correction to a real edit.

## Steps
1. **Pull the candidates.** There is no frequency-ranked promotion surface today
   (`/learnings` and its host-side `promotable` RPC were removed once no
   in-tree consumer remained; a replacement is deferred to a future unit).
   Until then, inspect what the watcher captured through the smallest existing
   recall surface: `/recall` (blank shows everything visible, up to 100 rows,
   most-recent-first) or a targeted `/recall <topic>`. This returns facts and
   learnings MIXED, in one undifferentiated recency-ordered list, not grouped
   or counted by kind, and is capped at 100 rows even when more exist, so a
   lesson that recurred outside that recent slice will not surface here. There
   is no recurrence count of any kind: use judgment on what has actually
   recurred, this step cannot tell you.
2. **Decide where each belongs.** A specific skill (whose rule should change), a
   convention doc, or AGENTS.md. If it is already covered, mark it handled.
3. **Propose the concrete edit.** Show the exact change to the skill or convention
   (the before/after). Do NOT apply a skill edit without the user's say-so.
4. **On approval, apply it,** then retire the learning so it does not keep
   resurfacing: `/forget` it (or tell the user it has graduated) and mark it handled.

Keep it tight: surface the top 3-5, propose edits, wait for approval. The point is
to close the loop from "we hit this again" to "the skill now prevents it."
