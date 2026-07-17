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
- Model registry used `ollama/gemma4`, which does not exist and disagreed with the
  `gemma3:4b` default the Makefile, README, and config use. Registry and
  scorecard now use `ollama/gemma3:4b`.
- `pi-stack evals --help` advertised flags (`--suite`) the host command does not
  accept and omitted `import`; the launcher help now matches the host.

### Added

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

- Removed company-specific wiring from the public tree: the `SNOW_CONN` env in
  `make serve` (now a generic overlay-populated `SERVE_ENV`), the `snow` probe in
  the `healthcheck` skill (now a generic `EXTRA_CLIS` hook), and the overlay-only
  `:11442` port from the public kit allowlist. Overlays add these themselves.
- Pruned stale docs: two `HANDOFF` snapshots, a superseded migration guide, a
  review artifact, and an upstream issue draft.
- README reworked to lead with the outcome, fix the launch command
  (`pi-stack run`), correct the pi link, and complete the launcher command list.
