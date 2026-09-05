# Onboarding redesign

> **HISTORICAL — pre-v2 design note.** This document predates the accepted
> Pix v2 surface and architecture (`docs/design/pix-v2-surface.md`,
> `docs/design/pix-v2-architecture.md`), which supersede it. Commands,
> files, and components described here may no longer exist. Nothing in it
> is a description of current behavior; read it as history only.


> **Historical, superseded.** This doc's concrete write-back mechanism (the
> in-sandbox agent writing `<workspace>/.pix/onboarding.json`, `onboard.go`'s
> `runOnboardNonInteractive`/`applyOnboardingResult`, and `setup.go` being torn
> down in favor of it) was never shipped: `pix setup` is still the live host
> wizard (`services/host/cmd/pix/setup.go`), onboarding conversation lives in
> `skills/onboarding/SKILL.md` + `hoststate.go`, and there is no
> `.pix/onboarding.json` file anywhere in the tree. Read this doc ONLY for the
> data-plane/control-plane split background (still true), NOT for the schema,
> the file mechanism, or the `Files`/`Fitness functions` sections below, which
> describe a design that was not built.
>
> It also predates the `gog` -> `gworkspace` rename, and Google Workspace has
> since left pix entirely. Every `gog_account` / `mcp "gog"` reference below
> names a config key and a server that no longer exist: there is no pix config
> key for a Google account at all, and `gog` is now declared by a PACK like any
> other MCP server, registered with `pix mcp add`. See `docs/gworkspace.md` and
> `docs/design/integrations-remediation.md`.

Status: PROPOSED (awaiting owner gate before the teardown) — the gate never
happened; see the historical notice above.
Supersedes: `pix setup` (the 4-step host TTY wizard) and the `onboard` skill.

## Problem

Onboarding today is two disjoint things that never hand off to each other:

1. `pix setup` (`services/host/cmd/pix/setup.go`): a host-side 4-step
   TTY wizard (keys, memory/knowledge, integrations, credentials). It writes
   host config but knows nothing about *you*, and reads like `apt-get` output.
2. The `onboard` skill (`skills/onboard/SKILL.md`): a conversational identity
   seed into the memory service, run *inside* a sandbox. It knows you but can't
   persist host state, and nothing tells you it exists.

First run nudges you to the wizard from bare `pix` (the one command that
launches only at a TTY). `pix run` (the command you actually type) has no
onboarding awareness at all. The result is a broken *shape*, not broken code.

## Constraints (owner-fixed, not up for debate)

- `pix run` just launches the agent. Onboarding is OPT-IN, never forced.
- Provider keys, `serve`, and the memory service are HOST-side prerequisites.
- The interactive `setup` wizard AND the `onboard` skill are deleted.
- A flag-driven non-interactive path stays for CI/automation.
- One contiguous experience: first run can (opt-in) drop you into an agent that
  onboards you conversationally and lands you in a ready working session.

## Shape (the load-bearing decision)

Onboarding is something the **agent does inside a normal `pix run`
session**, not a second top-level host command. The offer lives where you
actually type.

- `pix run` launches as it does today (host preflight = key check only; no
  onboarding gate). Constraint 4a honored literally: `runVerb` never onboards.
- On a first run, the in-sandbox agent's FIRST turn is an opt-in offer:
  "First time here. Want two minutes to set up how I work with you? [Y/n]".
  Decline -> a normal session, zero residue. Accept -> the onboarding
  conversation, then you land in the same session on a real first task.
- Re-onboard later by telling the agent "onboard me" (skill trigger). No host
  verb to remember.
- `pix setup` survives ONLY as the headless automation path (renamed to
  `pix onboard --non-interactive` with `setup` kept as a deprecated
  alias): deterministic host config from flags, no prompts, safe in CI.

Rejected alternative: a top-level `pix onboard` command that launches and
owns the lifecycle. It adds a second thing to remember for one concept and
reintroduces a host-side step in front of the session, which cuts against "one
contiguous experience." The lifecycle it wanted to own (bootstrap, reconcile)
still happens, just folded into `run` + the in-session flow (below).

## Two trust planes (the central technical problem)

The in-sandbox onboarding agent produces two kinds of output that persist very
differently. The sandbox is network-fenced and non-root; it CANNOT write the
host's `~/.config/pix/config.toml`.

- **Data plane (identity):** name, tone, pet peeves, values. Written LIVE via
  the memory service (`:11435` over `host.docker.internal`, the existing
  `memory-recall.ts` / `memory-capture.ts` channel). No new mechanism, applies
  immediately, so the same session benefits.
- **Control plane (host config):** `gog_account`, `knowledge_bundles`, `mcp`,
  `ollama_bridge_model`, `memory_watcher_model`. These govern HOST code
  execution (MCP subprocess spawns, kit stacking), so they must NOT be a live
  write surface from a fenced VM. Switching the active pack is a separate,
  explicit command (`pix pack use`), not a field this file proposes.

### Write-back mechanism: declarative file, host-applied after exit

The agent writes `<workspace>/.pix/onboarding.json` (ordinary file tools,
no new capability) describing the control-plane changes it proposes. On the
NEXT `pix run` for that workspace, the host reads it, VALIDATES against a
fixed allowlist schema, shows a diff, applies under a `[Y/n]` confirm gate
(`--yes` for CI), then deletes it.

Why this over a host JSON-RPC `config set` endpoint (the architect's Option 1,
rejected): a loopback listener that mutates config and can enable an `mcp`
entry (making the host spawn a subprocess) from a fenced VM is exactly the
backdoor shape AGENTS.md forbids. The memory endpoint is fine because it writes
data ROWS; a config endpoint has a code-execution blast radius. The declarative
file adds zero new listener and reuses the one-directional `.pix/` seam
`run.go` already owns, in reverse. Worst case a hostile VM can only PROPOSE
config within a fixed schema that the host declines at the gate.

