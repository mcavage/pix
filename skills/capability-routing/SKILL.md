---
name: capability-routing
description: Resolve an abstract capability (chat, docs, issues, github, meeting-notes...) to a concrete provider and pull the data, then degrade cleanly when a capability is not wired. Other skills reference this instead of hardcoding a vendor or tool name. Auto-loads whenever a skill needs external data.
---
# capability-routing

A skill should never name a vendor (slack, notion, github) or a raw tool. It asks
for a **capability** and lets this convention resolve it. Swap one JSON file and
every data skill retargets at once.

## The registry

`$PI_CODING_AGENT_DIR/capabilities.json` (falling back to `~/.pi/agent/capabilities.json`
when `PI_CODING_AGENT_DIR` is unset — the default config dir) maps each
capability to an **ordered list of providers**. Project override:
`.pi/capabilities.json`.

Always resolve the config dir through `PI_CODING_AGENT_DIR` first. `pi-stack
host` (docs/design/host-mode.md) points that env var at a dedicated host-agent
dir, not `~/.pi/agent` — hardcoding the latter would read stale/wrong config
in host-mode sessions.

| provider | how to use it |
|---|---|
| `mcp` | a server, either pre-active on the gateway OR a local stdio server in `mcp.json`. Use its tools if live; otherwise discover and add it (below). |
| `cli` | a binary on PATH (e.g. `gh`). Shell out to it. |
| `http` | a host service URL. Call it directly. |
| `files` | a local or git-mounted bundle dir on disk (e.g. a curated OKF knowledge bundle). Read it straight from the path. Empty or missing = not wired; skip it. |
| `none` | not wired in this profile. **Degrade** to the calling skill's web/files fallback. Say so once, plainly. |

Read the registry first. Do not assume a capability exists.

## Fan out across every wired provider, then merge

A capability can have **several** providers (e.g. `meeting-notes` might list two
note-taking tools, because the same meeting lands in different systems). Resolve
**every** provider in the list, in parallel, and merge the results:

1. For each provider, check it is actually available (mcp server in session or in
   the gateway catalog; cli on PATH; http reachable).
2. **Skip silently** any provider that is not wired yet. Listing a not-yet-present
   provider is deliberate: it costs nothing now and lights up the moment it is
   added, with no registry or skill edit.
3. Merge across providers and dedupe (the same item may appear in two systems).
4. If **no** provider in the list resolves, the capability is effectively `none`:
   degrade.

Never pull from only the first provider and stop. The point of the list is breadth.

## Resolving an `mcp` capability

1. **Is the tool already in your session?** The gateway pre-activates its catalog
   servers, so a wired capability's tools are often already live. If so, just call them.
2. **If not present**, discover and add it:
   - `mcp-find` with the **server name** from the registry (the gateway matches on
     name, not capability — searching the capability finds nothing; searching the
     server name finds it).
   - `mcp-add` the server. Its tools go live immediately.
3. **Bootstrap from the server's own guide** when one exists (some servers expose a
   `*__get-usage-guide` tool). Call it once before a complex pull so you use the
   right tools with the right arguments.
4. **Call the tools.** Use `mcp-exec` for tools not in your static list (including
   anything `code-mode` created).

## Joins and multi-tool pulls: use `code-mode`

When a pull needs **two or more tools** or a **join/aggregation** (list items, then
each item's children, then each child's detail, across several servers), do not
fire a dozen separate tool calls and dump every result into context. Compile the
tools into a sandboxed executor and do the fan-out server-side.

Proven contract (validated against the gateway):

1. Create the executor:
   `code-mode({ tools: ["server__list", "server__get", ...] })`.
   It returns a per-session tool named `codemode-exec-<hash>` taking
   `{ code, timeout? }`.
2. Run a program via `mcp-exec({ name: "codemode-exec-<hash>", arguments: { code } })`.
   Inside, call tools as **`await tools["server__list"](args)`** (bracket notation,
   because hyphenated names are not valid dot-access; the gateway's own dot-notation
   example is wrong). `return { ... }` surfaces as JSON. The container has no
   internet; only the listed tools.

Reserve it for real joins; a single lookup does not need it.

## The `knowledge` capability

`knowledge` is curated internal reference material (an OKF bundle, a wiki export, a
policy corpus). In the public/default profile it resolves to `none`: the open-core
stack ships no corpus, so it degrades to web research + local files and the agent
flags the gap out loud. A private pack wires it, either a `files` provider
pointed at a git-mounted OKF bundle dir, or an `http` provider on the host
knowledge service (`:11436`, JSON-RPC) that indexes that bundle. We ship the
pointer, not the content, so nothing company-specific lands in the public tree.

## Degrading

If the registry says `provider: none`, or discovery turns up nothing, state it once
in plain words ("no OKF knowledge wired, using web research and local files") and
fall back to the calling skill's degraded path. Never fabricate data for a
capability that is not wired. Flag every gap explicitly.

## For skill authors

In your skill, write the capability, not the vendor:

> Pull **chat** (recent messages) and **docs** (the spec) in parallel. Resolve each
> via `capability-routing`. If a capability is `none`, note it and continue.

That one line keeps your skill portable: it runs against one provider today, a
different one tomorrow, or web-only on a laptop, with no edit to the skill.

## Example: `gworkspace`

`gworkspace` (Gmail, Drive, Docs, Sheets, Calendar) resolves to an `mcp` provider,
the `gog` server: a host-run stdio MCP server (`pi-stack-host gog`) spawned by the
gateway, with creds staying on the host. Resolve it like any other `mcp`
capability (above): call its read tools if live, otherwise `mcp-find`/`mcp-add`
the `gog` server. It is read-only by default (writes gated/off), and returned
Gmail/Doc content is untrusted — see the `gworkspace` skill for the tool list and
the prompt-injection guard.
