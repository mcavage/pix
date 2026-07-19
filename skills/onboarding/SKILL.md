---
name: onboarding
description: "First-run onboarding. Read the host-state truth file, land the user in a real task FIRST, capture identity passively, and offer at most ONE context-picked track. Use on first run, for 'onboard me', 'set me up', or after a fresh install."
---
# onboarding

You are ALREADY in the user's session. Do NOT send them to a wizard, do NOT dump
a form, do NOT lecture. The bar is the Stripe bar: get them to a real result
FAST, then surface at most one useful thing. Task before teaching, always.

## Step 0: Read the truth file (never guess host state)

The host writes `<workspace>/.pi-stack/host-state.json`. READ IT before saying
anything about the environment. You are network-fenced and CANNOT see host
config any other way, so state ONLY what this file says. Never claim "no
knowledge base" or "MCP unavailable" from a guess.

```bash
cat .pi-stack/host-state.json 2>/dev/null
```

It carries: `provisioned`, `keys` (resolved + which), `memory`, `knowledge`
(bundles + seeded), `gog`, `mcp`, `overlay`, `models`. If the file is absent,
fall back to the probe in Step 2 and treat host config as unknown (don't invent
it).

## Step 1: Provisioned? Short-circuit.

If `provisioned` is true (keys resolved, a knowledge bundle already seeded, an
overlay kit stacked), the environment was set up for them. Do NOT onboard. One
line, then straight to work:

> You're set up (keys, knowledge, and the {overlay.kit} kit are wired). What do
> you want to work on? I can start with a healthcheck.

Then STOP the onboarding flow and do the task. Never re-ask settled setup.

## Step 2: Land in a real task FIRST (the aha)

Not provisioned: still lead with a real result, not questions. Derive identity
silently and pick a task from context:

```bash
echo "name:  $(git config user.name 2>/dev/null)"
echo "email: $(git config user.email 2>/dev/null)"
echo "gh:    $(gh api user --jq '.login' 2>/dev/null)"
echo "proj:  $(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)")"
```

Pick the obvious verb and run it now:
- a diff present -> `code-review`
- something broken / an error mentioned -> `debug`
- otherwise -> `healthcheck` on this repo (the safe default)

Do this BEFORE any identity questions or teaching. The fast real output is the
point.

## Step 3: Capture identity passively; ask at most one thing inline

Name/email come from git/gh (Step 2) — don't ask for them. Tone and preferences
are learned by the memory watcher from how they actually talk to you; you do not
need a values interview. If, and only if, a real preference surfaces during the
task (they snap at hedging, they want bullets), capture it with `/remember`
tagged `["soul","bootstrap"]` in one line: "Noted, I'll keep that." No form, no
"tell me your 2-3 values".

If they explicitly ask to set up how you work, then ask ONE short batched round
(style, pet peeves, tone), show what you'll save, confirm, `/remember`. Only on
request.

## Step 4: One context-picked track (after the aha, optional)

Offer AT MOST ONE follow-up, chosen from context — never a menu of options:

- **Knowledge**: if `knowledge.seeded` is false AND the repo has capture-worthy
  docs (design docs, ADRs, a real README), propose 3-5 specific candidates by
  name and offer to seed one via `enrich` (one confirm). If already seeded, say
  nothing.
- **Custom skill**: only if a repeatable task/gap actually surfaced. Draft it,
  show the file, save on confirm. Default location: the personal skills dir
  `~/.local/share/pi-stack/skills/<name>/SKILL.md` (loads in every sandbox); use
  a repo-local `skills/` path instead when it should be versioned with the
  project.

Confirm before writing anything. Skip both if neither is real.

## Step 5: Autonomy — default, don't teach

Default is `review` (you plan/spec before building). Do NOT run a
"pick your autonomy mode" step. The first time they say "just build it" / "don't
stop", flip to `deliver` for that work and mention once that "just build it" and
"spec it first" steer this any time.

## Step 6: Host config — only if they reach for it, never re-ask what's set

Do NOT proactively pitch Google Workspace / knowledge / models. You are fenced;
you cannot apply host config live. If the user asks for something host-side that
`host-state.json` shows is NOT already set, PROPOSE it by writing
`<workspace>/.pi-stack/onboarding.json` with ONLY the fields they chose:

```json
{
  "version": 1,
  "gog_account": "you@example.com",
  "mcp": ["gog"],
  "knowledge": {"action": "use", "source": "/path/or/git-url"}
}
```

Then: "Noted. `pi-stack run` will show these and ask before applying them next
session." Never include secrets (keys are 1Password `op://` refs the host owns;
integration creds too). Never propose something the truth file shows already on.

## Step 7: MCP / credential proxies — deferred, default no

Never demo MCP in first-run (it needs host creds and will error otherwise). At
most, once, offer the explanation and default to no:

> Want the 90-second version of how credentials reach tools without the sandbox
> ever seeing a token? [y/N]

If yes, 3-5 flat lines, end with "nothing to set up now; ask me to wire Google
Workspace or run `pi-stack doctor` when you're ready." Then drop it.

## Closing

One-line receipt of anything written (memory facts / a seeded concept / a
skill), then keep working. No "setup complete" banner.
