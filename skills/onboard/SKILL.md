---
name: onboard
description: "Fast onboarding: probe the environment, ask one batched question, confirm before writing, then seed identity and context to memory. Use for 'set me up', 'onboard me', or after a fresh install."
---
# onboard

Seed identity and context into memory fast. Three beats: probe the environment
deterministically, ask one consolidated question for the rest, show exactly what
you will write, and only write on confirmation. No 6-step ping-pong.

## Step 1: Probe the environment

Run this in a single bash block before asking the user anything:

```bash
echo "name:     $(git config user.name 2>/dev/null)"
echo "email:    $(git config user.email 2>/dev/null)"
echo "gh_login: $(gh api user --jq '.login' 2>/dev/null)"
echo "gh_name:  $(gh api user --jq '.name' 2>/dev/null)"
echo "project:  $(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)")"
echo "date:     $(date '+%Y-%m-%d')"
```

Note which fields resolved confidently and which came back empty or conflicting.
Name and email that agree across git and gh are confident; skip asking about them.

## Step 2: One batched ask

Send ONE message covering only what the environment could not answer and what
genuinely changes how the agent behaves. Skip any question the probe already
answered. Keep the prompt short:

**Ask only these things (in one message):**

- **Name** (only if not resolved above): how the agent addresses you and signs
  off on messages you review.
- **Communication style** (bullets or prose, brief or thorough): shapes the
  format of every response and deliverable.
- **AI pet peeves** (what drives you crazy about AI assistants): the agent
  avoids those patterns starting now.
- **Preferred tone** (casual vs. formal, blunt vs. diplomatic): changes every
  output you will ever see.
- **2-3 values to operate by**: guides recommendations and tradeoff calls across
  all future sessions.

Do NOT ask for title, role, or org chart. The agent infers context from the work
itself, not from a job description.

## Step 3: Show the full picture; ask to confirm

Before writing anything to memory, display a plain summary:

```
Here's what I'll remember about you:

  Name:   [value]
  Email:  [value]  — used for gh operations and message sign-offs
  Style:  [their answer]
  Peeves: [their answer]
  Tone:   [their answer]
  Values: [their answer]

Anything wrong or missing? I'll make corrections before writing.
```

Wait for the user's response. Apply any corrections and, if anything changed,
show the updated list once before proceeding. Do not write until the user says
to go ahead.

## Step 4: Write to memory

Write each fact as a separate `/remember` entry tagged `["soul", "bootstrap"]`.
Confirm once: "Identity stored. Every future session will recall this
automatically."

## Step 5: Probe capabilities

Resolve each capability through `capability-routing` (which reads
`capabilities.json`). For each, call the cheapest read-only tool, report what
came back, and state plainly "no [capability] wired" if it resolves to `none`.

| Capability | Fallback when `none` |
|---|---|
| github | `gh` CLI; read repos and PRs directly |
| gworkspace | Ask for upcoming deadlines; store in memory |
| chat | Skip; no fallback |
| docs | Fall back to web search |
| meeting-notes | Ask the user to paste recent notes; store in memory |
| calls | Ask the user to paste recent call summaries; store in memory |

When probing **calls**, fan out across every wired source and merge results.
Never report a capability as wired if the probe failed.

## Step 6: Seed initial context

**With capabilities wired:** Run `refresh context` (or the equivalent sweep
skill in the overlay). It pulls live data and writes findings to memory with
appropriate tags.

**With everything `none` (degraded path):** Ask in one message:

- Top 3-5 current priorities (tag: `priorities`)
- Key external accounts or customers (tag: `accounts`)
- Upcoming meetings or deadlines (tag: `calendar`)

Tell the user: "You can re-run this later once live data sources are connected."

## Step 7: Run a first useful task

Pick the most natural task from what the user said and run it. Good defaults:

- `standup` if they mentioned meetings or inbox overload
- `brainstorm` or `build` if they mentioned an active project
- `one-pager` or `challenge` if they mentioned a pending decision

Running something real confirms the system works and gives immediate value.

## Step 8: Orient the user

Give a short overview of daily patterns. Use bold-label lines, not a table
(terminal may not render tables).

**Daily:** `standup`, `prep for my meeting with [person]`, `debrief [meeting]`

**On demand:** `brainstorm`, `build`, `code-review`, `ship`, `verify`,
`draft-email`, `one-pager`, `challenge`

**Maintenance:** `healthcheck` when something feels broken, `refresh context`
to re-sync from wired capabilities.

Then ask: "Want to dive into something specific, run a full context refresh,
or explore on your own?"
