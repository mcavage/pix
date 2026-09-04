# AGENTS.md: pix

You're working on **pix v2**: a thin, personal launcher that runs the pinned
**pi** coding agent inside a **Docker Sandbox** (`sbx`), using a native
`.sbxenv.yaml` environment and the **sbx MCP Gateway** as the only
sandbox-facing integration path. This file is the harness's memory: read it
before changing things, and keep it current as you learn.

The accepted design is `docs/design/pix-v2-surface.md` (product surface) and
`docs/design/pix-v2-architecture.md` (code target, deletion ledger, host UAT
gate). Read both before making an architectural change; this file summarizes,
it does not replace them.

## What pix is, in one paragraph

`pix` is the only host binary and the only user-facing CLI. It resolves a
named environment (a directory under `~/.pix/envs`, PIX_HOME's default),
compiles it into one effective native sbx document, and runs `sbx env create`
/ `sbx exec` to launch the pinned **pix-agent** image. Memory is a separate
**pix-memory** Streamable HTTP MCP service running as one Docker container,
reached only through the sbx Gateway, never a direct sandbox connection.
There is no pack system, no model router, no `pix-host`, no resident daemon,
and no XDG path split: everything user-owned lives under `PIX_HOME`.

## Repo layout

| path | what |
| --- | --- |
| `services/host/` | the Go module that builds `pix` (`cmd/pix` is the launcher's dispatch tree; `pixhome`, `sandbox`, `session`, `launcher`, `hosttrust`, `secret`, `mcp`, `envinfo`, `recreatelog`, `container`, `health` are the domain packages). This is on its way to the target `cmd/`/`internal/` split in `docs/design/pix-v2-architecture.md` §3; treat that document as the ownership map, not a requirement to move a cohesive package just to match a diagram. |
| `services/memory/` | **`pix-memory`**: an independent Go module and Dockerfile. One Streamable HTTP MCP endpoint (`/mcp`) plus `/healthz`. Built and tagged separately from the agent image; `pix setup` reconciles it as a single named Docker container (`unless-stopped`, loopback-published, `~/.pix/.state/memory:/data` mounted). |
| `images/agent/Dockerfile` | **`pix-agent`**: the DHI Node/Debian sandbox image, containing the pinned pi build, core extensions, patches, and the entrypoint. Consumers pull this by name; `make load` builds and loads a local copy for dev. |
| `pi-kit/spec.yaml` | the strict kit-spec v2 the sandbox launches with: image ref, entrypoint, `credentials[]` (multi-model proxy), `permissions.network.allow`, `setup`. |
| `settings.json`, `keybindings.json`, `themes/` | shipped Pi defaults, mounted into every sandbox as `~/.pix/pi/*`. |
| `agents/*.md` | subagent presets (orchestration + role crew), mounted read-only unless a user forks one into their own `~/.pix/agents/`. |
| `skills/<name>/SKILL.md` | Agent Skills, same mount/fork rule as agents. |
| `extensions/*.ts` | pi extensions baked into `pix-agent` (memory-recall bridge, status, timestamps, subagents, ollama-bridge, and the rest). |
| `pix.toml` (in an environment) | the only sidecar file Pix reads besides `.sbxenv.yaml`: model/agent bindings, environment-local skill/agent paths, memory scope, and per-MCP-server credential/probe annotations. It cannot declare anything native sbx already owns. |

## PIX_HOME

Every user-owned Pix file lives under one root, default `~/.pix`, overridable
with `PIX_HOME` (no XDG config/data/state/cache split, ever):

```text
~/.pix/
  .git/  README.md
  config.toml            # sparse, explicit machine choices only
  secrets.env            # NAME=op://... references only, mode 0600
  envs/<name>/           # .sbxenv.yaml + optional pix.toml, a plain directory
  context/               # personal AGENTS.md, skills/, output-styles/
  runtime/<pix-version>/ # shipped skills + agents + Pi files, not user-edited
  .state/
    release.json
    effective/<sandbox>/effective.sbxenv.yaml
    memory/{memory.db,backups/,auth.token,port}
    sandboxes/<sandbox>/{record.json,fingerprint.json,invocation.json,keep.json,lifecycle.lock}
    sessions/<tree-id>/{tree.json,nodes/<node-id>.json}
    tasks/<repo-key>/{meta/,co/}
    trust/{creation-hmac.key,environments/<name>.json}
```

`config.toml` and `secrets.env` are the only two files with a single named
writer each (`pix env default NAME`, `pix setup`, `pix secret set`); there is
no generic `config get/set` verb in v2. `~/.pix` is initialized as an ordinary
`git init -b main` repo at first run; Pix never stages or commits on the
user's behalf. State-holding files are mode `0600`, their parent dirs `0700`.

## Coexistence: one PIX_HOME = one stack

Two Pix installations can share a host. A **stack id** (the first 16
lowercase hex characters of sha256(canonical `PIX_HOME` path)) suffixes or
prefixes every Pix-owned runtime resource, always, with no unscoped
fallback:

| resource | name |
| --- | --- |
| sandbox | `pix-<stack-id>-<basename>-<workspace-digest>` (an explicit `--name` is scoped too, and is refused rather than truncated when it does not fit) |
| memory container | `pix-memory-<stack-id>` |
| memory MCP server | `pix-memory-<stack-id>` |
| session MCP server | `pix-session-<stack-id>` |

`services/host/stack` is the single producer of the id and of every name
derived from it; a malformed id is an error there, never a bare
`pix-memory`/`pix-session`/`pix-<basename>`. The **MCP registry stays
host-global** (it is the sbx Gateway's, not Pix's): the built-ins simply
register under namespaced names, so two homes coexist in one registry.

Stack scoping prevents accidental collisions between two homes; it is not a
confidentiality boundary. The Gateway registration's URL carries that stack's
memory bearer token (sbx cannot express a secret header yet) and sbx's
registry is a host-global, same-user store, so another process running as the
same user can read it. Say that plainly in docs; never imply scoping isolates
one home's memory from another process under the same login.

Each home allocates its **own loopback memory port** (`memory_port` in that
home's `config.toml`, written by `pix setup`); every reader takes it from
there. Cleanup (`pix rm --all`, `pix rm --orphans`, `pix reset`) discovers
only sandboxes carrying the current stack's id.

**Credentials are sandbox-scoped.** `pix setup` creates
`$PIX_HOME/secrets.env` (`op://` references only); every create AND every
attach re-resolves those refs and writes them with
`sbx secret set -f --sandbox <name> <service>`, the value on stdin and never
in argv, so a rotation lands on the next run. **Host-global sbx secrets are ignored and never removed
automatically**. `pix doctor` grades the provider row off this home's refs
and reports globals separately, as ignored.

**Version identity.** Every launch carries two Pix-managed environment
facts, `PIX_LAUNCHER_VERSION` and `PIX_STACK_ID`, and the session
fingerprint carries `launcher_version`. Those two composed env keys are the
only recreation-safe ones, so a version bump takes the existing proof-gated
auto-recreate path while any other env drift still refuses. A **local build
is `X.Y.(Z+1)-beta.g<sha7>[.dirty.<12hex>]`** and the binary, runtime
archive, manifest and both local image tags share it; **release CI publishes
the clean semver**. `make load` scopes its unique tag and its prune to a
hash of the canonical worktree path, and `make run` derives its sandbox name
instead of pinning one.

## Command surface

Nine groups, plus help/version: `run`, `ls`, `rm`, `task {new,ls,path,rm}`,
`env {list,show,default,trust}`, `secret {list,set,rm,check}`, `setup`,
`doctor`, `reset`. `pix help --all` is the generated source of truth;
`docs/reference.md` §0 is the live capability map. **Removed verbs get the ordinary
unknown-command answer, never a retirement notice**: pix has no released
users to keep a migration path for. There is no `mcp`, `models`, `config`,
`agent`, `pack`, `serve`, `resume`, `status`, or `uat` verb, and none of
those names route anywhere.

An environment is a plain directory (`.sbxenv.yaml` + optional `pix.toml`);
there is no registration database and no `add`/`edit`/`use`/`forget`
mutation path. Create, clone, move, and remove one with ordinary filesystem
and Git tools. `pix env trust NAME` is the explicit host-execution approval
gate; `pix env default NAME` is the one writer of the machine default.

## Native sbx environments and the Gateway

The only sandbox declaration is native `.sbxenv.yaml`; Pix does not invent a
second environment grammar. One pure compiler produces the *effective*
document (adds the pinned agent/kit/workspace/memory-endpoint/session-control
facts, rejects literal secrets and unpinned host execution, emits a canonical
fingerprint) used by both `pix env NAME --effective` preview and `pix run`
create.

The **sbx MCP Gateway is the only integration path visible inside a
sandbox**. `mcp.servers` in `.sbxenv.yaml` performs host-global registration
and attachment; Pix neither runs a second MCP registry nor spawns those
processes itself. A same-name registration at a different endpoint/kind
refuses launch rather than being silently overwritten. Memory is registered
this way too: `pix-memory` is a regular remote MCP server from the Gateway's
point of view, reached over loopback, never dialed directly by the sandbox.

## Build, load, run

```bash
cd services/host && go build ./... && go test ./...   # the launcher module
cd services/memory && go build ./... && go test ./...  # pix-memory
npm test            # node --test tests/*.test.mjs
tsc --noEmit
bash scripts/gate.sh # the fast PR gate CI runs (build, vet, go test, node test, tsc, open-core, ...)
```

- **Image or baked files** (Dockerfiles, settings, keybindings, agents/skills/
  extensions/themes) need `make load` (build + `docker save` + `sbx template
  load`) before a NEW sandbox sees them; `sbx` has its own image store, so a
  local build is invisible until loaded. **You cannot run `make load`/`make
  run` from inside a pix sandbox**: they need the host's Docker + `sbx` CLI.
  Edit the source, sync into the live `~/.pi/agent/...` tree, `/reload` for
  this session, and tell the user to `make load` on their host for future
  sandboxes.
- **Kit only** (`pi-kit/spec.yaml`) applies at sandbox **creation**: just
  `make run` (or `pix run`) a fresh sandbox, no rebuild.
- **`make run` loads skills LIVE** from the host tree (`--no-skills --skill
  <repo>/skills`), so editing a `SKILL.md` + `/reload` is instant; no rebuild.
  A published consumer run gets the baked-in set instead.
- A running sandbox keeps its creation-time image; recreate (`pix rm NAME &&
  pix run`) to pick up an image change.
- **Testing:** use a separate `PIX_HOME` for each stack. `make run` leaves
  naming to the launcher; `NAME=test` is a short logical override that expands
  inside the current stack namespace.

## Safety invariants

Load-bearing properties, each pinned by a real test. Preserve every one when
you touch the surface it names.

1. **PIX_HOME is the single root, with no XDG split.** `$PIX_HOME` when
   set, else `~/.pix`, and nothing else (not `$XDG_CONFIG_HOME`, not
   `$XDG_DATA_HOME`) influences where Pix resolves its files.
2. **`config.toml` and `secrets.env` each have one named writer.** There is
   no generic config mutation command in v2; a field changes only through
   the verb that owns it (`pix env default`, `pix setup`, `pix secret set`).
3. **An implicit launch requires a TTY.** Bare interactive `pix` runs setup
   first when this `PIX_HOME` has no config, then behaves as `pix run .` only
   after setup succeeds. Non-interactive stdin never creates or attaches a
   sandbox and never mutates host state as a side effect of a script or pipe.
4. **An existing sandbox is never force-removed or replayed into.** `pix rm`
   verifies the current sbx instance ID before removal; unknown sbx state
   fails closed. `--force` is a named-sandbox override only, and it never
   widens the `pix-*` namespace or authorizes removing a non-Pix sandbox.
5. **`pix rm` is scoped to `pix-*` sandboxes only**; it can never reach a
   sandbox it did not create.
6. **A process claims liveness only by holding a reference lock bound to the
   recorded sbx instance ID**, never by a bare PID. The lock releases itself
   on normal exit, signal, or crash; PIDs remain human diagnostics only.
7. **`pix rm --orphans` requires five positive proofs**, not an absence of
   evidence: a fresh sbx listing, a `pix-*` name, a matching instance ID,
   zero reference locks, and no keep marker. Any unknown answer preserves
   the sandbox.
8. **Direct provider keys come from 1Password only** (`op://` references,
   never resolved to disk or stdout); missing `op` is fatal only when the
   selected environment needs direct key resolution. Keyless and
   Gateway-authenticated backends never trigger an irrelevant 1Password flow.
9. **Environment trust is HMAC-bound and stored outside the environment**
   (`~/.pix/.state/trust`), never inside the directory being approved. A
   changed fingerprint (any host-affecting fact: kit, workspace mounts, MCP
   command/URL, secret destinations, network expansion) refuses launch and
   names `pix env trust NAME`. Trust review defaults to No; `--yes`
   suppresses the prompt only, it never skips the fingerprint check.
10. **`pix-host`, packs, scored model routing, and the custom memory RPC are
    deleted, not merely hidden.** No code path reaches any of them; there is
    no `pack.toml`, no `routing.json`, no pix-owned top-level `memory` command,
    and no unsandboxed host-agent mode. A model is chosen by name
    (`--model`, then `[models].main`, then the shipped session preference
    OpenAI/Anthropic/Google among configured providers; no choice refuses rather than using Pi's stale default);
    nothing scores or auto-selects one.
11. **Memory is operated through MCP tools, never a private protocol.**
    `memory_recall`/`memory_remember`/`memory_forget`/`memory_observe`/
    `memory_stats`/`memory_status`/`memory_snapshot`/`memory_restore` are the
    whole surface; `/recall`, `/remember`, `/forget`, and the automatic
    recall/capture hooks all call the same Gateway-registered endpoint a
    model's own tool calls use.
12. **A launch approves exactly one thing on the user's behalf.** `sbx env
    create` prints its own plan and asks its own approval for a document
    Pix already composed, fingerprinted, and put through its own trust
    gate, and that text carries the token-bearing `pix-memory` URL. Pix
    answers that duplicate prompt internally, after its own gate, captures
    the create child's output, and shows it only on failure, bounded and
    with every credential redacted. The interactive `sbx exec` session
    keeps ordinary stdio; the two children are told apart by their
    `SessionDeps` seam, never by sniffing argv.
13. **An environment with no host footprint is not gated.**
    `BillOfMaterials.Tier1()` is the canonical answer and every trust
    caller asks it: a zero-footprint environment (the generated `default`)
    is never prompted for and never causes a trust-state write.
14. **`pix run` reconciles machine-owned stack artifacts after an upgrade,
    and nothing else.** A bundle/manifest mismatch runs only the shared
    `machineSetup` composition; credentials, environment trust and
    `[[setup]]` hooks stay in `pix setup`, a foreign-owned container still
    refuses, and a failure restores the previous release record so the next
    run retries.
15. **Success words are earned by a probe.** `ready`/`verified` appear only
    after a post-mutation check; `pix doctor` never repairs, registers,
    restarts, or authenticates, and never prints `configured`/`enabled` as a
    verdict.

## Models and subagents

Selection is by name, never by score: `--model`, else the environment's
`[models].main`, else the catalog's literal default in shipped provider order
(OpenAI, Anthropic, Google) among providers this `PIX_HOME` configures; no choice
refuses rather than falling into Pi's stale
native default. An agent preset's model comes from its
own frontmatter, else the environment's `[agents].<name>` mapping, else the
parent's model. There is no model catalog administration command and no
agent administration command; edit `agents/*.md` or `pix.toml` by hand.

The subagent tool (`extensions/subagents.ts`) still provides single /
parallel / chain modes and depth-capped trees, spawning each child as
`pi --no-extensions -e <inference> -e <self>` with an idle/tool-idle/wall
watchdog. `/subagents` and `/subagents doctor` are unchanged by the v2
cutover; see `docs/design/subagents-extension.md`.

## Writing extensions (`extensions/*.ts`)

- Shape: `export default function (pi: any) { ... }`. pi loads `.ts`
  directly, with full Node globals at runtime (`process`, `require`,
  `setInterval` all work; don't "fix" them by deleting them, it's a
  type-lint gap, not a real error).
- An extension that throws at load breaks pi startup; guard defensively.
- **`/reload` hangs are almost always a factory that never returns.** Settle
  every promise on both success and error paths; close listeners on
  `session_shutdown`.
- Core API: `pi.registerCommand`, `pi.registerShortcut`, `pi.on(event, ...)`,
  `pi.registerTool`, `pi.events`. In handlers: `ctx.ui.notify /
  setWorkingMessage / setStatus / setWidget`, `ctx.model`,
  `ctx.getContextUsage()`, `ctx.abort()`, `ctx.isIdle()`.
- Useful events: `turn_start`/`turn_end`, `tool_execution_start`/`update`/
  `end`, `message_update`, `before_provider_request`/
  `after_provider_response`, `tool_call` (return `{block, reason}` to gate),
  `session_shutdown`.
- **Never put a `.d.ts` (or any non-extension `.ts`) in `extensions/`**: pi
  tries to load every `.ts` there as a factory and crashes startup on a
  declaration file. Ambient types go in `types/`.
- **Display-only injected messages** need `deliverAs:"nextTurn"`, not the
  default `"steer"` (which triggers an LLM call to deliver the message and
  can 400 a reasoning model fired from an idle hook). Strip `nextTurn`
  messages in the `context` hook by `customType`, **except**
  `pix-recalled-context` and `pix-output-style`: both are append-only and
  must never be stripped once sent (see `extensions/timestamps.ts`).

## Delegation rule: a feature is not done until its caller is wired

**A feature is incomplete until its real, user-facing caller is wired and
integration-tested against it.** Landing the library function, the new CLI
struct, or the new MCP tool with only unit tests around the new code is
half a feature: prove it from the actual entry point a user or the agent
hits (`pix run`, a slash command, a Gateway tool call), not only from a test
that imports the new package directly.

**Never hand a bounded subagent a whole-repo cutover.** Shard by *production
caller*, not by file count or directory: one child gets "wire `pix env
trust` into `pix run`'s pre-launch check and prove it with an end-to-end
test that a stale fingerprint refuses launch", not "update the environment
package." A child with a slice bounded this way can always tell whether it
finished; a child handed an architectural layer with no caller in scope
routinely reports done on code nothing calls yet.

## Toolchain in the image

node 25, npm, git, gh, ripgrep, fd, ruff, clangd, pyright,
typescript-language-server, Go (`/usr/local/go`, pinned via `GO_VERSION` to
match `services/host/go.mod`, `GOTOOLCHAIN=local`), chromium + agent-browser,
python3, build-essential. Go is baked so you can build/test the launcher
(`services/host`) and the memory service (`services/memory`) from inside a
sandbox when hacking on pix itself.
