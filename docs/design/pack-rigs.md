# Named pack rigs

Status: PLAN, ready for implementation

## 1. Decision

Pix will finish the pack abstraction instead of adding profiles.

A pack is the named unit that selects a complete sandbox context: skills,
knowledge, MCP attachments, wrappers, memory scope, and model choices. The
normal switching surface becomes:

```console
pix pack use home       # persistent default on this machine
pix run --pack work     # one sandbox, no persistent mutation
pix run --pack luna     # one experiment
```

`pack use` sets a machine default. `run --pack` selects a pack for one sandbox.
Neither operation writes pack-contributed MCP, model, or routing state into the
machine's base configuration.

Docker's experimental `sbx env` is not a user-facing format in this design. Pix
may generate an environment file as an internal launch transport after that
interface stabilizes. `pack.toml` remains the only authored rig format.

## 2. Why this work exists

The current design says the active pack is the user's context, but the
implementation does not deliver that promise:

- `pack.toml` has a name, but Pix does not resolve a pack by name.
- `pix pack ls` reports one active pack instead of listing adopted packs.
- `pix run --pack` requires a path or Git URL and receives only part of the
  pack's host-side setup.
- Pack activation projects MCP and inference contributions into
  `config.toml`, then uses an activation ledger and `pack.lock` to reverse them
  on the next switch.
- The write-then-revert path makes pack selection machine-global and
  last-switch-wins. Two live sandboxes cannot safely use different packs.
- A pack can define low-level inference backends and bindings, but cannot state
  its default session model, allowed model set, or direct intent pins.
- A host MCP process launched through the current shared `op-refs.env` can
  receive every configured ref, not only the variables its integration
  declares.

The user job is simpler than the implementation:

> When I work on a particular machine or try a model for one session, I want to
> name the whole context once and start with the right models and tools.

Concrete target:

- A home machine defaults to `home`, with GLM, DeepSeek, and personal tools.
- A work machine defaults to `work`, backed by a large private pack.
- `pix run --pack luna` runs a temporary Luna rig without changing tomorrow's
  default.

## 3. Product contract

### 3.1 Terms

**Adopted pack:** A pack the launcher has inspected and recorded in its
launcher-owned pack store. Adoption records source and canonical root. It does
not imply Tier-1 host-execution acceptance.

**Pack name:** A display and lookup key recorded by the launcher at adoption.
It never carries trust. Trust remains canonical identity plus the byte-exact
host-execution fingerprint.

**Machine default pack:** The one adopted pack selected for bare `pix run` on
this machine.

**Selected pack:** The machine default or an explicit `--pack` override used to
build one sandbox.

**Live holder:** A sandbox whose creation fingerprint names a selected pack and
whose existing session lease proves it still exists.

### 3.2 Command behavior

```console
pix pack ls
pix pack show [NAME|PATH]
pix pack use <NAME|PATH|GIT-URL>
pix pack rm [NAME]
pix run --pack <NAME|PATH|GIT-URL>
```

`pix pack ls` lists every adopted pack, not only the default:

```text
NAME     SOURCE                         DEFAULT  LIVE  STATUS
home     ~/.local/share/pix/home        yes      1     ready
work     git+ssh://github.com/acme/...  no       0     ready
luna     ~/.local/share/pix/luna        no       0     model unavailable
```

`pix pack use work` means: make `work` the machine default. It performs pack
validation, the existing Tier-1 review when required, additive host
registration, and a post-mutation probe. It does not copy the pack's launch
facets into base config.

`pix run --pack luna` means: use `luna` for this sandbox. It never changes the
machine default or saves config. If the pack needs host execution that has not
already been accepted, the run path refuses and points to `pix pack use luna`.
It does not turn launch into an adoption prompt.

### 3.3 Resolution rules

A pack argument resolves in this order:

1. A value beginning with `.`, `/`, or `~`, or containing a path separator, is
   a path.
2. A `git+https://` or `git+ssh://` value is a Git source.
3. Every other value is an exact adopted name.

There is no fuzzy or prefix resolution. An unknown name is a usage error that
lists known names. A duplicate name is a hard adoption error that names both
canonical roots. A path and name collision is impossible under these rules;
`./work` means a path and `work` means a name.

The pack manifest's `name` is the proposed default at adoption. The launcher
records the final mapping in its own store. Re-pointing a name never transfers
trust acceptance.

### 3.4 Precedence

The session model resolves in this order:

1. `pix run --model ID`
2. `pix run --intent NAME`
3. selected pack `models.default`
4. selected pack `models.default_intent`
5. machine `run_intent`
6. pi's own default

A command-line override is constrained by the selected pack's allowed model set
and any exclusive backend boundary. Explicit does not mean authorized.

Pack selection resolves in this order:

1. `pix run --pack ...`
2. machine default pack
3. no pack

There is no repo-controlled automatic pack selection. A cloned repository must
not choose which credentials, inference endpoint, or MCP tools enter its
sandbox.

## 4. Scope

### 4.1 P0

1. Launcher-owned short-name index for adopted packs.
2. Truthful `pack ls`, with default, source, live-holder count, and health.
3. Name-aware `pack show`, `pack use`, `pack rm`, and `run --pack`.
4. A high-level model rig in `pack.toml`.
5. Launch-time model validation and pack intent pins in generated
   `routing.json`.
6. Full creation fingerprint coverage for the selected pack and its rig.
7. Per-server credential grants filtered to declared `env_keys`.
8. Additive host registration and live-holder-aware service supervision.
9. Removal of pack write-then-revert projection into `config.toml`.
10. Removal of public multi-pack stacking. One selected pack is one rig.

### 4.2 P1

- Select a subset of a pack's declared MCP integrations for attachment.
- `pack ls --json` and richer doctor/status provenance.
- A non-interactive `pix pack init NAME [--from NAME]` scaffold, if the private
  pack implementation proves hand authoring still requires source-code
  archaeology.
- Explicit-only model stubs for pack-bound models absent from the shipped
  catalog. Such models can be selected by exact ID but never win score-based
  routing until the host catalog contains scores.
- Export the effective launch as `.sbxenv.yaml` for upstream testing.

### 4.3 Non-goals

- No profile abstraction or `pix profile` commands.
- No user-authored `.sbxenv.yaml`.
- No pack marketplace, registry, or publish command.
- No live MCP attachment to an existing sandbox.
- No per-sandbox `pix-host` daemon.
- No model price, benchmark, or routing-objective overrides in a simple pack.
- No pack-selected host service teardown while another sandbox holds that
  pack.
- No automatic provider probing during every pack switch.

## 5. Pack schema

Add an optional high-level model block:

```toml
name = "home"
schema = 1
memory_scope = "personal"

[models]
default = "zai/glm-5"
allow = [
  "zai/glm-5",
  "deepseek/deepseek-v3",
  "deepseek/deepseek-r1",
  "google/gemini-3.1-pro-preview",
]

[models.intents]
code = "zai/glm-5"
breadth = "deepseek/deepseek-v3"
max-accuracy = "deepseek/deepseek-r1"
review = "google/gemini-3.1-pro-preview"
```

A portable pack can choose an intent rather than a model:

```toml
[models]
default_intent = "code"
allow = ["anthropic/*", "google/*"]
```

Validation rules:

- `default` and `default_intent` are mutually exclusive.
- Every model ID is fully qualified.
- Every intent key exists in the host policy vocabulary.
- An `allow` entry is a fully qualified ID or a provider wildcard of the form
  `provider/*`.
- `default` and every intent pin must be included by `allow` when `allow` is
  non-empty.
- Load validates syntax and internal consistency. Launch validates that models
  are callable on this machine.
- A pack may narrow the machine roster. It may not widen an exclusive backend
  boundary or manufacture verification evidence.
- Packs cannot supply scores, prices, or accuracy claims through this block.

Low-level `[inference]` remains the declaration for custom backends and model
bindings. Machine setup and `models add` own credentials and positive probe
evidence. The high-level block selects among callable bindings after the
selected pack's low-level inference contribution is projected in memory.

## 6. Architecture

### 6.1 Ownership boundary

```text
MACHINE, persistent and additive
  config.toml
    host autoserve and services
    memory settings
    provider backends and bindings
    machine run_intent
    default pack name
  pack-trust.json
    canonical pack adoption records
    short-name index
    Tier-1 acceptance fingerprints
  state
    provider probe evidence
    sandbox leases and live pack holders
    generated kits
  sbx gateway
    MCP registrations

PACK, portable declaration
  pack.toml
    model rig
    integrations
    wrappers and services
    memory scope
  skills, knowledge, capabilities, files

SANDBOX, selected exposure
  selected pack mixin
  generated inference and routing mixin
  selected --static-mcp names
  callable --models list
  pack-rig creation fingerprint
```

The pack contributes. The machine proves and registers. The selected sandbox
receives only the chosen exposure.

