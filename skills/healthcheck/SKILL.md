---
name: healthcheck
description: Check Pix itself with live probes of inference, memory, integrations, CLIs, and agent routing. Use for a self-check or harness healthcheck.
---
# healthcheck

Check whether Pix itself is working: inference, memory, integrations, and agents.
Call the actual tools and report actual results. Do not run repository quality
checks here; use `repo-healthcheck` when the user asks for code health.

**Exercise, don't assume presence.** A thing on PATH, a registered MCP server, or
a listed tool is NOT proof it works. Every external dependency gets a live, cheap,
read-only call, and you report what came back. `command -v <x>` (never `which` -
the DHI trixie base ships without it) tells you a CLI exists; only running it
tells you it works. No step is optional; skipping one and marking it OK is a
false signal.

### 1. Inference availability
The active session model answering a real turn is the primary evidence. A
runtime `inference.json` may exist when Pix synthesized custom providers, but
its absence is normal for a native provider. **NEVER mark a missing
`inference.json` degraded and NEVER recommend configuring an inference roster
for a native provider.**
`models-store.json` is Pi state, not proof that every listed provider is callable.
One working model is sufficient; every vendor is not required. `proxy-managed`
is a sentinel, not secret evidence. Exercise the current session plus the agent
smoke test in step 5; do not fan out across every model.

### 2. Memory service
Memory is MCP through the sbx Gateway. Do not curl `host.docker.internal`, the
Gateway URL, or a guessed `/healthz`; the sandbox does not own the container's
loopback endpoint. Call `memory_status`, `memory_stats`, and one small
`memory_recall` instead. A successful zero-count response proves the service is
reachable and the store is empty.

Call `memory_status` again after recall so the report reflects an exercised
embedding backend. `embed_healthy:false` means an embedding request failed:
report semantic recall as degraded, with keyword recall still available. Do not
assume embeddings were intentionally disabled because sandbox configuration is
absent; the host wires Ollama independently of the environment's chat models.
`embed_healthy:null` means unverified, not healthy. Host-side `pix doctor` is a
diagnostic, not a repair command; `pix setup` reconciles memory's host wiring.
`capture_mode: explicit` is the normal default, not a failure.

### 3. MCP servers
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

### 4. CLIs
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

An environment-declared MCP server is checked like any other backend in step 3, and
a tool list is not evidence. Call one cheap read-only tool with a tiny limit and
read the result. A credential failure appears on the call, not the list.

If expected tools are missing, that is host/environment state. The user runs
`pix doctor` on the host to check the selected environment and Gateway
registration. A changed MCP declaration reaches a new sandbox after the normal
proof-gated recreate; there is no live attach.

### 5. Agent roster
```bash
ls ~/.pi/agent/agents/*.md 2>/dev/null | xargs -n1 basename | sed 's/\.md$//' | tr '\n' ' '; echo
```
Expect the presets (fanout, deep, review) plus the specialists (architect,
engineer, designer, product-manager, qa-lead, security-lead, sre-lead, devrel,
dx-consultant, legal, finance-analyst, growth-marketing, ux-copywriter,
enterprise-admin). Smoke-test three representative agents in parallel through
the `subagent` tool with trivial tasks. Report each role, resolved model, and
ok/slow/fail. Use the runner's `model observed` metadata in the tool result,
which comes from the child's actual response events. Do not ask children to
identify themselves or run `env`; neither is needed to verify a read-only role.
Compare the observed model with the roster and report match/mismatch. If response
metadata is absent, say the check could not verify routing; never substitute a
different role and infer the original passed. Do not claim fixed model tiers: v2 resolves an explicit agent
model, then the environment's agent mapping, then the selected main model.
Agents resolving to the same vendor or model as the parent is the documented
fallback, not degraded. A retired model is always **FAIL**. When there is no
explicit agent model or environment roster, a child resolving to a different
model than the parent is also **FAIL**: inheritance broke and Pi selected an
unrelated fallback. Otherwise call routing degraded only when an authored
explicit agent model or environment `[agents]` mapping fails to resolve. Do not
launch the whole roster.

### 6. Skills + tool routing
```bash
ls ~/.pi/agent/skills | wc -l
```
Report the count (expect dozens); spot-check two or three have a non-empty
SKILL.md. Then confirm scoping holds: a read-only role (`fanout`, `qa-lead`) has
no write/edit in its `tools:`, a builder (`engineer`) does, read the frontmatter.

### Report
A table per check: OK / FAIL / optional + a one-line note, covering inference, memory,
MCP (per backend), CLIs (per CLI), roster, skills, routing. Verdict is ALL CLEAR
only when inference, roster, skills, and every present MCP backend + CLI are healthy. A
missing alternate model vendor or optional environment integration is fine; a
present-but-erroring backend or CLI IS a failure. If you skipped a subsystem, say
so; do not imply coverage you didn't run.
