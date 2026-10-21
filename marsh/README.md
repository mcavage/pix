# Pix on marsh

This directory builds Pix as a marsh Kit: upstream Pi plus Pix's public
skills, roles, themes and UI extensions. The published image is
`docker.io/mcavage/marsh-pix`. Inside marsh you run it as `pix`; the packaged
`pi` is unchanged.

There is no Pi fork, subagent runner, scheduler, memory service or model
gateway here. marsh runs child jobs (fanout/collect, split/join), and SBX holds
the API keys.

## Install

```sh
sbx secret set openai
marsh kit install pix --from docker.io/mcavage/marsh-pix:stable
marsh
pix
```

This needs marsh 0.1.7 or later (`brew upgrade marsh`). On an older marsh, put
the digest in `commands.json` instead (see "Build it yourself").

`kit install` resolves `stable` to the image it names now and runs exactly
that. When a new Pix is published, `marsh kit upgrade pix` moves to it;
`marsh kit uninstall pix` removes it. The packaged `pi` is unchanged.

The default model is `openai/gpt-6.1-sol`. To use another provider, pass it
explicitly, e.g. `pix --provider anthropic --model claude-sonnet-5-5`. The Kit
can use OpenAI, Anthropic and Google keys; SBX inserts them at those providers'
API hosts, and no key is in the image.

For ACP, add a `pix-session` entry next to packaged Pi's in your marsh
control directory's `agents.json` (`marsh status --json` shows it), with
`"command": "pix", "arguments": ["--acp"]` and the installed digest (from
`commands.json`) as `workload_digest`. It follows `marsh kit upgrade pix`.

## Build it yourself

```sh
python3 marsh/prepare.py ~/.local/share/pix-marsh-kit      # empty destination
docker buildx build --platform linux/arm64 --load -t pix-marsh:local \
  -f ~/.local/share/pix-marsh-kit/pix.yaml ~/.local/share/pix-marsh-kit
```

`prepare.py` copies only Git-tracked public files (the selected skills, roles,
themes and package files). It never reads `~/.pix` or `~/.marsh`. To use a
local build, add `"pix": "/absolute/path/to/the/prepared/kit"` to `commands.json`
in your marsh control directory instead of installing the published image.

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
does the same and writes the `pix` → digest mapping. The workflow also moves
`stable` to each new publication, which is what `marsh kit upgrade` follows.
