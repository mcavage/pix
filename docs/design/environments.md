# Native sandbox environments

Status: PLAN, ready for implementation after Story 0 proves the sbx contract

Supersedes the unimplemented named-pack-rigs plan on this branch. At landing it
also supersedes `packs.md`, `packs-v2.md`, `packs-v2-impl.md`, `routing.md`, and
`models-cli.md`.

## 1. Decision

Pix will use Docker Sandbox environment files instead of maintaining its own
pack format and sandbox-composition engine.

An environment is a local directory registered under a short name:

```text
~/.local/share/pix/envs/home/
  .sbxenv.yaml    # native sbx: sandbox, kits, mounts, secrets, MCP, ports
  pix.toml        # only what sbx cannot express for Pi and Pix
```

The normal workflow becomes:

```console
pix env add home ~/.local/share/pix/envs/home
pix env use home
pix run
pix run --env work
```

`pix env use` sets the machine default. `pix run --env` selects another
registered environment for one sandbox without changing the default.

The old surfaces are deleted, not deprecated:

- no `pix pack`, `pack.toml`, pack stack, activation ledger, or `pack.lock`
- no intent router, scorecard, policy, `routing.json`, or `make routing`
- no `wired`, `unwired`, or `retired` model inventory
- no `pix run --intent` or `run_intent`

Pix has no released users. Compatibility would preserve the exact machinery
this change exists to remove.

## 2. Why

`pack.toml` and `.sbxenv.yaml` describe the same thing. Both can select kits,
MCP servers, credentials, mounts, environment variables, resources, and ports.
Keeping both would give Pix a second grammar, a translator, and permanent lag
behind sbx.

The model router has the same problem. It uses hand-maintained benchmark,
latency, and price estimates to compute choices the user already knows they
want. The honest interface is a literal table:

```toml
[models]
main = "zai/glm-5"

[agents]
engineer = "zai/glm-5"
review = "google/gemini-3.1-pro-preview"
```

Pix remains useful because sbx does not provide Pix's Pi setup, exact model
roster, host memory service, task/session lifecycle, explicit host-execution
trust review, or closed development UAT.

## 3. Ownership boundary

### 3.1 sbx owns

`.sbxenv.yaml` is the only authored sandbox declaration. It owns:

- agent and template
- kits
- primary and additional workspaces
- environment variables
- CPU and memory
- sandbox-scoped secret references
- credential bindings
- registry credentials
- MCP declarations and attachment
- published ports
- environment create, run, exec, and remove primitives

Pix does not mirror these fields in `pix.toml` or `config.toml`.

### 3.2 Pix owns

Pix keeps seven jobs:

1. **Environment selection.** Exact-name aliases, one machine default, and an
   explicit per-run override.
2. **Trust.** Canonical source identity, host-execution review, accepted
   fingerprints outside the environment payload, and re-review on change.
3. **Pi setup.** The Pix agent kit, extensions, personal context, and generated
   Pi inference manifest.
4. **Literal model roster.** One main model and optional agent-to-model
   overrides, with no scoring or optimization.
5. **Lifecycle.** Task/session leases, exact resume, instance identity,
   zero-holder teardown, keep markers, orphan recovery, and scoped removal.
6. **Host-only services.** Memory and any explicit non-MCP service that must run
   outside the sandbox.
7. **Development UAT.** The closed `uat-mcp`, candidate builds, host-backed
   lifecycle checks, browser isolation, bounded artifacts, and cleanup.

If a future `pix.toml` field duplicates an sbx environment field, the Pix field
is wrong and must be deleted.

## 4. Verified sbx 0.39 contract

Story 0 must re-prove these facts against the installed host release, but they
are already documented upstream:

- `sbx env` requires sbx 0.39.0 or later and is experimental.
- `sbx env run [PATH...]` creates if needed and attaches the declared agent. It
  accepts no agent-command passthrough.
- `sbx env create [PATH...]` creates without attaching.
- `sbx env exec [PATH...] -- COMMAND` runs a command in an existing environment;
  Pix instead uses name-based `sbx exec` after positive identity checks.
- `sbx env rm [PATH...]` removes the sandbox and scoped credentials. Pix supplies
  `-f` only through its proof-gated removal planner and never supplies
  `--prune-bindings` automatically.
