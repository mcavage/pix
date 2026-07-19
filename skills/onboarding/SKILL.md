---
name: onboarding
description: "First-run agentic onboarding. Probe the environment, ask one batched question, confirm, seed identity into memory, propose host-config changes, and land on a real first task. Use on first run, for 'onboard me', 'set me up', or after a fresh install."
---
# onboarding

This is the contiguous first-run experience. You are ALREADY in the user's
session; do NOT send them to a separate wizard. Onboard them conversationally,
then land them on a real first task. Two beats matter: seed identity into
MEMORY (applies immediately), and propose host-config changes as a declarative
file the host applies under a confirm gate on the next run.

Keep it tight: probe deterministically, ask ONE consolidated question, show
exactly what you will write, and only write on confirmation. No 6-step
ping-pong.

## Step 0: The offer (only if not already accepted)

If the session opened with a first-run offer and the user has not yet answered,
ask once, plainly:

> First time I'm running here. Want two minutes to set up how I work with you
> before we start? [Y/n]

If they decline: "Skipped. Say 'onboard me' any time. Ready when you are." Then
STOP and let them drive. Never force it.

## Step 1: Probe the environment

Run this in a single bash block before asking anything:

```bash
echo "name:     $(git config user.name 2>/dev/null)"
echo "email:    $(git config user.email 2>/dev/null)"
echo "gh_login: $(gh api user --jq '.login' 2>/dev/null)"
echo "gh_name:  $(gh api user --jq '.name' 2>/dev/null)"
echo "project:  $(basename "$(git rev-parse --show-toplevel 2>/dev/null || pwd)")"
echo "date:     $(date '+%Y-%m-%d')"
```

Name and email that agree across git and gh are confident; skip asking about
them.

## Step 2: One batched ask

Send ONE message covering only what the environment could not answer and what
genuinely changes how you behave. Skip anything the probe already answered.

- **Name** (only if unresolved): how you address them and sign off.
- **Communication style** (bullets vs prose, brief vs thorough).
- **AI pet peeves** (what drives them crazy about AI assistants): avoid those
  from now on.
- **Preferred tone** (casual vs formal, blunt vs diplomatic).
- **2-3 values to operate by**: guides tradeoff calls in every session.

Do NOT ask for title, role, or org chart. Infer context from the work.

## Step 3: Show the full picture; confirm

Before writing anything, display a plain summary:

```
Here's what I'll remember about you:

  Name:   [value]
  Style:  [answer]
  Peeves: [answer]
  Tone:   [answer]
  Values: [answer]

Anything wrong or missing? I'll fix it before writing.
```

Wait. Apply corrections, show the updated list once if anything changed, and do
not write until they say go.

## Step 4: Write identity to memory (data plane, immediate)

Write each fact as a separate `/remember` entry tagged `["soul", "bootstrap"]`.
This applies to THIS session and every future one. Confirm once: "Identity
stored. Every future session recalls this automatically."

## Step 5: Offer host-config setup (control plane, next run)

Only if it comes up naturally. These change HOST config, so you cannot apply
them live from inside the sandbox; you PROPOSE them and the host applies them on
the next `pi-stack run` under a confirm gate. Ask only about what the user
reaches for; do not dump a form.

- Google Workspace (gmail/calendar/drive): if they want it, capture the account
  email.
- A knowledge base: if they have a shared OKF bundle (path or git URL) or want
  one scaffolded.
- The local ollama model for the in-sandbox bridge, if they have a preference.

If they proposed any, write `<repo>/.pi-stack/onboarding.json` (create the
`.pi-stack/` dir if needed). ONLY include fields they actually chose. Schema:

```json
{
  "version": 1,
  "gog_account": "you@example.com",
  "mcp": ["gog"],
  "knowledge": {"action": "use", "source": "/path/or/git-url"},
  "ollama_bridge_model": "qwen3.5:9b"
}
```

Then tell them: "I've noted the host-side changes. `pi-stack run` will show them
and ask before applying them next session." Never put secrets in this file
(keys are sbx secrets; integration creds are 1Password refs).

If they proposed nothing host-side, skip this step and write no file.

## Step 6: Run a first useful task

Pick the most natural task from what they said and run it now:

- `brainstorm` or `build` if they mentioned an active project
- `healthcheck` or `code-review` if they landed in a repo
- `one-pager` or `challenge` if they mentioned a pending decision

Running something real confirms the system works and delivers immediate value.
End inside a working session, not on a "setup complete" banner.

## Step 7: Orient (brief)

Bold-label lines, not a table (terminal may not render tables):

**On demand:** `brainstorm`, `build`, `code-review`, `ship`, `verify`,
`debug`, `plan`, `one-pager`, `challenge`

**Maintenance:** `healthcheck` when something feels off; say "onboard me" to
revisit any of this.

Then: "Want to dive into something specific, or explore on your own?"
