# Make Pix a thin sbx environment launcher

Status: **ACCEPTED FOR IMPLEMENTATION**. This document specifies the Pix v2
product surface. It does not describe current behavior. The code target and
direct cutover are defined in `docs/design/pix-v2-architecture.md`.

This proposal supersedes the product and CLI decisions in
`docs/design/environments.md`. That document's observed sbx 0.39 contract remains
source evidence until a newer host run replaces it.

Pix v2 keeps the parts users cannot get from `sbx` alone: a pinned DHI-based Pi
runtime, transient sandbox lifecycle, parallel task checkouts, narrow host
capabilities, local memory, and one setup path. Native Docker Sandbox
environments own sandbox configuration. The sbx MCP Gateway owns MCP attachment
and is the only MCP path into a sandbox.

## 1. Product definition

Pix does four jobs:

1. Run the pinned Pix build of Pi inside a Docker Sandbox.
2. Select an environment and turn it into one exact sandbox invocation.
3. Tear down ordinary sandboxes when their last Pix session exits.
4. Create and manage isolated task checkouts for parallel work.

Pix is not a second sandbox configuration system, model router, MCP registry,
package manager, or general host automation framework.

The daily path is:

```console
cd ~/dev/project
pix
```

The explicit form is:

```console
pix run [DIR] [--env NAME] [--model MODEL] [--resume SESSION]
```

## 2. Prerequisites and upstream posture

Pix v2 requires:

- Git on the host for task checkout creation and safety checks;
- Docker Desktop or a Docker Engine configuration supported by `sbx`;
- Docker Sandboxes 0.39.0 or later, with native environment support;
- 1Password CLI when an environment uses direct keys or host credentials; and
- llmman or Ollama when local models, memory embeddings, or automatic memory
  capture need local inference.

Cloud-only sessions do not require llmman or Ollama. Environments that use only
sbx-managed provider sessions do not require 1Password.

Native sbx environments are experimental in sbx 0.39. Pix v2 requires a
numeric version core of 0.39.0 or later. A tagged build is accepted only when
its numeric core is strictly newer than that floor, so development builds such
as 0.41.0-rc1 can test newer sbx behavior while 0.39.0-rc1 remains refused. It
has no legacy launch fallback. A new stable sbx release enters the supported
range only after host acceptance tests pass.

## 3. Command surface

Pix v2 has nine user-facing command groups, plus help and version.

```text
pix [DIR] [run options]
pix run [DIR] [--env NAME] [--model MODEL] [--resume SESSION] [--dev]
pix ls [--json]
pix rm NAME... | --all | --orphans [--keep NAME]... [--force]

pix task new NAME [--from REF] [--env NAME]
pix task ls [--json]
pix task path NAME
pix task rm NAME [--force]

pix env [NAME] [--path|--effective|--json]
pix env default [NAME]
pix env trust NAME [--yes]

pix secret [--json]
pix secret set NAME OP_REF
pix secret rm NAME
pix secret check [NAME]

pix setup [--env NAME]
pix doctor [--env NAME] [--json]
pix reset

pix help [COMMAND|--all]
pix version
```

### 3.1 `pix` and `pix run`

Bare `pix` is the normal command. On an interactive terminal with existing
config it behaves like `pix run .`; on the first run for a new `PIX_HOME`, it
runs setup and launches only after setup succeeds. Explicit `pix run` skips that
first-run setup. With non-interactive stdin, bare `pix` prints `pix ls` and never
mutates or launches as a side effect of a script or pipe.

A **holder** is one live node in a Pix session tree that still depends on a
sandbox. The interactive root process is one holder. A running child agent is
another. Tree-wide holder count, not an arbitrary in-sandbox shell process,
determines ordinary teardown.

`pix run`:

1. resolves the requested environment;
2. validates its native sbx declaration and Pix sidecar;
3. refuses untrusted host execution;
4. verifies required MCP registrations and local endpoints;
5. creates or attaches to the exact Pix-owned sandbox;
6. starts or resumes Pi with the selected model; and
7. releases its holder when the session exits.

A normal sandbox is removed after its final holder exits. A kept sandbox and a
sandbox with unknown holder state are not removed automatically.

`--env NAME` selects an environment for this run without changing the default.
An unknown name is an error. Pix never falls back to the default after an
explicit but invalid `--env`.

Pix never auto-selects a `.sbxenv.yaml` found in a project workspace. Environment
selection controls credentials and host execution, so selection is explicit or
comes from the machine default.

`--model MODEL` overrides the environment's main model for this session. Pix
does not score or choose a model.