### 6.2 New values

Add a launch-only value in `services/host/packinfo`:

```go
type Selection struct {
    Root          string
    Name          string
    DefaultModel  string
    DefaultIntent string
    Allowed       []string
    IntentPins    map[string]string
    MCPAttach     []string
}
```

`Selection` is derived from a loaded, trusted pack. It is never serialized into
`config.toml`.

Add a launcher-owned index entry to the existing trust store:

```go
type PackIndexEntry struct {
    Name      string
    Root      string
    Remote    string
    Commit    string
    AdoptedAt string
}
```

The index may use the manifest name as its proposed alias, but the stored root
and existing adoption record decide identity. Acceptance continues to use the
current canonical trust key and fingerprint, never `Name`.

### 6.3 Launch flow

The create path becomes:

```text
resolve selected pack name
  -> load pack
  -> verify launch trust against canonical identity and fingerprint
  -> derive packinfo.Selection
  -> project pack inference into an in-memory config copy
  -> intersect machine-callable models with Selection.Allowed
  -> compile host routing policy
  -> apply Selection.IntentPins
  -> validate selected/default model
  -> write generated routing.json + inference manifest kit
  -> synthesize selected pack kit
  -> compose selected --static-mcp names
  -> record pack-rig fingerprint and live holder
  -> create sandbox
```

No launch path calls `cfg.Save()`.

The router already generates `routing.json` at create in
`services/host/inference/live.go`. Add two pure operations:

```go
FilterAllowed(compiled, allowed)
ApplyPins(compiled, pins, callable)
```

A pin to a positively non-callable model is a hard launch error that names the
provider and exact setup command. An indeterminate provider probe follows the
existing tri-state rule and does not cause a false refusal.

`CallableRuntimeModels`, the inference manifest, and `--models` must continue to
read the same resolved model set. A model must never appear in one and not the
others.

### 6.4 Creation fingerprint

Add a stable `pack_rig` component containing:

- canonical pack identity
- pack host-execution fingerprint
- selected MCP attachment set
- generated wrapper/bin digest
- allowed model set
- intent pins
- exclusive backend/source
- memory scope

Changing the top-level `--model` or `--intent` remains a runtime override and
does not force recreation. Changing a create-time rig facet refuses attachment
with the existing `pix rm BOX && pix run ...` guidance.

Two sandboxes in the same workspace using different packs must not silently
share identity. The implementation may suffix the default sandbox name with the
pack name or keep one name and rely on fingerprint refusal. Prefer a visible
pack suffix so `home` and `work` sandboxes can coexist and `pix ls` explains the
machine without another lookup.

### 6.5 Host services and live holders

`pix-host serve` is machine-global. Pack selection must not replace its desired
service set.

Record the canonical selected pack in the existing per-sandbox session/lease
state when creation succeeds. Serve computes desired pack services as:

```text
machine default pack UNION every positively live holder pack
```

The existing sandbox lease and orphan-reaper state remains the liveness source.
Do not add a second standalone usage database.

Service reconciliation must fail closed when concurrently required packs claim
the same unit name or port. Errors name both packs and the holding sandboxes.
A unit is removed only after no selected default and no live sandbox holds its
pack.

The launch path performs the same unit-name and port-collision merge
synchronously before sandbox creation. A transient selection that conflicts
with the machine default or an existing live holder aborts with a visible error
naming both packs and the conflicting unit or port. Background reconciliation
is recovery and convergence, not the first place a launch-time conflict is
discovered.

A transient `run --pack` must never silently launch a partial pack. Before the
holder-aware supervisor lands, a transient pack with `[[services]]` must refuse
and instruct the user to make it the machine default. After holder-aware
supervision lands, transient services follow the same trust and readiness probe
as default-pack services.

### 6.6 MCP registration and attachment

MCP registration remains host-global and additive. Attachment remains
sandbox-scoped through create-time `--static-mcp`.

Before create, Pix verifies that every selected MCP name resolves to the same
registered definition digest recorded at adoption. Two adopted packs declaring
the same MCP name with different definitions fail closed. An undeclared stale
registration cannot satisfy a selected pack merely because the name exists.

Switching the machine default does not unregister MCP servers required by a
live holder. Registration cleanup can occur when no adopted pack and no live
holder references the definition.

### 6.7 Credential isolation

The current gateway wrapper must not pass the complete shared `op-refs.env` to
every host MCP server.

