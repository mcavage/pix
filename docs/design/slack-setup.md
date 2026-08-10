# Slack: externalized (migration reference, not an implementation)

Status: **MERGE-BLOCKED reference data.** The built-in Slack MCP server, its
guided OAuth setup CLI, and every launcher/config wrapper around it were
DELETED from this tree in W2/U02a (`services/host/slack.go`,
`services/host/slack_plugin.go`, `services/host/workflow/slack/`,
`services/host/slackoauth/`, the `slack.*` `pix config` keys, and their
subject tests). This document is what's left: a migration pointer for whoever
stands up the replacement, and the credential rules that replacement MUST
honor. **It is not a design for something implemented here, and nothing in it
should be read as evidence that an external Slack MCP integration exists, has
been built, or has been round-tripped.** No such artifact ships in this repo.
Do not merge a claim of "Slack works" against this document alone — merge only
once a real external artifact (a pack manifest, a container image, an actual
`sbx mcp add`/`pix mcp add` registration) has been built and proven end to end
(tool list at minimum; one real call for confidence) by whoever owns it.

## Why it moved

Slack was a WORKFLOW masquerading as a capability: `pix slack setup` sequenced
an OAuth grant, a 1Password write, an MCP gateway registration, and a
readiness report, all compiled into the public host binary. That is exactly
the shape `docs/design/packs.md` says belongs in a pack, not core: company- and
person-specific integration logic that not every install needs, reachable
without a `pix-host` recompile. Externalizing it also removes ~9,500 lines of
compiled-in OAuth/token-rotation machinery (`slackoauth/`,
`workflow/slack/`) from every install that never uses Slack.

## Required posture for the replacement

Whoever re-adds Slack (in the private `gm-pix-pack`, or any other pack) MUST
ship it as:

1. **A PINNED external MCP integration**, not a rebuilt built-in. Per the
   pack-trust gate (AGENTS.md invariant #8: two gates in series with
   `host.enabled`), any pack-contributed local MCP command or container is
   SHA-pinned / manifest-pinned and re-gated on any change to that pinned
   artifact — never re-trusted silently. A Slack MCP server is exactly the
   "host-executing integration" case `docs/design/packs.md` describes: either
   a container the sbx gateway runs (an OCI image + `server.json` manifest,
   creds Docker-side) or a host daemon with a thin `[[proxy]]` wrapper — NOT a
   second copy of `services/host/slack.go` compiled back into `pix-host`.
2. **ON-DEMAND, not preloaded.** Slack is not a universal need the way
   `github`/`gworkspace` arguably are for this stack; it should NOT be added to
   every sandbox's `--static-mcp` preload set by default. Wire it so a
   workspace/pack opts in explicitly and registers it with `pix mcp add
   slack` (or the pack's own on-demand hook), rather than `pix config set mcp
   slack` on every install. `capabilities.json`'s `chat` capability stays
   `"provider": "none"` in the public/default profile for exactly this reason
   (see `capabilities.json`); a pack that wires `chat` should route through
   the same on-demand posture, not flip it to always-preloaded.
3. **`op://` refs only, never a value on disk or in the pack payload** — same
   rule as every other MCP server (AGENTS.md invariant #10; `secret`
   capability). A token is per-person (see the safety rule below); it is never
   embedded in a pack, a container image, or a manifest.

## Stale registration removal (migration step, not automated here)

Any host that previously ran `pix slack setup` (or `make mcp-register` with
`slack` in `LOCAL_STDIO_MCP`, which this change also removes — see `Makefile`)
has a STALE gateway registration pointing at a command that no longer resolves
to a working built-in: `sbx mcp add slack --command ... -- pix-host mcp
slack` now spawns a `pix-host` that answers "no built-in MCP server named
\"slack\"" and exits non-zero. That registration does not clean itself up.
Before (or as part of) adopting a pack-provided Slack replacement:

1. Remove the stale registration: `sbx mcp rm slack` (or the equivalent your
   sbx version exposes) on every host that ever ran `pix slack setup`.
2. Drop `slack` from `~/.config/pix/config.toml`'s `mcp` list if present:
   `pix config unset mcp slack`.
3. Confirm `pix doctor` / `make doctor` no longer report a `slack`
   registration-vs-attachment mismatch before registering the pack's
   replacement under the same name.

This is a documented manual step, not a migration script: no tree in this repo
runs `sbx mcp rm` on a user's behalf, and a fully-automated stale-registration
sweep is exactly the kind of "claims a round trip that was never proven"
shortcut this document is warning against.

## Credential rules the replacement must keep (unchanged from the deleted design)

These rules governed the deleted `services/host/slack.go` / `slackoauth/`
implementation and apply unchanged to whatever server replaces it, whether
that's a rebuilt OAuth flow or a static token:

1. **The token is a personal `xoxp-` user token, not a service credential.**
   Slack's `oauth.v2.access` issues `xoxp-` tokens scoped to the *authorizing
   user*, not the app. Whoever's credential backs the server is the identity
   every call runs as: a search sees what that person can see, a channel read
   is scoped to channels that person is in, and a lookup runs with that
   person's directory visibility.
2. **Handing a token to a second person is impersonation, not sharing
   access.** Every message, search, or lookup that second person triggers
   shows up in audit logs as the original token owner. Sharing credentials
   collapses per-user authorization boundaries.
3. **Rule: a Slack token is always a single named person's user credential.**
   It is never a shared "employee" or "team" token, never a bot token standing
   in for the workspace, and never handed to a second person to reuse. Anyone
   else who wants Slack access runs their own grant against their own MCP
   registration.
4. **Read-only by default.** The deleted implementation exposed no write
   scopes (`chat:write`, `channels:write`, …) — `health`, `search_messages`,
   `list_channels`, `read_channel`, `read_thread`, `get_user`, `search_users`
   only. A replacement that adds write access is a deliberate, separately
   reviewed decision, not a default.

## What is NOT in this document

There is no code here, no working PKCE flow, no `slackoauth.Manager`, no
`pix slack` CLI — all of that was deleted. There is no external artifact
(pack manifest, container, registration) attached to this change either. This
file is reference data for the migration and the rules the replacement must
satisfy; it proves nothing about an external integration actually working.
