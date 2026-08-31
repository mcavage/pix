# Changelog

All notable changes to Pix are recorded here. CI publishes versioned images
from `main` and stamps the tag into `pi-kit/spec.yaml`; this file is the
human-readable summary of what changed and whether an upgrade is breaking.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

Pix v2 is a direct, breaking cutover, not an incremental release: the
scored model router, `pix-host`, the pack system, and the custom memory RPC
are deleted outright, not deprecated. There is no migration path and no
compatibility shim — `~/.pix` from a v1 install is not read by this build.

### Added — coexistence: one PIX_HOME = one stack

Two Pix installations (a release stack and a dev checkout, or two projects'
homes) can now run on the SAME host at the same time without either one
adopting, replacing, or deleting the other's resources.

- **One PIX_HOME = one stack, always suffixed.** A stack id is the first 16
  lowercase hex characters of the sha256 of the canonical (absolute,
  symlink-resolved) `PIX_HOME` path. Every Pix-owned runtime resource
  carries it, with no unscoped fallback anywhere: sandboxes are
  `pix-<stack-id>-<basename>-<workspace-digest>` (an explicit `--name` is
  scoped too), the memory container is `pix-memory-<stack-id>`, and the two
  reserved MCP servers are `pix-memory-<stack-id>` and
  `pix-session-<stack-id>`. A stack id that cannot be derived is an error,
  never a bare legacy name.
- **The MCP registry is host-global but namespaced.** `mcp.servers`
  registration still happens once per host, in the sbx Gateway; the two
  built-ins simply register under stack-scoped names, so two homes' entries
  sit side by side instead of overwriting one another.
- **Each home gets its own loopback memory port**, allocated by `pix setup`
  and persisted as `memory_port` in that home's `config.toml`. Every reader
  — the launch's trusted host-state payload, the readiness probe, the
  effective-document preview — reads THAT value; there is no compiled-in
  `11435` left in the launcher.
- **Cleanup only ever touches the current stack.** `pix rm --all`,
  `pix rm --orphans` and `pix reset` discover sandboxes through a listing
  filtered to this stack's id, and `pix reset` refuses outright rather than
  guessing a container name.

### Changed — credentials are sandbox-scoped, never host-global

- **`pix setup` creates the refs file; each run refreshes the sandbox's own
  secrets.** Provider credentials live in `$PIX_HOME/secrets.env` as
  `op://` references only. Every create AND every attach re-resolves them
  and writes them as `sbx secret set -f --sandbox <name> ...`. There is no
  "already set, skip it" branch, so a rotated 1Password item takes effect on
  the next run.
- **Host-global sbx secrets are ignored and never removed automatically.** A
  global belongs to whoever pushed it, and may be another stack's. Pix reads
  only its own refs as evidence — `pix doctor`'s provider row is now graded
  off `secrets.env`, not off `sbx secret ls` — and reports any globals it
  finds in a separate, read-only "ignored" row. Removing one is your call,
  never Pix's.

### Added — version identity in every launch

- The stamped launcher version travels on `RunOpts.LauncherVersion`, enters
  the session fingerprint as `launcher_version`, and is composed into every
  effective document as the Pix-managed `PIX_LAUNCHER_VERSION` environment
  fact (beside `PIX_STACK_ID`). `pix env --effective` shows the same two
  facts a real create writes.
- Exactly those two composed keys (`env.PIX_LAUNCHER_VERSION`,
  `env.PIX_STACK_ID`) are recreation-safe, so a version bump takes the
  existing proof-gated automatic recreate path (fresh listing, zero holders,
  no keep marker, direct host-mounted workspace). Every other environment
  variable's drift is substantive and still refuses.
- **Local builds carry a distinct, derived identity.** A local
  `make`-produced version is `X.Y.(Z+1)-beta.g<sha7>[.dirty.<12hex>]`, and
  the launcher binary, the runtime archive, the release manifest, and both
  locally built image tags (`pix-agent`, `pix-memory`) all share it. Release
  CI publishes the clean semver instead. `make load` tags and prunes sbx
  templates under a hash of the canonical worktree path, so one checkout
  never deletes another's loaded templates, and `make run` no longer pins a
  fixed `NAME=pix-pix` — the launcher derives its own stack-scoped name.

