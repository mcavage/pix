# Changelog

All notable changes to pi-stack are recorded here. CI publishes versioned images
from `main` and stamps the tag into `pi-kit/spec.yaml`; this file is the
human-readable summary of what changed and whether an upgrade is breaking.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Fixed

- `make evals`/`make route` with no ARGS printed usage and exited 2 (looked
  broken); they now default to the safe read-only `show`, and the promptfoo guard
  only fires for a `run`.
- `make evals ARGS=run` failed with an opaque `spawn pi ENOENT` on every case:
  the eval provider runs each model through a headless `pi`, but `pi` is not on
  the host by default (only `pi-stack`/`pi-stack-host` are). Now `make evals`
  preflight-checks for `pi` (and `PI_BIN`), the provider returns a clear
  install-hint on ENOENT, and an all-errored run exits non-zero with the likely
  causes instead of reporting a cheerful $0 run. Documented `pi`-on-host as an
  eval prerequisite.
- Eval errors were mis-prefixed `route:` and a promptfoo host failure (e.g. a
  native-module Node-version mismatch) surfaced as an opaque "produced no
  results". Now prefixed `evals:` with an actionable message that names the likely
  cause and the reinstall fix, and states promptfoo is an optional host dep.

- `deliver` skill failed to load: its frontmatter `description` was an unquoted
  YAML scalar containing `: ` sequences, which YAML parsed as a nested mapping.
- `enterprise-admin` agent resolved to a model (`anthropic/sonnet-5`) that is not
  in the registry; it now uses `intent: reasoning` like its peers.
- Model registry was built from stale/guessed model names and prices (e.g.
  `ollama/gemma4`, `claude-sonnet-4-6`). Re-grounded the whole registry on LIVE
  July 2026 vendor pricing + published benchmarks (see the new `model-refresh`
  skill). The local model is now `ollama/qwen3.5:9b` — a current (2026-02),
  Apache-2.0 all-rounder that actually fits a 16GB machine (~6.6GB), replacing
  the year-old `gpt-oss:20b` (and the 58GB `gemma4:31b` that never fit a laptop).
  The tiny `gemma3:4b` remains the memory-watcher default (a separate,
  DRAM-bound job).
- `pi-stack evals --help` advertised flags (`--suite`) the host command does not
  accept and omitted `import`; the launcher help now matches the host.

### Added

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

- **Local Ollama models are now coherent host config, not scattered env.** The
  stale `gemma3:4b` defaults are gone: the memory watcher defaults to
  `qwen3.5:4b` (small, current, tools-capable, still resident-friendly) and the
  new `ollama_bridge_model` setting (the sandbox's local chat model + the
  router's local option) defaults to `qwen3.5:9b`. Set it with `pi-stack config
  set ollama_bridge_model <tag>`; `pi-stack run` writes it into
  `<workspace>/.pi-stack/ollama-bridge.model` and the `ollama-bridge` extension
  reads it — no more hand-editing `/etc/sandbox-persistent.sh`. The bridge display
  label is now derived from the tag, so `OLLAMA_BRIDGE_MODEL_NAME` is optional
  (you set one value, not two). `make pull-models` pulls all three local models.
  NOTE: the host memory service uses the watcher + embed models; it does NOT use
  the bridge/router local model — those are separate roles.
- **evals is no longer a `pi-stack` command.** It was a consumer-facing launcher
  verb that consumers could not actually use (repo-rooted, needs promptfoo). It
  is now maintainer tooling in the Makefile only (`make evals`), running the
  repo-built `pi-stack-host evals` backend. Removed the `evals` verb from the
  launcher, help, and man page.
- **`agent ls` WHY reasons rewritten.** They said `sole fit` (meaningless) and a
  wall of precise-looking but unmeasured numbers. Now each WHY names the actual
  binding constraints that left one model (e.g. `only model matching anthropic,
  <=$0.18, >=0.80 acc`) or what the winner beat in a contest, with a footer that
  flags the metrics as seed priors until `make evals` measures them.
- **Routing redesign: a real tiered, multi-vendor crew instead of a monoculture,
  grounded in live model data.** Previously 13 of 18 agents collapsed onto one
  model and the rest onto Opus. The registry/scorecard/policy are now seeded from
  the current (July 2026) lineup and pricing: Claude Fable 5 for `deep`
  (`max-accuracy`); Opus 4.8 for `architect`/`product-manager` (`strategy`);
  Sonnet 5 for `engineer`/`designer` (`code`) and the `advisory` specialist crew;
  GPT-5.6 Sol for `review`; Gemini 3.1 Pro for `security-lead` (`red-team`);
  Gemini 3.1 Flash-Lite for `fanout` (`breadth`); Haiku 4.5 for `qa-lead`
  (`verify`). Three cloud vendors plus a local `qwen3.5:9b` option, tiered by
  leverage with the adversarial roles pinned cross-vendor via provider
  allowlists. New intents: `strategy`, `advisory`, `red-team`. Eval providers
  mirror the registry.
- Removed company-specific wiring from the public tree: the `SNOW_CONN` env in
  `make serve` (now a generic overlay-populated `SERVE_ENV`), the `snow` probe in
  the `healthcheck` skill (now a generic `EXTRA_CLIS` hook), and the overlay-only
  `:11442` port from the public kit allowlist. Overlays add these themselves.
- Pruned stale docs: two `HANDOFF` snapshots, a superseded migration guide, a
  review artifact, and an upstream issue draft.
- README reworked to lead with the outcome, fix the launch command
  (`pi-stack run`), correct the pi link, and complete the launcher command list.
