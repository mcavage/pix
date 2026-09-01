# Pix v2 architecture

Status: **ACCEPTED FOR IMPLEMENTATION**.

This document turns the agreed product surface in
`docs/design/pix-v2-surface.md` into one code target. It deliberately does not
prescribe migration waves. Pix has no released-user compatibility obligation,
the work is protected by Git, and the goal is the fastest low-cost cutover to a
smaller system. There is one implementation path, no feature flags, no dual
runtime, and no compatibility adapters.

## 1. Architectural rules

1. `pix` is the only host binary and the only user-facing CLI.
2. Native `.sbxenv.yaml` is the only sandbox declaration.
3. The sbx MCP Gateway is the only integration path visible inside a sandbox.
4. Pix owns selection, trust, Pi assembly, session/task lifecycle, setup, and
   diagnostics. It does not duplicate sbx configuration or supervision.
5. Docker owns the local memory container lifecycle. The Gateway owns local MCP
   subprocess lifecycle. Integration-specific launchd/systemd units own any
   unavoidable resident host process.
6. All user files and runtime state live below `PIX_HOME`, default `~/.pix`.
7. A process that cannot prove ownership or liveness fails closed. PIDs and
   names alone are never proof.
8. Removed behavior is deleted. It is not hidden, deprecated, translated, or
   left behind for a later cleanup.

## 2. Runtime topology

```text
                              host

 user ──> pix ──> sbx env create ──> sbx exec ──> pix-agent sandbox
          │             │                            │
          │             └── native environment      │ one Gateway endpoint
          │                                          v
          │                                  sbx MCP Gateway
          │                                    │           │
          │                           Streamable HTTP       │ stdio/remote
          │                                    │           │
          ├── docker ──> pix-memory <───────────┘           └── integrations
          │                 │
          │                 ├── ~/.pix/state/memory:/data
          │                 └── llmman or Ollama
          │
          └── ~/.pix/state/{sandboxes,sessions,tasks,trust}
```

The agent sandbox cannot connect directly to the host-published memory port.
The container publishes only on host loopback; the host-side Gateway connects
to it. Local processes running as the same user remain inside the personal-tool
trust boundary.

A future cloud memory deployment replaces the registered URL and adds real
service authentication and tenant authorization. It does not change the MCP
contract or Pi extensions.

## 3. Source tree and artifacts

The target tree is:

```text
cmd/
  pix/

internal/
  cli/
  config/
  doctor/
  environment/
  launch/
  mcp/
  secret/
  session/
  setup/
  sandbox/
  task/
  trust/

services/
  memory/
    cmd/pix-memory/
    server/
    store/
    Dockerfile

images/
  agent/
    Dockerfile

pi-kit/
runtime/
  agents/
  skills/
  pi/
    settings.json
    keybindings.json
    themes/

extensions/
lib/
tests/
```

This is an ownership map, not a requirement to spend time moving a package that
is already cohesive merely to match a diagram. During the cutover:

- `services/host/cmd/pix` becomes `cmd/pix`;
- surviving reusable launcher packages move under `internal` or are renamed in
  place if moving them adds no value;
- the memory store moves out of the launcher module into `services/memory`;
- the agent Dockerfile moves to `images/agent/Dockerfile`;
- shipped Markdown and Pi defaults move to `runtime`;
- deleted packages leave no empty compatibility package.

The repository produces exactly these release artifacts:

| Artifact | Purpose |
| --- | --- |
| `pix` | Host CLI, statically versioned Go binary |
| `pix-agent` | DHI Node/Debian sandbox image |
| `pix-memory` | DHI Go-built memory MCP image |
| runtime archive | Skills, agents, Pi settings, keybindings, and themes |
| strict kit | The pinned Pix custom-agent kit |

`pix-agent` and `pix-memory` have independent Dockerfiles and immutable digests.
A release manifest binds one Pix version to the binary, both image digests, the
runtime archive digest, and the kit revision. Setup and doctor read that one
manifest; there is no separately maintained version map.

## 4. Host CLI structure

`cmd/pix` is a thin command dispatcher over cohesive packages. The dependency
direction is:

```text
cmd/pix -> workflows -> domain packages -> os/exec/filesystem adapters
```

Domain packages do not import CLI rendering. Calls to `sbx`, `docker`, `git`,
and `op` pass through small interfaces so tests can provide deterministic
fakes. Production has one implementation of each command builder and one parser
for each external JSON response.

The surviving responsibilities are:

- `environment`: resolve names and symlinks, parse native sbx files and the thin
  sidecar, compose and render the effective environment;
- `trust`: build and verify the host-execution bill of materials;
- `launch`: decide create versus attach, persist create intent, invoke sbx, and
  record the positively observed instance;
