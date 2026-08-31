# Pinned live subagent tracker

> **HISTORICAL — pre-v2 design note.** This document predates the accepted
> Pix v2 surface and architecture (`docs/design/pix-v2-surface.md`,
> `docs/design/pix-v2-architecture.md`), which supersede it. Commands,
> files, and components described here may no longer exist. Nothing in it
> is a description of current behavior; read it as history only.


**Status: IMPLEMENTED** — shipped in `extensions/subagents.ts` (the pin-tracker
section). Successes use `PI_SUBAGENT_PIN_TTL_MS` (default 6000 ms); failures use
`PI_SUBAGENT_FAILED_PIN_TTL_MS` (default 15000 ms) so errors remain readable but
never permanently occupy the screen.

Add a persistent, pinned TUI panel that shows every running / finished subagent
in one stable place, updated in place via `ctx.ui.setWidget`, without touching
the watchdog or the child-process stability guarantees in
`extensions/subagents.ts`.

This is a plan (names + responsibilities), not full code. One file only.

## Design in one sentence

A module-level run registry, populated from the existing `emit()` / `onUpdate`
path inside `runSingle`, is rendered into a single pinned widget by a
self-stopping 1s ticker; every mode (single / parallel / chain / tree) funnels
through `runSingle`, so they all land in the same panel for free.

## Guiding constraints (do not violate)

- Never throw at load. Every new pi-API touch is guarded, same as `status.ts`.
- Keep the extension factory synchronous. No new `await` at load. `/reload`
  hangs come from a factory that never settles.
- Do not add a parallel event stream. Drive the registry from the lifecycle
  points that already exist (`emit()` in `runSingle`, the mode dispatch in
  `execute`). No new `pi.on("message_update", ...)` firehose.
- The tracker is pure decoration. If `ui` is absent (a headless `-p` child, the
  tree case) it must degrade to a no-op and never allocate a timer.
- Do not change any watchdog, kill, drain, or process-group logic.

## (1) Module-level run registry

Declared at module scope (survives across turns, one per pi process):

```
interface RunEntry {
  id:        string;   // stable, e.g. `${seq++}` at register time
  agent:     string;
  model?:    string;   // filled from agent.model, then corrected from first assistant msg
  mode:      "single" | "parallel" | "chain";
  depth:     number;   // CURRENT_DEPTH + 1 (see tree note below)
  status:    "running" | "done" | "failed" | "timeout" | "aborted";
  startedAt: number;   // Date.now()
  endedAt:   number | null;
  turns:     number;
  tokens:    number;   // input + output (whatever fmtTokens shows)
  cost:      number;
  lastTool:  string | null;  // most recent tool call name, e.g. "read" / "web_search"
  error:     string | null;  // errorMessage / stderr tail when failed
  step?:     number;   // chain step index, for the label
}

const runs = new Map<string, RunEntry>();   // insertion order = display order
let seq = 0;
let ui: any = null;              // stable ctx.ui, captured once (see 2)
let ticker: ReturnType<typeof setInterval> | null = null;
let lastRendered: string | null = null;   // dedup guard, like status.ts lastShown
```

`Map` (not array) so `updateRun` / `finalizeRun` are O(1) by id and insertion
order gives a stable panel order. `tokens` is a single rolled-up number to keep
the row short; the rich inline `renderResult` block still shows the full
breakdown, so the pin stays a glanceable rollup, not a second full report.

## (2) Capturing a stable `ctx.ui` and driving setWidget / setStatus

`setWidget` / `setStatus` live on `ctx.ui`, which is present on the tool
`execute` ctx and on event ctx (guard with `ctx.hasUI`). Mirror `status.ts`:
keep a module-level `ui` and refresh it whenever a ctx flows in.

- `captureUi(ctx)`: if `ctx?.hasUI` and `ctx.ui`, set `ui = ctx.ui`. Best-effort,
  guarded. Called at the top of `execute` (the one place a fresh ctx arrives per
  invocation) and, cheaply, from a `session_shutdown` handler for teardown.
- `renderWidget()`: the single writer. Builds a `string[]` from `runs`, calls
  `ui.setWidget("subagent-tracker", lines)` and
  `ui.setStatus("subagent-tracker", rollup)`. Dedup: join the lines, compare to
  `lastRendered`, return early if unchanged (avoids per-second repaints when
  nothing moved, same trick as `status.ts`). When `runs` is empty, call
  `ui.setWidget("subagent-tracker", undefined)` and `ui.setStatus(..., undefined)`
  to clear the pin. Wrapped in try/catch; a render failure never propagates.