### Changed (breaking)

- **One binary, not two.** `pix` is now the only host executable: it
  resolves a named environment, compiles it into one effective native `sbx`
  document, and launches the pinned `pix-agent` image with `sbx env create`
  / `sbx exec`. The `pix-host` daemon, `pix serve`, and every launchd/serve
  lifecycle command are deleted, not merely hidden.
- **Two images replace the pack system.** `pix-agent` (the sandbox image:
  pinned pi build, core extensions, patches, entrypoint) and `pix-memory`
  (an independent Streamable HTTP MCP service, one Docker container) are
  the whole of what ships. `pack.toml`, pack registration, and pack-scoped
  MCP wiring are gone.
- **Native `sbx` environments replace the v1 environment registry.** The
  only sandbox declaration is `.sbxenv.yaml` plus an optional `pix.toml`
  sidecar, both plain files under `~/.pix/envs/<name>/`. There is no
  `add`/`edit`/`use`/`forget` mutation path: create, move, and remove an
  environment with ordinary filesystem and Git tools. `pix env
  {list,show,default,trust}` replaces the old registry verbs.
- **Memory is MCP-only.** `pix-memory` is reached exclusively through the
  sbx MCP Gateway, over loopback, never a custom protocol and never a
  direct sandbox connection. `memory_recall`/`memory_remember`/
  `memory_forget`/`memory_observe`/`memory_stats`/`memory_status`/
  `memory_snapshot`/`memory_restore` are the whole surface; `/recall`,
  `/remember`, and `/forget` call the same Gateway-registered endpoint a
  model's own tool calls use.
- **`config.toml` is one schema with one writer per field.** `config.Config`
  is the sole schema for `<PIX_HOME>/config.toml` (`VersionPin`,
  `Inference`, `DefaultEnvironment`, `MemoryPort`); every mutation is
  load-modify-save under one file lock, so a concurrent `pix env default`
  and `pix setup` can never stomp each other's fields. There is no generic
  `pix config set` verb.
- **One secrets file.** `<PIX_HOME>/secrets.env` (`op://` references only,
  mode `0600`) is the only credential file Pix reads or writes — setup
  seeding, sync, the Gateway wrappers, and `pix secret` CRUD all resolve
  the same path under one `.secrets.lock` transaction lock.
- **Removed verbs answer the ordinary unknown-command error, never a
  retirement notice.** `mcp`, `models`, `config`, `agent`, `pack`, `serve`,
  `resume`, `status`, and `uat` route nowhere; there are no released users
  to keep a migration path for.

### Fixed

- **`pix setup` recovers from a lost port-bind race instead of failing
  opaquely.** A `docker create`/`docker start` failure that names the
  publish port Pix just tried (`port is already allocated` / `address
  already in use`) is classified as a recoverable port conflict, the
  orphaned just-created container is removed by its own ID (never by
  name), and setup reallocates a fresh loopback port under the config lock
  and retries, bounded. `config.toml`, the running container, and the
  registered Gateway URL can never disagree about which port `pix-memory`
  answers on.

### Unchanged

- **Session-continuity todo clearing is untouched by this cutover.** Task
  restore and compaction still clear a resumed session's stale todo list
  using the canonical `pi-stack-todo-cleared` marker (with one release of
  compatibility for the legacy `pix-todo-cleared` spelling); nothing in the
  v2 surface change touches this mechanism.

## 0.1.0 - 2026-07-25

### Breaking

- Renamed the product, CLI, host binary, image, Go module, sandbox namespace,
  runtime paths, workspace markers, environment variables, services, and public
  documentation to Pix. This is a clean pre-launch cutover with no legacy-path
  discovery or compatibility aliases.

### Added

- Added one evidence-backed readiness model shared by setup, doctor, status,
  run, onboarding, Ollama checks, MCP checks, and JSON output.