- `sandbox`: parse `sbx ls --json`, enforce the `pix-*` namespace, plan removal,
  and verify instance identity;
- `session`: persist trees/nodes and hold instance-bound references;
- `task`: create/list/path/remove isolated local clones and guard Git data;
- `setup`: initialize `PIX_HOME`, install release artifacts, reconcile the
  memory container, and run approved integration setup;
- `doctor`: read-only probes with one exact corrective action;
- `secret`: manage and validate `op://` references without resolving values to
  disk or stdout;
- `mcp`: inspect native declarations and verify their host-global sbx
  registrations. It is not a registry;
- `stack`: the single producer of the 16-hex stack id derived from this
  `PIX_HOME`'s canonical path, and of every name derived from it (sandbox,
  memory container, memory/session MCP servers). No other package composes
  one of these names on its own.

## 5. Pix home

```text
~/.pix/
  .git/
  .gitignore
  README.md
  AGENTS.md
  skills/
  agents/
  output-styles/
  envs/<name>/
  pi/
    settings.json
    keybindings.json
    themes/
  config.toml
  secrets.env
  runtime/<pix-version>/
  state/
    effective/<sandbox>/effective.sbxenv.yaml
    memory/
      memory.db
      backups/
    sandboxes/<sandbox>/
      record.json
      fingerprint.json
      invocation.json
      keep.json
      lifecycle.lock
      refs/
    sessions/<tree-id>/
      tree.json
      nodes/<node-id>.json
    tasks/<repo-key>/
      meta/
      co/
    trust/environments/<name>.json
```

Files containing machine state are mode `0600`; their parent directories are
`0700`. Durable writes use a same-directory temporary file, fsync where loss
changes authority or ownership, then atomic rename or no-replace link.

