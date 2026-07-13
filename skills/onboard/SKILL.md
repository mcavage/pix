---
name: onboard
description: Guided onboarding that seeds identity and context into memory, probes connected data sources, and gets the agent doing useful work. Use for "set me up", "first time setup", "onboard me", or after a fresh install.
---
# onboard

Walk a new user from "system starts" to "system is useful." Identity and
context live in the memory DB, not flat files. Run this after the install
completes and environment checks pass.

## Step 1: Verify the environment

Check that the model providers are reachable before asking the user anything.
Provider keys are injected by the sbx proxy, so probe the models rather than
looking for env vars (they aren't present inside the VM).

```bash
pi -p 'reply with: ok' 2>&1 | tail -1   # if this answers, the active model + key work
```

If that errors with "No API key", the user needs to set provider secrets on the
host (`sbx secret set -g anthropic`, etc. — see the README). Run `healthcheck`
for a full infrastructure check if multiple things look broken.

## Step 2: Seed identity into memory

Ask these questions one at a time. Store each answer as a separate memory
entry tagged `soul` and `bootstrap`.

1. "What's your name and title?"
2. "What's your role? What do you own?"
3. "How do you like information presented? (bullets vs prose, level of detail)"
4. "What annoys you about AI assistants?"
5. "Preferred tone? (formal/casual, direct/diplomatic)"
6. "Two or three core values you want me to operate by?"

For each answer, write a memory entry with the full text and tag it
`["soul", "bootstrap"]`. Confirm by saying: "Identity stored. Future
sessions will recall this automatically."

## Step 3: Probe which capabilities resolve

This step names **capabilities**, not vendors. For each one below, resolve it
through `capability-routing` (which reads `capabilities.json` and either pulls
from the wired provider(s) or reports `none`). For each:

- Resolve the capability and call the cheapest read-only tool (a list, a count,
  a status).
- Report what came back.
- If it resolves to `none` or the call fails, state plainly "no [capability]
  wired" and note the fallback.

**Fallbacks when a capability resolves to `none`:**

| Capability | Fallback |
|--------|----------|
| github | Read repos/PRs directly; `gh` is almost always available |
| gworkspace | Ask the user for upcoming deadlines; store in memory |
| chat | Skip; no fallback |
| docs | Skip; fall back to web search |
| meeting-notes | Ask the user to paste recent notes; store in memory |
| calls | Ask the user to paste recent call summaries; store in memory |

When you probe **calls**, fan out across every wired call source and merge.
Never pretend a capability is wired when it is not.

## Step 4: Seed initial context

**With capabilities wired:** Run `refresh context` (or the equivalent
sweep skill in your overlay). It pulls from the wired providers and writes
findings to memory with appropriate tags. Takes 15-30 minutes.

**With everything `none` (degraded path):** Interview the user:

- "Who are your direct reports?" (store tagged `people`)
- "Top three to five current priorities?" (store tagged `priorities`)
- "Key external accounts or customers you own?" (store tagged `accounts`)
- "Upcoming meetings or deadlines you care about?" (store tagged `calendar`)

Tell the user: "You can re-run this later with live data sources connected
to fill gaps automatically."

## Step 5: Run a first useful task

Pick the most natural task given what the user said. Good options:

- `standup` if they mentioned recent meetings or inbox overload.
- `brainstorm` or `build` if they mentioned a project they are driving.
- `competitive` or `one-pager` if they mentioned a pitch or decision.

Running something real confirms the system is working and gives the user
immediate value.

## Step 6: Orient the user

Give a brief overview of the most useful daily patterns:

**Daily:** `standup`, `prep for my meeting with [person]`, `debrief [meeting]`

**On demand:** `brainstorm`, `build`, `code-review`, `ship`, `verify`,
`draft-email`, `one-pager`, `challenge`

**Maintenance:** `healthcheck` when something feels broken, `refresh context`
to re-sync from the wired capabilities.

Then ask: "Want to dive into something specific, run a full context refresh,
or just explore on your own?"