`--resume SESSION` is an option on `run`, not a separate top-level verb.

`--dev` mounts the current Pix source needed for live development. It is not a
second production launch path.

The transport used to pass model and resume arguments to the custom Pix agent is
an architecture decision. The surface guarantee is that both options apply to
every attach. If the selected transport makes either option creation-time state,
Pix must say so before implementation is accepted.

#### Session trees

Every `pix run` creates or resumes a session tree. Its root is the interactive
Pi session. Agent delegation creates child nodes rather than unrelated
processes. Each node records its parent, environment, model, workspace, execution
target, sandbox identity, and lifecycle state.

The first implementation may run child agents as processes in the root sandbox.
The tree model must also support a child in another local sandbox or an sbx cloud
sandbox without changing the parent-agent API. Future `sbx --cloud` integration
selects a node execution target; it does not create a second Pix session model.

A child inherits its parent's environment and trust ceiling by default. It may
use fewer mounts, credentials, MCP servers, or host grants, but cannot broaden
any of them without a separately approved environment transition.

The root may exit while children finish. Pix retains every sandbox claimed by a
live node and tears down local sandboxes only after the full tree releases them.
`pix ls` renders the tree; `pix ls --json` exposes stable parent and node IDs for
automation.

### 3.2 `pix ls` and `pix rm`

`pix ls` reports Pix-owned sandboxes, their environment, project, holder count,
and task association. It does not report readiness. Health belongs to
`pix doctor`.

`pix rm` removes only positively identified `pix-*` sandboxes carrying the
current `PIX_HOME`'s own stack id (a 16-hex id derived from that home's
canonical path, §10). It verifies the current sbx instance ID before
removal. Unknown sbx state fails closed. A second `PIX_HOME` running on the
same host is never a candidate: `--all` and `--orphans` discover only
through a listing filtered to this stack's own scoped names.

`--force` is an explicit authority override for a named Pix sandbox. It does not
widen Pix's namespace and never authorizes removal of a non-Pix sandbox.

An **orphan** is a Pix-owned sandbox that still exists after no live session-tree
node claims it, usually because a launcher or machine crashed before normal
teardown. `--orphans` removes only those positively identified sandboxes and
preserves every keep marker. `--keep` excludes named sandboxes from a bulk
operation.

### 3.3 `pix task`

A task is an isolated Git checkout plus a recorded Pix environment.

`pix task path NAME` prints only the task checkout's absolute path. It exists so
a shell can change directory without parsing human output:

```console
pix task new fix-auth --from main
cd "$(pix task path fix-auth)"
pix
```

A task records its environment when created. Changing the machine default later
does not change an existing task's credential or model context.

`task rm` requires zero live holders. It removes the associated sandbox through
the normal identity-safe `pix rm` path before removing the checkout. It refuses
a checkout with uncommitted work or unpushed commits unless the user supplies
`--force`.

### 3.4 `pix env`

An environment is a directory under:

```text
~/.pix/envs/<name>/
```

`PIX_HOME` may replace `~/.pix` for tests and advanced installations. Pix does
not split one personal tool across XDG config, data, state, and cache roots.

The directory name is the environment name. Pix does not maintain an environment
registration database. An environment stored elsewhere can be represented by a
symlink in this directory.

Pix resolves a symlink once per invocation. Every read, containment check, and
fingerprint uses that resolved path. Trust includes the resolved path, so
repointing the symlink invalidates approval. Pix refuses an environment source
that is owned by another user, group- or world-writable, nested inside another
environment, inside Pix global context, or writable through any workspace in
the effective declaration for the current launch. Pix rechecks this on every
launch, so a later environment or mount change cannot reuse an earlier result.

`pix env` lists names, resolved paths, the current default, and trust state.
`pix env NAME` presents one environment's resolved model roster, agent mappings,
MCP names, requested host capabilities, skills, and trust state. `--effective`
uses the current
directory as the project workspace and prints the exact native sbx environment
Pix would use for a new sandbox without creating one.

`pix env default` prints the machine default. `pix env default NAME` changes it.
The command does not run setup, reconcile MCP, start a service, or mutate a
sandbox.

`pix env trust` is the explicit approval command for host-executing
configuration. It prints the full bill of materials and records approval outside
the environment directory. `--yes` suppresses the prompt but not the bill. It
approves only the displayed fingerprint.

An interactive `pix run` may present the same complete trust operation on first
use, defaulting to No. This is the same operation as `pix env trust`, not a
second authority path. A non-interactive run never grants trust.

Pix does not provide general environment create, edit, add, forget, update, or
delete commands. Users create, clone, edit, move, and remove directories with
filesystem and Git tools. `pix setup` may scaffold the built-in default
environment as a first-run convenience.

