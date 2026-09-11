---
name: design-review
description: Designer's-eye visual audit of a running web UI, screenshot each view, evaluate hierarchy, spacing, typography, consistency, AI-slop, score and fix. Use for "design review" or "polish the UI".
---
# design-review

Use an available authorized browser in an isolated profile to SEE the UI, then critique it like a
senior designer: specific and actionable, not vibes.

## Steps
1. **Capture.** Open each significant view; `screenshot` at a desktop and a
   mobile width. Snapshot structure where it helps.
2. **Evaluate** each view on: visual hierarchy, spacing/rhythm, alignment,
   typography (scale, line length, weight), color/contrast, cross-view
   consistency, and motion. Flag AI-slop tells: generic gradients, emoji
   headers, center-everything, identical card grids, default shadows.
3. **Score** each dimension 0-10 with one line on "what would make it a 10".
4. **Disposition.** In an independent acceptance role, return findings without
   editing source. When explicitly assigned implementation, fix the highest-impact
   issues, re-screenshot and send the changed candidate for independent review.

Prioritize the few changes that most raise perceived quality over a long nitpick
list.
