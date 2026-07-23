# pi-stack

[![test](https://github.com/mcavage/pi-stack/actions/workflows/test.yml/badge.svg)](https://github.com/mcavage/pi-stack/actions/workflows/test.yml)
[![publish](https://github.com/mcavage/pi-stack/actions/workflows/publish.yml/badge.svg)](https://github.com/mcavage/pi-stack/actions/workflows/publish.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**Claude writes it. GPT reviews it. Docker contains it.**

pi-stack is an opinionated, Docker-sandboxed distribution of the
[pi](https://www.npmjs.com/package/@earendil-works/pi-coding-agent) coding agent
for running autonomous coding tasks.

Give pi a repo and a task. It plans, edits, runs commands, tests the result, asks
a *different* model to review the diff, and opens a PR, without turning every
shell command into an approval prompt. The safety boundary is the sandbox, not a
stream of one-off confirmations.

> Building the image needs a DHI-entitled Docker account, but the hosted
> `sbx run` path below does not. A big chunk of the optional data integrations
> also depends on the sbx MCP gateway, which is not public yet. The core, and
> everything in the quickstart, works today.

pi-stack ships the reusable parts of that setup:

- a pinned Docker [sbx](https://docs.docker.com/ai/sandboxes/) kit for running pi
  inside a disposable, network-limited VM
- Claude, OpenAI, Gemini, and local Ollama model routing
- skills for planning, building, debugging, QA, review, and shipping
- cross-provider review, so the model that wrote a diff is not the only model
  judging it
- a host-side memory service with sqlite, FTS5, embeddings, and local capture
- an optional OKF knowledge service and CLI for indexed private corpora
- host-side data tools that keep credentials out of the sandbox
- an overlay model for private company integrations

## Status

The public path is usable today, but not every integration is public yet.

| feature | status |
| --- | --- |
| sandboxed pi coding agent | public |
| Anthropic, OpenAI, Google, and GitHub credentials via sbx secrets | public |
| local Ollama models | public |
| memory service | public |
| OKF knowledge service | public, but you provide the bundles |
| `pi-stack` and `pi-stack-host` launchers | public |
| Google Workspace and Slack MCP | requires the sbx MCP gateway |
| building the image from source | requires a DHI-entitled Docker account |

If you only want the agent, use the `sbx run` quickstart. If you want memory,
knowledge, or host data tools, install the host launcher as well.

## Quickstart

Install Docker Desktop and the `sbx` CLI, then store provider keys once:

```bash
sbx secret set -g anthropic
sbx secret set -g openai
sbx secret set -g google
sbx secret set -g github
```

Start pi-stack in the current directory:

```bash
sbx run pi-stack --kit "git+https://github.com/mcavage/pi-stack.git#dir=pi-kit"
```

The sandbox receives model responses, not your provider keys. The VM is
disposable; recreate it when you want a clean environment.

<!--
DEMO: drop a short (8-15s, sped up) terminal recording at docs/pi-stack-demo.gif.
Record it in a real pi-stack sandbox running `ship` end to end:
  asciinema rec /tmp/demo.cast
  agg --speed 3 /tmp/demo.cast docs/pi-stack-demo.gif
![pi-stack running ship: tests, cross-provider review, and a PR](docs/pi-stack-demo.gif)
-->

## Install the Host Launcher

The raw `sbx run` command is enough for a plain sandboxed agent. The launcher adds
the host side: memory, knowledge, MCP registration, diagnostics, and a stable
version-pinned way to start sandboxes without cloning this repo.

```bash
curl -fsSL https://raw.githubusercontent.com/mcavage/pi-stack/main/install.sh | sh
```

The installer downloads `pi-stack` and `pi-stack-host`, verifies checksums, and
installs them into `~/.local/bin` without sudo. To inspect it first, read
[install.sh](install.sh). To remove the binaries:

```bash
curl -fsSL https://raw.githubusercontent.com/mcavage/pi-stack/main/install.sh | sh -s -- --uninstall
```

Typical first run:

```bash
pi-stack setup
```

Setup prefers a signed-in 1Password CLI: it validates references for
Anthropic, OpenAI, and Google, reconciles them into `sbx`, creates the default
pack, and launches one upfront onboarding tour. It stores references, never
resolved keys. If `sbx` already has all three keys and you'd rather not set up
1Password right now, setup can trust them instead: pass `--use-sbx-keys`, or
just answer the one-time prompt it offers when that's the case (`--use-1password`
is the mutually exclusive opposite — force the strict 1Password flow for one
run). Both flags are setup-only; `pi-stack onboard` rejects either one, since
onboard never provisions provider keys at all. Whichever source succeeds is
remembered as `provider_key_mode` in `config.toml`, so the *next* `pi-stack
setup` reuses that same choice with no prompt. After that, `pi-stack run`
launches or reattaches without replaying onboarding.

`pi-stack setup` is the opposite: it *actually sets you up* — provisions model
keys (preferring 1Password, wiring both the sandbox and host mode; or a
complete existing sbx key set via `--use-sbx-keys`/prompt/persisted mode),
creates your default pack, and provisions + enables host mode (when the host
can run `pi`), then hands off to a one-shot upfront guide that names the
exact workflows, explains memory and packs, reports grounded setup gaps, and
asks for your real task. Repeat it any time: the host phase reconciles
keys/config again; an existing sandbox is left alone (reattach with
`pi-stack run`, or recreate it with your current settings *and* get the tour
via `pi-stack setup --replace`). Skipping 1Password wires the sandbox fully
from sbx's existing keys, but host mode still needs `op://` refs in
`hostmode.env` for cloud models; without them it stays local/Ollama-only, and
that's an expected, honest result, not a failure. Setup's own copy reflects
which source actually ran: keys are only reported as "validated this run"
after the strict 1Password flow resolved them; a run that used existing sbx
keys says "configured (not verified this run)" instead — real validation
still happens at every `pi-stack host` launch. For
scripted/CI hosts, `pi-stack onboard --account … --knowledge … --yes` writes
`~/.config/pi-stack/config.toml` non-interactively (host config only, no handoff).

You don't babysit the services daemon: `pi-stack run` / `memory` / `knowledge
query` lazily auto-start a detached `pi-stack-host serve` when its ports are
down (opt out with `PI_STACK_NO_AUTOSERVE=1` or `pi-stack config set
host.autoserve false`; log at `~/.local/state/pi-stack/serve.log`). Prefer an
always-on login service? `pi-stack serve install` registers it with launchd
(macOS) or systemd --user (Linux); `pi-stack serve uninstall` removes it. The
managed service logs to the SAME `~/.local/state/pi-stack/serve.log` — one
log file regardless of how serve was started.

Bare `pi-stack` (no args) prints a status dashboard; it never launches a sandbox.
Use `pi-stack run [DIR]` to launch. This is deliberate: launching is always
explicit.

`pi-stack run` matches sbx's own lifecycle: if no sandbox by that name exists
yet, it creates one; if one already exists — running or stopped — it
RE-ATTACHES to it as-is instead of refusing or recreating (sbx reads the agent
from the sandbox's own spec, so `--kit`/`--mcp`/create-only flags don't apply on
a re-attach). Pass `--replace` to force a recreate (`sbx rm -f` then create)
when you've changed the kit, MCP servers, or another create-only flag.

## Why pi-stack?

pi-stack is a *distribution* of pi, not a new editor or a new agent runtime. It
exists to make one specific bet safe: let a model work autonomously, and let a
different model check it.

| | plain `pi` | pi-stack | Claude Code / Cursor |
| --- | --- | --- | --- |
| Isolation | your shell | disposable Docker VM | your shell / IDE |
| Approval model | per-command prompts | sandbox is the boundary, full-auto | per-command prompts |
| Providers | multi | Claude + GPT + Gemini + local Ollama | mostly single-vendor |
| Review | you | a *different* vendor reviews the diff | you |
| Memory | none | host sqlite + FTS5 + vectors, survives sessions | limited |
| Private integrations | none | overlay repo, credentials stay on the host | plugins |
| Reproducibility | your machine | pinned image + kit | your machine |

The cross-vendor review is the part that pays off in practice: a second Claude
pass on a Claude diff has correlated blind spots, but GPT or Gemini objects in
different places.

New here and want the whole capability map in one place (memory, skills, the
crew, packs, knowledge, host mode, MCP)? See the reference manual:
[docs/reference.md](docs/reference.md).

## What You Get

**Autonomous coding in a sandbox.** pi runs inside an sbx VM with a locked-down
network allowlist. The agent can run normal development commands without receiving
host credentials or direct host filesystem access beyond the mounted workspace.

**Multi-model routing.** Claude, OpenAI, and Gemini run through sbx-managed
credentials. Ollama runs locally for no-key local models and for the memory loop.
Use `/model` to switch and `Alt+P` to cycle.

**Cross-provider review.** The `code-review` and `ship` flows ask another provider
to challenge the diff. That matters in practice: a second Claude pass on a Claude
diff has correlated blind spots; GPT or Gemini will often object in different
places.

**Memory that survives sessions.** The host memory service stores durable facts in
sqlite with FTS5 and vector search. A local watcher model extracts preferences,
decisions, and project conventions from conversation, and later turns retrieve
relevant memories automatically. Without Ollama, recall falls back to keyword
search and capture is disabled. See [docs/memory.md](docs/memory.md) for the full
picture: recall ranking, the capture loop, the commands, and the trust model.

**OKF knowledge retrieval.** The built-in knowledge service indexes OKF bundle
directories and serves retrieval over JSON-RPC. `pi-stack knowledge init`
scaffolds a spec-correct bundle, `pi-stack knowledge use` points the service at
an existing bundle, and `pi-stack knowledge ls` reports config plus daemon health.
Public pi-stack ships the engine, not a corpus. Private teams can mount their own
bundles through config or an overlay.

**Host-side credentials.** GitHub uses sbx proxy injection. Google Workspace,
Slack, and overlay connectors run as host-side MCP servers spawned by the sbx
gateway, so the sandbox talks to a gateway instead of holding tokens. Slack-style
secrets can come from 1Password via `op run`.

**Skills and role agents.** The public image includes generic development,
writing, review, QA, and harness skills plus role presets like `architect`,
`security-lead`, `sre-lead`, and `qa-lead`. Inside the sandbox, `/help` shows the
live skill, agent, and capability map.

**Private overlays.** The public repo contains the reusable harness. Private
skills, capability routing, credentials, and company connectors live in a separate
overlay repo. That keeps the open-source tree clean while still letting the same
skills run against real work systems when an overlay is present.

**Parallel work with `task`.** `pi-stack task new` spins up an isolated clone plus
sandbox for a branch of work, so several agents can run at once without stepping
on each other. `task ls` shows them, `task harvest` pulls the results back, and
`task rm` cleans up (with guardrails). See
[docs/design/worktree-tasks.md](docs/design/worktree-tasks.md).

## Launcher Commands

Core:

```bash
pi-stack                     # status dashboard (does NOT launch)
pi-stack run [DIR]           # launch a sandbox in DIR (default: current dir); re-attaches if it exists
pi-stack run --replace        # recreate instead of re-attaching (picks up changed --kit/--mcp)
pi-stack ls                  # list your pi-stack sandboxes (name, state, dir)
pi-stack rm <name>           # remove a sandbox (--all [--except <name>])
pi-stack status              # fast read-only control panel (alias: st)
pi-stack onboard             # host-side/CI config (guided onboarding is in-session via `pi-stack setup`)
pi-stack serve               # run enabled host services (auto-started lazily; install/uninstall for a login service)
pi-stack doctor              # diagnose host and sandbox prerequisites
pi-stack config show|path|set|unset  # inspect or update config (never hand-edit toml)
pi-stack help [--all] [verb] # tiered help: Core by default, --all for the rest
pi-stack version             # print the launcher version
```

Data, routing, and parallel work:

```bash
pi-stack memory recall|remember|forget|learnings|stats   # drive the memory daemon (alias: mem)
pi-stack knowledge init|use|ls|query|sync|remote         # OKF bundles (alias: kb)
pi-stack mcp register|ls     # register/list local stdio MCP servers with sbx
pi-stack secret ls|set|rm|sync|check   # 1Password op-refs for model keys + host MCP creds
pi-stack route pick|compile|show|models   # the model router (cost/latency/accuracy)
pi-stack agent ls|new|edit|rm|reassess    # manage subagents and their resolved models
pi-stack task new|ls|harvest|rm|gc        # isolated parallel-work sandboxes (see below)
pi-stack pack new|add|use|ls # your git-backed context bundle (skills + knowledge)
pi-stack state backup|restore|reset|uninstall  # on-disk state (also top-level aliases)
pi-stack man                 # render the full man page
```

Run `pi-stack help --all` for the complete tree with flags.

The model router (`route`) and agent authoring are **maintainer tooling**, run
from a repo checkout (`make route`). Scores live in a hand-maintained
`scorecard.json` (seeded from published benchmarks and pricing, no eval harness
required); edit it, then `pi-stack route compile`. Consumers get the compiled
`routing.json` baked into the image.

Do not hand-edit `config.toml`. `pi-stack onboard` and `pi-stack config set/unset`
are the supported writers, and `pi-stack doctor` prints copy-pasteable repair
commands when something is missing.

### Expert: `pi-stack host` (unsandboxed, gated off)

`pi-stack host [DIR]` runs pi **directly on your machine** — no sandbox, no
network fence, real credentials. It exists for one narrow case: developing
pi-stack itself, which needs the host's Docker/`sbx`/`make` that the VM
structurally cannot reach. `pi-stack setup` provisions and enables it for you
when the host can run `pi`. To do it by hand, use the safe order (provision
first, since the gate stays off until provisioning succeeds): `pi-stack host
setup` to provision `~/.local/state/pi-stack/host-agent`,
then `pi-stack config set host.enabled true`. Disable it any time with `pi-stack
config set host.enabled false`. Cloud keys come from op://
refs in `hostmode.env` next to `config.toml`, resolved just-in-time by `op run`
and never persisted; without that file the session is Ollama-only.

Host mode ships guardrails — a guard extension, workspace refusals
(`$HOME`/`/`/`/etc`/secret dirs), disabled subagents — but they protect against
**accidents, not attacks**. They are guardrails, not a security boundary. For
anything you wouldn't hand a shell to, use `pi-stack run`. Full threat model:
[docs/design/host-mode.md](docs/design/host-mode.md).

## Optional Data Tools

These are independent. Use the ones you need and skip the rest.

> **Note:** the sbx MCP gateway is currently Docker-internal and not yet publicly
> released. `--mcp`, `pi-stack mcp register`, Google Workspace, Slack, and gateway
> catalog tools require that gateway. External users can use the sandboxed agent,
> GitHub, memory, and OKF knowledge today.

```bash
pi-stack serve            # memory (:11435), knowledge (:11436 if enabled), broker if configured
                          # (auto-started on demand; `serve install` = managed login service)
pi-stack mcp register     # register local stdio MCP servers with the sbx gateway
pi-stack doctor           # check keys, services, models, gog, and MCP state
```

| tool | capability | setup | reaches the sandbox via |
| --- | --- | --- | --- |
| `gh` | `github` | `gh auth token \| sbx secret set -g github` | sbx proxy injection |
| memory | semantic recall | local Ollama watcher and embed models | host service on `:11435` |
| knowledge | OKF retrieval | `pi-stack knowledge init` or `pi-stack knowledge use <path>` | host service on `:11436` |
| Google Workspace | `gworkspace` | `gog auth login`, config account, MCP register | host `gog` MCP through sbx gateway |
| Slack | `chat` | `config/op-refs.env`, 1Password CLI, MCP register | host stdio MCP through sbx gateway |
| gateway catalog | `issues`, `docs`, etc. | `sbx mcp add` | sbx gateway |

Memory needs local Ollama models:

```bash
make pull-models
```

### Local Ollama models

The stack uses local Ollama models in three roles. All three are `pi-stack config`
settings on the host — you never hand-edit sandbox env. Pull them with
`make pull-models` (or `ollama pull <tag>`); whatever tag you set must be pulled
or the call 404s.

| role | config key | default | where it runs |
| --- | --- | --- | --- |
| fact capture | `memory_watcher_model` | `qwen3.5:9b` (~6.6GB) | HOST, **resident** during `pi-stack serve` (shares the bridge model) |
| semantic recall | `memory_embed_model` | `nomic-embed-text` | HOST, embeddings |
| local chat model | `ollama_bridge_model` | `qwen3.5:9b` (~6.6GB) | SANDBOX, loads on demand (Alt+P cycle + the router's local option) |

```bash
pi-stack config set memory_watcher_model qwen3.5:9b   # host watcher (resident)
pi-stack config set ollama_bridge_model  qwen3.5:9b   # local chat/router model
make pull-models                                      # pull all three
```

How the sandbox picks up `ollama_bridge_model`: `pi-stack run` writes the
configured tag into `<workspace>/.pi-stack/ollama-bridge.model`, and the
`ollama-bridge` extension reads it at startup — so a `pi-stack config set` +
next `pi-stack run` is all it takes. No `/etc/sandbox-persistent.sh` editing, and
you set ONE value (the display label is derived from the tag; an
`OLLAMA_BRIDGE_MODEL` env var still overrides for power users, and
`OLLAMA_BRIDGE_CONTEXT` shrinks the KV cache for less RAM).

Sizing for a 16GB box: the watcher and the bridge default to the SAME model
(`qwen3.5:9b`), so Ollama keeps one ~6.6GB model resident for both capture and
local inference instead of paying DRAM for two. On a tight machine, point the
watcher at something smaller (`pi-stack config set memory_watcher_model
qwen3.5:4b`); on a roomier one, bump either (`qwen3.5:27b`, `gemma4:12b`, ...).
Free disk with `ollama rm <tag>`.

Knowledge is opt-in. Create a local OKF bundle or attach an existing one:

```bash
pi-stack knowledge init
# or: pi-stack knowledge use /path/to/okf-bundle
```

Both commands wire the config AND propagate it: a managed or lazily-started
daemon is restarted automatically so the bundle gets indexed; a foreground
`pi-stack serve` is never killed — you're told to restart it (and if nothing is
running, the change simply applies on the next start).

Google Workspace is read-only by default. Authorize once on the host, then point
pi-stack at the account:

```bash
gog auth login
pi-stack config set gog_account you@example.com
pi-stack config set mcp gog
pi-stack mcp register
```

See [docs/gog-setup.md](docs/gog-setup.md) for the full walkthrough.

## Skills and Overlays

The flow: `brainstorm`, `plan`, `build`, and `ship` are the steps. `deliver` is
the operator that runs them to a finished result without you in the loop. It
plans, builds, runs UAT, gets a cross-vendor review twice, fixes every finding,
verifies, and ships. For most tasks `deliver "X"` is the whole flow; if you skip
`plan`, `deliver` plans itself.

`brainstorm` and `plan` are optional gates you add only when you want to stay
involved. Put `brainstorm` in front when the idea is still fuzzy and you do not
yet know what to build. Put `plan` in front when you want to read and approve the
spec before it starts building.

| Situation | Flow |
| --- | --- |
| You know what you want (most tasks) | `deliver "X"` |
| Fuzzy, then just go | `brainstorm` then `deliver` |
| Approve the approach first | `plan` then `deliver` |
| Fuzzy and want a design gate (rare) | `brainstorm` then `plan` then `deliver` |

You rarely type `deliver`. "build X, don't stop" or "take this all the way"
auto-loads it. For a throwaway, say "quick" and you get `build`'s lightweight
mode instead of the full loop.

A skill is a `SKILL.md` with a name, a trigger description, and the operating
procedure. Project-local skills can live in `.pi/skills/`. Reusable skill sets can
ship in a mixin kit:

```bash
sbx run pi-stack \
  --kit "git+https://github.com/mcavage/pi-stack.git#dir=pi-kit" \
  --kit ./my-kit
```

A mixin kit has a `spec.yaml` and a `files/` tree. Files under
`files/home/.pi/agent/skills/` land in the sandbox skills directory; the same
pattern works for prompts, extensions, environment, and network rules. Docker's
kit format is documented in the
[sbx kit docs](https://docs.docker.com/ai/sandboxes/customize/kits/).

The private overlay is a peer repo with two halves:

- `kit/`: private skills, full `capabilities.json`, in-sandbox wrappers, prompts,
  extensions, and network rules
- `host/overlay_*.go`: host plugins that self-register into `pi-stack-host`

When the overlay exists, `make run` stacks the kit and `make serve` compiles the
host plugins. Public code never imports overlay files. See [docs/OVERLAY.md](docs/OVERLAY.md)
and the scaffold in [examples/overlay](examples/overlay).

## Build from Source

Most users should use the `sbx run` or installer path. Build from source when you
are changing the image, baked extensions, or the public skill set.

```bash
git clone https://github.com/mcavage/pi-stack
cd pi-stack
docker login dhi.io
make load
make install
pi-stack
```

The base image is a Docker Hardened Image, so local image builds require a
DHI-entitled Docker account. The hosted `sbx run` path does not.

Use the right iteration loop:

- `Dockerfile`, extensions, themes, settings, baked files: `make load`, then
  recreate the sandbox
- `pi-kit/spec.yaml`: recreate the sandbox; no image rebuild
- skills during local development: edit `SKILL.md`, then `/reload`
- host binaries: `make install`

`make publish` pushes the image manually. CI publishes versioned images from
`main` and updates the kit pin.

## For Agents

If you are an agent working in this repo, read [AGENTS.md](AGENTS.md) before
changing files. It covers the repo layout, build loop, extension conventions,
overlay boundary, and the mistakes worth not repeating.