- Multiple files compose in argument order. Nested maps merge by key, lists
  concatenate, and later files replace scalar values. Host-variable
  interpolation is resolved before Pix hashes the effective declaration; Story
  0 records exact undefined-variable behavior.
- Only environment variables and MCP declarations reconcile on an existing
  environment. Kits, workspaces, ports, secrets, bindings, and sandbox options
  require removal and recreation.
- Credential bindings and MCP registrations are host-global. Environment
  removal preserves them by default.
- A failed create can leave scoped secrets, bindings, or MCP registrations.
- The loader rejects unknown fields and unsupported schema versions.

The environment schema version and kit-spec version are unrelated. Native
`.sbxenv.yaml` currently uses `schemaVersion: "1"`; Pix kits remain strict
kit-spec v2.

### 4.1 Local model limitation

sbx 0.39 can route the built-in Claude Code agent to an existing host Ollama:

```console
sbx run --model gemma4 --provider ollama claude
```

That feature is experimental, unsupported on Windows, absent from the
`.sbxenv.yaml` schema, and not documented for custom agents. Pix runs Pi through
a custom agent kit, so `extensions/ollama-bridge.ts` stays.

Delete the bridge only after host UAT proves that sbx exposes a stable local
model transport to the Pix custom agent. Until then it is a transport adapter,
not a place for roster policy.

## 5. Authored files

### 5.1 Native `.sbxenv.yaml`

A registered environment uses the upstream schema directly:

```yaml
schemaVersion: "1"
agent: pix

kits:
  - ./kit

sandboxOptions:
  memory: 16g

env:
  PIX_MEMORY_SCOPE: personal

secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key

bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com

mcp:
  servers:
    - name: github
      url: https://api.githubcopilot.com/mcp/

ports:
  - sandbox: 3000
```

Pix imposes six restrictions on top of the upstream parser:

1. Literal `value` entries are refused in both `secrets` and `registries`.
   Secret values do not belong in authored files.
2. A secret or registry `command` is host execution. It enters the Tier-1 bill
   of materials and non-TTY review fails closed without `--yes`.
3. `noVerify` is fingerprinted and visible in review; it never silently weakens
   a proof.
4. The canonical environment root must not resolve inside any writable
   workspace it mounts.
5. Local and Git kit sources used by a registered environment must be pinned or
   content-fingerprinted before host review succeeds.
6. The effective agent must be Pix. Story 0 determines whether an agent kit
   listed in `kits` satisfies `agent: pix` or whether host-side kit installation
   is required. Any install step remains pinned and reviewed.

A registered environment omits `name`. Pix computes the workspace-specific
`pix-*` sandbox name and writes it only to the generated effective file. This
prevents one environment used by two repositories from colliding on one name.

Pix never rewrites the user's `.sbxenv.yaml`.

### 5.2 Pix sidecar `pix.toml`

`pix.toml` contains only concepts missing from sbx:

```toml
schema = 1

[models]
main = "zai/glm-5"
exclusive = false

[agents]
architect = "deepseek/deepseek-r1"
engineer = "zai/glm-5"
fanout = "deepseek/deepseek-v3"
review = "google/gemini-3.1-pro-preview"

[memory]
scope = "personal"

# Pi-only load paths. Each path must resolve inside a workspace declared by the
# effective environment. Mounting a directory does not tell Pi to load it.
[pi]
skills = ["./skills"]

# Optional metadata for a local-command MCP declared in .sbxenv.yaml.
# sbx does not currently express per-command env grants or a health probe.
[host.mcp.warehouse]
env_keys = ["WAREHOUSE_TOKEN"]
probe_args = ["probe"]

# Optional non-MCP service that must run on the host.
[[host.services]]
name = "warehouse-proxy"
command = "warehouse-proxy"
args = ["serve"]
port = 19443
probe = "http://127.0.0.1:19443/health"
```

Custom Pi providers may add environment-local inference definitions:

```toml
[inference.backends.zai]
driver = "openai-compatible"
protocol = "openai-completions"
base_url = "https://api.z.ai/api/paas/v4"
auth = "1password"
key_env = "ZAI_API_KEY"

[[inference.models]]
id = "zai/glm-5"
backend = "zai"
upstream_id = "glm-5"
context_window = 200000
max_output_tokens = 32000
reasoning = true
```

Unknown keys fail validation. A typo in a model or host-service declaration
must not become a silent fallback.

