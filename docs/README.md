# pix docs

Start with the [top-level README](../README.md) and, if you are extending the
harness, [AGENTS.md](../AGENTS.md). This folder holds the deeper references.

## For adopters

- [gworkspace.md](gworkspace.md) — set up the host-side Google Workspace (`gog`)
  MCP server, read-only by default.
- [design/packs.md](design/packs.md) — the pack design: private company context
  ships as a **pack** (plus container/host MCP integrations), never a
  forked/recompiled tree.
- [memory.md](memory.md) — the memory service: what it stores, how recall and
  capture work, the commands, and the trust model.
- [../SECURITY.md](../SECURITY.md) — the trust boundary, what the sandbox does
  and does not protect, and how to report a vulnerability.
- [../CONTRIBUTING.md](../CONTRIBUTING.md) — build constraints, the open-core
  boundary, and how to contribute a skill or agent.

## Design notes (how it works, and why)

These document the reasoning behind subsystems. They are reference, not
tutorials.

- [onboarding-v3.md](design/onboarding-v3.md) — the proposed one-path setup,
  dependency, OAuth, pack, doctor, and provider-routing design.
- [slack-setup.md](design/slack-setup.md) — the accepted per-user Slack
  credential model and proposed guided setup flow.
- [routing.md](design/routing.md) — the model router: registry, scorecard,
  policy, and how `models route` bakes `routing.json`.
- [models-cli.md](design/models-cli.md) — the `pix models` noun rename and the
  first-run "add a second provider key" fix.
- [subagents-extension.md](design/subagents-extension.md) — the `subagent` tool
  (single / parallel / chain / trees) and the watchdog.
- [subagent-pin-tracker.md](design/subagent-pin-tracker.md) — the pinned
  live tracker for running subagents.
- [self-learning-loop.md](design/self-learning-loop.md) — the memory capture and
  recall loop.
- [serve-lifecycle.md](design/serve-lifecycle.md) — the `pix serve` daemon
  lifecycle: lazy auto-start, the managed login service, and the hardening addendum.
- [host-mode.md](design/host-mode.md) — `pix host`, the unsandboxed escape
  hatch for developing pix itself (Phase 1 built, gated off by default).
  Its guardrails reduce accidents; they are NOT a security boundary.
- [worktree-tasks.md](design/worktree-tasks.md) and
  [task-ux-decisions.md](design/task-ux-decisions.md) — the `pix task`
  parallel-work workflow.
- [cli-redesign.md](design/cli-redesign.md) — the launcher verb tree.

## Upstream (issues and patches we owe pi/sbx)

Working notes toward upstream contributions. Historical or in-flight, not
adopter docs.

- [tui-bottom-pin.md](upstream/tui-bottom-pin.md) — the vendored renderer patch.
- [pi-subagents-hang-pi-0.80.md](upstream/pi-subagents-hang-pi-0.80.md)
- [sbx-0.34-custom-kit-credentials.md](upstream/sbx-0.34-custom-kit-credentials.md)
- [sbx-version-box.md](upstream/sbx-version-box.md)
