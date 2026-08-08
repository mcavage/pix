# Runbook — pix host services (`pix serve`)

For the on-call engineer (that is: you, on your own laptop) who just saw
recall stop working, `pix doctor` go red, or `serve` refuse to start. Read the
top section to know what "healthy" means, then jump to the alert you have.

Everything here is grounded in code you can read:
`services/host/serve.go`, `services/host/serve_plugin.go`,
`services/host/supervise/{tree,service,report}.go`,
`services/host/service/ctl.go`.

---

## 1. What runs, and what depends on it

| unit | owner | listener | blast radius when it dies |
| --- | --- | --- | --- |
| `memory` | Suture tree, go-plugin subprocess (`pix-host plugin memory`) | none of its own — `serve` owns `:11435` and proxies (`memoryProxyMux`) | recall/remember in EVERY sandbox session fails fast; the session keeps working without memory |
| monitor ingest | composed directly in `serve` (not a supervised unit) | `:11437`, loopback by default | in-sandbox tap events are dropped; nothing else notices |
| pack `[[services]]` units | Suture tree, one go-plugin subprocess each | per pack | only that pack's capability |

The load-bearing property: **the child can die, the listener cannot.** `serve`
holds `:11435` for the whole process lifetime and dispenses a plugin client per
request, so a memory restart is a few failed calls, not a connection refused
and not a sandbox that has to be recreated. That property is asserted by
`TestMemoryListenerSurvivesUnitRestart`
(`services/host/serve_memory_restart_test.go`), which kills the real child with
SIGKILL and proves bound / fail-fast / recover.

## 2. SLIs, SLOs, error budget

Four golden signals, mapped onto what this host actually exposes. Every SLI
below is readable from `pix serve status --json` or `pix doctor --json`
(`supervisor` object) — no log grepping, no guessing.

| signal | SLI (how it is measured) | SLO | where |
| --- | --- | --- | --- |
| **Errors** | fraction of `:11435` JSON-RPC calls returning an RPC error, measured as: unit `state == running && health_ok` sampled every 5s | ≥ 99% of 5s samples healthy over 7 days (≈ 100 min/week of budget) | `units[].state`, `units[].health_ok` |
| **Latency** | health-probe wall time, `last_probe_us` | p95 < 250 ms; a probe over the 3 s `HealthTimeout` counts as a failure | `units[].last_probe_us` |
| **Traffic** | recall/remember calls per session — proxied by monitor ingest event counts | no target; it is the denominator for the error rate | `pix monitor --json` |
| **Saturation** | restarts per hour, `units[].restarts` deltas; plus generation churn | < 1 restart/day; 3 restarts in 10 min is the flap threshold | `units[].restarts`, `units[].generation` |

Supporting availability SLI: **snapshot freshness.** `serve` republishes the
supervision snapshot every 5 s (`unitsReportInterval`). A snapshot older than
20 s (`unitsStaleAfter`) is reported as *stale* and never as healthy — if
status says `units: unknown`, you have lost observability, which is its own
(sev-low) incident, not an all-clear.

**Error budget policy.** Burn the weekly budget and the next change to
`services/host/supervise/` is a reliability fix, not a feature. The budget is
deliberately small: this is one user's laptop, and a memory outage costs
context, not revenue — the point of the number is to stop "it restarts, it's
fine" from becoming the norm.

**What is NOT measured, and why.** There is no per-request latency histogram
for `:11435`: the proxy does not instrument individual calls, so the probe
latency is a leading indicator, not a user-facing latency SLI. If you need the
real one, that is the next piece of work, not something to infer from
`last_probe_us`.

## 3. Reading the signals

```bash
pix serve status            # human: pid, memory port up/down, one line per unit
pix serve status --json     # machine: units[] with the SLI fields below
pix doctor --json | jq .supervisor
```

Unit fields (`supervise.UnitReport`), and what each is FOR:

- `identity` — the sha256 admission fingerprint of the unit spec. A reattach
  must match it. **If this changed and you did not change config or a pack,
  something re-specced the unit.** Env grant *values* enter as digests only, so
  this is safe to paste in a bug report.
- `state` — `starting | running | degraded | backoff | stopped | failed`.
  `degraded` means it is up but failing probes; three in a row and it is
  replaced.
- `generation` / `restarts` — generation 1 with 0 restarts is a unit that has
  been up since `serve` started. Rising restarts is the flap signal.