The sidecar deliberately has no kits, workspaces, environment variables,
secrets, bindings, MCP command/URL definitions, resources, or ports. The
`host.mcp` block may only annotate an MCP server whose command definition lives
in `.sbxenv.yaml`. Environment-owned `kit/` may carry skills, agents, knowledge,
extensions, and `files/` such as a private `capabilities.json`; `.sbxenv.yaml`
mounts the kit while `[pi].skills` tells Pi what to load live.

### 5.3 Machine `config.toml`

The single machine config gains an environment index and default:

```toml
environment = "home"

[environments]
home = "/Users/alice/.local/share/pix/envs/home"
work = "/Users/alice/dev/work-pix-env"

[inference.backends.anthropic]
driver = "native"
auth = "sbx-session"

[[inference.models]]
id = "anthropic/claude-sonnet-5"
backend = "anthropic"
upstream_id = "claude-sonnet-5"
```

`pix env add|rm` are the only writers of `[environments]`. `pix env use` is the
only writer of `environment`. Input may use `~`, but Pix persists only canonical
absolute paths. `pix config set` refuses these keys and names the correct env
command, preserving the trust gate.

Machine inference declarations answer “which exact models can this machine
serve?” Presence is the declaration. The following fields disappear:

- `available`
- `verified`, `verified_by`, `verified_at`
- `allowed_models`
- `roster_providers`
- `run_intent`

`pix models add` probes before writing a model definition. `pix doctor` probes
again on demand. Probe results do not become a persisted model-status system.

P0 environment sources are local directories only. A user who keeps an
environment in Git clones it with Git, then registers the local path. Pix does
not retain the pack clone/update subsystem. Native Git kit references remain
available inside `.sbxenv.yaml`.

`config.toml`'s old `mcp` attachment list is deleted because environments own
attachment. `pix mcp add|ls|auth` remain explicit machine operations: hosted
OAuth still requires `auth`, while an environment declaration decides which
registered server enters a sandbox.

## 6. Precedence and composition

### 6.1 Environment selection

1. `pix run --env NAME`
2. `config.toml` `environment`
3. no registered environment

A workspace `.sbxenv.yaml` is never selected automatically. If one exists, Pix
may print a single hint, but it launches without it until the user runs
`pix env add` and reviews it.

### 6.2 Environment execution plan

Pix materializes one stable effective environment file per sandbox:

```console
sbx env create <state>/environments/<sandbox-name>/effective.sbxenv.yaml
sbx exec -it <sandbox-name> -- pi <exact invocation>
```

The leaf environment adapter parses the authored file, resolves local relative
paths against its source directory, applies documented sbx merge semantics, and
adds only Pix-owned runtime facts:

- canonical `pix-*` sandbox name
- pinned Pix template and `pullPolicy: missing`
- workspace in object form, including clone choice
- unconditional personal-context additional workspace
- generated Pi mixin kit
- development checkout kit and live skill arguments when `--dev`
- Pix-required environment variables
- reviewed local-MCP credential wrappers

A single rendered file is required because sbx concatenates lists. A second file
cannot safely replace one named MCP server or port. Pix records the authored and
sidecar digests, writes the effective file atomically, and retains it until a
positive absent probe. Create and remove always use this same path. The adapter
is the only package that knows native environment grammar; it does not revive a
second authored schema.

With no selected environment, the effective file is generated from Pix defaults.
First-run `pix run` remains possible without teaching environments during setup.
Story 0 proves relative paths, `${VAR}` and `${VAR:-default}` interpolation,
custom-agent selection, and candidate-image behavior before this adapter lands.

### 6.3 Session model

1. `pix run --model ID`
2. selected environment `pix.toml` `[models].main`
3. Pi's default

An explicit model must exist in the composed machine plus environment inference
definitions. It is not constrained by a general allowlist. When
`[models].exclusive = true`, only environment-local inference definitions are
valid; this is the narrow compliance-grade boundary for a private gateway.

### 6.4 Agent model

1. explicit `model:` in a custom agent file
2. selected environment `[agents].<agent-name>`
3. selected main model
4. inherit the parent session

Shipped `agents/*.md` declare no `intent:`, `fallback_intent:`, or `model:`.
The environment roster is the one editable table for shipped roles. A custom
project agent may deliberately pin its own exact model.

The existing parent-Ollama inheritance exception stays with the bridge until
native custom-agent local-model transport replaces it.

