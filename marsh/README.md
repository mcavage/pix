# Pix on marsh

This directory builds Pix as a marsh Kit: upstream Pi plus Pix's public
skills, roles, themes and UI extensions. The published image is
`docker.io/mcavage/marsh-pix`. Inside marsh you run it as `pix`; the packaged
`pi` is unchanged.

There is no Pi fork, subagent runner, scheduler, memory service or model
gateway here. marsh runs child jobs (fanout/collect, split/join), and SBX holds
the API keys.

## Use the published image

Add `pix` to the `commands.json` in your marsh control directory (shown by
`marsh status --json` as `control_home`), keeping existing entries:

```json
{"pix": "docker.io/mcavage/marsh-pix@sha256:<digest>"}
```

Give SBX an API key, then start marsh:

```sh
sbx secret set openai
marsh
pix
```

The default model is `openai/gpt-6.1-sol`. To use another provider, pass it
explicitly, e.g. `pix --provider anthropic --model claude-sonnet-5-5`. The Kit
can use OpenAI, Anthropic and Google keys; SBX inserts them at those providers'
API hosts, and no key is in the image.

For ACP, add a `pix-session` entry next to packaged Pi's in `agents.json`:

```json
{"schema_version": 1, "name": "pix-session", "protocol": "acp_v1",
 "command": "pix", "arguments": ["--acp"], "required_capabilities": [],
 "description": "Pix through upstream Pi ACP", "workload_digest": "sha256:<digest>"}
```

## Build it yourself

```sh
python3 marsh/prepare.py ~/.local/share/pix-marsh-kit      # empty destination
docker buildx build --platform linux/arm64 --load -t pix-marsh:local \
  -f ~/.local/share/pix-marsh-kit/pix.yaml ~/.local/share/pix-marsh-kit
```

`prepare.py` copies only Git-tracked public files (the selected skills, roles,
themes and package files). It never reads `~/.pix` or `~/.marsh`. To use a
local build, point `pix` in `commands.json` at the prepared directory's absolute
path and leave `workload_digest` empty.

## Your settings

Pix keeps its state in `~/.marsh/home/.pix-agent/agent/` (inside marsh:
`~/.pix-agent/agent/`). Use `/settings` for themes and `/pix-status` for
activity. Output styles live in `context/output-styles`, managed with
`/output-style`. `PIX_UI=off` turns off the activity display.

## Orchestration

Load `/skill:marsh-orchestration`. Roles are prompts given to a registered
command with an explicit model. Use fanout/collect for independent work,
split/join for parallel edits (join returns patches; it does not apply them),
and plain sequential calls when one step depends on another. `marsh jobs --tree`
and `marsh results` show what ran.

## Adding your own skills and tools

Your own skills go in `~/.marsh/home/.pix-agent/agent/skills/`. MCP servers for
Pix go in `~/.marsh/sbxenvs/pix.yaml`. Keep organization-specific material out
of this repository. For a complete private layer built on this image, see the
community example in the marsh docs.

## Tests

```sh
python3 -m unittest discover -s marsh/tests -v   # export boundary and preparation
python3 marsh/tests/runtime.py                   # builds the Kit and runs Pi with fake keys
```

## Publishing

The `publish` workflow's `marsh_preview` mode runs the tests above on ARM64 and
pushes a uniquely tagged `marsh-pix` image with the repository's Docker Hub
credentials, without touching the legacy release. Locally,
`python3 marsh/publish.py --publisher <marsh>/scripts/publish-kits.py --output DIR --tag TAG [--push]`
does the same and writes the `pix` → digest mapping. Install the digest, not the tag.
