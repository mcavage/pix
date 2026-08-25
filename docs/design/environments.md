# Native sandbox environments

Status: Story 0 proven on sbx v0.39.0. Implementation of Story 1 onward is in
progress; see `docs/upstream/sbx-0.39-environments.md` for the observed
contract this section summarizes.

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

1. **Environment selection.** Exact-name registrations, one machine default,
   and an explicit per-run override.
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

Story 0 re-proved these facts against a real `sbx v0.39.0` host release
(run `run-20260824-110322-d24dac52`, candidate
`33499a056a4390b5095d0b50d51475b3580cd2ec`); the full observed argv, output,
and corrections are in `docs/upstream/sbx-0.39-environments.md`. Three
facts below are corrected from the pre-Story-0 assumption, not merely
confirmed:

- **Local candidate image proof is exact-tag, no-pull, and running, never
  digest equality.** sbx 0.39's `sbx ls --json` exposes no created-sandbox
  digest field, and a sandbox is not a host-Docker container addressable by
  its sandbox name. The proof is: the run-unique tag is registered in
  `sbx template ls` before create, the create receipt names that exact tag
  with no mixed reference, no registry-pull marker appears in the log, and a
  fresh `sbx ls --json` poll confirms the instance running.
- **Interpolation has three observed outcomes**, not merely a documented
  mechanism: a defined `${VAR}` resolves to its host value; a missing
  `${VAR:-default}` resolves to the literal default; a bare missing `${VAR}`
  with no default resolved to a sandbox-side environment variable set to the
  empty string (create succeeded; it was not refused).
- **Custom-agent Ollama transport is unsupported**, with a concrete observed
  failure: sbx forwards `--model` to the container runtime as the command to
  execute (`executable file '--model' not found in $PATH`). Section 4.1
  below and `extensions/ollama-bridge.ts` reflect this result, not an
  assumption.

The remaining facts below were already documented upstream and Story 0
confirmed them:

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

Story 0 probed the analogous shape for the Pix custom agent
(`sbx exec -it NAME --model gemma4 --provider ollama -- pi ...`) and observed
it fail: sbx forwards `--model` to the container runtime as the exec command
rather than recognizing a transport flag
(`docs/upstream/sbx-0.39-environments.md`). This is recorded as an explicit,
non-failing `unsupported` capability result, not a design stop.

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

`pix env add|forget` are the only writers of `[environments]`. `pix env use` is
the only writer of `environment`. Input may use `~`, but Pix persists only canonical
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
3. `none`: no environment is registered or selected, and Pix launches with its
   own built-in defaults

An unknown explicit `--env` exits non-zero and launches nothing; it never
falls back to the configured default, since a typo silently launching the
wrong credential set is the worst outcome this feature can produce.

A workspace `.sbxenv.yaml` is never selected automatically. If one exists, Pix
may print a single hint, but it launches without it until the user runs
`pix env add` and reviews it.

### 6.2 Environment execution plan

Pix materializes one stable effective environment file per sandbox:

```console
sbx env create <state>/environments/<sandbox-name>/effective.sbxenv.yaml
sbx exec -it <sandbox-name> -- pi <exact invocation>
```

Sandbox identity is attributed before composition: the adapter first computes
the canonical `pix-*` sandbox name from the workspace and the selected
environment name, independent of anything the authored files declare. That
pre-composition identity names the effective file and every later probe;
composition never determines identity, it only fills in the file identity
already names.

The leaf environment adapter then parses the authored file, resolves local
relative paths against its source directory, applies documented sbx merge
semantics, and adds only Pix-owned runtime facts:

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

With environment selection resolved to `none`, the effective file is generated
from Pix's built-in defaults.
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
pix env show [NAME] [--json] [--path] [--effective]
pix env edit NAME pix|sbxenv
pix env review NAME [--yes]
pix env forget NAME

