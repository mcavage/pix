# pi-stack

A coding agent that runs full-auto, with no "allow this command?" prompts, ever.
The loop below is one task: fix the bug, run the tests, get a different model to
argue against the diff, open a PR. Nothing approved by hand.

<!--
DEMO: drop a short (8-15s, sped up) terminal recording at docs/pi-stack-demo.gif.
Record it in a real pi-stack sandbox running `ship` end to end, zero prompts:
  asciinema rec /tmp/demo.cast        # then drive the task to a PR, Ctrl-D to stop
  agg --speed 3 /tmp/demo.cast docs/pi-stack-demo.gif   # asciinema -> gif (brew install agg)
Trim to the good part. This is the whole pitch; it should show, not tell.
![pi-stack running ship full-auto: tests, a cross-vendor review, and a PR, with zero prompts](docs/pi-stack-demo.gif)
-->

It works because pi lives in a throwaway Docker
[sbx](https://docs.docker.com/ai/sandboxes/) sandbox that can't reach your host
unless you let it. The VM is disposable and isolated, so there is nothing to
approve and nothing the agent can break that you can't throw away.

This is my actual setup, not a demo:
[pi](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent) running
four inference providers at once, the skills I use to ship, a memory that learns
across sessions, and a clean split between the generic stack (this repo) and the
private, company-specific parts (a separate overlay).

## How it works

Five ideas, and they compose.

**The sandbox is the safety boundary.** pi runs inside an sbx VM. The VM is
disposable and its network is locked to an allowlist, so a bad command can't touch
your machine, your keys, or anything you didn't explicitly wire in. That is why it
runs full-auto: approval prompts exist to protect the host, and here there is no
host to protect. Throw the VM away and start another.

**Four inference providers.** Claude, GPT, and Gemini in the cloud, plus Ollama
running locally on your machine (no key, no cloud). `/model` switches, `Alt+P`
cycles, and subagents pick whichever fits, a cheap local model for breadth, a
frontier one for the hard part. The review step is the point. It runs a second
opinion on a *different* vendor than wrote the code, so a Claude diff gets argued
against by GPT or Gemini, not by another Claude. One model grading its own homework
is worth less. (Ollama also powers the memory loop below.)

**Your keys never enter the VM.** sbx stores your provider keys and its proxy hands
them to Anthropic and OpenAI directly; the sandbox only ever sees the responses.
Data tools work the same way through a small host-side Go binary (`pi-stack-host`):
it mints short-lived tokens and runs the real CLIs on the host, and the sandbox
reaches it over `host.docker.internal`. A `gh`, `gws`, or `snow` call leaves the VM
with no credential in it. The MCP servers go one step further: their secrets live
in 1Password, and the registered command is `op run --env-file=config/op-refs.env`,
so `op` resolves the `op://` references the moment the gateway spawns the server.
The token is never written to disk, never in the registration, never in the VM.

**A memory that learns.** A host-side service (sqlite with FTS5 and vector search)
holds facts across sessions. A local model watches each message you send and pulls
out the durable stuff: preferences, decisions, conventions. Relevant memories get
injected back on later turns without you asking. This is the one piece that needs
[Ollama](https://ollama.com) running locally, a small watcher model for capture and
an embed model for semantic recall (`make pull-models` fetches both). Skip Ollama
and recall falls back to keyword search and capture turns off, loudly, so you know
it's off.

**Open core.** This repo is the generic stack: ~35 dev, writing, and harness
skills, 17 role agents, the host binary, the memory loop. Anything
company-specific (proprietary skills, an internal `capabilities.json`, connectors
like a warehouse or an HR directory) lives in a private overlay you keep in your
own repo. Skills ask for a *capability* (`chat`, `docs`, `warehouse`), not a
vendor, so the same skill runs against your real provider at work and degrades to
web and files on a laptop. See [Extend it](#extend-it-skills-kits-and-a-private-overlay)
and [docs/OVERLAY.md](docs/OVERLAY.md).

## What you need

To run it, the `sbx run` path below: the [sbx CLI](https://docs.docker.com/ai/sandboxes/)
and the Docker Desktop it sits on, plus API keys for the three cloud providers,
Claude, GPT, and Gemini (I haven't tested subscriptions). That is the whole list
for a working agent.

Each data feature adds one dependency, and they're all optional:

- **Local models + memory**: a local [Ollama](https://ollama.com), the fourth
  provider. It serves the `ollama/*` models in the cycle (reached via the
  in-sandbox `ollama-bridge` extension) and runs the memory loop, a watcher model
  for capture and an embed model for recall (`make pull-models` fetches both).
  Without it, the `ollama/*` model is unavailable, recall falls back to keyword-only,
  and capture is off.
- **The credential-brokered MCP tools** (Slack, plus your overlay's connectors):
  the [1Password CLI](https://developer.1password.com/docs/cli/) (`op`) signed in,
  and a `config/op-refs.env` of `op://` references. `op run` pulls the real secrets
  at spawn, so nothing lands on disk.
- **`gh` and `gws`** bring their own auth (`gh auth`, `gws auth login`); no
  1Password involved.

Building the image from source (not the `sbx run` path) needs a DHI-entitled Docker
account, because the base is a Docker Hardened Image.

## Try it

```bash
sbx secret set -g anthropic
sbx secret set -g openai
sbx secret set -g google
sbx secret set -g github
sbx run pi-stack --kit "git+https://github.com/mcavage/pi-stack.git#dir=pi-kit"
```

That last line pulls the image and starts pi in the current directory. The keys
stay in sbx and never reach the VM.

## What's in it

The skills I reach for (in `skills/`):

- `ship` runs tests, then code-review, then opens a PR with `gh`.
- `code-review` reviews the diff, then has a different vendor argue against it.
- `investigate` finds the root cause before touching code.
- `spec` writes a short plan and builds against it.
- `qa` and `design-review` drive a headless browser against a running app.

Those are the highlights. The public image bakes ~35 generic dev, writing, and
harness skills (the exact set is the allowlist in `.dockerignore`) plus 17 role
agents (`architect`, `security-lead`, `sre-lead`, `qa-lead`, and so on) you
delegate to for the lens a change actually needs, not just a generic reviewer.

Plus `gh`, `gws`, a
browser, plan mode, MCP, and web search. The defaults are mine: dracula, emacs
keys, thinking collapsed, a status line, and a watchdog that cancels a stuck call
instead of spinning on "working..." forever. They're defaults, so swap them.

## Data tools (optional)

Beyond the model keys, pi-stack can reach external data through a set of optional
tools. They're independent, so set up the ones you want and skip the rest. Because
skills ask for a capability and not a vendor (see `capabilities.json` and the
`capability-routing` skill), nothing breaks when a tool is absent: the capability
resolves to nothing and the skill degrades to web and files.

Credentials never enter the sandbox. Tokens are injected by the sbx proxy or
brokered by the host-side service. One command starts the host services, another
shows status:

```bash
make serve         # host services: memory (:11435), gws-token (:11441)
make pull-models   # pull the Ollama models the memory loop needs (watcher + embed)
make mcp-register  # register stdio MCP servers (slack) with the sbx gateway
make doctor        # per tool: set up? service running? models pulled?
```

Registering a stdio MCP server does not put it in a sandbox. Local stdio servers
aren't surfaced by dynamic `mcp-find`, and there's no attach-to-running, so you
start the sandbox with them:

```bash
make run MCP="slack"   # == sbx run pi-stack --kit ./pi-kit --mcp slack .
```

| tool | capability | one-time setup | reaches the VM via |
| --- | --- | --- | --- |
| **gh** | `github` | `gh auth token \| sbx secret set -g github` | sbx proxy injects the token |
| **gws** | `gworkspace` | `gws auth login` on the host | host token service (`:11441`) |
| **slack** | `chat` | refs in `config/op-refs.env`, then `make mcp-register` | stdio MCP via the sbx gateway; `op run` pulls creds from 1Password |
| **memory** | semantic recall | `make pull-models` (a local Ollama with a watcher model for capture and an embed model for recall; without them, recall is keyword-only and capture is skipped, loudly) | host service (`:11435`) |
| gateway catalog (atlassian, notion, granola, linear) | `issues`, `docs`, ... | register with `sbx mcp add` | the sbx gateway; `make run MCP="<name>"` to eager-load |

Company-specific connectors (a warehouse proxy, an HR directory, a CRM) are not in
this repo. They live in a private overlay (next section).

## Extend it: skills, kits, and a private overlay

A skill is a `SKILL.md`: a name, a note on when to use it, the steps. Drop one in
`.pi/skills/` for a single project, or put a set in a mixin kit and pass a second
`--kit` so they ride along on every run. Kits stack:

```bash
sbx run pi-stack \
  --kit "git+https://github.com/mcavage/pi-stack.git#dir=pi-kit" \
  --kit ./my-kit
```

A mixin kit is a folder with a `spec.yaml` (`kind: mixin`) and a `files/` tree;
anything under `files/home/.pi/agent/skills/` lands in the skills directory,
and the same trick covers prompts, extensions, env, and network rules. Format is
in [Docker's kit docs](https://docs.docker.com/ai/sandboxes/customize/kits/).

The overlay is how the open-core split actually works, and it's the part most
"my AI setup" repos skip. Your private, company-specific surface lives in its own
peer repo (a sibling directory, kept private), not as hidden files in this one. It
has two halves: a mixin kit for the sandbox (private skills, the full
`capabilities.json`, in-sandbox wrappers) and `host/overlay_*.go` plugins for the
host binary (an extra exec proxy or MCP server). `make run` stacks the kit and
`make serve` builds in the host plugins, both automatically when the peer repo is
present, so nothing company-specific ever touches the public tree. A CI guard
fails the build if it does. The full guide is [docs/OVERLAY.md](docs/OVERLAY.md),
and there's a copyable scaffold in [`examples/overlay/`](examples/overlay).

## Build from source

To change the image, the baked-in skills, or the extensions:

```bash
git clone https://github.com/mcavage/pi-stack
cd pi-stack
docker login dhi.io   # the base image is dhi.io/node; needs a DHI-entitled Docker account
make load             # build the image, load it into sbx
make install          # put a `pi-stack` command on your PATH
pi-stack              # run it anywhere (keys set as above)
```

Run `make load` after changing the Dockerfile or an extension. **Skills, you don't
rebuild for:** `make run` (and `pi-stack --dev`) load skills live from your repo, so
edit a `SKILL.md`, `/reload` in pi, and it's live. `make load` only bakes your skills
into the image for people who run it the turnkey way (`sbx run --kit git+…`), which
uses the baked set. If you only changed the kit in `pi-kit/`, a fresh `make run` is
enough. `make publish`
pushes the image to Docker Hub by hand. A GitHub Action publishes automatically on
every push to `main`: it stamps a new version `0.0.<run_number>`, builds multi-arch,
pushes `:<version>` + `:latest`, and commits the version bump back into
`pi-kit/spec.yaml`. Because every push is a brand-new tag, `sbx run pi-stack --kit
git+…` (which reads the pinned image from `spec.yaml` on `main`) always pulls a fresh
image sbx has never cached — no `--template`, no `sbx template rm`. To run the latest
published build without a local checkout, `make run-published`. The base image is a
Docker Hardened Image, so building from source needs a DHI-entitled account; the
`sbx run` path above does not.

## For agents

If you are an agent working in this repo, read [AGENTS.md](AGENTS.md): the layout,
the build and run loop, how to write skills and extensions, and the mistakes not to
repeat.
