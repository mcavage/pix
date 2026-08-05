# Changelog

All notable changes to Pix are recorded here. CI publishes versioned images
from `main` and stamps the tag into `pi-kit/spec.yaml`; this file is the
human-readable summary of what changed and whether an upgrade is breaking.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Changed

- **U11m: `pix agent` cut to `ls` only — `new`/`edit`/`rm`/`reassess` retired.**
  Interactive authoring (`launchInteractiveAuthoring`, a `pi` re-exec), YAML
  frontmatter mutation (`agentNew`/`agentEdit`/`agentRm`, nine flags across two
  handlers, a yaml.Node round-trip), and the reassess host-exec wrapper
  (`repoRoutingTarget`/`readCompiledRoutes`/`resolveRoster`, a `route compile`
  passthrough duplicating what `pix models route` already does directly) are
  gone. `pix agent ls` (table + `--json`) is the entire surviving surface —
  same resolved-model-and-WHY roster read `subagents.ts` depends on
  independently (it reads `agents/*.md` itself and never shelled out to this
  command). Authoring/editing/removing an agent is now a hand-edit of its
  `agents/<name>.md` frontmatter, same as always for its prompt body; a new
  intent's scores go in `scorecard.json` by hand, then `pix models route`
  recompiles `routing.json` and a sandbox relaunch picks it up. Typing
  `new`/`edit`/`rm`/`remove`/`reassess` answers with the standard
  `PIX_RETIRED` notice (exit 2, no side effect) naming that path — five new
  entries in `retired.go` + `corpus/retirement.jsonl`, proved end to end by
  the existing real-binary retirement harness
  (`corpus/retired_dispatch_test.go`), no shard changes needed. Net: `agent.go`
  580 -> 225 prod LOC, `agent_cmd.go` 74 -> 33; two now-obsolete redrive
  regression tests (`TestAgentNew_NextStepsPointAtLiveScorecardPath`,
  `TestAgentReassessModel_PointsAtLiveScorecardPath`) removed with the
  surfaces they pinned. Shrink-only: no budget ceiling raised.

- **U11k: `cmd/pix/corpus` reclassified as test-only support (588 -> 0
  production LOC).** The golden CLI corpus + retirement-manifest harness had
  no runtime caller — it exists solely to be driven by `go test
  ./cmd/pix/corpus`, exercising the real compiled `pix` binary as a
  subprocess — so its four implementation files (`loader.go`, `regen.go`,
  `retirement.go`, `runner.go`, plus `types.go`) are folded into their
  matching `*_test.go` files (`types.go` simply renamed to `types_test.go`,
  no collision). Every `.go` file under the package is now a `_test.go` file:
  `go test ./cmd/pix/corpus` and the full golden-corpus run (CI's `metrics`
  job) are unchanged and still green, including the sharded corpus, the
  append-only retirement-manifest checks, and the real-binary behavioral
  tests. Folding in the duplicate `buildPixBinary`/`BuildPixBinary` wrapper
  pair into one `sync.Once`-cached test helper avoided growing total test
  LOC while merging: the package's combined line count actually shrank
  588+863=1,451 -> 1,421. `arch_test.go`'s `pkgLayer` map and
  `scanPackages` now skip packages with zero non-test `.go` files (a
  package that is only tests has no layer to place), and
  `scripts/arch-metrics/main.go`'s `scan()` mirrors that rule for the same
  reason, so `cmd/pix/corpus` is gone from both the architecture layer map
  and `scripts/arch-metrics/budgets.json` — not zeroed, removed. Both
  changes are shrink-only: `pix/host` and every other package's recorded
  budget ceiling is unchanged.

- **U07b: `pix-host backup`/`restore` collapse into `pix-host memory
  snapshot`/`memory restore`.** The multi-component hot archive (a versioned
  `tar.gz` carrying `memory.db` + `config.toml` + `op-refs.env` + a
  `manifest.json`, with retention/pruning, tar-bomb guards, a plain-file
  install/rollback stack and its own atomic-write helper — 1,610 lines across
  `memory_backup.go`/`memory_restore.go`) is gone. What replaces it is ONE
  artifact: a snapshot is a plain sqlite file written with `VACUUM INTO`
  through a read-only handle (`pix-host memory snapshot PATH`, hot, verified,
  `0600`, never clobbering), and the restore primitive
  (`pix-host memory restore PATH [--force]`) installs one with the service
  STOPPED — enforced by the same advisory store flock the daemon holds, taken
  first and held across the commit, with the previous db and its sidecars kept
  in a reversible `.bak-<ts>-<rand>` set. `config.toml` is reproducible with
  `pix config set` and `op-refs.env` holds `op://` pointers, not values, so
  neither needed an archive format. The DB path, schema and `0600` mode are
  unchanged; the retired top-level `backup`/`restore` verbs answer with
  `PIX_RETIRED` naming the new commands. Documented in `docs/memory.md`.