`config.toml` contains only the default environment (written by `pix env
default NAME`, and once, atomically, by `pix setup` itself the moment it
scaffolds a fresh host's first environment), the allocated pix-memory
loopback port (`memory_port`, written once by `pix setup` — see §9.1), and
installed release manifest identity. `secrets.env` contains only
`NAME=op://...` references. Neither is tracked by the initialized Git
repository. Local inference is never a machine-wide `config.toml` field: it
is authored per environment in that environment's own `pix.toml`
`[inference.*]` tables, and merged over machine config into an ephemeral,
never-persisted snapshot for the one `pix run` session that selected it.

There is no XDG fallback in v2: `config.Path`/`StateDir`/`DataDir`/`ContextDir`
resolve under `PIX_HOME` alone, with no `PIX_CONFIG`/`XDG_*` fallback of any
kind in production. Tests set `PIX_HOME` to a temporary directory.

## 6. Environment compilation and launch

### 6.1 Inputs

A launch resolves these inputs once:

- environment name from `--env`, task metadata, or machine default;
- canonical environment root under `~/.pix/envs`, following at most its named
  symlink;
- authored `.sbxenv.yaml`;
- optional `pix.toml`;
- canonical project workspace;
- pinned agent image and strict kit from the installed release manifest;
- global and environment-local Pi content paths;
- selected model and optional resume session;
- memory scope;
- exact authored MCP declarations and their reviewed host annotations.

All later checks, fingerprints, output, and execution use these resolved values.
Nothing re-resolves a symlink or environment variable midway through launch.

### 6.2 One effective document

One pure compiler produces the effective native environment used by both
`pix env NAME --effective` and `pix run`. It:

1. parses the native document without inventing Pix aliases for sbx fields;
2. applies Pix's fixed runtime facts: custom agent, pinned image, strict kit,
   primary workspace, global content mounts, generated sandbox name, the local
   memory MCP endpoint, and the narrow session-control MCP command;
3. preserves authored additional workspaces, kits, secrets, bindings, MCP,
   resources, and ports;
4. rejects literal secret values and unsafe/unpinned host execution;
5. emits canonical YAML and a canonical semantic fingerprint.

Preview omits only facts that cannot exist before launch, such as a newly
observed sbx instance ID. There is no second renderer for create.

The exact bytes handed to sbx are persisted at
`state/effective/<sandbox>/effective.sbxenv.yaml` before creation. Removal uses
that same file and positive instance identity.

### 6.3 Create and attach

Pix does not use `sbx env run` to launch Pi because that command cannot pass
session-specific model and resume arguments to the custom agent.

The target sequence is:

```text
new sandbox:
  sbx env create <effective-file>
  poll sbx ls --json until the exact pix-* instance is positively identified
  record instance id + fingerprint + invocation
  sbx exec -it <sandbox> -- <pix-entrypoint> [--model M] [--resume S]

existing sandbox, RUNNING:
  verify name, instance id, effective fingerprint, and holder state
  sbx exec -it <sandbox> -- <pix-entrypoint> [--model M] [--resume S]

existing sandbox, STOPPED:
  verify name, instance id, effective fingerprint, and holder state
  sbx run --name <sandbox> [-- [--model M] [--resume S]]
```

The command after `--` is inside the sandbox. Model and resume are Pi entrypoint
arguments, not sbx flags. The same builder is used on every RUNNING attach.

A STOPPED sandbox is never `sbx exec`'d (exec has no "start" of its own and
fails outright against an already-stopped container). It goes through the
same identity/fingerprint/review gate as a running attach (review round 1
blocker #2 — a stopped sandbox is a legitimate reattach target,
docs/getting-started.md: "A sandbox already exists -> reattach, running or
stopped, as-is", never an outright refusal), but the actual
start-then-attach uses the legacy `sbx run --name <name>` reattach argv
instead, which is what actually starts a stopped sandbox — no `sbx start`
verb is established in this codebase's observed sbx contract
(docs/upstream/sbx-0.39-environments.md), so this is the one supported
existing argv for that case. It still carries the session's CURRENT
`--model`/`--resume`, not a replay of a stale prior invocation.

Host UAT must prove this exact argv against the supported sbx release. If it
fails, implementation stops at this seam and records the observed contract; it
does not add a second launch path.

Creation intent is recorded before invoking sbx. A create is promoted to a live
record only after `sbx ls --json` returns a schema-valid, positively identified
instance ID. Unknown runtime state never becomes absence.

## 7. Session tree and leases

### 7.1 Records

A tree has a random stable ID and immutable creation metadata. Each node records:

```json
{
  "id": "node-id",
  "parent": "parent-node-id-or-empty",
  "environment": "work",
  "model": "provider/model",
  "workspace": "/absolute/path",
  "target": "local-process|local-sandbox|cloud-sandbox",
  "sandbox": "pix-name",
  "instance_id": "sbx-instance-id",
  "state": "starting|running|finished|failed",
  "created_at": "RFC3339",
  "finished_at": "RFC3339-or-empty"
}
```

Node files are diagnostic history, not liveness proof. State transitions are
atomic and monotonic. A stale `running` value after a crash is allowed and is
rendered as stale when no reference lease exists.

### 7.2 Liveness

The existing Unix `flock` design survives. Each host-side process responsible
for a live node holds a shared reference lock bound to the recorded sbx instance.
The lock closes automatically on normal exit, signal, or crash. PIDs remain
human diagnostics only.

- the interactive `pix run` process holds the root node reference;
- a separately executing child has a small `pix` child-runner process that holds
  its node reference;
- multiple nodes may share one sandbox;
- teardown requires zero reference locks, no keep marker, a fresh trusted sbx
  listing, and an exact instance-ID match.

The child runner is an invocation mode of the `pix` binary, not a daemon. An
in-sandbox extension requests it through a narrowly scoped Gateway-launched MCP
command implemented by `pix`. That command may create a node and spawn one
bounded child runner; it is not a general shell or plugin API. The runner can
outlive the interactive root, so root exit does not destroy an active child.

The first implementation may support only `local-process`, but the persisted
node schema and request contract include all three target values. Unsupported
targets return a capability error; they do not create a parallel session model.

### 7.3 Teardown

The last reference holder invokes the existing proof-gated teardown planner.
If it crashes before teardown, the sandbox becomes eligible for
`pix rm --orphans`. Orphan collection requires:

1. a fresh, parseable sbx listing;
2. a `pix-*` name;
3. a matching recorded instance ID;
4. zero reference locks; and
5. no keep marker.

Any unknown answer preserves the sandbox.

## 8. Tasks

Keep the current host-side `git clone --local` design. It creates a self-contained
checkout whose `.git` directory works inside a direct-mounted sandbox and whose
commits survive sandbox removal.

Task state moves to `~/.pix/state/tasks`. A task record binds name, repository,
checkout, branch, base commit, environment, sandbox, and creation time.

- `task new` resolves the source commit, clones, creates `pix/<name>`, writes
  metadata, and returns without implicitly changing the caller's shell;
- `task path` prints only the absolute checkout path;
- `task ls` joins metadata, Git state, and sandbox/session state;
- `task rm` requires zero holders and refuses dirty or unpushed work unless
  `--force` is explicit.

Before deleting a checkout, forced removal preserves otherwise unreachable
commits under a recovery ref in the source repository. Sandbox removal completes
before checkout removal. Existing correctness code for these properties should
be retained; obsolete harvest/gc/pack/profile surfaces should not.

## 9. Memory MCP service

### 9.1 Process and container

`pix-memory` is a Go service built with the official MCP Go SDK. It exposes one
Streamable HTTP endpoint, `/mcp`, and a non-MCP liveness/readiness endpoint,
`/healthz`.

`pix setup` reconciles one named container, scoped to this `PIX_HOME`'s own
stack (`stack.MemoryContainerName`, §4):

```text
name:        pix-memory-<stack-id>
image:       immutable digest from the release manifest
restart:     unless-stopped
publish:     127.0.0.1:<allocated-or-fixed-port>:8080
mount:       ~/.pix/state/memory:/data
network:     only the selected llmman/Ollama endpoint when semantic features run
```

`pix.managed` and `pix.fingerprint` labels gate ordinary reconciliation, as
before; `pix.stack.id` and `pix.home` labels additionally record which stack
owns the container, so Reconcile never adopts, starts, or replaces a
same-named container belonging to a different `PIX_HOME` (the name already
carries the stack id, but the labels are the proof Reconcile actually
checks).

The port is **per-`PIX_HOME` state, allocated once and persisted**, not a
process-wide constant: `container.EnsureMemoryPort` (called only by `pix
setup`, under `container.SetupLockPath`'s advisory lock held through the
whole allocate-then-reconcile sequence) reads `config.toml`'s `memory_port`
field if already set, otherwise adopts the historical default (18080) when it
is actually free right now, or otherwise draws a genuinely free loopback port
from the OS (bind `127.0.0.1:0`, read back the assignment, close) and persists
it. This is what lets two independent `PIX_HOME` installs on the same host
both run `pix setup` without forcing a collision on one fixed port. Every
other reader — the container spec, the registered MCP URL, `pix run`'s
effective built-in, `pix env --effective` preview, doctor, reset — reads the
same persisted value read-only (`container.ReadMemoryPort`) and never
allocates one itself; a host that has never run `pix setup` shows the
historical default for display only.

The exact `docker create` configuration is fingerprinted into container labels.
Setup adopts a healthy matching container, starts a stopped matching container,
and replaces a mismatched container only after showing the image/config change.
The `/data` directory is never removed during replacement. Setup reports ready
only after `/healthz` and an MCP initialization/tool-list probe succeed. A port
already in use when Setup tries to bind it (an unresolved same-host collision,
or anything else already listening) refuses with the exact remedy: stop
whatever holds it, then rerun `pix setup`.

Docker restart policy restarts a dead process, not an unhealthy one. Doctor
therefore performs behavioral probes and prints the exact `docker restart
pix-memory-<stack-id>` recovery command for a wedged container.

### 9.2 MCP tools

The service exposes all memory behavior through MCP:

| Tool | Semantics |
| --- | --- |
| `memory_recall` | relevance search or bounded `*` listing |
| `memory_stats` | active, fact, correction, and deleted counts |
| `memory_remember` | explicit durable insertion or reaffirmation |
| `memory_forget` | soft-delete by exact ID or unambiguous prefix |
| `memory_observe` | opted-in watcher extraction from one completed exchange |
| `memory_status` | schema, embedding backend, capture mode, and readiness |
| `memory_snapshot` | verified SQLite snapshot into `/data/backups` |
| `memory_restore` | verified atomic restore with backup of replaced data |

Inputs preserve the current content, kind, project, profile, limits, and
character-budget semantics. Secret filtering, semantic dedupe, watcher budget,
embedding fallback, schema migration, and source attribution remain server
internals. Existing database schema and on-disk rows are preserved unless a
specific migration is required.

Tools carry accurate MCP annotations. Snapshot and restore are visible like the
other operations; restore is destructive and must be marked accordingly. Skills
and extension policy decide when to call them. There is no private memory RPC.

Profiles are organizational scopes in the local personal service, not security
tenants. The server enforces query/write scope semantics, but the same trusted
Gateway client can request another profile. A future multi-user deployment must
add authenticated tenant identity rather than treating a profile argument as
authorization.

### 9.3 Pi integration

The Gateway exposes memory tools to the model through the normal Pi MCP adapter.
Baked Pix extensions also make deterministic calls through the same Gateway:

- `before_agent_start` calls `memory_recall` with a strict short timeout and
  appends each recalled row at most once;
- opted-in capture calls `memory_observe` after a complete exchange;
- `/recall`, `/remember`, and `/forget` call their corresponding tools and show
  failures;
- `memory_recall` and `memory_stats` remain available as ordinary model tools.

A small shared TypeScript MCP client reads the Gateway endpoint from the same
injected Pi MCP configuration as the adapter and invokes `tools/call` directly.
It does not connect to the memory container URL. Silent lifecycle recall/capture
is best effort; explicit user commands surface errors.

Host UAT must prove that a second client connection to the injected Gateway can
initialize and call the memory tools while the Pi adapter is connected. If the
Gateway forbids that, the fallback is an in-process call API exposed by the Pi
MCP adapter, not direct host networking or restoration of the old JSON-RPC
service.

## 10. MCP and host-native integrations

Authored `.sbxenv.yaml` declares environment MCP servers using native sbx
grammar. The effective compiler adds only Pix's two built-ins: the local memory
endpoint (`pix-memory-<stack-id>`) and the narrow session-control command
(`pix-session-<stack-id>`), both named for THIS `PIX_HOME`'s own stack id
(`stack.MCPMemoryName`/`stack.MCPSessionName`, §4), never the bare legacy
name. They are emitted as native sbx MCP declarations, not attached through a
second client or registry.

Pix does not provide `mcp add`, `mcp auth`, a catalog, or a registration
database. Setup may invoke the exact native `sbx mcp` command required by a
selected built-in environment; errors name the native command.

For an authored declaration, Pix verifies that the host-global registration
matches the reviewed URL or local command; a same-name mismatch refuses. Pix's
own `pix-memory-<stack-id>` name is different: it is derived from this
`PIX_HOME`, so setup owns that exact entry and reconciles a mismatched or
unreadable endpoint with `sbx mcp rm` followed by `sbx mcp add`. A matching
readable endpoint is retained. No authored or foreign-stack name is touched.
The registry itself is one list per host, not one
per `PIX_HOME`: a second `PIX_HOME`'s two built-ins register under their own
stack-scoped names in that SAME list, so the registrations sit side by side
instead of colliding.

Implementation choices are:

1. remote Streamable HTTP endpoint;
2. Gateway-launched host stdio command;
3. integration-owned launchd/systemd service exposing Streamable HTTP when a
   stable signed identity or GUI-session residency is unavoidable.

The third option is still MCP, not a Pix service API. Its installer, executable,
endpoint, credentials, UI grants, and device grants enter environment trust.
Pix doctor probes it; Pix does not supervise it.

## 11. Trust and secrets

Environment trust is an HMAC-bound record under `~/.pix/state/trust`, owned by
the launcher rather than the approved environment. The canonical bill of
materials includes every host-affecting fact:

- canonical environment root and authored file digests;
- local kit and executable content identity;
- local MCP command, literal argv, working directory, and image digest;
- remote MCP URL and credential destination;
- installer and authentication command identity and argv;
- writable host mounts;
- network expansion;
- secret reference names and destinations; and
- UI and device grants.

The renderer prints the same canonical structure that is fingerprinted. Trust
review defaults to No. `--yes` removes the prompt, not the rendered bill or the
fingerprint check. A changed fact requires a new approval. An unclassifiable
local-versus-remote declaration fails closed.

`pix secret set` accepts only `op://` references. Resolved values are never
written to disk or stdout. A spawned MCP host integration receives only its
declared references (`op run --env-file`), never the complete `secrets.env`
file. Every other configured ref (model provider keys, tool keys,
`GITHUB_TOKEN`) resolves on each create and each attach into a credential
scoped to the ONE sandbox that launch is entering (`sbx secret set -f
--sandbox <name> <service>`, with the resolved value written to that
command's stdin so it never appears in the host's process table); there is
no "already set, skip it"
branch, so a rotated 1Password item takes effect on the next run. Pix never
writes a host-global sbx secret and never reads one as evidence that a
credential exists; a global secret already on the host is reported by doctor
as ignored and is never removed automatically. Missing `op` is fatal only
when the selected environment needs direct 1Password resolution.

## 12. Setup, doctor, reset, and release

### Setup

`pix setup` is idempotent and mutation is explicit. It:

1. verifies Docker, sbx, Git, and conditional `op` prerequisites;
2. initializes `~/.pix` and `git init -b main` without staging or overwriting;
3. installs the runtime archive and records the release manifest;
4. verifies the agent image and strict kit;
5. creates a default environment only when none exists, and selects it as
   the machine default under the same config-lock write;
6. always seeds a refs-only `secrets.env` (a no-op if one already exists)
   and, on a TTY with `op` installed and no refs configured yet, offers to
   add one per model provider interactively; validates `--env NAME`'s
   declared requirements, when given, including any local inference backend
   its `pix.toml` authors — setup never interviews for a machine-wide
   choice, and never writes one;
7. creates or reconciles this `PIX_HOME`'s own stack-scoped `pix-memory-<stack-id>`;
8. runs `--env NAME`'s own `[[setup]]` hooks (the v2 replacement for a pack's
   install/auth hook, `envsetup`); and
