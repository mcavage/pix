# pix reference manual

This is the capability reference. `docs/getting-started.md` teaches you the
first session and gets out of the way; this doc covers everything pix can do.
Read the section you need, run the command, move on.

## 0. Command map

The authoritative verb/flag list is `pix help --all` (and `pix help <verb>`):
it is generated from the dispatch tree, so it cannot drift. This table says
what each verb is FOR and where the reasoning lives. It is the pointer target
for `AGENTS.md`, which deliberately carries no CLI reference of its own.

| Verb | For | Detail |
| --- | --- | --- |
| `run [DIR]` | launch, or reattach to, the sandbox for a directory | §1 |
| `ls` | list your `pix-*` sandboxes: environment, project, holder count, task | §9 |
| `rm` | remove positively identified `pix-*` sandboxes: names, `--all`, `--orphans` | §9 |
| `task` | isolated Git checkouts plus a recorded environment: `new`/`ls`/`path`/`rm` | §8, `docs/design/worktree-tasks.md` |
| `env` | a directory under `~/.pix/envs`: `list`/`show`/`default`/`trust`, no add/edit/use/forget | §5 |
| `secret` | manage `op://` references, never values: `list`/`set`/`rm`/`check` | §10 |
| `setup` | the guided path from an installed binary to a working first session | §6 |
| `doctor` | read-only probes with one exact corrective action each | §7 |
| `reset` | remove sandboxes and the memory container, then rename `PIX_HOME` aside | §12 |
| `version`, `help` | stamped launcher version; tiered help | (none) |

Removed verbs get the ordinary unknown-command answer, never a retirement
notice: pix has no released users to keep a migration path for. There is no
`mcp`, `models`, `config`, `agent`, `pack`, `serve`, `resume`, `status`, or
`uat` verb in v2, and none of those names route anywhere.
`docs/design/pix-v2-surface.md` §11 is the full deletion list and the reasoning
behind each removal.

## 1. What pix is

pix is a thin sbx environment launcher for a multi-model coding agent (Claude,
GPT, Gemini, plus local llmman/Ollama) that runs inside a throwaway Docker
Sandbox. It does four jobs: run the pinned pix build of Pi inside a sandbox,
turn a named environment into one exact sandbox invocation, tear down an
ordinary sandbox when its last holder exits, and manage isolated task
checkouts for parallel work. Everything else, sandbox configuration and MCP
attachment, belongs to native `sbx` and the sbx MCP Gateway.

```console
cd ~/dev/project
pix
```

is the daily path; the explicit form is:

```console
pix run [DIR] [--env NAME] [--model MODEL] [--resume SESSION]
```

A **holder** is one live node in a session tree that still depends on a
sandbox: the interactive root process is one holder, a running child agent is
another. Tree-wide holder count, not an arbitrary in-sandbox shell process,
determines ordinary teardown, and a normal sandbox is removed after its last
holder exits. `pix ls` renders the tree.

`--env NAME` selects an environment for this run without changing the
default; an unknown name is an error, and pix never falls back to the default
after an explicit but invalid `--env`. Pix never auto-selects a
`.sbxenv.yaml` found in a project workspace: environment selection controls
credentials and host execution, so selection is always explicit or the
machine default. `--model` overrides the environment's main model for this
session; pix does not score or choose one for you. `--resume SESSION` is an
option on `run`, not a separate top-level verb. `--dev` mounts the current pix
source for live development; it is not a second production launch path.

## 2. Memory

Memory is a separate service, **`pix-memory`**, a Go MCP server that speaks
Streamable HTTP. `pix setup` reconciles one named Docker container
(`unless-stopped`, published only on host loopback, `~/.pix/state/memory`
mounted). The sbx MCP Gateway registers that endpoint as a remote MCP server;
the sandbox never dials the container directly. **Pix has no top-level
`memory` command in v2**: memory is operated entirely through MCP tools
and the slash commands that call them.

```
/recall <query>       # what memory would surface for this query
/remember <text>      # pin a fact immediately
/forget <id|query>    # drop a memory; id from /recall, or a query to drop its top match
```

The full tool surface: `memory_recall` (relevance search or bounded `*`
listing), `memory_remember` (explicit durable insertion), `memory_forget`
(soft-delete by exact ID or unambiguous prefix), `memory_observe` (opted-in
watcher extraction from one completed exchange), `memory_stats`,
`memory_status`, `memory_snapshot`, and `memory_restore`. Snapshot and
restore carry accurate destructive/idempotency MCP annotations; skills and
extension policy decide when to call them.