- Added transactional, resumable setup and optional Google Workspace commands:
  `pix gworkspace setup|status|disable`.
- Moved dynamic memory and knowledge recall to append-only messages, preserving
  provider input-prefix caching with deterministic deduplication and byte caps.
- Added a prescriptive installation path, installer collision checks, absolute
  test timing budgets, workspace marker round trips, and rename guards.

### Changed

- Reduced the warm non-race gate from roughly 42 seconds to under 10 seconds.
- Made isolated worktree parallelism the default orchestration policy for
  independent delivery units.

### Fixed

- **`pi-stack gog setup` now reads current sbx registration tables.** Newer sbx
  builds expose local MCP commands in the plain `sbx mcp ls` table while
  omitting the older `mcp get` and JSON detail forms. Gog setup now parses that
  complete local command as a final bounded fallback, so an existing readable
  gog registration no longer blocks OAuth preflight as "unverifiable."
- **Custom sandbox names survive `mcp load` and `doctor`.** The create receipt
  now records the canonical workspace it was created for (additive schema
  field), and a hardened workspace→sandbox resolver lets `pi-stack mcp load
  NAME [DIR]` and doctor's workspace context find a `run --name pi-stack-demo`
  box again instead of deriving `pi-stack-<basename>` and missing it. A
  positively-clean "no mapping" scan still falls back to the derived default
  (old sandboxes), while an ambiguous or corrupt/tampered mapping refuses
  (`mcp load`) or renders unverifiable (doctor) — never targets an arbitrary
  box. `pi-stack reset --sbx` now clears each positively-removed sandbox's
  receipt through the same hardened helper (a failed removal retains it).
- **gog attachment is no longer claimed from config membership.** Doctor's gog
  "attached" check now reads the same receipt-backed join row as every other
  MCP server: a sandbox created before gog was configured reads
  registered-not-attached (with the exact `pi-stack mcp load gog <workspace>`
  command), not ready. Without a sandbox context, config membership renders as
  intent, never attachment.
- **`status` headline honesty:** an unverifiable per-sandbox MCP row (corrupt
  or absent receipt, failed listing) no longer reads "all systems go" — status
  says some checks are unverifiable without inventing a false TODO. Doctor's
  verified registered-not-attached gap is now an optional TODO with the exact
  load command, consistent with status.

- `pi-stack host` now launches its interactive session under `op run
  --no-masking`. op's default output masking pipes pi's stdout/stderr through a
  filter, which makes them non-TTYs, so pi's TUI then saw no terminal and exited
  immediately (banner, then straight back to the shell, exit 0). Non-interactive
  paths were unaffected, which is why it looked like a silent failure. (The
  mcp-gateway op-run path already used `--no-masking`.)
- Provider-key op:// refs are stored with **literal spaces** again. An earlier
  change percent-encoded spaces (`Anthropic%20API%20Key`) on a false premise;
  op 2.35.0's `op read` AND `op run --env-file` both reject `%20`, so any
  1Password item whose name has a space (very common) failed to resolve and
  `pi-stack setup` aborted. Existing encoded refs self-heal: they're decoded on
  read and rewritten literal on the next write.

### Added

- **`pi-stack doctor` now reports four verdicts, not two**: `ready`, `todo` (a
  verified, fixable gap with the exact command), `unverifiable` (a probe
  timed out or the tool needed to check isn't available; never counted as
  broken), and `denied` (an explicit policy/permission refusal). Exit codes
  follow: `2` on a usage error, `1` only for a positively verified core
  failure (a resolved key for any one of Anthropic/OpenAI/Google, or the
  config itself failing to load), `0` for everything else, including every
  optional or unverifiable gap. `doctor --json` gained `schema_version` (now
  `2`) so a script can tell the shape apart from an older run.
