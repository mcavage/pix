# Runtime invariants (host binary + launcher)

The long-form rationale behind a handful of one- or two-line comments in
`services/host/*.go` and `services/host/cmd/pix/*.go`. It lives here because it is
paragraphs of reasoning about ORDERING and TRUST that never changes when the code
around it is edited — but deleting it outright would lose why the code is shaped
the way it is. Each section names the code that depends on it.

## 1. One store, one holder (`lock.go`, `memory.go`, `memory_plugin.go`)

Every live-serving memory entry point — `serve`'s supervised unit, the bare
`pix-host memory` daemon, the `plugin memory` self-exec, and `memory restore` —
takes the SAME exclusive, non-blocking advisory flock (`config.MemoryLockPath()`)
BEFORE it opens the sqlite store, and holds it for the process lifetime.

The lock, not a port probe, is the authority: the daemon opens the db before it
binds :11435, so a port probe leaves a TOCTOU window in which a restore would swap
the file out from under a live daemon. Failure to take the lock is fatal and the
store is never opened.

## 2. Restore commits last (`memory_snapshot.go`)

Under that lock, in this order: validate the snapshot with the queries the app
actually runs (a table merely NAMED `memories` passes `integrity_check`), move the
current db AND its `-wal`/`-shm` sidecars aside into a kept `.bak-<ts>-<token>`
set, then rename the staged copy into place LAST. Nothing fallible runs after that
rename. An earlier failure rolls the `.bak` set back and, if the rollback itself
fails, says so loudly — a silent rollback failure can leave no live db at all.

A snapshot is one plain sqlite file (`VACUUM INTO` against a read-only handle), not
an archive: `memory.db` is the only unreproducible piece of pix state, `config.toml`
is reproducible with `pix config set`, and `op-refs.env` holds `op://` pointers
rather than secrets.

## 3. Readiness is an application answer, not a dial (`identity.go`)

A TCP dial proves something holds a port. Every readiness verdict about memory goes
through the `identity` JSON-RPC method instead, whose `name`/`version` the launcher
matches EXACTLY, so a foreign listener (realistically a surviving daemon from an
older install) renders as "port held by an unidentified process" rather than ready.
A degraded-but-serving daemon (no embedder, keyword-only recall) is still `ready`
with the reason stated: reporting not-ready for a working service trains users to
ignore the probe.

## 4. Capture degrades honestly (`memembed.go`, `memory.go`)

`/api/show` proves a model is PRESENT, not that inference works — it answers
instantly while `/api/chat` is wedged. So a real watcher failure starts a backoff
window during which capture reports unavailable WITHOUT re-probing, and `observe`
answers `{accepted:false, reason}` instead of claiming a capture the watcher cannot
deliver. Captures run under a bounded semaphore: one sandbox-issued JSON-RPC batch
must meet backpressure rather than spawn thousands of goroutines and Ollama calls.

## 5. Plugin units inherit an allowlist, never the parent env (`serve_plugin.go`)

`pluginEnvAllow` is the complete set of names a supervised subprocess may see, so a
plugin never picks up cloud creds, API keys or the ssh-agent socket. A per-unit
secret arrives only through `launch`'s `EnvGrant`, which no sibling unit sees.
Pinning is enforced by supervise at both ends (`UnitSpec.Validate` before any spawn,
and a re-hash of the bytes staged on every start), so nothing here re-hashes a path
that could still change before the exec.

## 6. Create/attach ordering belongs to one function (`cmd/pix/run_cmd.go`)

`launch.RunSession` owns the ordering: lifecycle lock EX, a FRESH probe under it,
the child started, the create-time facts recorded (instance id, fingerprint, the
exact pi invocation), the refs SHARED reference taken while lifecycle is still
held, lifecycle released, and only THEN the session waited out. The command layer
owns stdio, the exit code and the words — never the ordering.

Two refusals ride on that: an attach whose recorded create-time fingerprint
diverges is REFUSED with recreate guidance rather than attached with an unverifiable
warning, and a bare positional launch (never the explicit `run` verb) on a
non-interactive terminal refuses before the parser runs — no create, no attach, no
side effect.

## 7. Retirement is an answer, not silence (`retired.go`, both binaries)

An unknown verb guesses by edit distance and cannot guess `mcp register` from
`slack`. A retired surface therefore keeps exactly one behaviour: print a
machine-greppable `PIX_RETIRED` line naming its replacement, exit 2, and do nothing
else — no config read, no daemon, no sandbox, no file. That is what makes hitting
one from a stale script or shell history safe. Every entry has an append-only record
in `cmd/pix/corpus/retirement.jsonl`.

## 8. Registration is host state, not session state (`cmd/pix/mcp_cmd.go`)

Registering an MCP server makes it known to the gateway; a session sees its tools
only once it was preloaded at create or attached live. `pix mcp load` records
nothing, because "pix loaded this once" is not the state of a live session — status
and doctor report host REGISTRATION and say so.