**Capture is explicit by default.** Nothing is written unless a human or an
explicit command asks for it. `pix.toml`'s `[memory]` section can opt an
environment into an automatic watcher; see `docs/memory.md`.

**Privacy.** Extraction and embedding run on the selected local backend
(llmman or Ollama) and never leave the host, but recalled memory is not
private from your model provider: once a row is recalled, its content enters
the prompt sent to whichever model is active. Never store secrets, tokens, or
credentials in memory.

**Trust and scope.** Profiles are organizational scopes in this local
personal service, not security tenants: the server enforces query/write scope
on every request, but the same trusted Gateway client can request another
profile. A future multi-user deployment adds real tenant authentication
rather than treating a profile argument as authorization. Full detail,
including the on-disk schema and the capture budget: `docs/memory.md`.

## 3. Skills (the flows)

Skills are named, tested workflows for the moments that recur. Each is a
`SKILL.md` with tight steps, invoked automatically when the conversation
matches its intent, or explicitly:

```
/skill:plan
/skill:build
```

`/help` lists every skill available in this session with a one-line
description. The spine you will use most:

| skill | what it does |
| --- | --- |
| `plan` | idea to eng-ready plan: discovery, PR/FAQ, PRD, design, architecture, with a review gate |
| `build` | ship a feature with the crew: story files, parallel worktrees, code review, QA, verification |
| `ship` | working tree to open PR: rebase, tests, lint, review, version bump, push |
| `debug` | root-cause-first: reproduce, form a falsifiable hypothesis, verify it, then fix |
| `code-review` | review the current diff, then get a cross-vendor second opinion |
| `architecture-audit` | map and stress-test a whole system or major refactor with the full crew |
| `tdd` | failing test first, watch it fail, minimal code to pass, refactor |
| `verify` | prove a claim by running the command before you say "done" |
| `qa` | drive a running app in the browser, report bugs with screenshots |
| `healthcheck` | is the harness working (keys, memory, MCP, skills) and is the code healthy (tests, lint, dead code) |
| `enrich` | write a durable fact into a shared knowledge bundle, gated by PR |
| `promote` | review what the memory watcher keeps repeating and graduate it into a skill or convention |

**Limits.** Skills are mechanism, not personal config: a skill never
hardcodes your channel names or account IDs; that data lives in memory or an
environment's `context/` and the skill reads it at runtime. Shipped skills
live outside the agent image (mounted at run time from
`~/.pix/runtime/<version>/skills`); edit and version your own by adding a
`skills/<name>/SKILL.md` under `~/.pix/skills/` or an environment's own
`skills/` directory, which shadows the shipped copy of the same name.

## 4. The crew

pix is not one model. Providers rotate (Claude, GPT, Gemini, plus llmman or
Ollama), and specialist subagents (architect, engineer, qa-lead,
security-lead, and more) get dispatched for the parts of a task that need a
different lens. The point of cross-vendor review is not ceremony: a
different vendor than the one that wrote the code checks it, so its blind
spots do not overlap with the author's.

```
/model              # open the model picker
Alt+P               # cycle models without leaving the keyboard
```

There is no model router in v2: nothing scores a model, and nothing chooses
one for you. Selection order is `pix run --model MODEL`, then the selected
environment's `pix.toml` `[models].main`, then Pi's default. Agent selection
order is an explicit model in a custom agent definition, then the
environment's `[agents].<name>` mapping, then the session's main model, then
parent-model inheritance. There is no model catalog administration command
and no agent administration command: edit `agents/*.md` or `pix.toml` by
hand.

**Limits.** A model is only as good as the one you picked; pix measures none
of them and ranks none of them. Subagents run headless
(`pi --no-extensions`), so a child that gets stuck has no UI to show you; a
watchdog kills it after an idle or wall-clock timeout and reports the failure
instead of hanging forever.

## 5. Environments

An environment is a directory under `~/.pix/envs/<name>/`, `PIX_HOME` may
replace `~/.pix`. Pix does not maintain an environment registration database:
the directory name is the environment name, and an environment stored
elsewhere is represented by a symlink under `~/.pix/envs`.

```
pix env [NAME] [--path|--effective|--json]   # list, or one environment's detail
pix env default [NAME]                        # print, or set, the machine default
pix env trust NAME [--yes]                    # read and accept what NAME runs on your host
```