pix run [DIR] [--env NAME] [--model ID]
```

Seven verbs, no more: `ls`, `add`, `use`, `show`, `edit`, `review`, `forget`.
`pix env rm` is not one of them; see the refusal below.

### 8.1 Command behavior

- `add NAME PATH` registers a canonical local directory, validates both files,
  always completes host review, and records acceptance outside the directory.
  Non-host-executing environments produce an empty bill and need no prompt.
- `add NAME` scaffolds `~/.local/share/pix/envs/NAME`. The generated environment
  is runnable and equivalent to the current default, not an empty stub.
- `use NAME` only changes the machine default. It performs no adoption, host
  registration, or probing. An unreviewed environment is refused.
- `add NAME` with no path is refused outright when `$PWD` already contains a
  `.sbxenv.yaml`: one omitted token must not silently pick between registering
  that file and scaffolding an unrelated new one, so the refusal names both the
  register and the scaffold forms and lets the user pick.
- `show` is a lossy summary by default: authored environment and Pix facts,
  review state, and live-sandbox drift state, sized to fit one screen.
  `--path` prints only the canonical root, nothing else. `--effective` renders
  the byte-identical document Pix hands to `sbx env create`, and does so with
  no sandbox in existence: it is the answer to "what would a recreate produce",
  needed exactly before running one, never gated on a live sandbox.
- `edit NAME pix|sbxenv` takes an exact positional enum, not a flag: `pix`
  opens `pix.toml`, `sbxenv` opens the native file; there is no `--sbxenv`
  flag. With no token, a TTY prints a two-line file selection and reads a
  choice; non-TTY with no token exits 2 naming both paths. After the editor
  exits, `edit` validates and prints exactly one of three verdicts: valid with
  no host-executing change, valid and printing `pix env review NAME` (never an
  inline `[y/N]`, since "I meant that edit" and "I accept these host commands"
  are different authorities), or invalid with the file left unregistered. An
  optional `--review` flag that pre-commits to running review immediately
  after a valid edit is real but deferred to P1, not P0.
- `review` reruns the host bill-of-materials gate after an intentional local or
  Git change. It is the one explicit audit boundary; non-TTY fails closed
  without `--yes`.
- `forget NAME` unregisters the registration and never deletes the environment
  directory: the source is untouched. It refuses the current default and
  refuses a live holder, with no override of either refusal.
- `pix env rm` performs no action. It is a pointer error that names three
  distinct things a user may actually want, so the wrong one is never removed
  by accident: the **sandbox** (`pix rm SANDBOX`), the **source** directory on
  disk (Pix never deletes it; remove it yourself), and the **registration**
  (`pix env forget NAME`).
- There is no `pix env current` verb: `ls` marks the default and `show --path`
  prints the root, and a third read verb for one already-visible fact is
  exactly the accretion this design avoids.

Names are exact. Only `add` accepts a path. There is no fuzzy or prefix action;
a close-name suggestion, where printed, is informational data only
(`closest: home`), never an offer to run or select anything on the user's
behalf. `pix reset` renames scaffolded environment sources with the data
directory and invalidates every acceptance, scaffolded or externally
registered; it deletes no environment source.

Exit codes follow one scheme everywhere in `pix env`: `2` for a usage error or
a refusal, a nonzero code other than `2` for an operational failure, and `0`
only for a completed operation, including printing a path because `$EDITOR`
was unset.

Example errors:

```text
pix: no environment named "hoem".
     known: home, work, luna
     register one: pix env add <name> [path]
```

```text
pix: environment "work" changed what it runs on your host.
     changed: host.services.warehouse-proxy.command
     review it: pix env review work
```

```text
pix: sandbox pix-repo-home cannot be reused. Its environment changed since it
     was created.
     changed: mcp.servers[github].url, env.PIX_MEMORY_SCOPE
     recreate it: pix rm pix-repo-home && pix run --env home
```

```text
pix: `pix env rm` does not exist. Registering a name is not owning the files.
     pix env forget home     unregister the name (deletes no files)
     pix rm pix-repo-home    remove the sandbox
     rm -rf <path>           delete the source yourself; pix will not