### 3.5 `pix secret`

`pix secret` is retained because it enforces the product's 1Password-only secret
path. It manages references, never values.

- `pix secret` lists configured reference names and syntax state.
- `pix secret set NAME OP_REF` accepts only an `op://` reference.
- `pix secret rm NAME` removes one reference, never a 1Password item.
- `pix secret check [NAME]` resolves references through `op` without printing
  their values and reports whether the selected consumer can use them.

Setup calls this same capability rather than implementing a second secret path.
Native sbx receives dynamic references or resolved credentials through its
supported secret interfaces. No `sync` verb exists unless the architecture
proves a separate persisted synchronization step is unavoidable.

### 3.6 `pix setup`

`pix setup` is the supported path from an installed binary to a working first
session. It is interactive, repeatable, and safe to rerun.

It performs only these jobs:

1. checks Docker and the supported `sbx` version;
2. installs or verifies the pinned Pix kit and required DHI-based images;
3. creates global Pi settings from shipped defaults when they do not exist;
4. creates a runnable default environment when none exists, and selects it as
   the machine default in the same step (a fresh host must not scaffold an
   environment nothing then points at);
5. seeds and validates `op://` references without writing secret values;
6. installs and starts this stack's own local memory MCP container, named
   and ported for this `PIX_HOME`'s stack id (§10);
7. checks requirements declared by the selected environment (`--env NAME`),
   including validating any local inference backend that environment's
   `pix.toml` authors;
8. runs the selected environment's own `[[setup]]` hooks (§5.2) — the
   previously trusted installer or interactive authentication commands that
   replace a v1 pack's hook — check first, apply only on a failed check,
   then check again; and
9. probes the result before reporting it ready.

There is no setup interview. Setup never asks which cloud provider, llmman, or
Ollama to use: a provider key is added with `pix secret set`, and local
inference is authored directly in an environment's `pix.toml` (§7). Setup only
validates what an environment already declares; it never chooses on the
user's behalf and never silently prefers or migrates between backends.

`--env NAME` sets up one existing environment in addition to machine-level
prerequisites. It does not select that environment as the default.

When setup reaches an untrusted environment, it performs the same complete,
default-No trust operation as `pix env trust NAME` before running any installer
or authentication command. Non-interactive setup refuses and names that command.
Installer and authentication executable bytes follow the same pinning and
content-fingerprint rules as local MCP commands. Setup uses `pix secret` semantics
for every 1Password reference instead of creating another credential path.

Setup does not create a sandbox unless it ends with an explicit, confirmed first
run. The initial specification does not require that convenience.

### 3.7 `pix doctor`

`pix doctor` is read-only. It checks:

- Docker and sbx availability and version;
- the pinned Pix images, kit, and runtime-data version;
- global Pi configuration;
- environment schema and trust state;
- reachability of the selected environment's declared cloud and local model
  backends (Ollama and llmman, over their native or OpenAI-compatible
  transport, as that environment's `pix.toml` authors them);
- 1Password reference resolution where required;
- sbx Gateway MCP registration and authentication (Pix's own memory and
  session-control registrations use this stack's scoped names, so doctor
  never mistakes another `PIX_HOME`'s entry for its own);
- each required local command or integration-owned endpoint;
- 1Password reference resolution graded off THIS `PIX_HOME`'s own
  `secrets.env`, never a host-global sbx secret. A global provider or
  GitHub secret found on the host is reported separately, as ignored,
  never as evidence and never as something doctor offers to remove;
- the memory MCP endpoint, storage, embeddings, capture mode, and scope
  isolation; and
- sandbox declaration drift that requires recreation.

Every failing row names the owning system and one exact next action. Gateway
OAuth errors name the native `sbx mcp auth SERVER` command. Doctor does not
repair, register, restart, authenticate, or rewrite configuration.

### 3.8 `pix reset`

`pix reset` is the safe clean-slate recovery command. It removes this stack's
own Pix-owned sandboxes through the normal proof-gated removal path, stops
and removes this stack's own memory container, best-effort removes this
stack's own memory/session MCP registrations, then renames `~/.pix` with a
timestamped backup suffix. A second `PIX_HOME`'s sandboxes, container, and
MCP registrations are untouched. It contains no recursive delete.
Environment sources reached through symlinks outside `~/.pix` are never
moved. A post-operation probe must confirm the container is absent before
Pix moves its mounted state directory.

## 4. Files and ownership

Pix keeps all user-owned files under one root:

```text
~/.pix/
  .git/
  .gitignore
  README.md
  AGENTS.md
  skills/
  agents/
  output-styles/
  envs/
  pi/
    settings.json
    keybindings.json
    themes/
  config.toml
  secrets.env
  runtime/
  state/
```

`PIX_HOME` replaces `~/.pix` when set. The operating-system package manager
still owns the `pix` binary.

The Pix installer initializes `~/.pix` as an ordinary Git repository with
`git init -b main`. It creates the directory layout, README, and safe ignore
rules, but makes no commit, configures no remote, and never stages or pushes
files. Initialization is idempotent: an existing repository, file, or user edit
is preserved. `pix setup` and first run perform the same ensure step so package
managers that cannot modify the user's home directory do not leave a broken
installation.

The generated ignore rules exclude machine and runtime material:

```gitignore
/config.toml
/secrets.env
/runtime/
/state/
```

The shareable working tree is therefore the obvious part of `~/.pix`: global
instructions, skills, agents, output styles, Pi UX settings, and environments.
`secrets.env` is created with mode `0600`. Pix never runs `git add` on the user's
behalf.

### 4.1 Machine configuration

`config.toml` contains only machine-wide Pix choices that native sbx
environments cannot own:

- the default environment name, written by `pix env default NAME` (and once,
  atomically, by `pix setup` itself the moment it scaffolds a fresh host's
  first environment — see §3.6);
- the allocated pix-memory loopback port (`memory_port`), written once by
  `pix setup` on first run — a per-`PIX_HOME` allocation, never a shared
  constant, so two independent `PIX_HOME` installs on one host never collide;
- the pinned Pix image set and kit version, written by install or upgrade.

Local inference is never a machine-wide `config.toml` field: it is authored
per environment in that environment's own `pix.toml` (§7), so two
environments on the same host can name different backends, or none, without
either one touching the other.

Pix v2 has no generic config mutation command. Each field has one named writer.

Global Pi settings, keybindings, and themes apply to every environment. An
environment cannot replace them.

`~/.pix/secrets.env` contains 1Password references only. Pix never writes a
resolved secret value to disk.

### 4.2 Shipped runtime

```text
~/.pix/runtime/<pix-version>/
  manifest.json
  skills/
  agents/
```

The runtime directory is Pix-owned package data. It contains shipped skills and
agent definitions mounted into every sandbox instead of baked into the image.
Users do not edit it. Doctor verifies that runtime data and the pinned image
belong to the same Pix version.

The shipped skill set always includes a `pix` skill. It explains the current Pix
surface, environment authoring, host integrations, skill development, and the
sharing workflow from inside Pi.

### 4.3 User-owned content and sharing

The root-level `AGENTS.md`, `skills/`, `agents/`, and `output-styles/` directories
are the user's global writable content. Pix mounts them into every sandbox and
creates them even when empty.

Content precedence is:

1. environment-local content;
2. user-owned content at `~/.pix`;
3. shipped runtime content.

A higher layer shadows the same named skill or agent in a lower layer. `pix env
NAME` reports the winning source for every resolved name.

To hack a shipped skill, the `pix` skill copies it from the versioned runtime
into `~/.pix/skills/<name>`, where it becomes the live winning copy. The user
edits and tests it with `/reload`, then commits and publishes `~/.pix` with
ordinary Git tools.

Sharing is a product goal, but this proposal does not add `pix share` yet. The
first version is the Git repository the installer already initialized plus the
shipped `pix` skill. A CLI should be added only after the repeated workflow is
known; Pix should not wrap `git add`, `commit`, and `push` without a concrete job
Git fails to cover.

An environment under `envs/` can be tracked in the main `~/.pix` repository or
represented by a symlink to a separate repository. Pix never follows an
environment symlink while staging because Pix never stages files. A future share
check must refuse resolved secret values, machine state, and trust records before
publication.

### 4.4 Runtime state

Trust acceptance, sandbox instance records, session trees, leases, generated
effective sbx environments, task checkouts, and memory data live under
`~/.pix/state`. They remain outside every environment directory.

Deleting runtime state may require re-review or orphan recovery, but it never
deletes an environment source.

## 5. Environment format

An environment contains:

```text
~/.pix/envs/work/
  .sbxenv.yaml
  pix.toml
  skills/
  agents/
  context/
  README.md
```

Only `.sbxenv.yaml` and `pix.toml` are interpreted by Pix. The other directories
are inputs named by `pix.toml`.

### 5.1 `.sbxenv.yaml`

This is the native Docker Sandbox environment file. sbx owns its schema and
semantics. It declares:

- the `pix` agent and kits;
- sandbox resources and template;
- workspace mounts;
- environment variables;
- secrets and credential bindings;
- registries;
- MCP servers; and
- ports.

According to the sbx environment contract, `mcp.servers` performs host-global
registration and attachment to that environment's sandbox. Pix neither defines
a second MCP registry nor spawns those processes. A registration mismatch
refuses launch. Pix never overwrites a different host-global registration with
the same name.

Pix adds these restrictions before use:

1. literal secret values are refused;
2. `noVerify` is visible in trust review and enters the fingerprint;
3. the effective agent must be Pix;
4. the environment source cannot be writable through any workspace in the
   effective declaration for the current launch;
5. local executable and kit sources must be pinned or content-fingerprinted;
6. remote kit sources must be immutable or refused; and
7. host commands, writable mount expansion, and credential destinations require
   host trust approval.

Pix generates the final sandbox name and primary project workspace. An authored
primary workspace does not override the directory passed to `pix run`.

### 5.2 `pix.toml`

`pix.toml` contains only Pi and Pix facts that `.sbxenv.yaml` cannot express.
Field names below are part of the proposed surface; the next architecture review
must validate that they can remain this small.

```toml
schema = 1

[models]
main = "anthropic/claude-sonnet-5"

[agents]
fanout = "local/qwen3.5-9b"
review = "google/gemini-3.1-pro-preview"
deep = "anthropic/claude-opus-5"

[pi]
skills = ["./skills"]
agents = ["./agents"]
context = ["./context"]

[memory]
scope = "work"

[host.mcp.google-workspace]
env_keys = ["GOG_KEYRING_PASSWORD"]
probe = ["gog", "auth", "doctor"]

[[setup]]
id = "gh"
command = "./setup-gh"
check_args = ["check"]
apply_args = ["login"]
required = true
kind = "auth"           # install | auth; absent = install
```

The sidecar may declare:

- the main model and exact agent-to-model mappings;
- custom Pi inference backend and model definitions;
- environment-local Pi content paths;
- memory scope; and
- credential, health, and host-capability annotations for an MCP server
  declared in `.sbxenv.yaml`; and
- `[[setup]]` hooks: the host install/authentication commands `pix setup
  --env NAME` may run for this environment, and the only replacement for a
  v1 pack's authored hook. Each entry is strict (`id`, `command`,
  `check_args`, `apply_args`, optional `required`/`kind`; unknown keys,
  bare command names, control characters and `..` segments are refused),
  is executed as argv with no shell and no injected values, is
  content-fingerprinted into the trust bill, and never runs during a
  launch.

Environment content paths must resolve inside read-only or otherwise declared
workspaces present in the effective environment. Mounting a directory does not
tell Pi to load it, and naming an unmounted path is an error.

An environment sidecar cannot declare or supervise a host service. It also
cannot declare kits, workspaces, sandbox environment variables, secrets,
credential bindings, MCP transports, sandbox resources, sandbox ports, Pi
extensions, settings, keybindings, or themes.

Unknown keys are errors. Every model, backend, and agent reference must resolve
before a sandbox starts.

## 6. Runtime images

One source tree produces multiple independently pinned OCI images. At minimum:

- **`pix-agent`** uses the DHI Node/Debian base and contains the pinned Pix build
  of Pi, required system tools, intentional language toolchains, pinned Pi
  packages, core Pix extensions, required patches, and the Pix entrypoint.
- **`pix-memory`** is built from a DHI Go builder and a minimal DHI runtime. It
  contains only the memory MCP server and its runtime dependencies.

The images have separate Dockerfiles, build targets, tags, digests, and release
artifacts. The memory server is not installed in the agent image, and the agent
runtime is not installed in the memory image. Shared source code does not imply
a shared container boundary.

Neither image contains the shipped skill library, user keybindings or settings,
personal context, or private environment data. The memory image receives only
its dedicated state mount and explicit inference configuration.

Core Pi extensions stay in `pix-agent` because they are version-coupled
executable code. Shipped skills and agent Markdown stay outside because they
should update, compose, and reload without rebuilding an image.

### 6.1 Network ownership

The strict Pix kit owns the base network policy needed by Pi and built-in Pix
services.

MCP traffic flows through the sbx Gateway and does not require direct sandbox
egress to either a remote endpoint or a host-local implementation. Arbitrary
environment-specific sandbox egress belongs in an environment-provided sbx kit,
not `pix.toml`.

Network expansion is shown in trust review. An environment cannot silently
modify the shared base kit.

## 7. Models and local inference

Pix stores literal model choices. It has no scorecard, intent resolver, price
table, benchmark policy, or automatic winner.

Selection order is:

1. `pix run --model MODEL`;
2. selected environment `[models].main`;
3. Pi's default.

Agent selection order is:

1. an explicit model in a custom agent definition;
2. selected environment `[agents].<name>`;
3. selected main model;
4. parent model inheritance.

Local inference is an external host dependency. Pix supports llmman and Ollama,
reached over their native (Ollama) or OpenAI-compatible (llmman, or any other
OpenAI-compatible endpoint) transport. llmman serves Ollama-, OpenAI-, and
Anthropic-compatible APIs and loads models on demand. Ollama remains a
supported backend. There is no setup interview for either: the environment
author declares a backend and its models directly in that environment's own
`pix.toml` `[inference.*]` tables (docs/design/environments.md §5.2), and
`pix run` merges that declaration over machine config for the session it
launches. `pix setup --env NAME` and
`pix doctor` validate what an environment declares; neither ever silently
prefers or migrates one backend over another, and neither chooses on the
user's behalf.

Pix does not install model weights during an ordinary run.

If and when sbx exposes local-model transport to the custom Pix agent, Pix uses
it and deletes the in-sandbox compatibility bridge. Until a host acceptance test
proves that capability, the bridge remains a transport adapter for llmman or
Ollama and contains no model-selection policy.

Memory embeddings use the selected local backend when available and degrade to
keyword recall when it is unavailable.

## 8. MCP and host capabilities

Pix has one sandbox-facing integration path: the sbx MCP Gateway. It has no host
plugin system or generic service supervisor.

### 8.1 Gateway MCP

If a capability speaks MCP, the sbx Gateway owns sandbox attachment, routing,
policy, and tool exposure. The backing server may be a Gateway-launched local
stdio process or an independently running Streamable HTTP endpoint.

Examples include the Pix memory service, Google Workspace through `gog`, an
Arduino MCP server, GitHub, and SaaS remote MCP servers.

The native `.sbxenv.yaml` declares the server. Pix may annotate it in
`pix.toml` with required 1Password reference names and a doctor probe. Pix does
not proxy MCP around the Gateway.

Memory is a regular, independently versioned `pix-memory` OCI service using
Streamable HTTP, not a stdio subprocess. `pix setup` starts one container named
and ported for THIS `PIX_HOME`'s own stack (`pix-memory-<stack-id>`, §10)
with Docker's `unless-stopped` restart policy, a loopback-published endpoint,
and only `~/.pix/state/memory` mounted writable. The Gateway registers that
endpoint as a remote MCP server under the same stack-scoped name. This gives
all local sandboxes one durable, multi-client store without launching one
memory process per sandbox or sharing a live SQLite file among competing
containers, and lets a second `PIX_HOME` on the same host run its own
container and registration side by side with the first.

The memory server exposes recall, stats, remember, forget, observe, and its
administrative operations as MCP with accurate read-only, mutating, destructive,
and idempotency annotations. Pi extensions may call those tools
deterministically from lifecycle hooks for automatic recall and opted-in
capture. Skills define user and agent policy; there is no second private memory
protocol. Memory scope is enforced by the server on every read and write.

Streamable HTTP is also the deployment seam for a future remote or cloud memory
service. Local deployment does not require SSE for ordinary calls: the transport
may return plain JSON responses and use streams only when an operation needs
them.

MCP registration is host-global while sandbox attachment is environment-specific.
Two environments declaring the same MCP name with different command, arguments,
URL, or credential identity create a hard conflict. Pix refuses launch and
chooses neither. Pix's own two built-in registrations (memory and session
control) carry this stack's own id in their name, so a second `PIX_HOME`'s
built-ins add their own entries to that same host-global registry instead of
conflicting with the first one's. OAuth is performed with native `sbx mcp auth`.