**There is no `add`/`edit`/`use`/`forget`.** Create, clone, edit, move, and
remove an environment with ordinary filesystem and Git tools under
`~/.pix/envs`. `pix setup` may scaffold a default one as a first-run
convenience.

An environment contains only two files pix interprets:

```
~/.pix/envs/work/
  .sbxenv.yaml     # the native sbx environment file; sbx owns its schema
  pix.toml          # optional sidecar: Pi/pix facts .sbxenv.yaml cannot express
  skills/ agents/ context/ README.md   # inputs named by pix.toml, not interpreted directly
```

`.sbxenv.yaml` declares the pix agent and kits, sandbox resources and
template, workspace mounts, environment variables, secrets and credential
bindings, registries, MCP servers, and ports, all in native sbx grammar. Pix
adds restrictions before use: literal secret values are refused; the
effective agent must be pix; local executable and kit sources must be pinned
or content-fingerprinted; host commands, writable mount expansion, and
credential destinations require host trust approval.

`pix.toml` may declare the main model and agent-to-model mappings, a custom
Pi inference backend, environment-local Pi content paths, a memory scope, and
credential/health/host-capability annotations for an MCP server declared in
`.sbxenv.yaml`. It cannot declare or supervise a host service, and it cannot
declare kits, workspaces, sandbox variables, secrets, credential bindings, MCP
transports, sandbox resources or ports, Pi extensions, settings, keybindings,
or themes: anything native sbx or global Pi settings already own stays out.
Unknown keys are errors.

**Trust.** An environment that runs host code or handles a credential must be
approved with `pix env trust NAME` before a launch will use it. The
fingerprint is HMAC-bound and stored under `~/.pix/state/trust`, outside the
environment directory itself, and recomputed before every use: a changed
fact (kit, workspace mounts, MCP command or URL, secret destinations,
network expansion) refuses launch and reprints the same review. Trust review
defaults to No; `--yes` removes the prompt, not the printed bill or the
fingerprint check.

## 6. Setup

`pix setup` is the supported, repeatable, idempotent path from an installed
binary to a working first session. It:

1. checks Docker and the supported `sbx` version;
2. initializes `PIX_HOME` and `git init -b main`, without staging or
   overwriting anything already there;
3. installs the runtime archive and records the release manifest;
4. verifies the `pix-agent` image and strict kit;
5. creates a default environment only when none exists;
6. configures the selected local inference backend (llmman or Ollama);
7. seeds and validates `op://` references, never writing a secret value;
8. creates or reconciles the `pix-memory` container;
9. runs approved integration setup/authentication for the selected
   environment; and
10. probes the complete result before reporting it ready.

`--env NAME` sets up one existing environment in addition to machine-level
prerequisites; it does not select that environment as the default. When
setup reaches an untrusted environment, it performs the same complete,
default-No trust operation as `pix env trust NAME` before running any
installer or authentication command; non-interactive setup refuses and names
that command.

## 7. Doctor

`pix doctor` is read-only. It checks Docker and sbx availability and version,
the pinned images/kit/runtime-data identity, environment schema and trust
state, selected cloud and local model reachability, `op://` reference
resolution where required, sbx Gateway MCP registration and authentication,
each required local command or integration-owned endpoint, the memory MCP
endpoint/storage/embeddings/capture mode/scope isolation, session state, and
sandbox declaration drift that requires recreation. Every failing row names
the owning system and one exact next action; Gateway OAuth errors name the
native `sbx mcp auth SERVER` command. Doctor never repairs, registers,
restarts, or authenticates.

Exit codes: `2` on a usage error or safety refusal, other nonzero values on
an operational failure, `0` when nothing checked verified a gap. `ready`,
`verified`, and `removed` appear only after a corresponding probe.

**Required, always: the sbx CLI being installed, and at least one resolved
provider key** (a single key for any one of Anthropic, OpenAI, or Google
satisfies it; you do not need all three). Either one failing alone is enough
to fail doctor. Nothing in v2 makes launchd or a pack a required check:
there is no `pix-host serve` supervision tree and no pack system left to
check at all (`docs/design/pix-v2-architecture.md` §14).

## 8. Tasks

A task is an isolated Git checkout plus a recorded pix environment, created
with `git clone --local`: a self-contained checkout whose `.git` directory
works inside a direct-mounted sandbox and whose commits survive sandbox
removal.

```
pix task new NAME [--from REF] [--env NAME]
pix task ls [--json]
pix task path NAME
pix task rm NAME [--force]
```

