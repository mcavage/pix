# Pix on marsh

**Preview candidate, not a replacement for the legacy release.** The legacy Pix
launcher elsewhere in this repository is unchanged. This directory builds a
portable native Kit: upstream Pi, public skills/roles/themes, supported UI
extensions, and orchestration instructions that use marsh itself.

There is no Pi fork, custom subagent runner, scheduler, Pix launcher, Docker
hosted model gateway, memory service, or built-in business MCP configuration.
marsh supplies process trees, private split workspaces, fanout/collect,
cancellation and structural receipts. Receipts are not semantic memory.

## Public package / private additions

| Public image | Private local state, never exported |
|---|---|
| Upstream Pi 1.2.0 and ACP, inherited from an immutable marsh base | API credentials held by stock SBX |
| 36 selected generic skills plus `marsh-orchestration` | Organization-specific skills and references |
| 19 role prompts, not enforced agent/tool policies | Business MCP declarations and login stores |
| Public themes, Kanagawa default | Personal model/settings choices, auth and sessions |
| Display-only activity/timestamps and durable output styles | Output-style content under your Pix agent directory |

The preparation command copies only Git-tracked files under explicitly selected
public skills/roles/themes and an exact list of new package/Kit files. It never
reads an installed `~/.pix` or `~/.marsh` home. The generated build context has
an input inventory, not an image signature or authority receipt. Review the
outgoing source; an allowlist is not a substitute for a secret/content review.

## Build a source Kit

You need Docker with Buildx, marsh, stock Docker Sandboxes, Python 3 and Git.
From this checkout on the Mac:

```sh
python3 marsh/prepare.py "$HOME/.local/share/pix-marsh-kit"
```

Choose an empty destination outside both your project and marsh guest home.
Preparation refuses to overwrite existing work. To update, prepare into a new
versioned directory rather than writing over a live Kit generation.

Build that exact source with the pinned native frontend:

```sh
docker buildx build --platform linux/arm64 --load \
  -t pix-marsh:local \
  -f "$HOME/.local/share/pix-marsh-kit/pix.yaml" \
  "$HOME/.local/share/pix-marsh-kit"
```

This builds locally, without registering, authenticating or creating a sandbox.
The base owns locked Pi/ACP/MCP dependencies and their notices; this package adds
no installed npm dependency. Its JavaScript/TypeScript uses Pi's supplied APIs.
Updating the Pi/base pin requires the runtime checks below, not a floating
package install. The build rechecks the actual derivative's installed base/Pi
notices and complete added public payload, retaining separate receipts. These
are byte/notice checks, not a legal grant or authenticated-provider/runtime proof.

## Register separately from packaged Pi

Close active sessions before changing command mappings. From the Mac,
`marsh status --json` reports the scope's host-only `control_home`. Merge this
entry into its `commands.json`, preserving unrelated registrations:

```json
{"pix": "/absolute/path/to/the/prepared/pix-marsh-kit"}
```

Use the actual absolute path, not a literal `~` or `$HOME` in JSON. The directory
must remain outside mounted projects and guest home. This adds `pix`; it does
not replace `pi`. Do not copy the legacy launcher into the shell's PATH.

For optional ACP, add a separate `pix-session` declaration to the existing ACP
list rather than replacing packaged Pi's entry:

```json
{
  "schema_version": 1,
  "name": "pix-session",
  "protocol": "acp_v1",
  "command": "pix",
  "arguments": ["--acp"],
  "required_capabilities": [],
  "description": "Pix public stack through upstream Pi ACP",
  "workload_digest": ""
}
```

An empty digest here is for a **local source Kit**, whose generation marsh pins
at daemon startup. Published immutable Kits require their actual explicit digest.
Preserve existing ACP entries and stop/restart the idle marsh scope to load the
new mapping. Registration does not prove authenticated provider access.

Give SBX the provider you intend to use through its normal host-side setup:

```sh
sbx secret set openai
marsh
```

At the marsh prompt, `pix` starts your stack; `pi` remains the packaged agent.
The default is `openai/gpt-6.1-sol`, requiring suitable OpenAI API access. For
another configured provider, choose it explicitly, for example:

```sh
pix --provider anthropic --model claude-sonnet-5-5
```

The native Kit declares OpenAI, Anthropic and Google capabilities. SBX inserts
provider keys at the declared API endpoints; no real key is baked into this
image. OpenAI here is native API-key access, not ChatGPT OAuth. Explicit
SBX-bundled llmman routing is a separate integration, not silently enabled by
this Kit. No separate llmman service is installed or started.

