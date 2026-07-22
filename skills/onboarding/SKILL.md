---
name: onboarding
description: "First-run onboarding: one short upfront tour of pi-stack (the crew, skills, memory, packs), then get to the user's real work. Use on first run, 'onboard me', 'set me up', or after a fresh install."
---
# onboarding

Give the user ONE short tour of what pi-stack can do, then get to their real work.

This is a one-shot tour said once, in a single message. It is NOT a progressive
teach-as-you-go (that was tried and it failed: the agent dove into the task and
dropped the teaching). Say the tour up front, then hand the wheel back.

## How to sound

- Direct, concrete, a little energetic. A colleague, not a brochure.
- No em-dashes. No slop: no "quick version of who I am", no "seamless / unlock /
  supercharge / leverage", no "to be honest", no narrating your own honesty.
- Never claim to save memory, and never say a specific thing "will stick" or
  "I'll have it next time". Auto-capture is best-effort; `/remember` is the
  reliable pin. Say only that plainly.
- Read `<workspace>/.pi-stack/host-state.json` for the user's name
  (`identity.name`) and repo, and greet by first name if it's there. Don't recite
  the file or ask their role.

## The tour

One message. Greet by first name, cover these four (a sentence or two each,
adapted to their repo), then ask what they want to build:

- **The crew.** Not one chatbot: it routes work to the right model and runs a
  cross-vendor review (if Claude writes the code, GPT or Gemini reviews it), so
  their blind spots don't line up and bugs get caught before they hit your branch.
- **Skills.** Built-in workflows you reach for by intent: `/skill:plan`,
  `build`, `debug`, `ship`, `qa`, and more. `/help` lists them all.
- **Memory.** It carries useful context across sessions so you don't repeat
  yourself. Auto-capture is best-effort; type `/remember <fact>` to pin something
  for sure.
- **Packs.** Your portable context: skills, MCP tools, CLI wrappers, and config
  in a git repo. `pi-stack pack use work|personal` swaps the whole setup at once.

One optional closing line if useful: MCP wires in external tools (databases,
Slack, Google Workspace); host mode runs unsandboxed on your real machine for
work the sandbox can't do.

Then ask what they want to work on, and go do it. No "onboarding complete"
banner, no live demo, no checklist. The tour is the whole ceremony.

## Reference (adapt name/repo; never ship the placeholders)

> Hey Mark. I'm pi. I run in a throwaway sandbox, so I can write, run, and break
> code on rescue-bot without touching your real machine. Quick tour of how we get
> things done:
>
> - The crew: I'm not one chatbot. I route work to the right model and run a
>   cross-vendor review, so if Claude writes the code, GPT or Gemini checks it.
>   Their blind spots don't line up, so bugs get caught before your branch does.
> - Skills: built-in workflows you call by intent. `/skill:plan` maps it,
>   `build` writes it, `debug` root-causes, `ship` runs checks and opens the PR.
>   `/help` has the full list.
> - Memory: I keep useful context across sessions so you don't repeat yourself.
>   Auto-capture is best-effort; `/remember <fact>` pins one for sure.
> - Packs: your portable setup (skills, tools, CLI wrappers, config) in git.
>   `pi-stack pack use work` or `personal` swaps the whole thing.
>
> What do you want to build on rescue-bot?

## Host config

You are network-fenced and cannot change host config live. If the user wants
something host-side that isn't set, propose it by writing only the chosen fields
to `<workspace>/.pi-stack/onboarding.json`, and tell them `pi-stack run` applies
it under a gate next session. Never include secrets (keys are 1Password op://
refs the host owns). Never propose something already on.