```

`pix setup` does not require an environment and never walks `pix env` commands
inline. When environments are relevant it points only to `pix help env`.

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

**Story 1 requirement, not yet implemented:** resolving `${VAR}` before
hashing closes one gap and opens another Story 0 did not close. Every
authored `${VAR}` reference in the environment file must appear as its own
line item in the host trust review, naming the source host variable and the
destination field it resolves into (for example, `env.PIX_MEMORY_SCOPE` <-
`${PIX_MEMORY_SCOPE}`). The review must never display or persist the
resolved value, only the reference and its destination. The creation
fingerprint has a matching constraint: it must never hash a low-entropy
resolved value directly, because a bare hash of a short or
dictionary-guessable secret is offline-crackable from the fingerprint alone.
Each interpolated field's contribution to the creation fingerprint is instead
the unresolved expression plus a keyed digest (HMAC, or an equally concrete
non-reversible keyed construction) of the resolved value, keyed by a
fingerprint-local key that is itself never persisted in the fingerprint
document. That still detects a create-time change in the resolved value
without exposing it to offline guessing. Neither the review line-items nor
the keyed digest exist yet; both are Story 1 obligations, not implemented
behavior.

The trust fingerprint includes:

- canonical environment root
- local MCP command and ordered args
- MCP URL/OCI identity and definition digest
- secret and registry `ref` or `command` declarations, never resolved values
- authored `${VAR}` interpolation references, by source variable name and
  destination field only, never the resolved value (Story 1)
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

`pix env review` prints a summary by default: counts, plus each host command
and host service name, every credential destination, and every mount
expansion. Full argv and content digests are behind `--verbose`; counts alone
train a reflexive `y`, and full argv by default is noise that also trains one,
so the default view names what executes and where secrets go without either
failure mode.

A creation-fingerprint attach refusal names the drifted facets by canonical
key path, read from the adapter's pre-composition semantic tree rather than
the composed document: the composed document concatenates lists, so a later
unrelated addition shifts a post-composition index and would name the wrong
facet. A facet is named by its own schema identity wherever the native schema
has one (`mcp.servers[github].url`, bindings by service/domain, host services
by port); an indexed path (`mounts[2]`) is used only where the schema gives no
identity. No refusal is ever hash-only.

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
warning and a positive post-mutation probe. `pix env forget` unregisters the
registration, not shared upstream state. Explicit review/reconciliation and
`pix reset` own safe cleanup. UAT/session-owned registrations remain scoped and
cleaned by their existing lease.

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

Pix retains at most 100 create-intent records, evicting the oldest first, so
this diagnostic list can never grow unbounded on a host that fails creation
repeatedly. A create-intent record is failed-create residue bookkeeping only:
it gates removal authority for a sandbox that never reached a positive
receipt. It is not the `I4` recreate log below, and `pix doctor` never reports
create-intent records; conflating the two would tell a user a routine drifted
attach looks like a failed create, or the reverse.

### 9.4 Recreate diagnostics (I4)

A creation-fingerprint attach refusal (§10.2) is a *recreate*, not a failed
create: sbx already holds a running sandbox, and P0 treats any effective
declaration drift as recreate-only. Each such refusal appends one record to a
separate bounded log, `I4`, so the recreate tax stays measurable without
becoming a project narrative in casual output:

- the log keeps at most 100 records, oldest dropped first, in
  `~/.local/state/pix/recreates.log`, plain text, local only, never uploaded;
- a record carries a timestamp, the environment name, and the drifted
  canonical key paths (§9.1) only — no facet values, no credential names, no
  argv, no path outside the environment root;
- `pix doctor` prints exactly one line, and only when the count is nonzero:

  ```text
    environments   12 unplanned recreates recorded   pix doctor --recreates
  ```

  At count zero, `doctor` says nothing about recreates; a user with no drift
  never learns the counter exists;
- full facet key paths need the explicit, separate `pix doctor --recreates`:

  ```text
  recreate records: 12 (cap 100, oldest dropped)
  file: /Users/alice/.local/state/pix/recreates.log

  2026-07-14T09:02:11Z  work  mcp.servers[github].url
  2026-07-14T11:40:03Z  home  env.PIX_MEMORY_SCOPE, mounts[] (2 entries changed)

  local only, never uploaded. delete the file whenever you like; that is not an error.
  ```

This is a `doctor` flag, not an eighth `env` verb: the seven-verb contract
(§8) is a stated boundary, and an eighth verb for one counter is exactly the
accretion it exists to refuse.

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

All six checks passed against a real host and candidate
(`docs/upstream/sbx-0.39-environments.md`). That document is the source of
truth for observed argv, output, and the exact-tag/interpolation/Ollama
corrections above; this section states scope, not evidence. It also records
external cleanup debt: an earlier run leaked a `pix-uatenv-fixture-image`
sandbox before the run-unique-name fix landed. Story 0 is proven, not
leak-free.

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
- add `pix env ls|add|use|show|edit|review|forget`; `pix env rm` is refused
  with a pointer error naming the sandbox, source, and registration
- gate `pix run` and `doctor` on a positively parsed sbx version `>= 0.39.0`;
  unknown fails closed with an exact upgrade instruction
- lift canonical identity, trust store, fingerprint, atomic write, and lock
  primitives out of pack code with environment names
- refuse `pix config set` for launcher-owned environment keys
- add host trust review line items for every authored `${VAR}` reference
  (source variable, destination field), and keyed-digest/HMAC fingerprinting
  of resolved interpolated values, never a raw hash of the resolved value

Acceptance:

- `pix env add home` creates a runnable scaffold
- exact names resolve from any working directory
- environment roots inside writable workspaces or through symlinks are refused
- dangerous changes require review; names never transfer acceptance
- `use` changes only the default field
- `forget` never deletes source files; `pix env rm` performs no action
- host trust review lists every authored `${VAR}` reference by source
  variable and destination field, never a resolved value; the creation
  fingerprint records each interpolated field as expression plus keyed
  digest/HMAC, never a raw hash of the resolved value

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
- exact name/path resolution
- canonical path and workspace-containment refusal
- model/backend/roster reference validation
- semantic trust and creation fingerprints
- local MCP annotation matches native MCP command
- host-service collision and desired-set union
- create-intent recovery state machine

### Golden

- `none`: no environment registered or selected
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
- the pre-composition `pix-*` sandbox name equals the recorded name before
  create or remove
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

## 17. Decision alignment (D1–D24)

D1–D24 here are the PRD's own decision IDs (`PRD: native sandbox environments`
§2, "Fixed decisions and taste calls", closed by §9 for D21–D24). This table is
the PRD-to-design traceability index, not a second copy of the PRD's prose and
not a second, differently-numbered decision list: an earlier draft of this
section invented its own 24 decisions and reused the D1–D24 labels for them,
which collided with the PRD's real D1–D24 under the same names. Each row below
carries the PRD's actual one-line decision text and names the section that
carries its mechanism; when a decision's mechanism moves, edit the owning
section and this row's pointer only, never the decision text.
`tests/environments-decision-alignment.test.mjs` is the anti-drift gate: D1–D24
must each appear exactly once, in order, with text matching the PRD and every
section pointer resolving to a heading that still exists.

| ID | Decision | Section |
| --- | --- | --- |
| D1 | One authored sandbox grammar, `.sbxenv.yaml`, owned upstream; one narrow sidecar `pix.toml` for Pi/Pix gaps only. | §1, §3.1, §3.2 |
| D2 | The verb is `pix env forget NAME`, not `env rm`. | §8 |
| D3 | `pix env rm` is dispatched to a pointer error that performs nothing and names all three removal objects. | §8, §8.1 |
| D4 | `pix env edit NAME pix\|sbxenv` is an exact positional enum; TTY with no token prints a two-line selection, non-TTY with no token exits 2. | §8.1 |
| D5 | After `edit`, print `pix env review NAME`; no inline `[y/N]`. | §8.1 |
| D6 | The optional `--review` pre-commitment flag is P1. | §8.1 |
| D7 | `env show` is a lossy summary by default, `--effective` renders the byte-identical document, `--path` prints only the path. | §8.1 |
| D8 | `--effective` renders with no sandbox in existence. | §8.1 |
| D9 | An unknown explicit `--env` exits non-zero and launches nothing; it never falls back to the default. | §6.1 |
| D10 | Zero-path `pix env add NAME` is refused when `$PWD` contains a `.sbxenv.yaml`, naming both register and scaffold intents. | §8.1 |
| D11 | The recreate line is `pix rm NAME && pix run --env ENV`; Pix never asks the user to invent `--name`. | §8.1 |
| D12 | The recreate refusal names the drifted facets by canonical key path. | §8.1, §9.1 |
| D13 | `pix setup` has no environment step, no prompt, no probe; one closing `pix help env` pointer, plus one conditional launch hint. | §6.1, §8.1 |
| D14 | Exact-name suggestions are informational data, never an offer; `closest: home` is allowed, `did you mean home? [Y/n]` is not. | §8.1 |
| D15 | Trust review shows counts plus host commands/services, credential destinations, and mount expansion by default; full argv and digests behind `--verbose`. | §9.1 |
| D16 | `pix env review NAME` stays the explicit audit boundary; non-TTY fails closed without `--yes`. | §9.1 |
| D17 | The no-environment state is named `none`; the prose is built-in defaults. | §6.1 |
| D18 | Model selection is a literal table; no score, price, benchmark, status taxonomy, or `WHY` column. | §6.3, §7 |
| D19 | Exit codes: 2 for usage errors and refusals, non-zero-not-2 for operational failure, 0 only for a completed operation. | §8.1 |
| D20 | No `pix env current` verb. | §8 |
| D21 | `pix reset` invalidates every environment trust acceptance, scaffolded or externally registered, and deletes no environment source. | §8.1 |
| D22 | The `I4` recreate log keeps at most 100 records, oldest dropped; `pix doctor` prints one line, only when the count is nonzero; full facet key paths need explicit `pix doctor --recreates`. | §9.4 |
| D23 | Drift attribution reads the adapter's pre-composition semantic tree; facets are attributed by stable identity where the native schema has one, indexed paths only where the schema has no identity, never an opaque hash-only message. | §9.1 |
| D24 | A new `pix env` verb is not how diagnostics ship; `--recreates` is a flag on the existing `doctor` verb. | §9.4 |
