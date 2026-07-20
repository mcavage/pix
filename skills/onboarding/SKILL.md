---
name: onboarding
description: "First-run onboarding. Read the host-state truth file, then EITHER a guided walkthrough (when invoked by `pi-stack setup`) that teaches the flow + co-builds a pack artifact, OR a quick task-first start (for 'onboard me'). Use on first run, 'onboard me', 'set me up', or after a fresh install."
---
# onboarding

You are ALREADY in the user's session. Never dump a form, never lecture, never
just print a status summary and stop. Read the truth file, then run in one of two
modes.

## Step 0: Read the truth file (never guess host state)

The host writes `<workspace>/.pi-stack/host-state.json`. READ IT first; you are
network-fenced and cannot see host config any other way. State ONLY what it says.

```bash
cat .pi-stack/host-state.json 2>/dev/null
```

Fields: `provisioned`, `keys`, `memory`, `knowledge`, `gog`, `mcp`, `overlay`,
`models`, `pack` (`active`/`path`/`git_initialized`/`skills`/`knowledge`). If the
file is absent, treat host config as unknown (don't invent it).

## Step 0.5: Pick the mode

- **GUIDED** — the invoking message says "guided" / "full walkthrough" / came
  from `pi-stack setup`. Do the full walkthrough (Steps G1-G6).
- **QUICK** — "onboard me", or anything else. Do the minimal task-first flow
  (Steps Q1-Q3).

There is no "setup wizard" to skip; never say there is. Keys or the host looking
configured is NOT a reason to stop — you still land a real task.

---

## QUICK mode (task-first, minimal)

### Q1: Land a real task FIRST
Derive identity silently (git/gh), pick the obvious verb, and ACTUALLY RUN IT
(don't just describe the repo): a diff -> `code-review`; something broken ->
`debug`; else -> `healthcheck`. Running a real verb and showing its result IS the
aha; summarizing the directory is not.

### Q2: Capture identity passively
Name/email come from git/gh — don't ask. Let the watcher learn tone from real
work. If a real preference surfaces mid-task, `/remember` it in one line. Only if
they ask to set up how you work, ask ONE short batched round, confirm, `/remember`.

### Q3: One context-picked track (optional), then work
If capture-worthy docs exist and no KB is seeded, offer to seed ONE via `enrich`.
If a repeatable gap surfaced, offer to author a skill (see G5). Otherwise just
keep working.

---

## GUIDED mode (the full walkthrough — this is `pi-stack setup`)

Pace it: one concept at a time, each tied to something real that just happened.
Keep each beat to a few lines. The goal is that by the end they GET the model and
their pack has at least one real artifact in it.

### G1: Orient from the truth file (facts, one breath)
Say what's wired, from host-state — e.g. "Keys: resolved via 1Password. Memory:
up. Your pack: {pack.path}." State host mode ready ONLY when `host.ready` is true
(that means enabled AND actually provisioned); if it's false, don't call it
ready. Never claim anything the file doesn't say. Do not re-ask any of it.

### G2: Land a real task (the aha, BEFORE teaching)
Pick + RUN a real verb on this repo (`healthcheck` is the safe default). Show the
result. This proves the system does work, not just talks.

### G3: Teach the flow by doing (THREE core, a few lines each)
Keep it tight — attention is finite. Teach exactly three, each tied to what just
happened:
- **Memory:** it remembers durable facts across every session. Demo: state one
  fact, act on it next turn. "You correct it by acting differently, not editing."
- **Skills:** what you just ran was a *skill* (a named, repeatable way of
  working); the always-on ones (anti-slop, verify) shaped the output already.
  Invoke by intent, not a command list.
- **Packs:** your pack ({pack.path}) is the git-backed home for the skills and
  knowledge you author — portable across machines, versioned like code.

Then offer, don't lecture: "There's also the crew (multiple models, `agent ls`)
and knowledge bundles (shared domain truth vs personal memory) — ask and I'll
show you." Only go deeper if they bite.

### G4: Co-author ONE real artifact into the pack
Find a genuine, repeatable task the user does (ask them, one question). Draft a
real SKILL.md for it and SHOW it. The `pi-stack` launcher runs on the HOST, not
this sandbox, so the durable save is host-side. Give the user the exact steps:
1. On your host: `pi-stack pack add skill <name> {pack.path}` (pass the pack path
   from host-state so it lands in the ACTIVE pack, not always the personal one;
   it PRINTS the file path — it does not open an editor).
2. Save my draft (above) into that printed path.
3. `git -C {pack.path} add -A && git commit -m "add <name> skill"` (packs live in
   git; push to your own remote when you want cross-machine sync).
A KB concept via `enrich` works the same way. End with a real drafted artifact
the user can land, not a vague description.

### G5: Capture identity to memory
Ask ONE short batched round (style, pet peeves, tone, 2-3 values), show exactly
what you'll save, confirm, then `/remember` each tagged `["soul","bootstrap"]`.

### G6: Close — land on real work
One-line receipt of what got written (memory facts, the drafted pack artifact).
If (and only if) `host.ready` is true in the truth file, note that **host mode**
(`pi-stack host`) is also ready for work the sandbox can't do; otherwise don't
mention it. Then pivot straight into a real task.

## Host config (either mode)
You are fenced; you cannot apply host config live. If the user asks for something
host-side that the truth file shows is NOT set, PROPOSE it by writing
`<workspace>/.pi-stack/onboarding.json` (only the fields they chose), and say
`pi-stack run` will apply it under a gate next session. Never include secrets
(keys are 1Password op:// refs the host owns). Never propose something already on.
Never demo MCP/proxies that need creds you can see aren't wired.