- **U05b: monitor ingest ownership moved under `pix-host serve`; `pix
  monitor` is now a pure offline reader.** The loopback ingest listener that
  receives NDJSON events from the in-VM monitor tap (`services/host/monitor`,
  `:11437`) used to be started only by `pix monitor` itself. It now composes
  directly inside `runServe` (`services/host/serve.go`), alongside memory,
  gated by the same `services` config/CLI mechanism (`serveServiceAliases`
  gained a `monitor` entry) — `pix config set services monitor`, or the
  existing "empty config means all" default, enables it. `--bind`/`--port`
  moved down with it: they are `pix serve`/`pix-host serve` flags now, not
  `pix monitor` flags. `pix monitor` (`services/host/cmd/pix/monitor.go`)
  lost its listener entirely — `[name] [--path DIR] [--json]` only — and
  tails the new `config.MonitorStoreRoot()` (`<state-dir>/monitor`), the same
  root `serve` writes to, so a reader works whether or not serve is running
  right now. `pix status`/`doctor` still see ingest via the existing
  `monitor.DefaultPort` dial, unchanged by who started it. The wire schema,
  the `:11437` loopback-only bind default, and the kit's `PIX_MONITOR`/
  `PIX_MONITOR_URL` env contract and network allowlist entries are untouched.
  See `docs/design/monitor.md`.

### Removed

- **pix's host is macOS-only now.** Deleted the `systemd --user` managed-
  service implementation (`serve_install_linux.go`, the `pix-serve.service`
  unit generator + template + tests), the Windows lock/process/service/
  credential shims (`lock_windows.go`, `service/ctl_windows.go`,
  `service/start_windows.go`, `slackoauth/*_windows.go`, `sys/lock_windows.go`,
  `workflow/gworkspace/gog_credentials_snapshot_windows.go`), and every
  install/upgrade/release path that shipped a Linux `pix`/`pix-host` binary
  (`install.sh`, the `publish.yml` release-binaries matrix). launchd-managed
  `pix serve install`/`stop`/PID ownership/spawn-lock/plist validation are
  unchanged. `services/host` still `go build`/`go test`s under `GOOS=linux` —
  the pix sandbox IMAGE stays a Linux container and devs hack on this repo
  from inside one — via a single non-darwin compile stub
  (`service.ErrUnsupportedHost`) with no lifecycle behavior. See
  `docs/design/serve-lifecycle.md`.

### Fixed