## 7. Generated Pi runtime

The host writes one backward-compatible `~/.pi/agent/inference.json` version 1
into a generated mixin kit. `roster` is an additive field; existing version-1
readers ignore it, so a new host binary cannot brick a sandbox using an older
image:

```json
{
  "version": 1,
  "environment": "home",
  "backends": {},
  "models": [
    {
      "id": "zai/glm-5",
      "backend": "zai",
      "name": "glm-5",
      "context_window": 200000,
      "max_tokens": 32000,
      "reasoning": true
    }
  ],
  "roster": {
    "main": "zai/glm-5",
    "agents": {
      "engineer": "zai/glm-5",
      "review": "google/gemini-3.1-pro-preview"
    }
  }
}
```

`extensions/inference.ts`, `extensions/ollama-bridge.ts`, and
`extensions/subagents.ts` read the same file. There is no second generated
routing artifact that can disagree with provider registration.

Every roster reference must resolve to a model in `models`. Every model must
reference a defined backend where Pix generates the provider. Validation names
the source file and key.

`pix agent ls` prints `AGENT`, `MODEL`, and `SOURCE`. `pix models` prints only
models declared by machine config or the selected environment, with `BACKEND`
and `SOURCE`. Neither command computes a winner or prints `WHY`.

## 8. CLI contract

```console
pix env                         # alias for ls
pix env ls [--json]
pix env add NAME [PATH] [--yes]
pix env use NAME
pix env show [NAME] [--json] [--path]
pix env edit [NAME] [--sbxenv]
pix env review NAME [--yes]
pix env rm NAME [--force]

pix run [DIR] [--env NAME] [--model ID]
```

### 8.1 Command behavior

- `add NAME PATH` registers a canonical local directory, validates both files,
  always completes host review, and records acceptance outside the directory.
  Non-host-executing environments produce an empty bill and need no prompt.
- `add NAME` scaffolds `~/.local/share/pix/envs/NAME`. The generated environment
  is runnable and equivalent to the current default, not an empty stub.
- `use NAME` only changes the machine default. It performs no adoption, host
  registration, or probing. An unreviewed environment is refused.
- `show` displays authored environment and Pix facts. `--path` prints only the
  canonical root.
- `edit` opens `pix.toml`; `--sbxenv` opens the native file. It validates after
  the editor exits and reports whether `review` is required.
- `review` reruns the host bill-of-materials gate after an intentional local or
  Git change.
- `rm` unregisters but never deletes the environment directory. It refuses the
  default or a live holder unless the explicit force contract permits it.

Names are exact. Only `add` accepts a path. There is no fuzzy or prefix action.
`pix reset` renames scaffolded environment sources with the data directory and
invalidates every acceptance; it does not delete those sources.

Example errors:

```text
pix: no environment named "hoem".
     known: home, work, luna
     register one: pix env add <name> [path]
```

```text
pix: environment "work" changed what it runs on your host.
     review it: pix env review work
```

```text
pix: sandbox pix-repo-home was created with environment "home".
     create-time environment state differs.
     create the work environment: pix run --env work --name pix-repo-work
```

`pix setup` does not require an environment. Environments are progressive
disclosure for users who want a named context beyond the working default.

## 9. Trust and credentials

### 9.1 Two fingerprints

Pix keeps two separate canonical documents:

- **Host trust fingerprint:** only facets that execute on the host, change
  credential disclosure, route model traffic, or expand mounted host access.
  A change requires `pix env review`.
- **Creation fingerprint:** every effective create-time facet after upstream
  environment composition. A change refuses attach and names the recreate
  command.

Comments and harmless formatting do not change either fingerprint. Canonical
semantic values do. Host interpolation is resolved before canonical hashing;
undefined-variable behavior is pinned by Story 0. Referenced local executable
and kit bytes are hashed with symlink refusal. Remote sources must be immutable
or rejected.

The trust fingerprint includes:

- canonical environment root
- local MCP command and ordered args
- MCP URL/OCI identity and definition digest
- secret and registry `ref` or `command` declarations, never resolved values
- registry host and `noVerify` state
- credential bindings and destination domains
- local kit paths and content digests
- additional workspace canonical paths and read-only bits
- custom inference endpoints and auth/header format
- `host.mcp` credential names and probe args
- host service executable bytes, args, port, env-name set, and probe