`task new` resolves the source commit, clones, creates `pix/<name>`, writes
metadata, and returns without implicitly changing the caller's shell.
`task path NAME` prints only the absolute checkout path, so a shell can `cd`
without parsing human output:

```console
pix task new fix-auth --from main
cd "$(pix task path fix-auth)"
pix
```

A task records its environment when created; changing the machine default
later does not change an existing task's credential or model context.
`task rm` requires zero live holders, removes the associated sandbox through
the normal identity-safe `pix rm` path first, and refuses a checkout with
uncommitted work or unpushed commits unless you supply `--force`. Before
deleting a checkout, forced removal preserves otherwise unreachable commits
under a recovery ref in the source repository.

## 9. `pix ls` and `pix rm`

`pix ls` reports pix-owned sandboxes, their environment, project, holder
count, and task association. It does not report readiness; health belongs to
`pix doctor`.

`pix rm` removes only positively identified `pix-*` sandboxes, verifying the
current sbx instance ID before removal. Unknown sbx state fails closed.
`--force` is an explicit authority override for a NAMED pix sandbox: it does
not widen the `pix-*` namespace and never authorizes removing a sandbox pix
did not create.

An **orphan** is a pix-owned sandbox that still exists after no live
session-tree node claims it, usually because a launcher or machine crashed
before normal teardown. `--orphans` removes only sandboxes that pass five
positive proofs (a fresh listing, a `pix-*` name, a matching recorded
instance ID, zero reference locks, no keep marker) and preserves every keep
marker; any unknown answer preserves the sandbox. `--keep NAME` excludes a
named sandbox from a bulk operation.

## 10. Secrets

`pix secret` manages 1Password `op://` references, never values.

```
pix secret [--json]           # list configured reference names and syntax state
pix secret set NAME OP_REF    # accepts only an op:// reference
pix secret rm NAME            # removes one reference, never a 1Password item
pix secret check [NAME]       # resolves through op without printing values
```

Setup calls this same capability rather than implementing a second secret
path. Native sbx receives dynamic references or resolved credentials through
its own supported secret interfaces. Missing `op` is fatal only when the
selected environment needs direct 1Password resolution: keyless and
Gateway-authenticated backends never trigger the flow.

## 11. MCP and host capabilities

Pix has one sandbox-facing integration path: the sbx MCP Gateway. There is no
pix-owned `mcp` command, no Pix-owned registration database, and no built-in vendor
integration; the native `.sbxenv.yaml` declares every server, and Pix may
annotate one in `pix.toml` with a required 1Password reference name and a
doctor probe.

`mcp.servers` performs host-global registration and attachment to that
environment's sandbox; Pix neither defines a second MCP registry nor spawns
those processes. A registration mismatch (same name, different command, URL,
or credential identity) refuses launch and is never silently overwritten.
OAuth is performed with native `sbx mcp auth`.

An MCP implementation that needs direct host GUI, credential-store, or
device access runs through the sbx Gateway's on-demand host command, or an
integration-owned launchd/systemd unit exposing Streamable HTTP when it needs
a stable signed identity or durable host residency. Pix trust-checks the
exact executable identity, arguments, credential destinations, and requested
host grants before launch; `pix doctor` probes the declared endpoint but does
not supervise the process. See `docs/gworkspace.md` for a worked example.

Skills never hardcode a vendor: they ask for a **capability** (`chat`,
`docs`, `github`, `meeting-notes`), and `capabilities.json` maps that
capability to a concrete provider (an `mcp` server, a `cli` on PATH, an
`http` service, a `files` bundle, or `none` if unwired). See the
`capability-routing` skill for the resolution and fan-out rules.

## 12. Reset

`pix reset` is the safe clean-slate recovery command. It removes pix-owned
sandboxes through the normal proof-gated removal path, stops and removes the
named `pix-memory` container (and proves it is absent before proceeding),
then renames `PIX_HOME` to a timestamped backup. It contains no recursive
delete, and environment sources reached through symlinks outside `PIX_HOME`
are never moved.

```bash
pix reset
```

## 13. Your first hour

1. `pix run` in a real project directory: not a toy repo, the thing you
   actually need to get done today.
2. Do the task. Say what you want in plain language. A skill loads on its
   own when the conversation matches one (`debug` on a bug report, `build`
   on "implement X").
3. Let the crew show up uninvited. If you ask for a code review, a
   cross-vendor subagent checks it without you naming a model.
4. When you catch yourself repeating a preference across sessions, `/recall`
   first to see what memory already captured, then `/remember` the rest.

That is the whole loop: run, work, let the parts introduce themselves.
