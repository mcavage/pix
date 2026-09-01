---
name: healthcheck
description: Two checks in one, HARNESS health (keys, memory, MCP, CLIs, agent roster, skills, routing) and CODE health (tests, types, lint, dead code, ranked fixes). Use for "healthcheck" or before `ship`.
---
# healthcheck

Run after a build or config change, when something feels off, or before shipping.
**Call the actual tools and report actual results. Do not assume success.**

Two parts. Run the one the user asked for; run both for a full check ("is
everything healthy?"). Lead with a one-line verdict per part, then the detail.

- **Part A, Harness health:** is *pix itself* working?
- **Part B, Code health:** is *this repo's code* in good shape?

---

## Part A, Harness health

**Exercise, don't assume presence.** A thing on PATH, a registered MCP server, or
a listed tool is NOT proof it works. Every external dependency gets a live, cheap,
read-only call, and you report what came back. `command -v <x>` (never `which` -
the DHI trixie base ships without it) tells you a CLI exists; only running it
tells you it works. No step is optional; skipping one and marking it OK is a
false signal.

### A1. Inference availability
The active session model answering a real turn is the primary evidence. A
runtime `inference.json` may exist when Pix synthesized custom providers, but
its absence is normal for a native provider and must not be called degraded.
`models-store.json` is Pi state, not proof that every listed provider is callable.
One working model is sufficient; every vendor is not required. `proxy-managed`
is a sentinel, not secret evidence. Exercise the current session plus the agent
smoke test in A5; do not fan out across every model.

### A2. Memory service
Memory is MCP through the sbx Gateway. Do not curl `host.docker.internal`, the
Gateway URL, or a guessed `/healthz`; the sandbox does not own the container's
loopback endpoint. Call `memory_status`, `memory_stats`, and one small
`memory_recall` instead. A successful zero-count response proves the service is
reachable and the store is empty.

`embed_model` is the service's built-in candidate name, not proof that the
selected environment configured an embedding backend. `embed_healthy:false`
means recall is keyword-only; by itself it does not make overall harness health
degraded. Call it a degradation only when independent environment evidence
proves the user configured or required semantic recall and that backend failed.
In that case point only to host-side `pix doctor`.
`capture_mode: explicit` is the normal default, not a failure.

### A3. MCP servers
```
mcp({})                    # server count + names
mcp({ server: "<name>" })  # tools for one server (e.g. "gateway")
```
Zero servers is fine unless a kit wired some. When one IS present (e.g. the sbx
`gateway`), **smoke-test each backend behind it, not just the server.** The
gateway multiplexes backends by tool-name prefix (`gateway_<name>__*`). Group the
tool list by prefix, call one cheap read-only tool per backend (a `health`, a
`get-*`, a `search`/`list` with a tiny limit), and report each: name, ok/fail,
one-line evidence. Prefer an identity/account/organization lookup when offered.
A successful representative call proves the backend is authenticated; a later
`permission denied` from a specialized or permission-gated tool means only that
capability is unavailable, not that the backend or OAuth is unhealthy. Report
it separately as optional/permission-gated unless the pack explicitly requires
that capability. A registered backend can still be unauthed or down.

### A4. CLIs
```bash
for c in gh $EXTRA_CLIS; do
  command -v "$c" >/dev/null 2>&1 && echo "present: $c" || echo "absent: $c"
done
```
For each present CLI, run a cheap live probe and read the output: `gh --version`
(`gh auth status` saying "not logged in" is expected, the proxy injects creds at
the network layer, NOT a fail). `gh` is always baked. Any environment-provided
wrapper CLI is optional: absent is fine, present-but-erroring on its cheapest
read-only probe is a failure. Wrappers may reject `--help`/`--version`, so probe
a real read-only subcommand when one is documented.

An environment-declared MCP server is checked like any other backend in A3, and
a tool list is not evidence. Call one cheap read-only tool with a tiny limit and
read the result. A credential failure appears on the call, not the list.

If expected tools are missing, that is host/environment state. The user runs
`pix doctor` on the host to check the selected environment and Gateway
registration. A changed MCP declaration reaches a new sandbox after the normal
proof-gated recreate; there is no live attach.

### A5. Agent roster
```bash
ls ~/.pi/agent/agents/*.md 2>/dev/null | xargs -n1 basename | sed 's/\.md$//' | tr '\n' ' '; echo
```
Expect the presets (fanout, deep, review) plus the specialists (architect,
engineer, designer, product-manager, qa-lead, security-lead, sre-lead, devrel,
dx-consultant, legal, finance-analyst, growth-marketing, ux-copywriter,
enterprise-admin). Smoke-test three representative agents in parallel through
the `subagent` tool with trivial tasks. Report each role, resolved model, and
ok/slow/fail. Do not claim fixed model tiers: v2 resolves an explicit agent
model, then the environment's agent mapping, then the selected main model.
Do not launch the whole roster.

### A6. Skills + tool routing
```bash
ls ~/.pi/agent/skills | wc -l
```
Report the count (expect dozens); spot-check two or three have a non-empty
SKILL.md. Then confirm scoping holds: a read-only role (`fanout`, `qa-lead`) has
no write/edit in its `tools:`, a builder (`engineer`) does, read the frontmatter.

### Part A report
A table per check: OK / FAIL / optional + a one-line note, covering inference, memory,
MCP (per backend), CLIs (per CLI), roster, skills, routing. Verdict is ALL CLEAR
only when inference, roster, skills, and every present MCP backend + CLI are healthy. A
missing alternate model vendor or optional environment integration is fine; a
present-but-erroring backend or CLI IS a failure. If you skipped a subsystem, say
so; do not imply coverage you didn't run.

---

## Part B, Code health

Run every detectable quality check, score it, compare to history, emit a compact
dashboard with the highest-impact fixes first.

### Detection (auto-detect from project files; SKIPPED, not CRITICAL, when a tool genuinely isn't present, never invent one)
| Category | Weight | Detect via |
|---|---|---|
| Tests | 30% | `package.json` scripts, `pytest.ini`, `go.mod`, `Cargo.toml` |
| Type check | 22% | `tsc`, `pyright`, `mypy`, `cargo check` |
| Lint | 18% | `eslint`, `ruff`, `flake8`, `golangci-lint`, `cargo clippy` |
| Dead code | 15% | `knip`, `ts-prune`, `vulture`, `deadnix` |
| Shell lint | 10% | `shellcheck` (only if shell scripts exist) |
| Other | 5% | formatting drift, schema validation, generated-file drift |

Composite = sum of (score × weight), rounded to 1 decimal; exclude SKIPPED from
the denominator.

### Scoring
10 clean · 8-9 pass with minor warnings · 5-7 localized/easy failures · 2-4 major
failures or blocked workflow · 0-1 broken/dangerously noisy. Failing tests on
critical paths score below lint failures at the same volume; missing type
coverage in a typed project scores below 6.
Status labels: CLEAN (9-10), WARNING (7-8.9), NEEDS WORK (4-6.9), CRITICAL (0-3.9).

### Steps
1. Detect tools; print what was found and skipped.
2. Run each detected tool once. Capture command, exit code, duration, key output.
3. Score each category. Never hide a failure.
4. Read `data/health-history.jsonl` if present; find the run closest to 7 days
   ago (or the most recent earlier run) for trend comparison.
5. Emit the dashboard. 6. Append one JSONL record.

```
HEALTH DASHBOARD
Repo: <name>  Branch: <branch>  Commit: <sha>
Overall: 8.3/10  WARNING  (+0.7 WoW)
Tests: 9.0 CLEAN   Type check: 8.5 WARNING   Lint: 6.0 NEEDS WORK
Dead code: 5.5 NEEDS WORK   Shell lint: 10.0 CLEAN   Other: 8.0 WARNING
What ran: …   Trends: …   Recommendations (by impact): …
```
Trend language: Up (>= +0.5), Flat (< 0.5), Down (>= -0.5); only call out
meaningful deltas. History record:
```json
{"timestamp":"ISO-8601","repo":"name","branch":"branch","commit":"sha","scores":{"tests":8.5,"typecheck":10,"lint":7,"deadcode":6,"shelllint":10,"other":8},"overall":8.3}
```

### Recommendations (sort by impact)
(1) broken tests on changed paths, (2) type-safety regressions, (3) high-volume
lint that hides signal, (4) dead code with maintenance drag, (5) shell issues in
release flows, (6) nice-to-have cleanup. Each: what it is, why it matters, scope,
expected score lift.

### Guardrails
Do not claim CLEAN when major categories failed. Do not penalize unused
categories. Do not bury a CRITICAL finding under a strong composite. Prefer
evidence over general statements.

---

## Closing
Per part you ran, three lines: current state (one sentence), top fix
(highest-impact next step), risk (low/medium/high with reason). When code health
is low, suggest `debug` for root causes or `tdd` to rebuild coverage; when it's
strong, `ship` is the natural next step.