### 8.2 Host-native MCP implementations

An MCP implementation that needs direct host GUI, credential-store, or device
access runs through one of the lifecycle systems already responsible for it:

- the sbx Gateway launches an on-demand host command over stdio; or
- an integration-specific launchd or systemd unit exposes Streamable HTTP when
  it needs a stable signed identity or durable host residency.

Pix neither launches a generic plugin host nor supervises these processes. It
trust-checks the exact executable identity, arguments, credential destinations,
and requested host grants before launch; `pix doctor` probes the declared
endpoint. An integration-owned resident service is installed only by an
explicit, trusted setup command and owns its own lifecycle and health recovery.

A host-native implementation is declared once as MCP in `.sbxenv.yaml`. Optional
credential, probe, UI, and device annotations live in `pix.toml`; users do not
declare a duplicate Pix service. Loopback binding is not authentication. A
credentialed resident implementation must authenticate callers and enforce its
scope on every request.

### 8.3 Computer-use automation

CUA uses a host-native MCP implementation because it may need the real display
session, browser profile, keyboard, pointer, accessibility permissions, or
physical devices. The sbx Gateway remains its only sandbox-facing transport.

An environment must request exact capabilities, for example:

```toml
[host.capabilities]
ui = ["screen.read", "input.pointer", "input.keyboard"]
devices = ["/dev/tty.usbmodem*"]
```