- **`pi-stack gog setup`**: guided, version-aware Google Workspace onboarding.
  It detects which auth subcommands your installed `gog` supports, imports
  your OAuth client, authorizes your account with explicit read-only scopes,
  verifies the exact headless command the sbx gateway will spawn (not just
  interactive auth), and only then registers gog with the gateway and saves
  config, rolling the registration back if the config write fails. Replaces
  hand-running the raw auth + manual `config set`/`mcp register` steps as the
  documented path; `doctor` and `status` now point here for any gog gap.
- **Per-sandbox MCP status, backed by launcher receipts.** `pi-stack status`
  and `pi-stack doctor` now report one of five states per configured server
  per running sandbox: `preloaded` (shipped at create), `loaded` (attached
  live via `pi-stack mcp load`), `registered-not-attached` (known to the
  gateway, but no receipt for this sandbox), `not-registered`, or
  `unverifiable` (an old or externally created sandbox pi-stack has no
  receipt for). Both read from the same shared join of registration evidence
  and receipts, never a live gateway poll, so the two commands can't tell
  different stories from the same facts.
- **`pi-stack setup` never downloads local models without consent.**
  Interactive setup asks once, defaulting to No, before pulling any
  confirmed-missing configured Ollama model; non-interactive setup pulls
  nothing unless you pass `--pull-models`, the only consent it honors (a
  broad `--yes` never downloads). A model setup couldn't positively verify as
  missing is never a pull candidate either way, and setup never installs
  Ollama itself.

### Changed

- The sandbox and opt-in host mode now use pi `0.82.1`. Curated pi
  extensions were re-pinned to the newest versions published by the 0.82.1
  release, and CI now checks both vendored runtime patches against the exact pi
  and todo-list package pins. Host setup and launch now reject a missing or
  stale pi core before loading extensions, with the exact pinned install command;
  readiness and launch also require the matching curated-extension lock marker.
- Fixed intermittent adjacent duplicate lines in terminal scrollback. The
  bottom-pin patch had repainted a row copied from immutable scrollback, leaving
  both physical copies behind. Bottom-anchored shrinks now rebuild the terminal
  buffer under synchronized output, which also keeps the editor and footer from
  jumping.