- `reattached` — true when this generation ADOPTED a surviving child instead of
  spawning one, i.e. `serve` restarted but memory did not.
- `last_error` — the most recent failure, scrubbed (`supervise.ScrubError`
  redacts `*_TOKEN=`/`Bearer `/`op://` shapes). It is a diagnosis, not a secret
  store.
- `last_probe_us` — the last health probe's wall time in microseconds.

## 4. Alerts → response

Incident command applies even solo: decide the mitigation FIRST, investigate
second. Restoring memory takes under a minute; do it, then root-cause.

### A. `pix serve status` says `memory (:11435): down`

1. Is serve running at all? `pix serve status` first line. If not running:
   `pix serve` (foreground) or `pix serve install` (managed).
2. Running but the port is down → the listener died with `serve` alive, which
   should be impossible (`serve` exits on a listener error). Check
   `~/.local/state/pix/serve.log` for `serve: fatal:`.
3. Port bound by something else: `lsof -nP -iTCP:11435 -sTCP:LISTEN`. `serve`
   fails fast on a bound port on purpose — two daemons splitting recall traffic
   is worse than none.

### B. `units: unknown (...)` in status

You have lost the supervision snapshot, not necessarily the service.

- `no supervision snapshot` → this `serve` predates the surface, or the state
  dir is unwritable. Check `ls -l ~/.local/state/pix/serve.units.json` and
  `serve.log` for `cannot publish unit status`.
- `belongs to pid N` → a previous daemon's snapshot survived. Stop cleanly
  (`pix serve stop` — mode-aware; never `kill -9` a managed daemon, launchd's
  `KeepAlive` respawns it instantly) and restart.
- `is Ns stale` → serve is wedged. Grab a stack (`kill -QUIT` writes a Go
  traceback to `serve.log`) BEFORE restarting; a wedged supervisor with no
  traceback is an incident you get to have twice.

### C. `state: degraded` or rising `restarts`

1. Mitigate: `pix serve stop && pix serve` (or `pix serve install` to restart
   the managed service through launchd).
2. Diagnose from `last_error` and `serve.log`'s `supervise: memory ...` lines.
   Common causes, in the order they actually happen:
   - SQLite file locked or on a full/backed-up volume → memory's advisory lock
     fails at start. Check disk, check for a second `pix-host memory`.
   - Ollama down while `MEMORY_EMBED_MODEL` is set → probes exceed the 3 s
     `HealthTimeout`. `last_probe_us` near 3 000 000 is this fingerprint.
   - An external `[plugins.memory]` binary whose SHA no longer matches → the
     unit refuses to start at all (by design; re-pin it deliberately).
3. If Suture gives up (`state: failed`, `do_not_restart`), it exceeded
   `FailureThreshold` 5 within the decay window. That is a real bug: capture
   `serve.log` and the snapshot before restarting.

### D. `serve` will not start

`serve` fails LOUDLY at startup rather than serving a half-tree: an unhealthy
first generation, an unknown service name, or an unwritable state dir all exit
1 with the reason. Read the last three lines of `serve.log` — they name it.

## 5. Recovery, in escalating order

```bash
pix serve status --json        # 1. observe, do not act blind
pix serve stop                 # 2. mode-aware stop (launchd/systemd via supervisor)
pix serve                      # 3. foreground restart; watch the supervise: lines
pix serve install              # 4. reinstall the managed service
pix doctor --verbose           # 5. evidence for every check, not a verdict
```

Memory data lives in the DATA dir, not the state dir: restarting `serve`,
removing `serve.units.json`, or reinstalling the launchd job never touches it.
`pix memory` is the surface for the store itself.

## 6. Toil ledger

Things in this runbook that a human should stop doing by hand, in priority
order. Each is a step that recurs; a step that recurs is automation waiting to
be written.

1. Flap detection is manual (compare `restarts` between two `status` calls).
   The snapshot has the fields; nothing alerts on them.
2. `lsof` for a port squatter should be part of `pix doctor`'s memory check.
3. Stale-snapshot cleanup after an unclean kill is manual (`serve` removes it
   on a clean shutdown only).

## 7. Postmortem

Any unplanned memory outage over 15 minutes, or any `state: failed`, gets a
written review: timeline, contributing factors, and the system fix. Blameless
by construction — everything here is a process or a program, and if a person
had to remember a step, the missing automation is the finding.