No declaration means no grant. A device glob authorizes current and future paths
matching that pattern, and the trust prompt states that explicitly.

A tool vocabulary without a shell primitive does not make UI input safe. Pointer
and keyboard control over the real session are equivalent in power to host code
execution. New or expanded UI and device grants require the same explicit trust
tier as host commands. The service enforces grants on every request.

The first CUA release requires a bounded metadata-only action log.

## 9. Trust and credentials

Native sbx owns sandbox isolation, base network policy, credential injection,
and sandbox-scoped secrets. Pix retains a host trust boundary because native
environments can cause code to run as the host user.

Host trust covers:

- local MCP commands and arguments;
- secret and registry resolver commands;
- declared installer and interactive authentication commands;
- local kit executable content;
- integration-owned resident service identities;
- credential names and destinations;
- writable host mounts and network expansion; and
- host UI or device grants.

Approval is stored under `~/.pix/state`, never inside the environment being approved.
Pix recomputes the fingerprint before every use. A changed fingerprint refuses
launch and names `pix env trust NAME`.

Secret values remain in 1Password. An MCP implementation's own refs resolve
only into that implementation's host command; one implementation cannot
inherit the complete machine reference file. Every other configured ref
(model provider keys, tool keys, `GITHUB_TOKEN`) resolves into a credential
scoped to the ONE sandbox a launch is entering, written fresh on every
create and every attach: Pix never writes a host-global sbx secret, and a
host-global secret it finds already on the host is read-only evidence of
nothing, reported separately and never removed automatically.