9. probes the complete result.

**Setup hooks (`envsetup`, L2).** An environment's `pix.toml` may declare
`[[setup]]` entries: `id`, `command`, `check_args`, `apply_args`, optional
`required`/`kind` (`install`|`auth`). They are the only environment-authored
code Pix executes on the host, and the path is narrow by construction:

- Parsing is strict and filesystem-free (`envinfo`): id grammar, argv
  presence, control characters, `..` segments, and unknown keys are refused
  at the exact file and line. A bare command name is refused — PATH is
  ambiguous and unfingerprintable.
- Resolution and identity are `envsetup`'s: a RELATIVE command or input is
  resolved against the environment root AND proven contained there —
  `envsetup.ProveContained` resolves every symlink in the target's parent
  chain (`filepath.EvalSymlinks`) and refuses a result that lands outside
  the root's own real location, closing the gap a purely lexical join
  leaves open when an ancestor directory is a symlink. An absolute command
  is allowed only because it is fingerprinted the same way — a regular,
  non-symlink file with a sha256 content hash (the executable must also be
  executable; a companion `inputs` entry need not be). `envsetup.Hook` is
  the type; `workflow/env.SetupHookFact` is a type ALIAS for it, so the
  value that is fingerprinted and rendered is the value that is executed.
- An `[[setup]]` entry may declare `inputs = ["relative/path", ...]`:
  companion scripts or data the hook's command reads. Each entry is
  strictly validated (relative, clean, `..`-free, unique per hook),
  resolved and containment-proven identically to a relative command, and
  fingerprinted as path + sha256 content hash — mutating one after
  acceptance re-gates exactly like mutating the executable does.