Tradeoff accepted: control-plane changes land the NEXT session, not the
onboarding one. Identity (the thing that shapes the conversation) applies
immediately via memory, so the felt experience is contiguous; wiring gog/mcp/
knowledge one session later is fine (those need a fresh sandbox created with
`--mcp` anyway).

### Schema (allowlist, security-critical)

```jsonc
{
  "version": 1,
  "gog_account": "you@example.com",
  "mcp": ["gog", "slack"],              // allowlisted names ONLY
  "knowledge": {"action": "scaffold|use|skip", "source": "<path|git-url>"},
  "ollama_bridge_model": "qwen3.5:9b",
  "memory_watcher_model": "qwen3.5:9b"
}
```

Identity is deliberately absent (it is memory data). Validation rejects: unknown
`mcp` names (only `gog` + known catalog + pack-registered locals pass, so the
file can never make the host spawn an attacker-chosen command); any
`host.enabled`, `plugins.*`, `kits.stack`, or arbitrary `services`.

## First-run detection without a host gate

The host must NOT gate `run` on onboarding, but the in-sandbox agent needs to
know it is a first run. `run` writes a one-shot marker into the mounted
workspace when `!configExists()` AND no identity is recalled:
`<workspace>/.pix/onboarding.offer`.

SUPERSEDED, and this is what actually ships: there is no offer marker and no
`extensions/onboarding.ts`. `pix setup` execs `run` with the kickoff message
composed as generated input (`launch.GeneratedInputMarker` +
`provision.OnboardingKickoff`, see `cmd/pix/setup_cmd.go`), and the
`onboarding` skill owns the flow from there. `.pix/onboarding.json` survived
but changed job: it is the PROPOSAL the skill writes, reconciled by
`pix setup --apply` / `provision.ReconcileOnboarding` under a confirmation
gate. Nothing injects a first turn behind the user's back, so the TTY-gating
that guarded CI has nothing left to guard.

## Files

Removed:
- `skills/onboard/` (replaced by `skills/onboarding/SKILL.md`).
- `setup.go` wizard core: `runSetup`, `setupIO`, `parseSetupArgs`,
  `setupSecretsSection`, the 4-step prose. Reusable helpers (`secretCheck`,
  `credentialDetermination`, `gogAuthed`, `opRefsResolvable`, `setupKnowledge`,
  `registerServers`) move to `onboard_apply.go`.
- `firstRunHook()` call is already absent from `runVerb`; keep it that way.
- The bare-`pix` first-run nudge retargets from `setup` to a one-line hint
  that `pix run` will offer setup in-session.

Added:
- `skills/onboarding/SKILL.md`: the salvaged `onboard` flow (single probe, one
  batched question, confirm-before-write) plus a terminal step that `/remember`s
  identity (data plane) and writes `.pix/onboarding.json` (control plane),
  then runs one real first task.
- `cmd/pix/setup_cmd.go`: composes the kickoff as generated input and execs
  `run` (there is no offer marker and no `extensions/onboarding.ts`).
- `workflow/provision`: `OnboardingKickoff`, `ReconcileOnboarding`,
  `applyOnboarding` — the proposal read/confirm/write path.
- `cmd/pix/run_cmd.go`: reconciles a pending `.pix/onboarding.json` before
  `LoadResolvedConfig`, so a fresh create picks it up.

Config keys touched: `gog_account`, `mcp`, `knowledge_bundles` (+ `services`
knowledge), `ollama_bridge_model`, `memory_watcher_model`.

## Fitness functions (tests that must stay green)

1. `TestRunVerb_NeverOnboards`: `run` launches without ever invoking onboarding
   (spy hook).
2. `TestApplyOnboarding_RejectsNonAllowlisted`: unknown `mcp`, `host.enabled`,
   `plugins.path`, arbitrary `services` are rejected.
3. `TestApplyOnboarding_Idempotent`: re-applying identical input yields
   identical `config.toml` bytes.
4. Grep guard: no new `serve.go` HTTP handler for onboarding (enforces "no new
   listener").
5. `TestOnboardingOffer_TTYOnly`: the offer never injects under non-interactive.

## Phased plan

- P0: refactor reusable helpers out of `setup.go` into `onboard_apply.go`; add
  `pix onboard --non-interactive` + `setup` deprecation alias. No behavior
  change yet.
- P1: schema + `applyOnboardingResult` + validation + CI path. Fitness 2, 3.
- P2: `skills/onboarding/SKILL.md` + `extensions/onboarding.ts` + the
  `run.go` marker/reconcile wiring. Fitness 1, 4, 5. Manual e2e in a
  `pix-test` sandbox.
- P3: delete the wizard core + `skills/onboard/`; update help/man/README/AGENTS;
  open-core guard.

## Open questions for the owner

- O1: first-run offer default on bare Enter. Recommend default-Yes on a real
  TTY (a keyed, config-less user who typed `run` wants help); one keystroke to
  skip. Acceptable to flip to default-skip to honor "never forced" maximally.
- O2: multi-pack onboarding depends on memory scope-tagging that does not
  exist yet (documented v2 gap: the memory store is shared across packs, the
  in-store scope column stays dormant). Ship with the caveat, or block on
  tagging? Recommend ship with caveat.
- O3: control-plane changes landing next-session (not in the onboarding
  session). Acceptable? Recommend yes; the alternative is the rejected live
  endpoint.
