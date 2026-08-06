# Migration guide

For people upgrading an existing install, not for a first-time setup (start at
`docs/getting-started.md` instead).

## Upgrading pix itself

```bash
brew upgrade pix
pix doctor
```

`pix run` always tracks the pinned kit/image for the version you have
installed; there is no separate `pix upgrade` verb (retired: Homebrew is the
one distribution mechanism). An existing sandbox keeps its creation-time
image, so after a version bump, recreate any sandbox you want the new image
in: `pix rm NAME && pix run`.

## If you scripted a retired verb

Every retired verb or flag answers a `PIX_RETIRED` line naming its exact
replacement and exits 2: it does nothing else (no config read, no daemon, no
sandbox touched), so a stale script fails loudly instead of half-running. Grep
your scripts for `PIX_RETIRED` after an upgrade, or run them once and read the
stderr. The full table, with the reasoning for each retirement:
`services/host/cmd/pix/corpus/retirement.jsonl`.

| Old | New |
| --- | --- |
| `pix reset` / `pix state reset` | no automated wipe: `pix doctor`, back up the paths it names, `pix setup` |
| `pix host` | `pix run` (host mode is deleted, not just off) |
| `pix upgrade` | `brew upgrade pix` |
| `pix man` / `pix --man` | `pix help --all` |
| `pix knowledge` / `pix kb` | `pix pack use` |
| `pix backup` / `pix restore` | `pix-host backup` / `pix-host restore` |
| `pix evals` | `pix models route` |
| `pix route` | `pix models` |
| `pix onboard` | `pix setup --no-agent` |
| `pix slack` / `pix gworkspace` (verbs) | `pix mcp register` |
| `pix agent new\|edit\|rm\|reassess` | edit `agents/*.md` by hand, then `pix models route` |
| `pix pack new\|add` | edit `pack.toml` / `skills/*/SKILL.md` by hand |
| `pix task harvest` | `pix task path` (git does the rejoin) |
| `pix task gc` | `pix task rm` |
| `pix run --replace` / `pix setup --replace` | `pix rm BOX`, then `pix run` |

## If you were running raw `sbx run`

pix is a launcher in front of `sbx`, not a replacement for it. `pix run DIR`
composes the kit, credentials, network allowlist, and MCP set your
`~/.config/pix/config.toml` describes, then hands off to `sbx`'s own
create/attach lifecycle: a bare `sbx rm -f` still works on a `pix-*` sandbox
if you need to bypass pix entirely, but `pix rm` is the supported path (it is
never forced except on an explicitly named sandbox) and is the only one that
also updates pix's own host-side records. Do not hand-edit `mcp.json` to add
`host.docker.internal` entries; register a server with `pix mcp register` /
`pix mcp bundle` instead: see `docs/reference.md` §8.

## If you relied on the built-in knowledge service (:11436)

It is deleted, not disabled: there is no `config.toml` key or code path left
(`hostmode_gone_test.go` is the permanent regression sentinel). Knowledge is
now pack-delivered: a private pack wires the `knowledge` capability in
`capabilities.json` to a `files` or `http` provider directly. See
`docs/design/packs-v2.md`.

## If you had host mode (`pix host`) enabled

There is nothing to migrate off of: host mode and the credential-broker
plugin slot were removed from the binary entirely. The sandbox is pix's one
supported execution mode. If you had host-only workflows, they now run
through `pix run` inside a sandbox, or through a pack's host daemon +
`[[proxy]]` wrapper (see `docs/design/packs-v2.md`) if they truly cannot be
sandboxed.

## Naming a sandbox in an old script

`--replace` (a forced `sbx rm -f` before create, with no zero-holder proof) is
retired. Recreate deliberately: `pix rm BOX` (proof-gated, or `--force` on a
name you typed yourself), then `pix run`.
