# pix monitor — live wiretap of the agent's back-and-forth

Status: shipped (MVP). Owner: Mark.

## Problem

Skills, turns, hooks, MCP servers, and recall all inject into the agent's flow
before anything crosses the sandbox boundary. From the host you currently can't
see the assembled result: what context actually got built, what the model was
sent, what it sent back, which tools/MCPs fired with what args and results.

We want `pix monitor` on the host: a live, one-way mirror into a running
sandbox showing the real traffic between the agent and everything outside it.

Goal is **debugging / observability**, not security audit. We tap what *pi*
sends, at the source, in the clear, not what physically leaves the box.

## Chosen approach: tap 1 (in-VM pi extension)

Of the three physical tap points (in-VM extension, host TLS-intercepting proxy,
persisted session JSONL), we pick the in-VM extension. It has the highest
fidelity and needs no cooperation from the sbx proxy.

pi already exposes the exact hooks:

- `before_provider_request` → `event.payload` is the **full plaintext provider
  request body right before send**: system prompt, every injected skill, all
  tool + MCP tool schemas, the whole conversation, every prior tool result. This
  is the single richest event. Everything the user asked about is visible here
  because it has all been baked into one payload by this point.
- `after_provider_response` → HTTP status + headers.
- `tool_execution_start` / `tool_call` / `tool_execution_end` → tool name, args,
  results, duration, directly (including in-VM bash and MCP tools).
- `model_select`, `session_compact`, `thinking_level_select` → control plane.

Explicitly out of scope (accepted): a compromised sandbox shelling out with raw
`curl` bypasses this tap. That is the proxy's job, not this tool's.

Secrets note: provider keys are proxy-managed, so request bodies carry the
`proxy-managed` sentinel, not real keys. Tool results / bash output can still
contain workspace secrets, so the TUI is host-local and treated as sensitive.

## Architecture

Follows the house split (HOST = Go, SANDBOX = TypeScript):

```
sandbox: extensions/monitor.ts
   taps before_provider_request / after_provider_response / tool_* / model_select
   computes per-turn SUMMARIES in-VM, content-addresses big blobs by hash
   → ships NDJSON events to host.docker.internal:11437  (node:http, NO_PROXY-safe)

host: pix monitor   (launcher verb; hub + TUI in one process for the MVP)
   binds :11437, keeps a bounded in-memory ring + blob cache (no sqlite, no serve)
   renders the live delta view with per-stream toggles + full-payload-on-demand
```

The VM → `host.docker.internal:<port>` over `node:http` plumbing is already
proven by `extensions/memory-recall.ts` and `knowledge-recall.ts` (must use
`node:http`, not `fetch`, because sbx's `NO_PROXY` excludes
`host.docker.internal`). Reuse that verbatim.

### Why single-process for the MVP

Live-follow only, no durable store, so no `serve` integration and no daemon.
`pix monitor` binds the port and runs the TUI. The in-VM extension connects
to `host.docker.internal:11437`; if nothing is listening it silently no-ops and
retries occasionally (best-effort, never blocks or throws — per the extension
gotchas in AGENTS.md). Start the TUI = wiretap on; quit = wiretap off. Zero
config.

Every event carries `sandboxId` (`$SANDBOX_VM_ID`) + `sessionId` so one hub can
host several sandboxes; `pix monitor [name]` filters to one.

## Event model (delta-first)

