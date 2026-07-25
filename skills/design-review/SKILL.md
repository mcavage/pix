---
name: design-review
description: Designer's-eye visual audit of a running web UI — screenshot each view, evaluate hierarchy, spacing, typography, consistency, AI-slop, score and fix. Use for "design review" or "polish the UI".
---
# design-review

Use the `agent_browser` tool to actually SEE the UI, then critique it like a
senior designer: specific and actionable, not vibes.

## Steps
1. **Capture.** Open each significant view; `screenshot` at a desktop and a
   mobile width. Snapshot structure where it helps.
2. **Evaluate** each view on: visual hierarchy, spacing/rhythm, alignment,
   typography (scale, line length, weight), color/contrast, cross-view
   consistency, and motion. Flag AI-slop tells: generic gradients, emoji
   headers, center-everything, identical card grids, default shadows.
3. **Score** each dimension 0-10 with one line on "what would make it a 10".
4. **Fix.** Apply the highest-impact changes in source (respect `DESIGN.md` if
   present), re-screenshot before/after to prove it, commit atomically.

Prioritize the few changes that most raise perceived quality over a long nitpick
list.
