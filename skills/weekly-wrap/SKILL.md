---
name: weekly-wrap
description: Friday synthesis and weekly retro that pulls the week's meetings, chat, calendar, and git activity, scores priority progress, surfaces blockers, and builds a concise wrap doc. Use for "weekly wrap".
---
# weekly-wrap

Friday synthesis: what moved, what stalled, what needs attention next week.

## Capabilities (check availability, fall back gracefully)

This skill names **capabilities**, not vendors. Resolve each through
`capability-routing`, which reads `capabilities.json` and either pulls from the
wired provider(s) or tells you it is `none`. Run available pulls in parallel. A
capability resolving to `none` means degrade to web/files and flag the gap: say
plainly "no <capability> available, using <fallback>". Never fail silently.

`gworkspace`, `chat`, `calls`, and `crm` are provided by a pack — in the default
public profile they resolve to `none`, so most runs take the degraded path
(git + memory + the user's files) unless a pack wires them. `github` is always
available.

**Full path (all capabilities wired):**

- **gworkspace**: meetings for the past 7 days + next 7 days preview
- **chat**: search threads from this week on priorities and blockers
- **calls**: decisions and action items from this week's recorded meetings
- **crm**: deal movement, customer activity, pipeline changes
- **github** (always available): commits, PRs, lines changed across repos this week

**Degraded path (no capabilities wired):**

- Git history is the primary record of code activity
- Memory (auto-injected) carries prior session context, decisions, and action items
- User's own notes, files, or repos in the working directory
- Ask the user to paste any meeting notes or blockers if nothing else is available

## Step 1: pull the week's activity

Resolve each capability through `capability-routing`. Where a pull spans 2+
systems or needs a join (calls plus crm movement plus calendar), prefer
`code-mode` for the fan-out rather than separate tool calls.

**Git activity (**github**, always run):**

```bash
git log --oneline --since="7 days ago" --no-merges 2>/dev/null | head -30
git shortlog --since="7 days ago" -sn --no-merges 2>/dev/null
```

Summarize: total commits, features shipped, PRs merged, repos touched, shipping streak (consecutive days with commits), gaps.

**calls:** fan out across all wired call sources, merge, and pull decisions and action items from the past 7 days.

**gworkspace:** what's coming next week that needs prep (calendar).

**chat:** threads on active priorities, anything flagged as a blocker.

**crm:** deal stage changes, customer touchpoints, pipeline movement.

If a capability is `none`, note it and continue. Do not block on missing capabilities.

## Step 2: score priority progress

For each active priority (from memory context):

- **Green**: clear progress this week (meetings happened, commits landed, decisions made)
- **Yellow**: some activity but stalled or waiting on someone
- **Red**: no movement, or a blocker surfaced

Apply `challenge` discipline inline. For each RED:

- How many consecutive weeks has it been red? (check prior wrap summaries in memory)
- If 3+ weeks: "This has been red for N weeks. Escalate, drop, or name the actual unblock?"

Cross-reference action items from last week against this week. Carried-over items with no movement: "Escalate, reassign, or drop?"

**Time allocation check:** count meeting hours by category. If over 60% of the week was meetings, flag it.

## Step 3: build the wrap doc

```
# Weekly Wrap: [date range]

## Priority Scorecard
- **[priority]:** GREEN/YELLOW/RED, [what happened]. Next: [what's next]
  [challenge question inline for any RED]

## Key Meetings This Week
[3-5 most important with 1-line outcome each]

## Decisions Made
[Bullet list from meetings and memory]

## Open Action Items
- **[owner]:** [action] (from [source], due [date if known])

## Code Activity
- Commits: N | PRs merged: N | Repos: [list]
- Streak: X consecutive days (or: gap on [days])

## Next Week Preview
[Key meetings, deadlines, anything needing prep]

## Retro: what worked, what to change
[1-2 things that went well and are worth repeating; 1-2 things to change next week. Honest, specific, not a process platitude.]

## Flags
[Anything stalled, at risk, or needing attention. Direct, no softening]
```

## Step 4: offer follow-up

Ask: "Want me to write a status update to share (use `draft-email` or `one-pager`), or capture any decisions/priorities that changed this week?"

Do not auto-write to memory. Offer it; wait for confirmation.
