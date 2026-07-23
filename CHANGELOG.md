# Changelog

All notable changes to pi-stack are recorded here. CI publishes versioned images
from `main` and stamps the tag into `pi-kit/spec.yaml`; this file is the
human-readable summary of what changed and whether an upgrade is breaking.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Fixed

- `pi-stack host` now launches its interactive session under `op run
  --no-masking`. op's default output masking pipes pi's stdout/stderr through a
  filter, which makes them non-TTYs — pi's TUI then saw no terminal and exited
  immediately (banner, then straight back to the shell, exit 0). Non-interactive
  paths were unaffected, which is why it looked like a silent failure. (The
  mcp-gateway op-run path already used `--no-masking`.)
- Provider-key op:// refs are stored with **literal spaces** again. An earlier
  change percent-encoded spaces (`Anthropic%20API%20Key`) on a false premise;
  op 2.35.0's `op read` AND `op run --env-file` both reject `%20`, so any
  1Password item whose name has a space (very common) failed to resolve and
  `pi-stack setup` aborted. Existing encoded refs self-heal: they're decoded on
  read and rewritten literal on the next write.

### Changed

- The sandbox and opt-in host mode now use pi `0.82.0`. Curated pi
  extensions were re-pinned to the newest versions published by the 0.82.0
  release, and CI now checks both vendored runtime patches against the exact pi
  and todo-list package pins.
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
- Identity seeded from git config is now **first name only** — no surname, no
  email. It's recalled into every session, so it carries the minimum to greet.
  (`readGitIdentity` no longer reads `user.email`; memory stores one first-name
  fact instead of name + email.)

### Changed (breaking)

- **MCP migrated to sbx's nightly gateway.** The sandbox flag is now
  `--static-mcp` (sbx removed `--mcp`), and MCP runs through sbx's **local
  data-plane gateway** — always available, **no `SBX_MCP_URL`** needed (dropped
  the old SBX_MCP_URL gate + "gateway off" warning; `pi-stack mcp register` no
  longer requires it). pi-stack's own `pi-stack run --mcp M` CLI flag is unchanged.
  **Per-server attach mode:** a configured server is attached eagerly at create
  (`--static-mcp`, tools always in context) or left dynamic (the in-VM agent
  discovers + calls it on demand via mcp-find/mcp-exec/code-mode; the daemon
  spawns local stdio servers host-side, so local and remote behave the same). The
  **default is dynamic for every registered server** — keeps heavy tool schemas
  out of context until needed. Pin a server eager with the new `mcp_static` list
  (`mcp_dynamic` is the explicit opposite, and wins if a server is in both). New:
  - `pi-stack mcp load <name> [DIR]` — attach an already-registered server to a
    RUNNING sandbox live, no recreate (`sbx mcp load`).
  - `pi-stack mcp auth [args…]` — hosted-control-plane OAuth for remote servers
    (`sbx mcp auth`; e.g. `auth --all`).
  - `pi-stack mcp bundle` — register the shipped public catalog
    (notion/atlassian/granola, `config/mcp-catalog.bundle.json`) in one step.
  `make mcp-auth` already used native `sbx mcp auth`; its tail now points at
  `pi-stack mcp load` instead of a recreate. `doctor` guidance no longer mentions
  `SBX_MCP_URL` (a failed `sbx mcp ls` now points at the sbx daemon).

- **Kit migrated to kit-spec v2** (`pi-kit/spec.yaml`, `schemaVersion: "2"`).
  Credentials are now a `credentials[]` list of `service` + `apiKey`
  (name/proxyManaged/inject[]); egress is `caps.network.allow`. Replaces the v1
  `network.serviceDomains`/`serviceAuth`/`allowedDomains` + `credentials.sources`
  + `environment.proxyManaged`. Injection is unchanged (proxy-managed sentinels;
  all four providers verified). **Requires a recent `sbx` nightly** (v0.37+); the
  per-credential `service:` is mandatory or `sbx run` panics. Overlay/mixin kits
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
  reuses that exact choice with no prompt — a persisted `sbx` mode still
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
  verified this run)" when a run used existing sbx keys instead — real
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
  beat or that a constraint left a `sole fit` — plus a legend. Previously it said
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
  resident for the bridge/router — so a fresh install ran capture on a model that
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
  and the `ollama-bridge` extension reads it — no more hand-editing
  `/etc/sandbox-persistent.sh`. The bridge display label is now derived from the
  tag, so `OLLAMA_BRIDGE_MODEL_NAME` is optional (you set one value, not two).
  `make pull-models` pulls the local models. NOTE: the host memory service uses the
  watcher + embed models; it does NOT use the bridge/router local model — those
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
  `make serve` (now a generic overlay-populated `SERVE_ENV`), the `snow` probe in
  the `healthcheck` skill (now a generic `EXTRA_CLIS` hook), and the overlay-only
  `:11442` port from the public kit allowlist. Overlays add these themselves.
- Pruned stale docs: two `HANDOFF` snapshots, a superseded migration guide, a
  review artifact, and an upstream issue draft.
- README reworked to lead with the outcome, fix the launch command
  (`pi-stack run`), correct the pi link, and complete the launcher command list.

### Removed

- **Tore out the automated eval harness** (`evals/`, `pi-stack-host evals`,
  `make evals`, `pi-stack agent reassess --model`'s auto-measure path). The
  router never needed it: it only ever reads `scorecard.json`, regardless of
  how the numbers got there. Scores are now hand-maintained — edit
  `services/host/routing/defaults/scorecard.json` directly (seeded from
  published benchmarks/pricing; see the `model-refresh` skill), then
  `pi-stack route compile`. `pi-stack agent new`'s starter eval-suite
  scaffolding is also gone.

### Fixed

- **Automatic fact capture could be silently dead.** The watcher-availability
  check was one-shot at startup and latched: once it marked the watcher
  unavailable, `observe` short-circuited and never retried, so capture stayed off
  until a full daemon restart even after the model was pulled — and the client
  swallowed the failure reason, so nothing surfaced. Added a throttled live
  re-probe (capture recovers within 30s of the model appearing, no restart), a
  one-time client warning when capture is off, and a `pi-stack doctor` line that
  reads the daemon's live capture flag with the exact fix.
- `deliver` skill failed to load: its frontmatter `description` was an unquoted
  YAML scalar containing `: ` sequences, which YAML parsed as a nested mapping.
- `enterprise-admin` agent resolved to a model (`anthropic/sonnet-5`) that is not
  in the registry; it now uses `intent: reasoning` like its peers.