The accepted record lives in launcher-owned state outside the environment. A
name never carries trust, and repointing a name never transfers acceptance.
Non-TTY review fails closed without `--yes`.

### 9.2 MCP registration

sbx env owns registration and attachment. Pix verifies before launch that every
selected host-global MCP definition matches the reviewed digest. A stale
registration with the same name does not satisfy an environment.

Native `.sbxenv.yaml` currently cannot express per-command environment grants.
For a local command annotated by `pix.toml [host.mcp]`, Pix generates a private
0600 refs file containing only declared `op://` references and rewrites the
generated effective command to:

```console
op run --no-masking --env-file=<server-specific-file> -- <declared argv>
```

A server declaring `A` must not observe configured ref `B`. Delete this wrapper
only after host UAT proves upstream per-server credential scoping.

Ordinary sandbox teardown never removes host-global MCP registrations or
credential bindings. Pix never passes `--prune-bindings` on an automatic path;
that flag deletes complete shared binding entries and can break other sandboxes.
Only a future explicit named command may expose it, with the shared-binding
warning and a positive post-mutation probe. `pix env rm` unregisters the alias,
not shared upstream state. Explicit review/reconciliation and `pix reset` own
safe cleanup. UAT/session-owned registrations remain scoped and cleaned by their
existing lease.

### 9.3 Failed creation

Before spawning sbx, Pix records a bounded create intent containing environment
identity, sandbox name, and desired creation fingerprint. A verified create
receipt replaces it with the normal instance-bound session record.

sbx resolves scoped secrets, bindings, and MCP registrations before creating the
sandbox. The create intent records a positive pre-create absent probe, but that
alone is not removal authority because another creator can race. Pix calls
`sbx env rm -f` after failure only when it first obtained a positive create
receipt and a fresh probe still reports that exact instance id. Then scoped
secrets are removed, while bindings and MCP registrations survive. If create
failed before a receipt, Pix fails closed and reports possible residue instead
of risking another sandbox. The next run and orphan sweep use the create intent
to diagnose partial state without guessing.

## 10. Lifecycle

Pix continues to decide create versus attach from a positive sbx listing. An
unknown listing authorizes nothing.

### 10.1 Create

```text
resolve registered environment
  -> parse native env + pix.toml
  -> validate location and model references
  -> recompute host trust fingerprint
  -> compose machine + environment inference
  -> generate Pi mixin and stable effective environment
  -> compute creation fingerprint
  -> write create intent
  -> sbx env create <effective> [--clone]
  -> poll for positively identified instance
  -> bind lease/session record to instance id
  -> sbx exec -it <name> -- pi <exact invocation>
```

The existing post-exit settle poll stays. A fast child exit must not strand a
sandbox that appeared just after the process ended.

### 10.2 Attach

An existing sandbox attaches only when:

- sbx reports a schema-verified running row
- the instance id matches the recorded session
- the recreate-only creation fingerprint matches
- the environment remains reviewed

Pix uses name-based `sbx exec -it <name> -- pi <exact invocation>` after those
checks. The exact session model is injected as `--model` on first exec and every
reattach. It does not ask sbx to re-derive the Pi invocation from the environment
path.

Although upstream can reconcile environment-variable and MCP changes in place,
Pix P0 treats every effective declaration change as recreate-only. That keeps
one fingerprint and avoids invoking `sbx env run`, which would attach the agent
without Pix's exact resume/model arguments. Reconciliation can be added later
only with UAT proof and no second launch path.

### 10.3 Remove

`pix rm` retains the current proof chain:

- `pix-*` scope
- lifecycle and holder exclusivity
- same instance id on a fresh probe
- no keep marker
- unknown state fails closed
- forced removal only through explicit `pix rm --force`

A new `sandbox.PlanEnvRemove` composes the same stable effective path, recomputes
its effective name, refuses anything outside `pix-*` or unequal to the recorded
instance name, and appends `-f` only inside the existing proof-gated removal
seam. It never appends `--prune-bindings`. The normal mutation is
`sbx env rm -f <effective>`. State and the effective file clear only after a
positive absent probe.

When the effective file is absent, as with a pre-migration sandbox or a hard
crash that lost state, Pix falls back to the existing name-based `sbx rm` planner
with the same `pix-*`, holder, keep, instance-id, and fresh-probe proofs. It
reports that environment-scoped secret cleanup could not run; it never guesses
or prunes shared state. `pix reset` sweeps sandboxes before renaming state so its
normal path retains effective files. A failed sweep stops reset before any
rename.