- `pi-stack setup` no longer provisions or enables **host mode** (the unsandboxed
  escape hatch). It was noisy (it needs `pi` on PATH, which sandbox-only users
  don't have) and only relevant to some people. Host mode is now opt-in via a
  single command: `pi-stack host setup` now PROVISIONS **and** enables it (when
  provisioning succeeds), so the separate `config set host.enabled true` step is
  gone.
- `pi-stack setup` no longer prints the redundant up-front sbx provider-key
  status block; the 1Password flow reports each provider's ref + sync itself.
- Identity seeded from git config is now **first name only**: no surname, no
  email. It's recalled into every session, so it carries the minimum to greet.
  (`readGitIdentity` no longer reads `user.email`; memory stores one first-name
  fact instead of name + email.)

### Changed (breaking)

- **MCP migrated to sbx's nightly gateway.** The sandbox flag is now
  `--static-mcp` (sbx removed `--mcp`), and MCP runs through sbx's **local
  data-plane gateway**: always available, **no `SBX_MCP_URL`** needed (dropped
  the old SBX_MCP_URL gate + "gateway off" warning; `pi-stack mcp register` no
  longer requires it). pi-stack's own `pi-stack run --mcp M` CLI flag is unchanged.
  **Every configured server preloads at create.** Registering a server
  (`sbx mcp add`, `pi-stack mcp register`) only makes it known to the gateway;
  it does not attach it to any session. Every server in the resolved `mcp`
  list, and every integration an active or transient pack carries, is passed
  to sbx as `--static-mcp <name>` when the sandbox is created, so its tools
  are in context from the start. There is no dynamic discovery and no
  attach-on-run. New:
  - `pi-stack mcp load <name> [DIR]`: attach an already-registered server to a
    RUNNING sandbox live, no recreate (`sbx mcp load`), recording a receipt so
    `status`/`doctor` can report it as `loaded`.
  - `pi-stack mcp auth [args...]`: hosted-control-plane OAuth for remote servers
    (`sbx mcp auth`; e.g. `auth --all`).
  - `pi-stack mcp bundle`: register the shipped public catalog
    (notion/atlassian/granola, `config/mcp-catalog.bundle.json`) in one step.
  `make mcp-auth` already used native `sbx mcp auth`; its tail now points at
  `pi-stack mcp load` instead of a recreate. `doctor` guidance no longer mentions
  `SBX_MCP_URL` (a failed `sbx mcp ls` now points at the sbx daemon). An
  earlier draft of this change introduced `mcp_static`/`mcp_dynamic` config
  keys for an eager-vs-lazy attach split; those keys never shipped in a
  release and are gone before this reaches one, superseded by the
  always-preload-at-create model above (`doctor` flags either key as a
  retired config key if it's still in an existing `config.toml`).

- **Kit migrated to kit-spec v2** (`pi-kit/spec.yaml`, `schemaVersion: "2"`).
  Credentials are now a `credentials[]` list of `service` + `apiKey`
  (name/proxyManaged/inject[]); egress is `caps.network.allow`. Replaces the v1
  `network.serviceDomains`/`serviceAuth`/`allowedDomains` + `credentials.sources`
  + `environment.proxyManaged`. Injection is unchanged (proxy-managed sentinels;
  all four providers verified). **Requires a recent `sbx` nightly** (v0.37+); the
  per-credential `service:` is mandatory or `sbx run` panics. Mixin kits
  should move their network rules to `caps.network.allow` too.
- **1Password is now the only provider-key source; the `op` CLI is required.**
  Removed `pi-stack setup --use-sbx-keys` / `--use-1password` (both now error),
  the one-time "use existing sbx keys?" convenience prompt, and the persisted
  `provider_key_mode` config key (dropped from `config.toml`, `config
  get/set/unset`). `pi-stack setup` fails without `op` installed + signed in.
  `pi-stack run` still launches when a usable key is already in `sbx` (op is
  required at setup, not re-checked every run). `install.sh` now warns when `op`
  or `sbx` is missing.

### Known issues

- On `sbx` v0.37.0-rc1 the cosmetic `credential ... discovered but no domains
  allowed by your bindings; not injecting` line prints once per stored provider
  key even though injection works (verified: cloud providers return HTTP 200
  through the proxy). Hand-written `credentials.yaml` bindings are not honored by
  rc1, so it can't be silenced from our side; left visible and filed upstream
  (see `docs/upstream/sbx-0.37-binding-warning.md`). Do not mask `sbx` output.

### Added

- Guided `pi-stack setup` now establishes a complete host: validated 1Password
  references for Anthropic, OpenAI, and Google, rational `sbx` reconciliation,
  memory, the default pack, host mode, and a one-shot in-session handoff.
- `pi-stack setup` accepts `--use-sbx-keys`: trust a COMPLETE existing `sbx`
  provider key set (anthropic, openai, google) instead of the strict
  1Password flow, skipping every op install/signin/ref/reconciliation step. It
  requires an exact successful sbx probe with all three keys (absent,
  erroring, or incomplete sbx fails with a clear message, naming exactly which
  provider(s) are missing), and never deletes an existing 1Password ref or
  synced record, it just isn't used that run. `--use-1password` is the
  mutually exclusive explicit opposite: it forces the strict flow for this
  run even when sbx already has all three keys or a prior run persisted the
  sbx mode. Both flags are **setup-only**; `pi-stack onboard` rejects either
  one (onboard never provisions provider keys at all).
  Interactively, with no flag and no persisted mode, setup also offers a
  one-time convenience prompt when sbx already has all three keys and no
  provider ref is configured yet (default yes); declining falls through to
  the strict flow with no further retries, and the prompt never reappears
  once a ref exists. `--yes` alone does NOT imply the skip.
  Whichever source succeeds is PERSISTED as `provider_key_mode` (`sbx` or
  `1password`) in `config.toml`, so a repeat `pi-stack setup` with no flags
  reuses that exact choice with no prompt; a persisted `sbx` mode still
  re-runs the exact all-three probe every time (never a cached bypass), an
  explicit flag always overrides the persisted choice for that one run, and a
  mode-save failure fails setup honestly rather than reporting success while
  silently failing to remember the choice. Inspect or clear it with
  `pi-stack config get/unset provider_key_mode`.
  Setup no longer claims every run is always cloud-ready: skipping 1Password
  leaves host mode local/Ollama-only until you configure `hostmode.env` refs,
  and `setupHostMode` reports that as an expected result instead of a
  should-not-happen message. Host-mode/setup copy also no longer overclaims
  cloud keys as "wired": it says keys were "validated this run" only after
  the strict 1Password flow actually resolved them, and "configured (not
  verified this run)" when a run used existing sbx keys instead. Real
  validation still happens at every `pi-stack host` launch via `op run`.
  `pi-stack secret set` for a provider key now mirrors the ref into
  `hostmode.env` as well as `op-refs.env` in one step, so three `secret set`
  commands (one per provider) really are enough to wire both the sandbox and
  host mode, no separate step needed.
- Read-only `memory_recall` and `memory_stats` tools let the agent inspect memory
  without exposing memory mutation as a normal tool action.
- Long autonomous tasks resume after threshold compaction when their structured
  todo list still has work in progress. Queued user messages and `/todos clear`
  take precedence.
- `model-refresh` skill: teaches how to re-ground the router (registry +
  scorecard + policy) on LIVE model cards and pricing instead of training data,
  then compile + verify. Baked into the public image.
- `pi-stack agent ls` WHY column is now actionable: it shows the intent, the
  objective, the chosen model's accuracy/per-task-$/latency, and either what it
  beat or that a constraint left a `sole fit`, plus a legend. Previously it said
  only `intent <name>`, which explained nothing.
- `pi-stack agent ls` flags an explicit `model:` pin that is not in the registry
  (`pinned (UNKNOWN ...)`) instead of silently resolving to a model that fails at
  spawn.
- Slack MCP results that carry user-authored text (messages, channel
  topics/purposes, profile names/titles) now stamp an untrusted-content guard,
  matching the `gog` server's `--wrap-untrusted` behavior.
- Bounded MCP frame size (Content-Length) so a hostile peer cannot force an
  unbounded host allocation.
- Community health files: `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`,
  this changelog, and GitHub issue/PR templates.
- `docs/README.md` index for the docs tree.

### Changed

- **The built-in pack is named `default`, not `personal`.** Existing `personal`
  and older `pack` directories migrate with their git history, config, trust,
  wrapper ownership, and activation provenance intact. Bare `personal` remains a
  deprecated compatibility alias.
- **Onboarding is one upfront, grounded handoff.** Trusted host facts travel in
  the launcher-generated initial prompt rather than through a workspace file,
  so a cloned repository cannot forge setup guidance.
- **Memory capture is quieter and easier to inspect.** Questions and routine
  watcher narration no longer become durable facts, perishable watcher entries
  expire after seven days, and literal `*` recall lists stored rows newest first.
- **The memory watcher defaults to `qwen3.5:9b`, the same model the ollama-bridge
  uses.** Two defaults previously disagreed (the daemon fell back to `gemma3:4b`,
  the launcher config to `qwen3.5:4b`), and neither matched the model already
  resident for the bridge/router, so a fresh install ran capture on a model that
  usually was not pulled, and capture silently did nothing. Now capture reuses the
  one local model already loaded, so it works out of the box for anyone running
  the bridge. Override with `pi-stack config set memory_watcher_model <model>` on
  a memory-constrained machine.
- **Local Ollama models are now coherent host config, not scattered env.** The
  stale `gemma3:4b` defaults are gone: the new `ollama_bridge_model` setting (the
  sandbox's local chat model + the router's local option) defaults to `qwen3.5:9b`,
  matching the memory watcher, so Ollama keeps a single model resident for both
  capture and local inference. Set it with `pi-stack config set ollama_bridge_model
  <tag>`; `pi-stack run` writes it into `<workspace>/.pi-stack/ollama-bridge.model`
  and the `ollama-bridge` extension reads it, no more hand-editing
  `/etc/sandbox-persistent.sh`. The bridge display label is now derived from the
  tag, so `OLLAMA_BRIDGE_MODEL_NAME` is optional (you set one value, not two).
  `make pull-models` pulls the local models. NOTE: the host memory service uses the
  watcher + embed models; it does NOT use the bridge/router local model, those
  are separate roles.
- **Routing redesign: a real tiered, multi-vendor crew instead of a monoculture,
  grounded in live model data.** Previously 13 of 18 agents collapsed onto one
  model and the rest onto Opus. The registry/scorecard/policy are now seeded from
  the current (July 2026) lineup and pricing: Claude Fable 5 for `deep`
  (`max-accuracy`); Opus 4.8 for `architect`/`product-manager` (`strategy`);
  Sonnet 5 for `engineer`/`designer` (`code`) and the `advisory` specialist crew;
  GPT-5.6 Sol for `review`; Gemini 3.1 Pro for `security-lead` (`red-team`);
  Gemini 3.1 Flash-Lite for `fanout` (`breadth`); Haiku 4.5 for `qa-lead`
  (`verify`). Three cloud vendors plus a local `qwen3.5:9b` option (a current,
  Apache-2.0 all-rounder that fits a 16GB machine, replacing the year-old
  `gpt-oss:20b` and the 58GB `gemma4:31b` that never fit a laptop), tiered by
  leverage with the adversarial roles pinned cross-vendor via provider
  allowlists. New intents: `strategy`, `advisory`, `red-team`.
- **`agent ls` WHY reasons rewritten.** They said `sole fit` (meaningless) and a
  wall of precise-looking but unmeasured numbers. Now each WHY names the actual
  binding constraints that left one model (e.g. `only model matching anthropic,
  <=$0.18, >=0.80 acc`) or what the winner beat in a contest, with a footer that
  flags the metrics as seed priors.
- **`evals` is no longer a command.** The automated eval harness was removed (see
  Removed); the `evals` verb is gone from the launcher, help, and man page.
- Removed company-specific wiring from the public tree: the `SNOW_CONN` env in
  `make serve` (now a generic pack-populated `SERVE_ENV`), the `snow` probe in
  the `healthcheck` skill (now a generic `EXTRA_CLIS` hook), and the
  company-only `:11442` port from the public kit allowlist. A pack adds these
  itself.
- Pruned stale docs: two `HANDOFF` snapshots, a superseded migration guide, a
  review artifact, and an upstream issue draft.
- README reworked to lead with the outcome, fix the launch command
  (`pi-stack run`), correct the pi link, and complete the launcher command list.

### Removed

- **Tore out the automated eval harness** (`evals/`, `pi-stack-host evals`,
  `make evals`, `pi-stack agent reassess --model`'s auto-measure path). The
  router never needed it: it only ever reads `scorecard.json`, regardless of
  how the numbers got there. Scores are now hand-maintained: edit
  `services/host/routing/defaults/scorecard.json` directly (seeded from
  published benchmarks/pricing; see the `model-refresh` skill), then
  `pi-stack route compile`. `pi-stack agent new`'s starter eval-suite
  scaffolding is also gone.

### Fixed

- **Automatic fact capture could be silently dead.** The watcher-availability
  check was one-shot at startup and latched: once it marked the watcher
  unavailable, `observe` short-circuited and never retried, so capture stayed off
  until a full daemon restart even after the model was pulled, and the client
  swallowed the failure reason, so nothing surfaced. Added a throttled live
  re-probe (capture recovers within 30s of the model appearing, no restart), a
  one-time client warning when capture is off, and a `pi-stack doctor` line that
  reads the daemon's live capture flag with the exact fix.
- `deliver` skill failed to load: its frontmatter `description` was an unquoted
  YAML scalar containing `: ` sequences, which YAML parsed as a nested mapping.
- `enterprise-admin` agent resolved to a model (`anthropic/sonnet-5`) that is not
  in the registry; it now uses `intent: reasoning` like its peers.