- Use the `string[]` form, not a render fn, for the first cut. Colors via
  `ui.theme?.fg?.(...)` guarded (theme may be absent). Icons reuse the existing
  vocabulary: running spinner frame, `✓` done, `✗` failed, `⏱` timeout,
  `⊘ aborted`. Keep each row one line so a shrink never jitters the bottom pin
  (the tui-bottom-pin patch handles anchor, but constant-ish height is cheap
  insurance).

Rollup header (first line) and `setStatus`: `N running · M done[ · K failed]`.

## (3) Where to register / update / finalize (hook the existing path)

Every child, in every mode, is spawned by exactly one call to `runSingle`. That
is the choke point. Put the three lifecycle calls there so single, parallel,
chain, and tree all funnel in with no per-mode wiring:

- Add one parameter to `runSingle`: `mode: "single" | "parallel" | "chain"`.
  The four call sites in `execute` already know their mode (`md` is built with
  it), so pass it. This is the only signature change.
- REGISTER: right after the `result` object is constructed (before spawn), call
  `const runId = registerRun({ agent: agentName, model: agent.model, mode,
  depth: CURRENT_DEPTH + 1, step })`. `registerRun` creates the entry with
  `status: "running"`, `startedAt: Date.now()`, then `ensureTicker()` and
  `renderWidget()`.
- UPDATE: inside the existing `emit()` closure (which fires on every
  `message_end` and metered `compaction_end`), also call `updateRun(runId, {
  turns: result.usage.turns, tokens: result.usage.input + result.usage.output,
  cost: result.usage.cost, model: result.model, lastTool:
  lastToolNameFrom(result.messages) })`. Tool results arrive as `message_end`
  events with `role: "toolResult"`; there is no `tool_result_end` event. Include
  their optional usage so nested subagent work appears in the live tracker.
  Include `compaction_end.result.usage` as well. `emit()` is the sanctioned live
  hook named in the brief. No new event subscription. `lastToolNameFrom` is a
  tiny helper that reads the last `toolCall` part from `result.messages` (reuse
  `displayItems`).
- FINALIZE: in the `finally` block that already does tmp cleanup (or immediately
  before each `return result`), call `finalizeRun(runId, result)`. It maps the
  outcome to a status (`isFailed(r)` plus `r.timedOut` / `r.stopReason ===
  "aborted"` to pick `failed` / `timeout` / `aborted`, else `done`), sets
  `endedAt`, copies `error` from `errorMessage`/`stderr`, then `renderWidget()`
  and `maybeStopTicker()`. Doing it in `finally` guarantees a run never gets
  stuck showing "running" even if `runSingle` throws.

Because registration is keyed to `runSingle`, the doctor canary also shows up
briefly; that is acceptable (it is a real run) and self-expires. If undesired,
gate it with a flag threaded from the `/subagents doctor` path.

The final subagent tool result also returns a top-level `usage` aggregate. Pi
persists that on the parent `ToolResultMessage`, so delegated work contributes
to the session footer and cost breakdown instead of living only in tool
`details`. The aggregate includes each child's assistant messages, nested
subagent tool-result usage, and child compaction usage exactly once.

## (4) The 1s ticker (elapsed timers, no leak)

Model it on `status.ts`'s single shared `setInterval`, but gate it on the
registry:

- `ensureTicker()`: if `ui` is set and `ticker` is null and there is at least
  one entry, `ticker = setInterval(tick, 1000)`. If `ui` is null (headless
  child) it does nothing, so a tree child never allocates a timer.
- `tick()`: guarded top to bottom. `renderWidget()` (advances elapsed for
  running rows), then `pruneExpired()` (see 5), then `maybeStopTicker()`.
- `maybeStopTicker()`: if `runs` is empty, `clearInterval(ticker)`, `ticker =
  null`, and `renderWidget()` once more to clear the pin. So the timer starts on
  the first registered run and stops when the last one has finished AND expired.
  The registry (not "active count") drives it, so finished-but-not-yet-expired
  rows keep the timer alive just long enough to prune themselves, then it stops.
- `session_shutdown`: a guarded handler calls `teardown()` which clears the
  interval, empties `runs`, and clears the widget. This is the hard guarantee
  against a leaked interval outliving the session.

The ticker is the ONLY periodic timer added. It cannot overlap the child
watchdog timers (those are per-spawn, inside `runSingle`, untouched).

## (5) Auto-collapse and expiry of finished runs

Keep the pin bounded so a big fanout does not grow it without limit:

- Successes expire after `PI_SUBAGENT_PIN_TTL_MS` (default 6000); failures,
  timeouts, and aborts expire after `PI_SUBAGENT_FAILED_PIN_TTL_MS` (default
  15000). The durable tool result remains in conversation history.
