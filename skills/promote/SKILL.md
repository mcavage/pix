---
name: promote
description: Review the lessons the memory watcher captured repeatedly and graduate the recurring ones into a real skill or convention edit. Use for "review learnings" or "promote learnings".
---
# promote

The watcher captures corrections as you work. When the same lesson recurs, it
should stop being a note and become changed behavior. This is the gated step that
does that: it closes the loop from a repeated correction to a real edit.

## Steps
1. **Pull the candidates.** Run `/learnings` (it returns the captured learnings
   that have recurred at least 3 times, highest first). Each is a lesson the
   watcher has seen you repeat.
2. **Decide where each belongs.** A specific skill (whose rule should change), a
   convention doc, or AGENTS.md. If it is already covered, mark it handled.
3. **Propose the concrete edit.** Show the exact change to the skill or convention
   (the before/after). Do NOT apply a skill edit without the user's say-so.
4. **On approval, apply it,** then retire the learning so it does not keep
   resurfacing: tell the user it has graduated and mark it handled.

Keep it tight: surface the top 3-5, propose edits, wait for approval. The point is
to close the loop from "we hit this again" to "the skill now prevents it."