### 10.4 Host services

`pix-host serve` remains machine-global. Desired environment services are:

```text
services from the machine default environment
UNION
services from every positively live environment holder
```

The launch path checks unit-name and port collisions synchronously. Unknown
holder state preserves a running service. A unit stops only after no default or
live holder references it. This is the one environment feature sbx does not
provide because the process runs outside the sandbox.

## 11. Development UAT

UAT stays. It is a Pix-owned capability, not pack code:

- `uat-mcp` remains ephemeral, dev-only, launcher-created, and absent from
  `pix-host mcp --list`
- the runner remains a closed action vocabulary with no arbitrary shell action
- candidate image, Darwin binaries, browser profile isolation, bounded logs,
  status, artifacts, and abort remain
- existing candidate and memory checks remain

Story 0 extends candidate smoke with capability-named checks, not a generic
`sbx_env_smoke` action:

- `environment_create_then_exec_invocation`
- `environment_uses_local_candidate_image`
- `environment_recreate_boundary`
- `environment_failed_create_cleanup`
- `environment_rm_scope_refusal`
- `environment_custom_agent_ollama`

The scenario remains `uat/scenarios/smoke.yaml`; capabilities report the named
checks. Each check owns typed inputs internally, so the MCP caller cannot supply
host commands, argv, environment variables, or arbitrary paths.

The UAT matrix must prove:

1. one rendered native file creates with the local candidate image
2. custom Pix agent selection works, then name-based exec receives the generated
   kit, personal context, live skills, exact model, and resume arguments
3. direct and clone workspaces work
4. every effective declaration change requires explicit recreation in P0
5. failed creation leaves no untracked session-owned resources
6. two holders preserve the sandbox until the last exits
7. non-`pix-*` and instance-name-mismatched removal is refused
8. instance-id reuse refuses teardown
9. reset removes candidate sandboxes through Pix and preserves rename-only state
10. Ollama passthrough either works for the custom agent or records the bridge as
    still required

## 12. Implementation plan

Every story ends green. “No parallel paths” governs launch entry points reachable
from `pix run` and generated artifacts consumed at runtime. Dead pack-builder
code may survive until Story 5 and unused router code until Story 4, but neither
is user-selectable beside its replacement.

### Story 0: Prove the native environment contract

**Goal:** Replace upstream assumptions with host evidence before production
architecture depends on them.

Changes:

- add the six named UAT matrix checks above
- generate authored and stable effective environment fixtures inside the UAT run
- exercise `sbx env create|rm` plus name-based `sbx exec` against candidate Pix
- capture rendered files, sbx version, listings, and bounded logs as artifacts
- write `docs/upstream/sbx-0.39-environments.md` with observed argv and results

Acceptance:

- the existing UAT MCP submits the committed candidate and returns a passing
  verdict for create/attach and recreate-boundary checks
- no generic UAT shell/argv/env input is added
- unsupported custom-agent Ollama is an explicit, non-failing capability result
  that keeps the bridge requirement
- any failure that invalidates the design stops Story 1

### Story 1: Add environment schema and inventory

**Goal:** Register, inspect, scaffold, select, edit, and review local native
environments without changing launch behavior yet.

Changes:

- add a leaf native-env parser/renderer package isolated from Pix workflows
- add strict `pix.toml` parsing and model/host metadata validation
- add config environment index/default fields
- add `pix env ls|add|use|show|edit|review|rm`
- gate `pix run` and `doctor` on a positively parsed sbx version `>= 0.39.0`;
  unknown fails closed with an exact upgrade instruction
- lift canonical identity, trust store, fingerprint, atomic write, and lock
  primitives out of pack code with environment names
- refuse `pix config set` for launcher-owned environment keys

Acceptance:

- `pix env add home` creates a runnable scaffold
- exact names resolve from any working directory
- environment roots inside writable workspaces or through symlinks are refused
- dangerous changes require review; names never transfer acceptance
- `use` changes only the default field
- `rm` never deletes source files

### Story 2: Launch through `sbx env`

**Goal:** Cut `pix run` over to native environments and remove every selectable
pack launch path. Dead pack internals may remain until Story 5.

Changes:

- generate the stable effective environment and Pi mixin kit
- launch registered environments through `sbx env create` followed by exact
  name-based `sbx exec`
- adapt create receipt, exact attach, creation fingerprint, and teardown to the
  stable environment path
- add create-intent recovery for partial creation
- retain session-owned UAT MCP injection in the generated effective environment
- convert the private work repository and complete one live run before cutover
- delete `pix pack`, `pix run --pack`, pack config selection, and pack-driven
  host-service activation in the same cutover

Acceptance:

- selected and no-environment launches both work
- transient `--env` changes zero config bytes
- attach refuses every effective declaration drift in P0
- safe removal and orphan recovery retain every existing proof
- help and dispatch expose no pack launch or activation path
- old builder code is unreachable and marked for Story 5 deletion

### Story 3: Replace routing with the literal roster

**Goal:** One authored table determines Pi's main and subagent models.

Changes:

- simplify machine/environment inference definitions
- add the exact roster to backward-compatible `inference.json` v1
- update inference, Ollama, and subagent extensions to read that file
- update `pix models` and `pix agent ls` to print facts and source
- strip `intent:`, `fallback_intent:`, and shipped `model:` from agents
- preserve only the temporary parent-Ollama transport exception

Acceptance:

- every referenced model resolves or launch names the exact bad key
- an unlisted shipped agent uses the main model
- custom project agents may use their own exact model when unlisted
- machine and selected-environment model output contains no status taxonomy
- no score, price, or benchmark affects model selection

### Story 4: Delete the router

**Goal:** Remove every obsolete scored-routing surface in one reviewable cut.

Delete:

- resolver, scorecard, policy, compile, and routing tests
- `services/host/routing/defaults/*.json` except a small setup-only local Ollama
  hardware table moved under inference
- `services/host/route.go`
- checked-in and baked `routing.json`
- `make routing` and compile-oriented model targets
- `pix models pick|show --catalog|ls --catalog`
- `pix run --intent`, `run_intent`, and intent documentation
- `skills/model-refresh`
- every routing/model-refresh reference in healthcheck, help, Ollama, subagents,
  onboarding, and documentation

Acceptance:

- a sentinel rejects `scorecard`, `prefer_providers`, `max_cost_usd`,
  `run_intent`, and `CompiledRouting` in live code
- local Ollama setup still uses explicit RAM/download/context facts, with a
  shape test preventing that table from becoming a general catalog
- full Go and Node tests pass

### Story 5: Delete packs

**Goal:** Remove the second sandbox format and all reversible projection.

Delete:

- dead `pix pack` and `pix run --pack` implementations and tests made
  unreachable in Story 2
- `pack.toml` loader and `packinfo` schema
- workflow/pack activation, projection, stack, lock, contribution rollback,
  clone/update, setup hooks, and compatibility paths
- pack-generated MCP, inference, Ollama, wrapper, skill, and context projection
- pack-specific service and doctor readers after environment replacements land
- dead config readers for `pack`, `packs`, and kit-stack fields removed from the
  public config surface in Story 2

Lift rather than delete:

- canonical trust/fingerprint primitives
- server-specific credential filtering
- host-service reconciliation
- exact probes and success-word discipline

Acceptance:

- grep and AST sentinels reject live `pack` commands/types/config keys
- environment launch performs no config save
- the already-converted private work environment expresses every required live
  facet using native env plus the narrow Pix sidecar
- net production code is materially smaller

### Story 6: Documentation and concurrent acceptance

**Goal:** Leave one current mental model and prove it against home and work.

Changes:

- replace pack/routing sections in README, reference, getting started,
  SECURITY, AGENTS, onboarding, healthcheck, and design index
- mark historical pack/router design docs as superseded or delete docs that
  describe an unshipped surface
- add `docs/how-to/environments.md` with model, MCP, and sharing recipes
- update the upstream MCP/static-attachment gotcha and minimum sbx version
- update `tests/agents-md-invariants.test.mjs`, the AGENTS context-budget ratchet,
  and semantic-diff config-key/lifecycle rules as invariants, not deletions
- run home and work environments concurrently through UAT/manual acceptance

Acceptance:

- `pix help --all` and docs enumerate the same command/config surface
- open-core checks contain no private names or endpoints
- no current doc tells a user to edit routing files, run `make routing`, or use
  packs
- existing sandboxes are recreated rather than silently attached under a new
  environment identity

