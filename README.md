# pi-stack

pi-stack is an opinionated Docker-sandboxed distribution of
[pi](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent) for
running autonomous coding tasks.

The goal is simple: let the agent edit code, run commands, test the result, ask a
second model to review the diff, and open a PR without turning every shell command
into an approval prompt. The safety boundary is the sandbox, not a stream of
one-off confirmations.

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

Typical flow:

```bash
sbx secret set -g anthropic
sbx secret set -g openai
sbx secret set -g google
sbx secret set -g github

pi-stack setup
pi-stack serve
pi-stack
```

`pi-stack setup` writes `~/.config/pi-stack/config.toml`, registers configured MCP
servers, and enables memory. Re-run it when your host setup changes.

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
search and capture is disabled.

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

## Launcher Commands

```bash
pi-stack                     # launch a sandbox in the current directory
pi-stack run [DIR]           # same, explicit
pi-stack setup               # guided setup for config, memory, and MCP
pi-stack serve               # run enabled host services
pi-stack doctor              # diagnose host and sandbox prerequisites
pi-stack config show|path    # inspect resolved config
pi-stack config set|unset    # update config without hand-editing toml
pi-stack mcp register|ls     # register/list local stdio MCP servers with sbx
pi-stack knowledge init|use|ls  # create, attach, or inspect OKF bundles
pi-stack version             # print the launcher version
```

Do not hand-edit `config.toml`. `pi-stack setup` and `pi-stack config set/unset`
are the supported writers, and `pi-stack doctor` prints copy-pasteable repair
commands when something is missing.

## Optional Data Tools

These are independent. Use the ones you need and skip the rest.

> **Note:** the sbx MCP gateway is currently Docker-internal and not yet publicly
> released. `--mcp`, `pi-stack mcp register`, Google Workspace, Slack, and gateway
> catalog tools require that gateway. External users can use the sandboxed agent,
> GitHub, memory, and OKF knowledge today.

```bash
pi-stack serve            # memory (:11435), knowledge (:11436 if enabled), broker if configured
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

Knowledge is opt-in. Create a local OKF bundle or attach an existing one, then
restart the host services:

```bash
pi-stack knowledge init
# or: pi-stack knowledge use /path/to/okf-bundle
pi-stack serve
```

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
