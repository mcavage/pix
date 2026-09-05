# monitor — REMOVED. What is left is a jsonl file.

> **HISTORICAL — pre-v2 design note.** This document predates the accepted
> Pix v2 surface and architecture (`docs/design/pix-v2-surface.md`,
> `docs/design/pix-v2-architecture.md`), which supersede it. Commands,
> files, and components described here may no longer exist. Nothing in it
> is a description of current behavior; read it as history only.


Status: **removed.** The subsystem this document used to specify — an
in-sandbox tap POSTing NDJSON to a host ingest listener on `:11437`, a
file-backed store, an eviction pass, and a `pix monitor` reader verb — is gone.
Owner: Mark.

## What remains

`extensions/monitor.ts` taps pi's turn/tool/model hooks and **appends NDJSON to
a file**. Nothing in pix reads it. There is no listener, no host service, no
store, no reader verb, and no doctor row.

    <dir>/<sandboxId>=<sessionId>/events.ndjson   one event per line
    <dir>/<sandboxId>=<sessionId>/blobs.ndjson    {hash,bytes,text} per line

`<dir>` is `$PIX_MONITOR_DIR`, default `<cwd>/.pix/monitor`. The workspace is
bind-mounted from the host, which is what makes those files readable outside the
sandbox at all. `PIX_MONITOR=0` disables the tap.

The layout and the event schema are **unchanged** from what the retired ingest
store wrote, so anything that could read the old store reads this. Be the
monitor with `tail -f`, `jq`, or a tool of your choosing.

## Why it went

The storage layer had already been simplified to NDJSON (`13c4989` replaced a
content-addressed sharded blobstore with `blobs.ndjson`), and the live TUI plus
the in-memory hub and ring buffer were already deleted (`bce2b1b`, ~3,150 LOC).
What survived was the part with no remaining justification: a network hop
between two processes on the same machine, and a reader for a format `jq`
already reads.

Everything the hop cost is now visible in what its removal deleted:

* **The transport.** `httpPostRaw`, plus `node:http`/`node:https`, plus the
  `:11437` entry in `pi-kit/spec.yaml`'s network allowlist — a hole in the
  sandbox's egress policy that existed only to reach it.
* **The delivery machinery.** A bounded queue with drop-oldest overflow,
  exponential backoff with a cap, a `disabledUntil` window, a retry timer, an
  abort-the-in-flight-request-on-shutdown dance, a bounded quit-flush that
  raced `drainAll()` against a deadline, and an injectable `RetryClock` seam
  built to make all of that testable. An append to a local file cannot be
  down, so none of it has anything left to do. `session_shutdown` needs no
  hook at all: every event is already on disk the moment it is emitted.
* **The host side.** `services/host/monitor/` (ingest, store, follow, event —
  ~1,200 prod LOC), the `pix monitor` verb, `health.MonitorProbe`, the
  `monitor` entry in `serveServiceAliases`, `config.MonitorStoreRoot`, and
  `serve`'s whole third shutdown phase: the ingest was a front door with its
  own drain, so `performShutdown` carried a `cancelMonitor` and a
  `waitMonitor`, and `runServe` held a `context.WithCancel` that existed for
  nothing else.
* **A permanently red row.** `pix doctor` probed `:11437` unconditionally, and
  `serve` only starts services named in `services`. A host with
  `services = ["memory"]` therefore reported `✗ monitor not running` forever,
  with `pix serve start` as the prescribed fix — a command that would not start
  it, because the config did not ask for it.

## If you want a monitor

Read the jsonl. `pi` also writes its own full session transcript to
`.pi-sessions/*.jsonl` in the same host-mounted workspace; between the two you
have the conversation and the provider/tool-level trace, in the same place, in
the same format, with nothing running.