For each registered server, maintain a host-private, `0600`, server-specific
refs file that contains only the variables named by that integration's
`env_keys`. Values remain `op://` references and are resolved by `op run` at
each spawn. Store these files under a launcher-owned directory such as
`~/.local/state/pix/mcp-refs/`, outside every mounted workspace. Keep a file for
as long as its registration can respawn; rewrite it atomically when declared
keys or refs change, and remove it only when the corresponding registration is
retired.

Required property:

```text
server declares env_keys = [A]
configured refs contain A and B
spawned server receives A and cannot observe B
```

Credential metadata must not enter the sandbox. Pack status may show missing
variable names, never refs or values.

## 7. Configuration cleanup

Today pack activation writes contributed MCP, Ollama bridge, and inference
state into `config.toml`, then stores attribution so the next activation can
remove it. That path must disappear.

After launch-time selection and live-holder supervision exist:

- stop writing pack MCP names into `cfg.MCP`
- stop writing pack inference bindings into persistent config
- stop writing pack Ollama bridge choices into persistent config
- remove activation-ledger contribution reversal
- remove `pack.lock` prior-value and contribution ownership fields
- remove singular/plural active pack ambiguity
- remove public pack stacking

Retain:

- canonical adoption and commit provenance
- Tier-1 acceptance fingerprints
- installed host wrapper evidence
- machine provider wiring and probe evidence
- the machine default pack name

Move `Available`, `Verified`, `VerifiedBy`, and `VerifiedAt` out of declarative
model binding configuration into a launcher-owned evidence cache. Preserve a
read-through migration only if tests or existing development installations
need it. Pix has no released users, so do not keep compatibility code without a
concrete local-state need.

## 8. Security invariants

1. A short pack name is never a trust key.
2. Pack trust remains canonical identity plus byte-exact host-execution
   fingerprint.
3. The launcher is the only writer of the short-name index, adoption records,
   acceptance records, and live-holder records.
4. Repo or workspace files cannot select a pack automatically.
5. Any host-execution change re-gates, including MCP argv, wrappers, binaries,
   services, and custom inference endpoints.
6. A pack that routes model traffic to its own endpoint states that prominently
   in the Tier-1 bill of materials.
7. MCP names, service names, and service ports fail closed on cross-pack
   collision.
8. Each host MCP server receives only its declared credential variables.
9. Explicit model/default/intent pins are validated against the launch-time
   callable set.
10. Existing sandbox attachment refuses when the requested pack rig differs
    from the creation fingerprint.
11. Unknown sandbox or holder state fails closed toward preserving a running
    service, never toward tearing one down.
12. Success words remain probe-backed.

## 9. CLI details and errors

Examples:

```text
$ pix pack use hoem
pix: no pack named "hoem".
     known packs: home, work, luna
     adopt one: pix pack use <path|git-url>
```

```text
$ pix run --pack luna
pix: pack "luna" selects luna/luna-1, but no verified backend on this host serves it.
     fix: pix models add luna
```

```text
$ pix run --pack work
pix: sandbox pix-repo-home was created with pack "home".
     packs, MCP servers, and wrappers are create-time state.
     create the work rig: pix run --pack work --name pix-repo-work
```

```text
$ pix pack use work
pix: work is now this machine's default pack
pix: MCP warehouse registered and probed
pix: recreate existing sandboxes to attach the new pack
```

Do not add fuzzy matches that perform an action. Suggestions may be displayed,
but the user must type the corrected command.

`pix config set pack ...` must refuse and point to `pix pack use`; direct config
mutation skips adoption and host trust review.

## 10. Implementation plan

### Story 1: Named pack index and truthful inventory

**Goal:** Every adopted pack has a stable launcher-owned name and every pack
command resolves it consistently.

Changes:

- Extend `services/host/workflow/pack/truststore.go` with the name index.
- Add exact name/path/Git resolution in `services/host/packinfo` or a lower
  dependency package shared by command and launch workflows.
- Record the manifest name during adoption; reject collisions atomically.
- Change `pack ls` to list index entries plus the default and live status.
- Make `pack show`, `pack use`, `pack rm`, and `run --pack` use the resolver.
- Refuse `pix config set pack`.
- Update generated help and `docs/reference.md` in the same commit.

Acceptance:

- A local or Git pack adopted as `work` launches with `--pack work`.
- Re-pointing `work` to different bytes re-runs the trust gate.
- Duplicate names leave the previous mapping byte-identical.
- An unknown name exits 2 and lists known names.
- `pack ls` survives a broken/missing pack root and marks it broken.

Dependencies: none.