- The trust bill of materials (§11) carries id, command, content hash, both
  argv lists, kind, required, and every declared input's path + hash; all
  of them render in the DEFAULT consent screen (argv rendered with
  per-element shell quoting, never a bare space-`Join`, so a reviewer sees
  the real argument boundaries), and any change re-gates (fingerprint
  `v3`).
- Execution happens only under an explicit `pix setup --env NAME`, from ONE
  environment snapshot whose fingerprint is proven equal to the accepted
  record. Immediately before running, `envsetup.Run` builds a fresh,
  PRIVATE 0700 directory and copies the executable AND every declared input
  into it, verifying each one's content against its accepted sha256 in the
  SAME read that copies the bytes (`hosttrust.ReadVerifiedFile`: an
  O_NOFOLLOW open, fstat-regular on the already-open descriptor, then the
  hash) — never a separate "hash the path, then open the same path again to
  exec it" sequence, which is the actual TOCTOU gap a swap could still land
  in. check/apply/postcheck execute ONLY that snapshot's copied bytes, with
  the snapshot's own root as cwd; the directory is removed unconditionally
  when the hook finishes. Because the snapshot holds nothing but the
  reviewed executable and its declared inputs, an environment sibling file
  the hook did not name in `inputs` is unavailable to it by default. Beyond
  that: os/exec with argv, no shell, no injected environment. Check first
  (output captured and bounded, sanitized with `sys.TerminalSafe` before
  ever building a rendered message); apply only on a nonzero check, stdio
  inherited; then check again — only that post-check earns a success word.
  A required hook that stays unready fails setup; an optional one warns
  honestly. An `auth` hook with no TTY refuses and names the exact command.