- **`pix reset` asked a PORT whether the daemon was running, and got it wrong
  in both directions.** The pre-stop "was it up" answer and the post-stop "is it
  down" proof were both a `MEMORY_PORT` health dial, so a daemon whose memory
  service was disabled (monitor-only) or had crashed read as DOWN: reset stopped
  it and never restarted it, and — worse — it moved `~/.local/share/pix` out from
  under that still-live process and deleted the pidfile that was `pix serve
  stop`'s only handle on it. A stop that FAILED or refused an unverifiable pid
  was equally invisible: its error was printed and the destructive steps ran
  anyway. Both questions are now asked of the daemon's IDENTITY
  (`service.ServeIdentityUp`: a loaded managed unit, or a pidfile naming a live
  process that is not provably a stranger's), the same ownership answer
  `serve stop`/`serve status` already share. A daemon that cannot be PROVEN dead
  blocks the data move, keeps its pid/lock files, and is not "restarted" behind
  its own back; `--force` still overrides the data move, but never the runtime
  files. Reproduced by real-process/real-pidfile tests
  (`workflow/reset/reset_process_test.go`) that fail against the old probe.

- **The model router described the shipped catalog, not your host.** `pix
  models show|ls|pick|route` (i.e. the whole `pix-host route` tree) loaded
  `models.json` and nothing else, so its `AVAIL` column reported "Pix ships
  support for this" under a name every user reads as "you can call this" — and
  every intent resolved against it. On a host with no OpenAI key, `pix models
  show` reported the default intent routing to `openai/gpt-5.6-sol`, and `pix
  models route` WROTE that into `routing.json`, which host-mode subagents read.
  The binding-aware resolve already existed but lived in the launcher, out of
  the host binary's reach. It is now `services/host/inference` and both
  binaries share it. `AVAIL` becomes `STATUS` (`wired` / `unwired` /
  `retired`), an intent with no callable model is DROPPED from the compiled map
  and named rather than pointed at an unreachable provider, and `--catalog`
  restores the host-independent view for baking the image default.
- **A declined model download made `pix setup` exit non-zero.** Choosing Ollama
  local and answering "no" to the multi-gigabyte pull bound a candidate,
  probed it, and — since the weights were not on disk — hard-failed with
  "ollama models are bound, but none answered a request", contradicting the
  documented contract that declining is a decision, not a failure. The consent
  check now precedes that error. It was invisible because the covering test
  wired no probe at all (see below).
- **Verification could be silently switched off.** `verifyDirectInference` /
  `verifyOllamaInference` returned `0 attempted, 0 verified, no failures` when
  handed a `shellEnv` with no probe function — a value indistinguishable from a
  clean pass, so callers printed "0 model(s) answered a live request" and
  exited 0. They now return `(probeOutcome, error)` with an explicit
  `errNoProbeSeam`, which is what surfaced the setup bug above.
- **Three `pix doctor` tests failed on a clean checkout** because they read the
  developer's real `~/.config/pix/op-refs.env`. A `TestMain` guard now points
  the launcher package's config resolution at a temp dir.

### Added

- **Trusted pack `[[services]]` now wire into the supervisor (U07d).** The
  pack side exports exactly one seam, `pack.AcceptedGoPluginServices`: the
  minimal normalized view (name, activation, absolute path, sha pin, argv,
  env reference names, loopback front door) of a pack's go-plugin services —
  and ONLY after the pack's current host-exec surface matches the fingerprint
  accepted at the Tier-1 gate, so consent strictly precedes any staging or
  start. The supervisor side (`pack_units.go`) is the integrator hook:
  `packUnitSpec` (view → `supervise.NewExternalUnit`, dispense kinds limited
  to the closed `plugin.PluginMap` set) and `supervisor.reconcilePackUnits`
  (add-only, collision-safe; never replaces a running unit). Root `serve`
  composition is untouched — nothing calls the hook yet. Reserved-port,
  reserved-name, loopback-only, and env-names-only rules are re-validated at
  export, and the staged binary is re-hashed against the consented pin on
  every start, so a binary swapped after acceptance is refused at launch.

- **`pix models add ollama`** — the keyless half of `models add`. `models add`
  derived its provider list from `providerKeyRefOrder`
  (anthropic/openai/google), so the one backend that needs no credential had no
  post-setup path at all: pulling a new local model or gaining a cloud
  entitlement meant re-running `pix setup`. It reads what the daemon lists,
  proves each with a real generate, and widens the roster. `--local` /
  `--cloud` narrow it; the default is both. Downloads nothing — it names a tag
  worth pulling and leaves the decision to you. An explicit `models add
  <provider>` now also widens the roster for a provider already recorded in
  `roster_providers`, without which the SECOND add of any provider bound and
  probed models that then sat outside the roster while the command reported
  success.
- **Sandbox sessions no longer warn `No models match pattern "ollama/…"`.**
  `pix run` passes pi a `--models` cycle built from every callable binding,
  but `extensions/ollama-bridge.ts` registered exactly one hardcoded model. It
  now registers what the host's generated `inference.json` declares, with the
  configured bridge tag guaranteed present.

### Retired

- **The direct `[plugins.*]` config declaration is retired and inert (U07d).**
  A config.toml can no longer name an executable for `pix-host` to launch:
  every declared `plugins.<slot>` is swept at load into the same
  `RetiredKeys` notice surface as other stale keys (shown by `doctor`/
  `config show`), `Config.Plugin()` always answers builtin, and the
  `[plugins.mcp]` MCP-bridge override path is gone. External service units
  are pack-trust-admitted `[[services]]` declarations only (AC-SUP-05).

### Changed

- **`shellEnv` is gone; OS seams live in `services/host/sys`.** It was 22
  nullable function pointers threaded through 254 functions and guarded by 125
  hand-written nil checks that disagreed with each other — for `env.run == nil`
  alone the package held fourteen distinct behaviours, and three shipped bugs
  came out of the gap. `sys` splits the seams into four interfaces by what they
  touch (`Exec`, `FS`, `Env`, `Net`), so a signature says what a function can
  reach; `sys.Real` holds no nullable state, which is what let the guards be
  deleted rather than rewritten (**125 -> 11**, all 11 on domain probes that
  leave in a later phase). Nullability survives only in `sys/systest.Fake`,
  where an unwired method fails loudly instead of returning a zero value that
  reads like an answer. Net **-623 lines**. No user-visible behaviour change,
  but several fixtures were found to have been testing paths no user took —
  see docs/design/rearchitecture.md.

- **An intent's `providers` list is a PREFERENCE, not an allowlist**, and is
  spelled `prefer_providers` in `policy.json` (the old key still loads, with
  the new semantics). The resolver ranks by objective and then floats preferred
  vendors to the front, so a preference can reorder the feasible set but never
  exclude the last usable model, and an unreachable vendor is reported via
  `Decision.PreferenceMet` instead of `ConstraintsMet: false`. The relaxation
  ladder loses its provider rung — the code implementing it already said
  "vendor diversity is a PREFERENCE encoded as a constraint". This mattered
  because the shipped policy pins `overlord` (the interactive orchestrator and
  the default `run_intent`) to OpenAI while `pix setup` wires Anthropic: every
  default install resolved through the ladder and reported `FALLBACK` on its
  most important route while working perfectly. A genuinely hard vendor rule
  belongs in `inference.exclusive_backend` / `exclusive_source`, which enforce
  it at the binding layer where an excluded vendor is uncallable rather than
  merely outranked.

- **Renamed `pix route` to `pix models`** (docs/design/models-cli.md): the
  noun a user actually wants ("what models can pix use, and which are wired
  up") replaces the mechanism it was filed under. `pix models ls|show|pick`
  and the mutating `pix models route` (`--out PATH`; `models compile` stays as
  an undocumented muscle-memory alias) are thin passthroughs to the unchanged
  `pix-host route` subcommand tree — nothing on the host side moved. Bare
  `pix models` is a new read-only status screen: runtime, bound providers,
  the roster, and the resolved session model, ending with a `Next:` line.
  `pix route` keeps working for one release as a hidden alias, printing a
  one-line deprecation to stderr only (stdout/`--json` unaffected);
  `retiredVerbs["route"] = "models"` (help.go) is permanent and is what a
  typed `pix route` resolves to after the alias is removed. `pix models add
  <provider>` and `pix models setup` — the fix for "I can't find how to add a
  second provider key later" — land in a follow-up change; this rename only
  builds the verb tree and leaves the extension point.

### Removed

- **The pre-public pack-directory migration is gone** (`~/.local/share/pix/pack`
  / `.../personal` -> `.../default`, its manifest `name` rewrite, its
  trust-path migration, and the stale-state repair pass that cleaned up after
  an older non-transactional version of itself). Those two directory names were
  only ever written by pre-0.1.0 builds under the OLD product name, and 0.1.0
  was a clean pre-launch cutover with no legacy-path discovery — so on every
  released build the migration probed for directories no pix build creates,
  and `DefaultPackRoot()` did a stat, a flock, a config load and a trust-store
  load to decide "nothing to do". It now returns the path. What is kept, and
  tested: the bare `pix pack use personal` token remains a deprecated alias for
  the default pack (a CLI spelling, not a path probe), and a directory that
  happens to be named `personal` or `pack` is now left strictly alone —
  never renamed, never rewritten, never repointed. -525 lines of production
  code, and the pack package is back under its pre-`[[services]]` budget.
- The `[[services]] UnitSpec` vocabulary (runtime/activation constants, the
  reserved name+port sets, and the value-shape patterns) is one internal value
  instead of ten package-level names, and `packService`/`packServiceResources`
  are unexported: no supervisor consumes a service declaration yet, so the
  exported surface it will need is the supervisor story's to earn. Manifest
  syntax, every rejection, the consent screen, and the fingerprint are
  unchanged.

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
