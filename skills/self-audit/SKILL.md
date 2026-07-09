---
name: self-audit
description: System self-audit. Confirms the harness is healthy, keys, the memory service, the full agent roster, and the skills all load and respond. Use for "self-audit", "is everything working", "check the system", after a build or config change.
---
# self-audit

Run after a build or config change, or when something feels off. Call the actual
tools and report actual results. Do not assume success.

**Iron law: no step is optional. Skipping a step and marking it OK is a false signal.**

**Exercise, don't assume presence.** A thing on PATH, a registered MCP server, or
a listed tool is NOT proof it works. Every external dependency gets a live,
cheap, read-only call, and you report what came back. "Installed" is not "OK".
`command -v <x>` (never `which` — the DHI trixie base ships without it) tells you
a CLI exists; only running it tells you it works.

## 1. Environment

```bash
for k in ANTHROPIC_API_KEY OPENAI_API_KEY; do
  [ -n "${!k}" ] && echo "OK: $k" || echo "FAIL: $k missing"
done
[ -n "$GEMINI_API_KEY" ] && echo "OK: GEMINI_API_KEY" || echo "optional: GEMINI_API_KEY unset"
```

Anthropic and OpenAI are required (the model cycle and the cross-vendor `review`
agent). Gemini is optional. MCP servers are optional too: `mcp` reporting 0
servers is normal unless a kit wired some.

## 2. Memory service

```bash
curl -s -m3 -X POST "${MEMORY_URL:-http://host.docker.internal:11435}" \
  -d '{"jsonrpc":"2.0","id":1,"method":"stats"}' || echo "no memory service"
```

A stats response means recall and capture are live. "no memory service" means the
host has not started it (`make memory-serve` on the host); the harness still works,
recall is just empty.

## 3. MCP servers

List what's actually connected and how many tools:

```
mcp({})                       # server count + names
mcp({ server: "<name>" })     # tools for one server (e.g. "gateway")
```

Zero servers is fine unless a kit wired some. When a server IS present (e.g. the
sbx `gateway`), **smoke-test each backend behind it, not just the server.** The
gateway multiplexes several backends, grouped by tool-name prefix
(`gateway_opine__*`, `gateway_granola__*`, `gateway_<slack>_*`, BambooHR, …).
Group the tool list by prefix to enumerate the backends, then call **one cheap,
read-only tool per backend** and confirm a sane payload. Examples of good probes:
a `health` tool, a `get-organization` / `get_account_info`, or a `search`/`list`
with a tiny limit. Report each backend: name, ok or fail, one-line evidence.
Counting tools or probing a single backend is NOT enough — a registered backend
can still be unauthed or down.

## 4. CLIs

The harness leans on external CLIs (`gh` for ship/PRs, `gws` for Google
Workspace, plus overlay wrappers like `snow` for the warehouse). Discover which
are present, then **run a live read-only command for each** — presence on PATH
proves nothing:

```bash
for c in gh gws snow; do
  command -v "$c" >/dev/null 2>&1 && echo "present: $c" || echo "absent: $c"
done
```

For each present CLI, run a cheap live probe and read the output:

- `gh --version` (and `gh auth status` is expected to say "not logged in" in the
  sandbox — the proxy injects creds at the network layer, so that is NOT a fail).
- `gws drive files list --params '{"pageSize": 1}'` — a real row back proves the
  host-token auth path works.
- `snow connection test` — expect `Status OK`. Note wrapper CLIs often reject
  `--help`/`--version` (e.g. `snow` injects `--connection`), so probe with a real
  read-only subcommand, not the help flag.
`gh` is always baked; `gws`/`snow` are optional (overlay/host-token) — absent is
optional, not a failure, but present-but-erroring IS a failure.

## 5. Agent roster

List every agent the harness actually exposes (the real roster, not a hardcoded set):

```bash
ls ~/.pi/agent/agents/*.md 2>/dev/null | xargs -n1 basename | sed 's/\.md$//' | tr '\n' ' '; echo
```

Expect the three presets (fanout, deep, review) plus the specialists: architect,
engineer, designer, product-manager, qa-lead, security-lead, sre-lead, devrel,
dx-consultant, legal, finance-analyst, growth-marketing, ux-copywriter,
enterprise-admin. Report the full list, and flag any you expected but do not see.

Then smoke-test one agent per model tier in parallel via the `subagent` tool
(`{tasks:[{agent, task}, …]}`, a trivial task each, confirm a sane reply): a
haiku-tier role (`qa-lead` or `fanout`), an
opus-tier role (`architect` or `deep`), and the cross-vendor `review`. Report each:
role, model, ok or slow or fail. Three is enough to prove every model family and
the dispatch path; you do not need to invoke all fourteen.

## 6. Skills

```bash
ls ~/.pi/agent/skills | wc -l
```

Report the count (expect dozens) and spot-check that two or three have a non-empty
SKILL.md.

## 7. Tool routing

Confirm scoping holds: a read-only role (`fanout`, `qa-lead`) has no write/edit in
its `tools:`, and a builder (`engineer`) does. Read the frontmatter to check.

## Report

A table of each check with OK / FAIL / optional and a one-line note. Cover ALL
subsystems: keys, memory, MCP (per backend), CLIs (per CLI), roster, skills,
tool routing. Verdict is ALL CLEAR only when keys, the roster, skills, and every
present MCP backend and CLI are healthy. A missing memory service, Gemini key,
MCP server, or overlay CLI (`gws`/`snow`) is optional, not a failure — but a
present-but-erroring backend or CLI IS a failure. If you skipped a subsystem,
say so explicitly; do not imply coverage you didn't run.