### Story 2: Transient named launch parity and fingerprint

**Goal:** `run --pack NAME` gets the same sandbox facets as the machine default
without changing persistent config.

Changes:

- Thread `packinfo.Selection` through `LaunchContribution`.
- Include pack identity and launch facets in the session fingerprint.
- Give different selected packs distinct default sandbox identities or a hard
  attach refusal.
- Idempotently verify MCP registration before create.
- Record the selected pack in session/lease state.
- Refuse transient packs with host services until Story 4 lands.

Acceptance:

- `config.toml` is byte-identical before and after a transient run.
- Selected pack skills, wrappers, MCP attachments, memory scope, and inference
  bindings enter the sandbox.
- An unaccepted Tier-1 pack refuses without prompting inside `run`.
- A sandbox created under `home` never attaches as `work`.

Dependencies: Story 1.

### Story 3: High-level model rig

**Goal:** A pack can set its session model and subagent routes without editing
host scorecards or persistent config.

Changes:

- Add `[models]` schema and validation in `services/host/packinfo/pack.go`.
- Add `Selection` model fields.
- Add pure allowed-set filtering and intent-pin application in routing.
- Make inference runtime compilation selection-aware.
- Apply the documented session-model precedence.
- Validate explicit `--model` against the same callable set.
- Show the effective model rig in `pack show` and doctor.

Acceptance:

- `home` can default to GLM and pin breadth to DeepSeek.
- Generated `routing.json` contains the pack pins.
- No scorecard, policy, or baked routing file changes during a switch.
- A positively uncallable pin refuses with an exact fix command.
- A pack without `[models]` produces byte-identical runtime routing and model
  manifests to current behavior.

Dependencies: Story 1. It may run in parallel with Story 2 after `Selection` is
agreed.

### Story 4: Credential and host lifecycle isolation

**Goal:** Different pack rigs can run concurrently without losing services or
seeing each other's credentials.

Changes:

- Generate persistent, server-specific filtered refs files from declared
  `env_keys`, with atomic reconciliation for gateway respawns.
- Add MCP definition digests and collision checks.
- Read live pack holders from the existing session lease state.
- Run service unit-name and port-collision checks synchronously before sandbox
  creation; background reconciliation must not be the first failure surface.
- Reconcile host service desired state as machine default plus live holders.
- Detect service name and port collisions.
- Trigger a bounded reconcile after holder creation and periodically for crash
  recovery.
- Add doctor rows for default, holders, desired units, running units, and stale
  registrations.

Acceptance:

- A server granted `A` never receives configured ref `B`.
- Home and work sandboxes retain their own host services concurrently.
- A transient pack with a service name or port collision is refused before
  sandbox creation with both packs named.
- Switching the machine default does not remove a live holder's units.
- Last-holder teardown eventually removes no-longer-selected units.
- A hard-crashed sandbox is reclaimed by the existing orphan path.
- Name, MCP definition, service name, and port collisions fail closed naming
  both packs.

Dependencies: Story 2.

### Story 5: Delete global projection and multi-pack stacking

**Goal:** Pack selection is a launch overlay, not reversible mutation of base
configuration.

Changes:

- Stop applying pack MCP, inference, and Ollama choices to persistent config.
- Remove activation contribution rollback and pack-lock prior-value state.
- Remove singular/plural pack ambiguity and public stack composition.
- Preserve one machine default pack name and the adopted-pack index.
- Move provider verification evidence to state.
- Add grep/AST sentinels preventing launch-time config saves and removed
  projection code from returning.
- Reconcile every pack design/reference document and `AGENTS.md`.

Acceptance:

- Pack switches cause zero pack-facet writes to `config.toml`.
- Existing machine-owned MCP and inference configuration survives every switch.
- The old write-revert types and functions are absent.
- Documentation and `pix help --all` describe one selected pack and named rigs.
- Full Go, Node, open-core, and AGENTS invariant gates pass.

Dependencies: Stories 2, 3, and 4.

## 11. Test plan

### Unit and table tests

- Pack reference resolution: exact name, relative path, absolute path, `~`, Git
  URL, unknown name, duplicate name, broken root.
- Pack model schema: mutually exclusive defaults, malformed IDs, unknown intents,
  wildcard syntax, default outside allow, pin outside allow.
- Allowed filtering and intent pinning: callable, non-callable, exclusive source,
  no pins, empty allowed set.
- Credential filtering: declared variable present, undeclared variable absent,
  missing declared variable reported.
- MCP and host service collisions: name, definition digest, unit name, port.

