# Model routing — cost / latency / accuracy tradeoff selection

**Status:** built (v1). The router replaces hand-pinned model ids on every agent
with a declared *intent* (a hard constraint) that resolves to a concrete model
from measured scores. Evals feed the scores; the resolver picks; nobody toggles
models by hand.

## The problem

Every subagent preset hard-codes a `model:`. Thirteen of eighteen were pinned to
`anthropic/claude-opus-4-8`, so any crew task fired a wall of Opus in parallel.
That is the bill. Worse, the pin is a guess: no one measured whether Opus is
actually better than Sonnet or a local model *for that specific task*, and there
was no way to react to a new model (Sonnet 5, a GPT 5.6 variant, Kimi, GLM,
local gemma) except hand-editing eighteen files.

Any agent is measurable on three axes: **cost**, **latency**, **accuracy**. Cost
and latency are cheap to measure from a real run (token usage × price, wall
clock). Accuracy is the hard axis — it needs a task-representative eval with a
checkable outcome. So the design splits along that seam.

## Shape (mirrors `capabilities.json`)

`capabilities.json` already established the pattern: declare an abstract need,
resolve it in one file, swap the file to retarget everything. The router is the
same idea for models.

```
HOST (Go, tested, owns the truth)            SANDBOX (TS, reads one file)
┌───────────────────────────────┐            ┌──────────────────────────┐
│ models.json    (registry+price)│            │ routing.json (resolved)  │
│ scorecard.json (measured)      │  compile   │  intent -> model + why   │
│ routing-policy.json (intents)  │ ─────────▶ │                          │
│      │                         │            │ subagents.ts reads it:   │
│  Resolve() constrained-opt     │            │  intent: -> model id     │
│      ▲                         │            │  (explicit model: wins)  │
│  evals run (measures accuracy) │            └──────────────────────────┘
└───────────────────────────────┘
```

The sandbox never calls the host at spawn time (that path can hang a subagent).
It reads a precompiled `routing.json` — deterministic, offline, auditable. The
host regenerates that file with `pi-stack route compile`, and the user bakes it
(`make load`) on a new-model release. This matches the "on new model release,
manual, easy to plug a model in" cadence the feature was scoped to.

## Data model (`services/host/routing`)

Dependency-light subpackage (JSON only, no sqlite) so both the host binary and
the launcher can import it, exactly like `config`.

- **Model** — `id` (fully qualified `provider/id`), `provider`, token prices
  (`input_per_mtok`, `output_per_mtok`), `local` (Ollama/DMR: unmetered),
  `available` (wired in this stack right now). **Adding a model is one entry** in
  `models.json`; everything downstream picks it up.
- **Score** — one `(model, task_type)` row: `accuracy` 0..1, `latency_ms_p50`,
  `cost_usd` (mean per task), `n` samples, `source` (`eval|seed`), `updated`.
  Bootstrapped with seed priors so routing works before the first eval.
- **Intent** — the declared need for a role/task. The hard-constraint form the
  feature was scoped to: `max_cost_usd`, `max_latency_ms` (hard ceilings),
  `min_accuracy` (floor), `objective` (`accuracy|cost|latency|balanced`, what to
  optimize among the feasible set), optional `providers` allowlist (keeps
  `review` cross-vendor; lets a task force local-only), and a `fallback` model.

## Resolver (the heart)

`Resolve(registry, scorecard, intent) → Decision`, deterministic:

1. **Candidates** = available models with a score for `task_type`.
2. **Feasible** = candidates satisfying every hard constraint (cost ≤, latency ≤,
   accuracy ≥, provider ∈ allowlist).
3. If feasible is non-empty, pick by `objective` (accuracy = max accuracy,
   tiebreak cheaper then faster; cost = cheapest; latency = fastest; balanced =
   normalized blend). Ranked `alternatives` come back too.
4. If **nothing is feasible**, fall back to `intent.fallback` (or the policy
   default) and set `constraints_met=false` with a human reason. The router never
   returns "no model" — a crew task always gets *a* model, just flagged.

## Eval harness

`pi-stack-host evals run` measures accuracy (and records real cost + latency) so
the scorecard is earned, not guessed.

- **Suite** = a directory of case files (`*.json`). Each case: `id`,
  `task_type`, `prompt`, and a scorer.
- **Scorers** (pluggable): `contains`/`regex` (deterministic smoke), `command`
  (write the model output to files, run a shell command, exit 0 = pass — the
  mechanical coding scorer: build/test/patch), `judge` (LLM rates 0..1 vs a
  rubric — for soft tasks, off by default, costs money).
- **Runner** = an interface. The real one (`piRunner`) invokes a model exactly
  like a subagent does (`pi --model <id> -p <prompt> --mode json --no-session
  --no-extensions`) and reads token usage + wall time back. Tests inject a
  **fake runner**, so the whole harness is unit-tested with **zero spend**.
- **Budget guard**: `--budget <usd>` caps a sweep; the runner aborts before
  exceeding it. `--dry-run` prints the plan and estimated cost, calls nothing.
- Results aggregate per `(model, task_type)` into the scorecard, then
  `route compile` regenerates `routing.json`.

Deliberately NOT run unattended: evals cost real money, which is the exact thing
this feature exists to control. It is a one-command sweep the user runs on a new
model release.

## CLI

Host (`pi-stack-host`): `route pick <intent>`, `route compile`, `route show`,
`models`, `evals run|show|ls`.
Launcher (`pi-stack`): `route`, `evals`, `models` (passthrough to the host
binary), and `run --intent <name>` resolves the interactive session model.

## Sandbox integration

`subagents.ts` model resolution order:

1. explicit `model:` frontmatter (back-compat — always wins),
2. `intent:` frontmatter → `routing.json` resolved map → model id,
3. neither → inherit the parent model (current default).

Agent presets migrate from a hard-coded `model:` to an `intent:`. `routing.json`
is baked at `~/.pi/agent/routing.json` next to `capabilities.json`.

## Adding a model later (the whole point)

1. Add one entry to `models.json` (`id`, `provider`, prices, `available:true`).
2. `pi-stack evals run --models <new-id>` to earn its scores (budget-guarded).
3. `pi-stack route compile` and `make load`.

No agent files change. The router reconsiders every intent against the new model
automatically.