- There is no hook registry, no supervisor, and no plugin API: a hook exists
  only inside the environment directory that declares it.

### Doctor

Doctor is read-only and runs independent probes concurrently. It checks release
artifact identity, environment parsing/trust, sbx registrations, model access,
secret references, memory container configuration and MCP behavior, session
state, and declaration drift. The secret-reference row is graded off THIS
`PIX_HOME`'s own `secrets.env`, never a host-global sbx secret; any
host-global provider or `GITHUB_TOKEN` secret the host holds is reported in a
separate, read-only row as ignored, never as evidence and never as
something doctor offers to remove. Every failure names one owner and one
exact next action.

### Reset

Reset first removes THIS stack's own positively identified Pix sandboxes
through the ordinary planner. It then stops and removes THIS stack's own
`pix-memory-<stack-id>` container and proves it is absent, and best-effort
removes THIS stack's own `pix-memory-<stack-id>`/`pix-session-<stack-id>` MCP
registrations. Only then may it rename `~/.pix` to a timestamped backup. A
second `PIX_HOME`'s sandboxes, container, and registrations are untouched. It
never recursively deletes user state or follows environment symlinks.

### Release

CI builds and scans both images, builds the one `pix` binary, assembles runtime
content, validates the strict kit, and emits the release manifest. Published
references are immutable digests; convenience tags are not launch identity.

## 13. Direct cutover