### Golden tests

- No selected pack.
- Legacy pack without `[models]`.
- Home rig with GLM and DeepSeek pins.
- Work rig with an exclusive custom backend.

For each, compare generated `routing.json`, inference manifest, `--models`,
pack kit, `--static-mcp`, and creation fingerprint. A pack without new fields
must preserve current output until Story 5 intentionally removes old projection.

### Lifecycle tests

- A transient launch never changes config bytes or mtime.
- A changed rig refuses attach.
- Two live holders preserve both packs' services.
- Last holder teardown removes only unselected units.
- Unknown sandbox state preserves services.
- Pack name re-pointing never transfers acceptance.
- Non-TTY unaccepted host execution fails closed.

### Architecture fitness tests

- Launch packages cannot call `cfg.Save()`.
- `packinfo` imports no workflow package.
- Routing remains independent of config.
- Removed write-revert identifiers cannot reappear.
- The profile noun and `pix profile` command do not appear in the public command
  surface.
- Docs enumerate every pack verb and public config key from generated help.

### Verification commands

At minimum for each implementation landing:

```console
cd services/host && go test ./...
npm test
npm run typecheck
node scripts/check-open-core.sh
node tests/agents-md-invariants.test.mjs
```

Use the repository's actual package scripts if names differ at implementation
time. The final agent must read `package.json` before running the Node gates.

## 12. Rollout and migration

Pix has no released users, so prefer direct schema cuts over compatibility
layers. Preserve local developer state only where the cost is small and the
state cannot be reproduced.

Recommended order:

1. Backfill the name index from canonical adopted roots and the current default.
   Duplicate manifest names remain path-addressable but are not indexed until
   the user resolves the collision.
2. Treat packs without `[models]` exactly as today.
3. Land filtered credentials before allowing concurrent holder service
   reconciliation.
4. Stop writing pack facets only after launch parity and holder lifecycle tests
   pass.
5. Remove old stack and activation machinery in the same change that stops its
   writers. Do not leave a dead compatibility surface.

Rollback remains ordinary Git rollback until Story 5. After Story 5, rollback
requires moving aside the generated name index and re-running setup because the
old activation ledger no longer exists. Document that only in the development
handoff, not as a public migration promise.

## 13. Success measures

- One short command selects a complete known rig.
- A transient selection changes zero persistent config bytes.
- Two different packs can serve live sandboxes without service teardown or
  credential crossover.
- Every selected model and MCP server is validated before the sandbox claims it
  is ready.
- Pack switching requires no scorecard edit, route build, image build, or manual
  MCP attachment step.
- Story 5 removes more state-reversal code than the new selection/index code
  adds.

The counter-metric is launch refusals. If correct configurations routinely fail
because validation cannot distinguish unavailable from unprobeable, preserve
the existing tri-state rule rather than weakening safety or claiming false
readiness.

## 14. Handoff checklist for the private work machine

The implementation agent on the work machine should begin with the real private
pack, not a synthetic fixture:

1. Record its current `pack.toml` facets, integrations, `env_keys`, custom
   inference backends, wrappers, and `[[services]]` unit names and ports.
2. Run every declared MCP probe through a filtered environment containing only
   that server's `env_keys`. Fix undeclared dependencies in the private pack.
3. Identify cross-pack MCP names, service names, and ports that would collide
   with the home rig.
4. Confirm whether the private pack needs a pinned default model or a portable
   default intent.
5. Implement Story 1 and its tests before changing model or lifecycle code.
6. Use the real pack as the acceptance fixture for Stories 2 through 4, but do
   not copy private names, endpoints, credentials, or hierarchy into this public
   repository.
7. Keep private-pack compatibility fixes in the private pack repository. Only
   generic schema and launcher behavior belong here.

## 15. Resolved design calls

- **Pack, not profile.** Final.
- **One selected pack, no public stack.** Final. The always-on personal context
  remains separate and does not require stack composition.
- **Launcher-owned exact-name index.** Final. Manifest name proposes the alias;
  the launcher records it after explicit adoption.
- **Global machine default, explicit transient override.** Final. Repositories do
  not choose packs.
- **Explicit-only uncatalogued model support is P1.** Final. Packs do not supply
  scores in P0.
- **Concurrent host lifecycle is P0 for the complete design.** Before it lands,
  transient packs with host services refuse rather than partially launch.
- **`pack init` is not part of P0.** Reconsider after implementing against the
  private pack; add only if a no-prompt scaffold removes measured ceremony.
