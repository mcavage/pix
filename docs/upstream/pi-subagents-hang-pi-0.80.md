# pi-subagents 0.13.0: a spawned subagent never completes on pi 0.80.x (get_subagent_result hangs forever)

**Extension:** `@tintinweb/pi-subagents@0.13.0` (latest on npm as of 2026-07) · **Host:** pi `0.80.3` **and** `0.80.5` (both reproduce identically) · **Platform:** Docker Sandboxes (sbx 0.33) on a Docker Hardened Debian base, macOS arm64.

> Confirmed on both pi 0.80.3 and 0.80.5 (latest): identical hang, `exit 124` after a 140s test timeout, while a bare model call on the same sandbox returns instantly. A downstream wall-clock bound on the wait (a `Promise.race` timeout wrapper around the extension's `await record.promise`) does **not** fire — the run consumed the full 140s, not the 90s bound — which suggests the blocking wait is not that `await`, or the event loop is starved such that timers don't run.

## Summary

A subagent spawned via the `Agent` tool **never produces a result**. The follow-up `get_subagent_result(agent_id, wait: true)` blocks forever — the subagent's promise never settles. It is **not** a parallelism, model, or network problem: it reproduces with a **single** subagent, using the same model (`anthropic/claude-opus-4-8`) that answers a direct prompt instantly in the same sandbox. Because `get_subagent_result` parks the event loop, the pi TUI becomes unresponsive (Esc does not cancel the in-flight tool call) and the only recovery is killing the process from the host.

## Minimal repro

In a sandbox with the extension installed and any working provider:

```bash
# [1] baseline — a bare model call returns instantly:
pi -p "Reply with exactly the word OK and nothing else."
#   -> OK   (exit 0)

# [2] one subagent — hangs until killed:
timeout 140 pi -p --mode json "Spawn exactly one subagent with the deep preset \
  whose only task is to output OK. Then call get_subagent_result to wait for it \
  and report what it returned. Only one agent."
#   -> hangs; timeout kills it at 140s (exit 124)
```

(The `deep` preset here is just `model: anthropic/claude-opus-4-8`, read-only. Any preset reproduces it.)

## Evidence from the `--mode json` stream of [2]

- The `Agent` tool call **completes** (`tool_execution_end`) and returns an `agent_id` — the spawn itself works.
- The subsequent `get_subagent_result` emits `tool_execution_start` and then **nothing** — no matching `tool_execution_end`, no error, no result, through the 140s timeout.
- Across the whole run there is exactly **one** `tool_execution_end` for **two** `tool_execution_start`s.
- No separate subagent session/artifact is created under `--session-dir`; the spawned agent appears never to execute a turn of its own.
- The bare call in [1] proves the provider/model/gateway path is healthy in the same sandbox.

So: the agent is registered on spawn, but its run never drives to completion, and `get_subagent_result(wait:true)` waits on a promise that never resolves.

## Environment notes that are probably not the cause

- Reproduces with a **single** agent → not concurrency / a parallel-connection limit.
- Reproduces on `anthropic/claude-opus-4-8`, which works fine for the top-level agent → not the model or credentials.
- `--models` is only the Ctrl+P cycle; the preset models (`haiku-4-5`, `sonnet-4-6`, `opus-4-8`) are all present in `pi --list-models` → not a model-registration gap.

## What would help

- Confirmation of the pi version range `@tintinweb/pi-subagents@0.13.0` is expected to run against. Its `peerDependencies` floor is `@earendil-works/pi-* >=0.74.0`; if the subagent-execution API changed between 0.79 and 0.80.3, the extension may spawn an agent it can no longer drive.
- Failing that, a fix so the spawned agent's turn loop actually runs (or so `get_subagent_result` rejects instead of hanging when the agent can't be driven).

## Downstream mitigation (pi-stack) — RESOLVED by a first-party replacement

We no longer depend on this extension. `@tintinweb/pi-subagents` stays disabled in
the image, and pi-stack now ships its own `extensions/subagents.ts` (a `subagent`
tool with single/parallel/chain modes and depth-capped trees). See
`docs/design/subagents-extension.md`.

While building the replacement we found a second, stack-specific root cause that
would bite *any* subprocess-based subagent here (including pi's own stock
`subagent` example): a child `pi` that loads the full extension set re-runs
`ollama-bridge` / the memory extensions, which `await server.listen(<port>)`. The
parent already holds those ports, so the child's `listen` throws `EADDRINUSE`, the
error is swallowed, the factory promise never resolves, and the **child pi
deadlocks at startup before running a turn** (verified: silent exit, ~280ms, zero
output). Our extension avoids it by spawning every child with
`--no-extensions -e <self>` (no port-binding extensions; the subagent tool itself
is re-added explicitly so trees still work), plus an inactivity + wall-clock
watchdog that kills and reports a stuck child instead of hanging. `/subagents
doctor` runs a live end-to-end self-audit — the check that never passed before.