This is one replacement, not a sequence of supported intermediate products.
Implementation may use commits for local recovery, but no intermediate shape is
a compatibility target.

The fastest safe order inside the working tree is:

1. establish the new module/tree and release manifest;
2. extract memory, add MCP HTTP, and build `pix-memory`;
3. switch Pi memory extensions to Gateway MCP;
4. collapse paths into `PIX_HOME`;
5. reduce environment and sidecar handling to the agreed native form;
6. make create/attach use effective `sbx env create` plus explicit `sbx exec`;
7. implement the session-tree records on the retained lease primitives;
8. reduce setup, doctor, reset, tasks, and CLI help to the v2 surface;
9. delete every superseded package, command, config key, test, doc, dependency,
   and build target;
10. run focused automated verification and host UAT.

This ordering exists only to reduce rework. It does not require separate PRs,
release waves, compatibility shims, or green intermediate commits. If an
approach is wrong, reset or amend it in Git.

## 14. Deletion ledger

Delete, rather than port:

- `pix-host` and its command dispatcher;
- `pix serve`;
- Suture supervision, go-plugin, RPC proxy, front door, reattach, service-unit,
  holder-driven service lifecycle, and launchd/systemd supervisor installers;
- pack parsing, activation, trust, setup hooks, generated kits, receipts, and
  `pix pack`;
- Pix-owned MCP registration/authentication/catalog commands;
- scored routing, scorecards, intent selection, model administration, and stale
  model inventory;
- environment add/edit/forget/use registries and clone/update machinery;
- generic config get/set/unset command surfaces;
- top-level memory CLI and custom JSON-RPC transport;
- obsolete profile files written into project workspaces;
- user-facing UAT commands;
- old XDG path splitting and migration fallbacks;
- compatibility aliases and retirement messages;
- tests whose only subject is one of the above.

Remove `github.com/hashicorp/go-plugin` and `github.com/thejerf/suture/v4` from
the Go module. Split the memory service into its own module only if that lowers
its dependency/build surface; do not create a multi-module workspace merely for
visual purity.

## 15. Test retention rule

Tests survive only when they prove a v2 behavior or a destructive safety
property. Line coverage and preservation of historical test volume are not
goals.

Keep or rewrite focused tests for:

- CLI grammar and non-interactive implicit-run refusal;
- environment parse, symlink containment, effective compilation, and unknown-key
  refusal;
- canonical trust fingerprint and TOCTOU recheck;
- sbx JSON parsing, `pix-*` scope, instance identity, create receipt, and
  proof-gated removal;
- flock lease behavior under process exit and SIGKILL;
- session-tree parentage and zero-holder orphan classification;
- task Git guards and recovery refs against real temporary repositories;
- secret-reference validation and no-value output;
- memory schema, filtering, dedupe, recall, watcher budget, snapshot/restore,
  and MCP conformance;
- setup memory-container reconciliation with a fake Docker adapter;
- doctor probe classification;
- one end-to-end fake-sbx create/attach/remove path.

Delete broad tests that pin old package topology, prose, command aliases,
retired config keys, exact internal call ordering with no safety consequence, or
removed services. Prefer table tests and real temporary files/Git repositories
over large mock matrices.

The fast local gate is:

```text
go test ./...
npm test
tsc --noEmit
git diff --check
```

CI may add race tests, image builds/scans, MCP conformance, and host UAT. Do not
keep an arbitrary wall-clock gate if it makes the simpler architecture harder to
test honestly.

## 16. Host UAT gate

The PR is ready to merge only after one supported host proves:

1. install and setup from an empty temporary `PIX_HOME`;
2. both DHI images build and the release manifest points to exact artifacts;
3. `pix-memory` starts, survives Docker restart, retains its database, and is
   reachable through the sbx Gateway but not directly from the sandbox;
   **auth (security re-review round 1 blocker #1), exact steps:**
   `pix setup` generates `~/.pix/state/memory/auth.token` (mode 0600, the
   RAW `<64 hex chars>`, no `KEY=value` wrapping) and `docker inspect
   pix-memory` shows `-v ~/.pix/state/memory/auth.token:/run/secrets/
   pix-memory-auth:ro` in its create config — never `--env-file`/a literal
   `-e MEMORY_AUTH_TOKEN=...` argument, and never a
   `MEMORY_AUTH_TOKEN=<value>` entry in `docker inspect`'s `Config.Env`
   (that field is world-readable by anything on the host with inspect
   access, which is exactly what a bind-mounted, container-local FILE
   avoids) or in `ps aux` while `docker create`/`docker run` executed. Then:
   `curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:<port>/mcp`
   (no credential) must print `401`; the same curl with
   `-H "Authorization: Bearer $(cat ~/.pix/state/memory/auth.token)"`
   must reach the MCP handshake instead of `401`; and
   `curl -s http://127.0.0.1:<port>/healthz` must succeed with NO credential
   at all (§9.1's stated exception). `sbx mcp ls` (or the registered
   `.sbxenv.yaml`/effective document) must show the `pix-memory` URL carrying
   `?token=<the same 64 hex chars>` — the loopback-URL credential fallback,
   used because neither `.sbxenv.yaml`'s `mcp.servers` schema nor `sbx mcp
   add` can express a custom header (envinfo.MCPServer is name/url/command/
   args only). The token must never appear in `pix env trust`'s bill of
   materials, `pix env show --json`'s output, **`pix env show --effective`'s
   output (L1, security re-review: the display path redacts the token
   query-parameter VALUE to a fixed marker — workflow/env's
   RenderEffectiveDocument/redactBuiltinMemoryToken — while the canonical
   effective document a real `sbx env create` actually reads still carries
   the real value; this is presentation-only, never applied to executable
   bytes)**, `docker inspect`'s `Config.Env`, or any `pix` log line.

   **Accepted upstream limitation (host UAT risk, not a code defect):** the
   token DOES reach `sbx mcp add`'s own argv and whatever `sbx mcp ls`/the
   Gateway's own registry storage retains, because registering a
   loopback-URL-credentialed MCP server with sbx has no header-bearing
   alternative (same schema limit as above) and sbx's own registry is a
   host-global, same-user store outside Pix's control. Any other local
   process running as the SAME host user could in principle read it back
   out of that store or a process listing at the moment of registration.
   Host UAT must confirm this is an accepted, documented risk (not
   silently rediscovered later) rather than attempt to "fix" it inside
   Pix: the fix would require an upstream sbx capability (a header-bearing
   MCP declaration, or per-registration ACLs) this project does not own.
4. Pi lists and calls all memory tools;
5. deterministic recall and capture hooks call MCP through a second Gateway
   client connection;
6. `sbx env create` followed by `sbx exec -- <entrypoint> --model ...` selects
   the requested model;
7. the same attach path resumes the requested Pi session; **a SECOND `pix
   run --model <different> --resume <different>` against the SAME already-
   running sandbox (QA re-review F1) carries the NEW model/resume into the
   `sbx exec` argv — never the first run's create-time invocation;**
8. root and child nodes render in `pix ls`, and root exit does not remove a
   sandbox still held by a child runner;
9. final-holder exit removes an ordinary sandbox;
10. keep, unknown-state, and instance-mismatch cases preserve it;
11. `pix rm --orphans` removes only a positively identified zero-holder Pix
    sandbox;
12. a task checkout can commit and push from inside its sandbox and guarded
    removal preserves work;
13. trust changes re-prompt and non-interactive first use fails closed;
13b. an environment's `[[setup]]` hook converges under `pix setup --env NAME`
    (check, apply, check), is idempotent on rerun, and never runs during
    `pix run`;
14. `pix reset` removes the memory container before renaming `PIX_HOME`;
15. `pix help --all` contains only the accepted v2 surface;
16. a second concurrent `PIX_HOME` derives a distinct stack id, gets its own
    memory container and port, and its own `pix-memory-<id>`/
    `pix-session-<id>` Gateway registrations coexisting with the first
    stack's; `pix rm --all`/`pix reset` run against ONE stack leaves the
    other stack's sandboxes, container, and MCP registrations untouched;
17. the real `sbx` CLI accepts `sbx secret set -f --sandbox <name> <service>`
    with the value on stdin for a model provider key, a tool key, and
    `GITHUB_TOKEN`, and the resulting sandbox can use each one (a live model
    call, a tool call, a push), not merely that the command exits zero; and
18. `make load`'s worktree-scoped template tag and prune leave a second
    concurrent worktree's own loaded template untouched.

Items 16-18 are the coexistence proofs `scripts/host-uat.sh` is written to
exercise (its own U8/U9 steps); as of this docs sync no run against a real
host has reported them passing, so they remain open until a human runs the
script and pastes back the evidence lines, the same discipline every other
row in this gate already requires.

A failed item changes the architecture only at that seam. It does not restore a
retired subsystem as a fallback.

## 17. Definition of done

The cutover is complete when:

- the acceptance criteria in `pix-v2-surface.md` pass;
- the host UAT above passes;
- one host binary and two images are produced;
- `pix-host`, packs, routing, supervision, custom memory RPC, duplicate MCP
  ownership, and XDG-split state are absent from executable code;
- the dependency graph contains no go-plugin or Suture;
- current docs and help name no removed command as available behavior;
- retained tests pass; and
- code that exists only to ease migration to v2 has itself been deleted.