## 13. Deletion ledger

Expected removals include:

- `services/host/workflow/pack/`
- pack-shaped parts of `services/host/packinfo/`
- `services/host/routing/` scored resolver and defaults
- `services/host/route.go`
- `services/host/cmd/pix/pack_cmd.go`
- pack activation/launch/doctor/setup call sites
- most of `services/host/workflow/launch/sbxargs.go`
- pack and router tests whose subjects no longer exist
- `routing.json`
- `skills/model-refresh/`
- old pack/routing command and config documentation
- `config.toml` pack, kit-stack, scored-routing, `run_intent`, and MCP attachment
  list fields

Expected retained or adapted code includes:

- `services/host/uat/`, `uatmatrix/`, and `workflow/uat/`
- `services/host/sandbox/`, `lease/`, task and resume workflows
- memory and supervision infrastructure
- inference provider generation and Ollama endpoint probing
- `extensions/ollama-bridge.ts` temporarily
- `pix mcp add|ls|auth` for explicit host registration and OAuth
- environment-renamed trust tests and semantic-diff safety rules
- MCP `op run` wrapper only where upstream lacks per-server grants
- trust store locking, symlink refusal, atomic writes, and canonical hashing

The implementation PR must report measured production and test line deltas, not
repeat this estimate as fact.

## 14. Test strategy

### Unit

- strict environment and sidecar parsing
- exact alias/path resolution
- canonical path and workspace-containment refusal
- model/backend/roster reference validation
- semantic trust and creation fingerprints
- local MCP annotation matches native MCP command
- host-service collision and desired-set union
- create-intent recovery state machine

### Golden

- no environment
- scaffolded home environment
- environment with native remote and local MCP
- private exclusive custom backend
- generated `inference.json` v1 with roster
- generated stable effective environment
- effective trust bill of materials

### Lifecycle

- create, exact attach, last-holder removal
- transient environment with zero config mutation
- recreate-only effective declaration drift
- failed-create residue
- instance-id reuse
- unknown listing fails closed
- default plus live-holder host services
- reset with live environment and shared MCP registration

### Fitness functions

- no public `pack` command or config key
- no scored routing symbols or files
- no workspace-controlled implicit environment selection
- no launch-time config save
- one package owns native sbx env grammar
- one generated file owns Pi models and roster
- composed effective name equals the recorded `pix-*` sandbox name before create
  or remove
- local Ollama table contains no price, accuracy, or routing fields
- `uat-mcp` stays dev-only and absent from host MCP listing
- docs command/config enumerations match generated help

### Verification commands

The minimum final gate is the repository's CI-equivalent target:

```console
make gate
```

That target owns Go build/vet/tests, Node tests, TypeScript checking, open-core,
semantic-diff, and invariant checks. Stories add their new tests to the gate
rather than maintaining a second command list here.

The final candidate also runs the host-backed UAT scenario through the UAT MCP.
No release claim is made from local tests alone.

## 15. Risks and stop conditions

- **Experimental upstream schema:** all sbx grammar stays in one adapter and is
  pinned by golden UAT artifacts. A breaking sbx release updates one boundary.
- **Custom agent unsupported:** if Story 0 cannot run Pix as the environment
  agent, stop. Do not build a second launcher beside native env.
- **Global-resource residue:** failed creation and unregister never guess about
  shared bindings or MCP. Unknown state preserves and reports.
- **Two files drift:** strict validation and one-line ownership rule prevent
  sandbox fields from entering `pix.toml`.
- **Trust regression during deletion:** pack trust tests are translated by
  invariant before pack code is removed. They are not deleted merely because
  their old subject was named pack.
- **Private environment gap:** Story 5 does not land until the private work
  context is representable without adding company-specific fields to public
  Pix.

Stop the migration if Story 0 disproves custom-agent launch, exact attach, or
safe removal through native environments. Those are requirements, not polish.

## 16. Success criteria

- One native environment file describes everything sbx already owns.
- One small sidecar describes only Pi/Pix gaps.
- One short name selects the complete context.
- A transient selection changes no machine config.
- The exact roster, not an optimizer, chooses every shipped subagent model.
- Pix retains safe lifecycle, host services, memory, trust, and closed UAT.
- Home and work environments coexist without service teardown or credential
  crossover.
- Pack and scored-routing production code are absent.
- The resulting change removes substantially more code than it adds.