MCP registrations are host-global in sbx: the registry itself is one list per
host, not one per `PIX_HOME`. Pix's own two built-ins register under a
stack-scoped name (a 16-hex id derived from that home's canonical path, §10),
so a second `PIX_HOME` on the same host adds its own entries instead of
colliding with the first one's. Pix verifies that a selected registration
matches the reviewed declaration rather than trusting its name. It never
overwrites a conflicting registration and never automatically prunes a
registration or a credential binding it did not create.

## 10. Lifecycle and drift

Only environment variables and MCP declarations reconcile on an existing sbx
environment. Changes to kits, workspaces, resources, ports, secrets, bindings,
or sandbox options require recreation.

Creation-time declaration drift refuses attach and prints the exact
`pix rm ... && pix run ...` sequence. Automatic recreation could destroy
unpushed work in clone mode and bypass holder intent, so refusal is the default
for every drift a user authored or a host could not classify.

One bounded exception exists, because the blanket rule made every ordinary Pix
upgrade a manual removal loop. A drift set whose every facet is a Pix-owned
construction pin — the pinned agent image and pull policy, the pinned kit
references, or the two composed environment facts every launch carries
(`PIX_LAUNCHER_VERSION`, `PIX_STACK_ID`) — is recreation-safe: the user
changed nothing, Pix did. Pix removes and recreates that sandbox
automatically, and only behind the complete proof set ordinary teardown
already demands:

1. a listing re-read on this launch;
2. a positively zero holder census, never an unreadable one;
3. no keep marker;
4. a direct host-mounted workspace, never a sandbox-side clone or an
   undetermined mode;
5. an exact match against the recorded sbx instance; and
6. a still-reviewed environment, so an unreviewed kit change cannot ride in.

Removal uses the ordinary name-scoped, instance-checked path with no authority
override, and at most one recreate happens per invocation. Any missing proof
refuses, names the blocker, and prints the manual sequence. Every other drift —
an environment variable, a mount, a secret, a binding, an MCP server, a port —
still refuses.

Failed native environment creation may leave sandbox-scoped secrets and
host-global MCP or binding state. Pix records create intent before mutation and
uses positive instance identity before cleanup. It never guesses which shared
host-global resource is safe to delete.

## 11. Removed surfaces

Pix v2 has no:

- pack command, `pack.toml`, pack stack, activation ledger, or pack lock. A
  pack's authored install/authentication hook maps to one `[[setup]]` entry
  in the environment's own `pix.toml` (§3.6): it runs on the host only
  through an explicit `pix setup --env NAME`, after that environment's
  default-No trust review accepted its argv and its executable's content
  hash, and never as a side effect of a launch. That is a per-environment
  declaration, not a restored plugin system: there is no hook registry
  outside the environment directory, no supervisor, and no activation
  state;
- scored model router, scorecard, policy, intent mapping, or `routing.json`;
- Pix-owned MCP registration or authentication command;
- generic config mutation command;
- model catalog administration command;
- agent administration command;
- environment registration database;
- automatic environment clone or update mechanism;
- alternate unsandboxed host-agent mode;
- `pix-host`, `pix serve`, the host plugin API, or a generic service supervisor;
- top-level memory command, because memory is operated through `/recall`,
  `/remember`, `/forget`, and the memory tools inside Pi;
- user-facing UAT command, because release UAT is a development and CI surface;
  or
- compatibility path for removed commands.

## 12. Error and output rules

- Names are exact. Suggestions are information and never authorize an action.
- Exit code `2` means usage error or safety refusal. Other nonzero values mean an
  operational failure.
- `ready`, `verified`, and `removed` appear only after a corresponding probe.
- An error names the owning system, offending file or key where applicable, and
  one exact next action.
- Doctor never mutates. Setup mutates only after showing what it will do.
- Run never asks a setup questionnaire. It may render the exact environment
  trust operation on interactive first use; non-interactive first use fails
  closed.

## 13. Surface acceptance criteria

The surface is agreed only if all of these are acceptable:

1. `cd repo && pix` is the normal daily workflow.
2. `pix help --all` fits on one terminal screen.
3. Pix has one environment noun and no pack noun.
4. Native `.sbxenv.yaml` is the only sandbox declaration.
5. `pix.toml` contains no field native sbx already owns.
6. Pi settings, keybindings, and themes are global, never environment-specific.
7. All user-owned Pix files live under `~/.pix`, with `PIX_HOME` as the override.
8. The DHI-based agent and memory images, strict kit, pinned Pi build, and core
   extensions remain.
9. Skills and agent Markdown are distributed outside the image, mounted at run
   time, and easy to fork into the user-owned `~/.pix` Git repository.
10. The shipped skill set includes a `pix` skill for configuring, hacking, and
    sharing Pix itself.
11. `pix setup` can take a new machine to a verified first session.
12. Memory is a separate DHI-based OCI image and a Streamable HTTP MCP service
    using Docker's `unless-stopped` restart policy.
13. Pix ships one host binary, has no resident daemon or plugin supervisor, and
    uses the sbx Gateway as the only sandbox-facing integration path.
14. Host-native implementations use Gateway-launched commands or their own
    narrowly scoped OS service, not a Pix supervisor.
15. llmman and Ollama are supported local backends; no native sbx transport claim
    is made until custom-agent UAT passes.
16. Every run is a session tree that can later place child agents in local or
    sbx cloud sandboxes without replacing the session model.
17. A normal sandbox disappears after its final session-tree holder exits.
18. Existing task safety and scoped-removal guarantees survive.
19. Host execution, credential destinations, UI grants, and device grants
    require approval stored outside the environment.
20. Removed commands receive the ordinary unknown-command response. There is no
    migration compatibility layer.

## 14. Closed decisions and implementation evidence

The accepted defaults are:

1. interactive first-use launch offers the complete default-No trust prompt;
2. external environments use symlinks under `~/.pix/envs`, with the resolved
   path included in trust;
3. host setup is limited to installer and authentication commands attached to a
   declared MCP implementation;
4. the first CUA release requires a bounded metadata-only action log;
5. shipped agent Markdown lives outside the image with shipped skills;
6. llmman and Ollama are both supported initially;
7. sharing uses the initialized Git repository and shipped `pix` skill, with no
   `pix share` command;
8. memory uses a Docker `unless-stopped` container and Streamable HTTP MCP;
9. Pix ships no `pix-host`, resident daemon, plugin system, or generic service
   supervisor; and
10. one `PIX_HOME` is one stack (a 16-hex id derived from its canonical path),
    suffixing every sandbox name, the memory container, and the two reserved
    MCP server names, with no unscoped fallback, so two `PIX_HOME`s coexist on
    one host without either one adopting, replacing, or deleting the other's
    resources.

One question is deliberately evidence-gated rather than open for product debate:
the supported sbx host must prove that explicit `sbx env create` followed by
name-based `sbx exec -- <pix-entrypoint> --model ... --resume ...` carries model
and resume choices on every attach. The UAT and failure rule are in
`docs/design/pix-v2-architecture.md`. No compatibility launch path is authorized.
