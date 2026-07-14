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
them to Anthropic, OpenAI, and Google directly; the sandbox only ever sees the
responses. `gh` rides the same proxy: it injects your GitHub token, so a `gh` call
leaves the VM with no credential in it. The other data tools run entirely on the
host. Google Workspace is the external `gog` CLI, and Slack is a `pi-stack-host`
subcommand; both run as MCP servers that the sbx gateway spawns on the host, so the
sandbox reaches them through the gateway and never holds a token. Their secrets live
in 1Password: the registered command is `op run --env-file=config/op-refs.env`, so
`op` resolves the `op://` references the moment the gateway spawns the server. The
memory service is a plain host process the sandbox reaches over
`host.docker.internal`. Nothing lands on disk in the VM, in a registration, or in
the sandbox.

**A memory that learns.** A host-side service (sqlite with FTS5 and vector search)
holds facts across sessions. A local model watches each message you send and pulls
out the durable stuff: preferences, decisions, conventions. Relevant memories get
injected back on later turns without you asking. This is the one piece that needs
[Ollama](https://ollama.com) running locally, a small watcher model for capture and
an embed model for semantic recall (`make pull-models` fetches both). Skip Ollama
and recall falls back to keyword search and capture turns off, loudly, so you know
it's off.

**Open core.** This repo is the generic stack: the dev, writing, and harness
skills, the role agents, the host binary, the memory loop. Anything
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
- **`gh`** brings its own auth (`gh auth`); no 1Password involved. Google
  Workspace is the host-side `gog` MCP server, run by the sbx gateway (see
  `make mcp-register`).

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

## The host launcher (no checkout needed)

The `sbx run` line above is enough for a plain agent. To get the data tools
without cloning the repo, install two host binaries: `pi-stack`, a launcher that
wraps `sbx run` and pins the kit to its own release
(`--kit "git+...#ref=v<version>&dir=pi-kit"`), and `pi-stack-host`, the host-side
service binary. One line installs both:

```bash
curl -fsSL https://raw.githubusercontent.com/mcavage/pi-stack/main/install.sh | sh
```

It detects your OS and arch, downloads both binaries plus `SHA256SUMS`, verifies
each checksum before it installs anything, and drops them in `~/.local/bin` (no
sudo, never touching an existing config). It prints a PATH hint if `~/.local/bin`
isn't on yours. Read it before you pipe it to a shell:
[install.sh](install.sh). To remove the binaries later,
`curl -fsSL .../install.sh | sh -s -- --uninstall`.

Then the flow is set your keys, configure, start services, launch:

```bash
sbx secret set -g anthropic      # keys live in sbx, proxy-injected, never in the VM
sbx secret set -g openai
sbx secret set -g google
sbx secret set -g github
pi-stack setup                   # wizard: writes config, registers gog, enables memory
pi-stack serve                   # host services: memory (:11435), knowledge (:11436 if on)
pi-stack                         # launch the sandbox on the pinned release
```

`pi-stack setup` is the whole onboarding: it detects which secrets are set,
prompts for your Google Workspace account (or takes `--account you@x.com` /
`--non-interactive`), writes `~/.config/pi-stack/config.toml`, registers the
`gog` MCP server, and ensures the `memory` service. It's idempotent, so re-run it
any time.

The launcher covers the whole host surface:

```bash
pi-stack                     # launch a sandbox on the pinned release (== pi-stack run)
pi-stack run [DIR]           # same, explicit; the kit is pinned to this build's version
pi-stack setup               # guided setup: writes config + registers gog
pi-stack serve               # run the host services (memory, knowledge)
pi-stack doctor              # verdict + per-check TODOs (copy-paste; mix of pi-stack/sbx/ollama/gog)
pi-stack config show|path    # show the resolved config and its path
pi-stack config set|unset    # change config without hand-editing the toml
pi-stack mcp register|ls     # register / list local stdio MCP servers with sbx
pi-stack version             # print the launcher version
```

**You never hand-edit `config.toml`.** `pi-stack setup` and `pi-stack config
set/unset` are its only writers (they rewrite the file, so hand edits and
comments are lost on the next save). To enable gog after the fact, run
`pi-stack config set gog_account you@x.com` then `pi-stack config set mcp gog`,
not an editor. `pi-stack doctor` prints copy-pasteable fix commands (a mix of
`pi-stack`, `sbx`, `ollama`, and `gog`); its config fixes are always a
`pi-stack config set`, never "edit the toml".

Inside the sandbox the agent has `/help` (a live map of the loaded skills,
agents, and capabilities, so it never goes stale) and `/getting-started` (a
first-run tour).

Host services are overridable plugins. `pi-stack serve` reads
`~/.config/pi-stack/config.toml` and, for each service slot it finds under
`[plugins.*]`, launches the plugin binary named there instead of the built-in.
The service slots `serve` runs are `memory` (the recall backend), `knowledge`
(the OKF retrieval index), and `broker` (an overlay-only credential broker,
dormant by default). A fourth slot, `mcp`, overrides the MCP servers the sbx
gateway spawns rather than a `serve` process. Each entry names an `impl`, a
`path` to the plugin binary, and a `sha` it must match, so a company can swap in
a private broker or memory backend without touching this repo (see
[docs/OVERLAY.md](docs/OVERLAY.md)).

## What's in it

The skills I reach for (in `skills/`):

- `plan` takes an idea to an eng-ready spec with the crew (PM, design, arch, review).
- `build` compiles that spec into stories and executes them with the crew.
- `ship` runs tests, code-reviews the diff, and opens a PR with `gh`.
- `code-review` reviews the diff, then has a different vendor argue against it.
- `debug` finds the root cause before touching code.
- `qa` and `design-review` drive a headless browser against a running app.

Those are the highlights. The public image bakes a set of generic dev, writing,
and harness skills plus role agents (`architect`, `security-lead`, `sre-lead`,
`qa-lead`, and so on) you delegate to for the lens a change actually needs, not
just a generic reviewer. The exact set of each is the allowlist in `.dockerignore`.

Plus `gh`, `gog`, a
browser, plan mode, MCP, and web search. The defaults are mine: dracula, emacs
keys, thinking collapsed, a status line, and a watchdog that cancels a stuck call
instead of spinning on "working..." forever. They're defaults, so swap them.

## Data tools (optional)

Beyond the model keys, pi-stack can reach external data through a set of optional
tools. They're independent, so set up the ones you want and skip the rest. Because
skills ask for a capability and not a vendor (see `capabilities.json` and the
`capability-routing` skill), nothing breaks when a tool is absent: the capability
resolves to nothing and the skill degrades to web and files.

Credentials never enter the sandbox. `gh`'s token is injected by the sbx proxy;
the MCP servers (gog, slack) authenticate on the host and are spawned there by
the sbx gateway, so nothing lands in the VM. The launcher runs the host services
and reports status:

> **Note:** the sbx MCP gateway is currently Docker-internal and not yet publicly
> released. `--mcp`, `pi-stack mcp register`, and therefore the `gog` Google
> Workspace path all depend on it, so today they work only where the gateway is
> available. External users get **memory** and **gh** now, and Google Workspace
> (plus Slack) once sbx MCP ships.

```bash
pi-stack serve            # host services: memory (:11435), knowledge (:11436 if configured)
pi-stack mcp register     # register local stdio MCP servers (gog, slack) with the sbx gateway
pi-stack doctor           # per tool: set up? service running? models pulled?
```

**memory** is on by default. It needs a local [Ollama](https://ollama.com) for
the watcher and embed models (`ollama pull gemma4` + `ollama pull
nomic-embed-text`, the defaults); without them recall falls back to keyword
search and capture turns off, loudly.

**Google Workspace** is the `gog` host MCP server (read-only). Authorize it once
on the host (`gog auth login`), point pi-stack at your account, and register it:

```bash
pi-stack config set gog_account you@example.com   # or run pi-stack setup
pi-stack config set mcp gog
pi-stack mcp register
```

Full walkthrough, including the headless-keyring gotcha, is in
[docs/gog-setup.md](docs/gog-setup.md). **gh** brings its own auth: the sbx proxy
injects the token, so `gh auth token | sbx secret set -g github` is all it takes.

Registering a stdio MCP server does not put it in a sandbox. Local stdio servers
aren't surfaced by dynamic `mcp-find`, and there's no attach-to-running, so a
server in your config (`gog`, `slack`) is attached when `pi-stack run` starts the
sandbox.

| tool | capability | one-time setup | reaches the VM via |
| --- | --- | --- | --- |
| **gh** | `github` | `gh auth token \| sbx secret set -g github` | sbx proxy injects the token |
| **gog** | `gworkspace` | `gog auth login`, then `pi-stack config set gog_account …` + `pi-stack mcp register` | host `gog` MCP server via the sbx gateway (read-only) |
| **slack** | `chat` | refs in `config/op-refs.env`, then `pi-stack mcp register` | stdio MCP via the sbx gateway; `op run` pulls creds from 1Password |
| **memory** | semantic recall | a local Ollama with a watcher + embed model; without them, recall is keyword-only and capture is skipped, loudly | host service (`:11435`) |
| gateway catalog (atlassian, notion, granola, linear) | `issues`, `docs`, ... | register with `sbx mcp add` | the sbx gateway; `pi-stack run --mcp <name>` to eager-load |

From a clone, the same steps are `make serve`, `make mcp-register`,
`make pull-models`, and `make doctor` (they read `config/local.mk`).
Company-specific connectors (a warehouse proxy, an HR directory, a CRM) are not
in this repo. They live in a private overlay (next section).

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

This is the secondary path, for changing the image, the baked-in skills, or the
extensions. Everyone else takes the `install.sh` path above.

```bash
git clone https://github.com/mcavage/pi-stack
cd pi-stack
docker login dhi.io   # the base image is dhi.io/node; needs a DHI-entitled Docker account
make load             # build the image, load it into sbx
make install          # build + put both Go binaries (pi-stack, pi-stack-host) on your PATH
pi-stack              # run it anywhere (keys set as above)
```

Run `make load` after changing the Dockerfile or an extension, then **recreate
the sandbox** (`sbx rm -f <name> && make run`) to pick it up: a running sandbox
keeps its creation-time image. **Skills, you don't
rebuild for:** `make run` (and `pi-stack run --dev`) load skills live from your repo, so
edit a `SKILL.md`, `/reload` in pi, and it's live. `make load` only bakes your skills
into the image for people who run it the turnkey way (`sbx run --kit git+…`), which
uses the baked set. If you only changed the kit in `pi-kit/`, a fresh `make run` is
enough. `make publish`
pushes the image to Docker Hub by hand. A GitHub Action publishes automatically on
every push to `main`: it stamps a new version `0.0.<run_number>`, builds multi-arch,
pushes `:<version>` + `:latest`, and commits the version bump back into
`pi-kit/spec.yaml`. Because every push is a brand-new tag, `sbx run pi-stack --kit
git+…` (which reads the pinned image from `spec.yaml` on `main`) always pulls a fresh
image sbx has never cached, so there is no `--template` and no `sbx template rm`. To run the latest
published build without a local checkout, `make run-published`. The base image is a
Docker Hardened Image, so building from source needs a DHI-entitled account; the
`sbx run` path above does not.

## For agents

If you are an agent working in this repo, read [AGENTS.md](AGENTS.md): the layout,
the build and run loop, how to write skills and extensions, and the mistakes not to
repeat.