Model requests re-send the entire growing context every turn. Logging raw
payloads buries the signal, so the extension summarizes in-VM and
content-addresses large blobs (system prompt, each tool schema, each message) by
hash. The wire normally carries the hash; the TUI fetches full text by hash only
when the user toggles a payload open. Deltas come for free (same hash = "no
change").

Event kinds:

- `turn_start` `{turnId, sessionId, model, trigger}`
- `provider_request` `{turnId, model, summary:{systemPromptHash, systemPromptBytes,
  messageCount, newMessages[], toolCount, toolNames[], mcpToolNames[], estTokens},
  changedBlobs[]}`
- `provider_response` `{turnId, status, stopReason, usage, headers?}`
- `tool_start` `{turnId, toolId, source:"builtin"|"mcp:<server>"|..., name, argsSummary, argsHash}`
- `tool_end` `{turnId, toolId, ok, resultBytes, resultSummary, resultHash, durationMs}`
- `context_event` `{turnId, kind:"skill_loaded"|"compaction"|"model_change"|..., detail}`
- `blob` `{hash, bytes, text}` — sent lazily on TUI request, cached host-side.

MCP calls need no separate kind: pi surfaces MCP tools as tools, so `tool_*`
covers them; the `source` tag distinguishes `mcp:<server>` from builtin/skill.

Streaming tokens (`message_update`) are available via the hook but noisy and
**not wired in the MVP** — the extension emits no stream events, so there is no
stream toggle in the TUI (a `s:stream` toggle that flipped state while doing
nothing would just lie to the user).

## TUI (live follow + toggles)

Turn-by-turn scroll, newest at bottom, current turn pinned. Each row is a
one-line summary; expand for detail.

Per-stream toggles (the user's #3 ask):

- `f` full payloads on/off (off = summaries; on = fetch + show full text by hash)
- `m` model requests/responses · `t` tools · `p` MCP · `x` thinking/reasoning ·
  `c` context/control events
- `/` text filter · `space` expand row · `q` quit

Delta view example (payloads off):

```
turn 12  opus-4-8  ▲ req  sys=41KB(unchanged) msgs=+1 tools=14 (+enrich skill) ~38k tok
              ▼ resp 200  stop=tool_use  in 37.9k out 512
   tool  bash        source=builtin   `go test ./...`            → ok 2.1KB 4.3s
   tool  slack_post  source=mcp:slack {channel:#eng, ...}        → ok 118B 0.6s
```

Toggle `f` on any row to expand the literal system prompt / tool schema / args /
result.

## Build order

1. `extensions/monitor.ts`: tap the four hook families, build summaries + blob
   hashes in-VM, ship NDJSON over `node:http` to `:11437`. Best-effort,
   bounded queue, never blocks the agent, guarded at load.
2. `pix monitor` (Go, `cmd/pix`): hub on `:11437` (POST ingest / event
   stream + blob-by-hash fetch) with a bounded ring + blob cache, plus the TUI.
3. Toggles + delta rendering + full-payload-on-demand.
4. Multi-sandbox filter (`pix monitor [name]`).

MVP is 1 + 2 with summaries only; toggles land in 3.

## Resolved decisions

- **TUI stack:** bubbletea. Shipped as a real TUI in the host module
  (`services/host/cmd/pix/monitor_tui.go`), not a plain ANSI scroll
  renderer.
- **Process shape:** single-process hub + TUI in one binary, wired as the
  `pix monitor` launcher verb. No `serve` integration, no daemon, no
  sqlite. Start the TUI = wiretap on; quit = wiretap off.
- **Event model:** delta-first summaries. The extension content-addresses big
  blobs (system prompt, tool schemas, messages) by hash and sends each blob
  body exactly once, hash-only thereafter. The host cache is the source of
  truth for full text; the TUI resolves a blob on demand when the user
  expands a row.
- **Bind default:** `127.0.0.1`, opt in to LAN exposure with `--bind`. There
  is no auth token in the MVP, so a non-loopback bind prints a loud runtime
  warning naming exactly what's exposed (full agent context and tool
  output). This is the security-sensitive default done right, not an open
  question.
- **No host-to-VM reverse channel.** If the extension elided a blob and the
  host never captured its body (for example the TUI started mid-session),
  there is no way to ask the VM to resend it in the MVP. Accepted limitation,
  not a bug: the extension sends every blob body once, and that's the only
  chance to capture it.
- **Streaming token deltas are NOT emitted in the MVP.** The extension never
  wires `message_update`, so there is no `s`/stream toggle in the TUI. It was
  cut on purpose rather than shipped as a toggle that would silently do
  nothing.
- **MCP classification:** derived from pi's tool `sourceInfo` (the
  pi-mcp-adapter package is the positive signal for "this tool came from an
  MCP server"), tagging each tool call `source:"mcp:<server>"` vs.
  `"builtin"`. No separate event kind for MCP; it rides the same `tool_*`
  events as everything else.

**Known limitation, still accepted:** the real VM<->host wire only works
against a monitor-enabled sandbox image. `extensions/monitor.ts` has to be
baked in and `pi-kit/spec.yaml`'s `host.docker.internal:11437` allowlist entry
has to exist, which means `make load` on the host plus a fresh (or recreated)
sandbox. A sandbox created before this shipped will never phone the hub, and
the TUI has no way to distinguish that from "nothing has happened yet."
