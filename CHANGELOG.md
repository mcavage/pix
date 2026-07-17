# Changelog

All notable changes to pi-stack are recorded here. CI publishes versioned images
from `main` and stamps the tag into `pi-kit/spec.yaml`; this file is the
human-readable summary of what changed and whether an upgrade is breaking.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Fixed

- `deliver` skill failed to load: its frontmatter `description` was an unquoted
  YAML scalar containing `: ` sequences, which YAML parsed as a nested mapping.
- `enterprise-admin` agent resolved to a model (`anthropic/sonnet-5`) that is not
  in the registry; it now uses `intent: reasoning` like its peers.
- Model registry was built from stale/guessed model names and prices (e.g.
  `ollama/gemma4`, `claude-sonnet-4-6`). Re-grounded the whole registry on LIVE
  July 2026 vendor pricing + published benchmarks (see the new `model-refresh`
  skill). The local model is now `ollama/gemma4:31b` — the current open-weight
  leader (Apache 2.0; beats the Qwen 3.5 and Llama 4 families on coding +
  reasoning), replacing the year-old `gpt-oss:20b`. The tiny `gemma3:4b` remains
  the memory-watcher default (a separate, DRAM-bound job).
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

- **Routing redesign: a real tiered, multi-vendor crew instead of a monoculture,
  grounded in live model data.** Previously 13 of 18 agents collapsed onto one
  model and the rest onto Opus. The registry/scorecard/policy are now seeded from
  the current (July 2026) lineup and pricing: Claude Fable 5 for `deep`
  (`max-accuracy`); Opus 4.8 for `architect`/`product-manager` (`strategy`);
  Sonnet 5 for `engineer`/`designer` (`code`) and the `advisory` specialist crew;
  GPT-5.6 Sol for `review`; Gemini 3.1 Pro for `security-lead` (`red-team`);
  Gemini 3.1 Flash-Lite for `fanout` (`breadth`); Haiku 4.5 for `qa-lead`
  (`verify`). Three cloud vendors plus a local `gemma4:31b` option, tiered by
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