- Collapse: in `renderWidget()`, if the count of finished rows exceeds a small
  cap (reuse the spirit of `COLLAPSED_ITEMS`, say `MAX_VISIBLE_FINISHED = 3`),
  render only the most recent few and fold the rest into the header count
  (`… +N done`). Running rows are always shown in full. This mirrors the "auto
  collapse finished into the rollup" item in the brief.
- Result: while work runs you see live rows; as they finish they linger briefly
  with their final status, then fold into the header and disappear; when all are
  gone the pin clears itself.

## (6) How parallel / chain / tree funnel into one panel

- Single: one `runSingle` -> one row.
- Parallel: `mapWithLimit` calls `runSingle` N times; N rows appear, each ticks
  independently, header shows `k running · m done`.
- Chain: sequential `runSingle` calls; each registers on start and finalizes on
  finish, so the panel shows the chain advancing step by step (use `step` in the
  row label, e.g. `chain 2/4`).
- Tree: a child subagent that itself calls `subagent` does so in a separate
  `pi --no-extensions -e <self> -p` process. That child is headless
  (`hasUI === false`), so its own registry renders to nothing and never starts a
  ticker. The nested run is visible only as `lastTool: "subagent"` on the parent
  row. So the panel reflects the runs spawned by THIS (interactive) process,
  which are all at `depth = CURRENT_DEPTH + 1`. `depth` is recorded for future
  use and indentation but will be constant within one process. Call this out as
  a known limitation: true cross-process tree nesting is not surfaced in the pin
  (it would need the child to stream a tree event up, which is out of scope and
  would touch the child protocol).

All of the above require zero per-mode tracker code because registration lives
in `runSingle`.

## (7) Defensive guards (no throw at load, no /reload wedge)

- All new module-level state is plain declarations. No I/O, no `await`, no port
  bind at load. The factory stays synchronous.
- Every tracker function body is wrapped in try/catch with a comment
  (`best-effort; must not break the agent`), matching `status.ts`.
- `registerRun` / `updateRun` / `finalizeRun` no-op safely if `ui` is null; they
  still maintain the map (cheap) but skip rendering and skip the ticker. Since a
  headless child is short-lived and exits, the map never leaks there.
- `renderWidget` guards `ui.setWidget` / `ui.setStatus` / `ui.theme` each
  (older pi or a non-UI ctx may lack one). A missing method degrades to no pin.
- `session_shutdown` teardown clears the interval and the widget. Add the
  handler with the same guarded `on(...)` wrapper `status.ts` uses.
- The ticker callback is fully guarded; one bad render cannot kill the loop or
  the agent.

## Risks and mitigations

- Repaint churn while a big fanout streams. Mitigate with the `lastRendered`
  dedup and one-line rows. The bottom-pin patch already handles anchor jitter.
- `setWidget` id collision with plan-mode / todo pin. Use a unique id
  (`"subagent-tracker"`); pi keys widgets by id so they coexist.
- `ui` going stale after `/reload` (old ctx). `captureUi` refreshes it on every
  `execute`, and teardown on `session_shutdown` resets it, so a reload rebinds
  cleanly.
- Coupling `runSingle` to UI state. Kept minimal: three call sites plus one new
  param. `runSingle` stays a pure-ish async function; the tracker calls are
  fire-and-forget and individually guarded, so they cannot change its return or
  its watchdog behavior.
- Map growth if a run is registered but never finalized. The `finally` block in
  `runSingle` guarantees finalize runs even on throw, and `pruneExpired` is the
  backstop.

## Smallest-diff approach (recommended)

1. Add the registry block + helpers (`registerRun`, `updateRun`, `finalizeRun`,
   `renderWidget`, `ensureTicker`, `maybeStopTicker`, `pruneExpired`,
   `captureUi`, `teardown`, `lastToolNameFrom`) as one new section, all guarded.
2. Add `mode` as a parameter to `runSingle` and pass it from the four call sites
   in `execute` (they already know it).
3. Three lines inside `runSingle`: `registerRun` after `result` is built,
   `updateRun` inside `emit()`, `finalizeRun` in the existing `finally`.
4. `captureUi(ctx)` at the top of `execute`; guarded `pi.on("session_shutdown",
   teardown)` near the other registrations.

No changes to watchdog, kill, drain, spawn, or process-group code. No new
files. No new load-time async. Ship as a PR; the user bakes it with `make load`.

## Deferred (the NEW 10% bet, gated separately)

True background dispatch (tool returns immediately, run keeps streaming into the
pin, results collected later) is deliberately out of this plan. It changes the
tool return contract and the watchdog ownership model, so it should be its own
gated change on top of a shipped proven+better pin.