## Personal configuration and UI

On the Mac, Pix state is under `~/.marsh/home/.pix-agent/agent/`; inside marsh
it is `~/.pix-agent/agent/`. CLI and ACP both use that directory. Its first
settings file is created only when absent; subsequent starts preserve existing
settings. Packaged Pi's `~/.pi/agent` is never copied or changed. This separates
discovery/state, not filesystem authority: jobs still share the selected home.

The image's public package is `/opt/pix/package`. It loads its skills explicitly,
so packaged agents do not discover them from a shared `.agents/skills` directory.
To migrate the earlier personal prototype, remove its old `packages` entry and
custom subagent extension deliberately; don't load both full generic libraries.
This branch does not edit your running installation or private files for you.

UI uses supported upstream APIs: phase/elapsed display, concurrent-tool names,
quiet-stream warning, last reply time (UTC), and `/pix-status`. It never cancels,
retries or launches jobs. Pi keeps its own footer/model/context/token/cost display;
there is no patched powerbar. Set `PIX_UI=off` to skip the activity extension.

Use `/settings` for themes; use `terminal.trueColor` in your private settings
only when you need to override detection for a known-capable terminal. Output
styles are optional and live under `.pix-agent/agent/context/output-styles`;
`/output-style` lists/selects/disables them. The `output_style` tool can save a
style only on explicit user request. Styles change prose, not task authority.

## Orchestration is a skill, not another engine

Load `/skill:marsh-orchestration`. A role is a bounded prompt supplied to a
registered command with an explicit model where needed. Use fanout/collect for
shared-file-safe concurrency, split/join for parallel edits, and sequential
calls for dependencies. Join returns patches; it does not apply them. A reviewer
that needs implementation changes runs after those changes are integrated,
not in a sibling fork of the original snapshot.

Historical generic skill references to subagent/chain/task objects are work-order
intent only. There is no `subagent` tool here. `marsh jobs --tree` and
`marsh results` show actual lineage/exit/cleanup, not inferred model identity or
usage. Use response metadata for those claims. No memory/automatic recall is
promised, and exit zero alone is not task acceptance.

## Optional private skills and MCP

Add only private skills under `~/.marsh/home/.pix-agent/agent/skills/`, not this
public checkout. A symlink target must also be visible inside the guest. Keep
business configuration in private storage and out of image build inputs.

Host-only MCP additions for this command go in `~/.marsh/sbxenvs/pix.yaml`, not
the shared `sbxenv.yaml`. No real business example or endpoint ships here. Keep
credential values out of YAML/argv; use the server's host authentication. After
editing, finish active work and restart marsh to load its configuration snapshot.
New local-server processes may require authentication; this public package does
not disable or cache those approvals.

## Checks and remaining gates

```sh
python3 -m unittest discover -s marsh/tests -v
python3 marsh/tests/runtime.py
```

The first gate checks the public export boundary, resource inventory and
non-destructive preparation. Runtime checks build the real source and invoke
upstream Pi using isolated temporary homes and fake sentinels, with no SBX,
model/provider, MCP or personal authentication calls. These do not prove real
provider access, marsh child admission, ACP sessions or business integrations.
Those real-caller gates must pass before calling this a released replacement.
Nothing is auto-published.

## Publish a public preview

The intended public image is `docker.io/mcavage/marsh-pix`; the registered command
remains `pix`. `marsh/publish.py` exports only public inputs, then delegates to
marsh's native cache-before-push publisher. Use an interpreter with that
publisher's hashed requirements installed. Without `--push` it validates only:

```sh
python3 marsh/publish.py \
  --publisher /path/to/marsh/scripts/publish-kits.py \
  --python /path/to/publisher-venv/bin/python \
  --output /absolute/private/new-pix-publication \
  --tag preview-20261015 \
  --push
```

The publisher checks inputs before/after builds, retains a publication receipt,
and writes a `pix` → immutable registry digest mapping. Install that exact digest,
not the moving preview tag, and set the optional Pix ACP entry's workload identity
to the same digest. A preview is not a stable legacy Pix release or a promise
that the user's private services have authenticated successfully.

The existing `publish` workflow also has an explicit `marsh_preview` manual mode.
It uses the repository's existing Docker Hub release credentials, runs the public
export/runtime gates on an ARM64 runner, and publishes only a unique `marsh-pix`
preview tag. All legacy release roots are skipped in that mode: no main-branch
version bump, legacy image/release, or tap change. Ordinary legacy publication
remains the default when that input is false.
