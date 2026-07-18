---
name: model-refresh
description: 'Refresh the model router (registry + scorecard + policy) from LIVE model cards and pricing, not training data. Use when a new model ships, when `pi-stack route show` / `agent ls` names stale or wrong models, when someone says "the models are ancient", "re-level the crew", "update the model registry", "which models should we use", or when onboarding the stack to a new provider lineup.'
---
# model-refresh

Keep the crew on the **current** state-of-the-art. The single biggest failure
mode here is grounding in TRAINING DATA: model names, versions, and prices drift
fast, and the model you "remember" is usually a version or two behind and priced
wrong. **Always pull live data first.** If you skip step 1, you are guessing.

The router has three source-of-truth files in `services/host/routing/defaults/`
(override at `~/.pi-stack/routing/`):

- `models.json` — the registry: every callable model + real `provider/id` + real
  `$/Mtok` in/out. Adding a model is one entry.
- `scorecard.json` — per-`(model, task_type)` accuracy / cost / latency priors.
- `policy.json` — the intents (the tiering + cross-vendor posture). Usually stable.

`route compile` resolves every intent against these and writes `routing.json`,
which the sandbox reads offline. You are re-grounding the first two, and only
touching `policy.json` if the price points moved enough to break a cost cap.

## 1. Pull LIVE data (do not skip)

Get the current lineup, pricing, and per-task benchmarks for each vendor the
stack uses (Anthropic, OpenAI, Google) plus the local option. Use `web_search`
with the real date, and prefer authoritative sources:

- **Pricing:** each vendor's official API pricing page, or an aggregator that
  syncs from it (note the sync date). Record exact `$/Mtok` input and output per
  model and the exact API model string (`provider/id`).
- **Local (Ollama) models are their own refresh.** Do NOT trust a remembered tag:
  check the live Ollama library / a current catalog for the exact tag, the
  release date, and the download size, then pick by CURRENT generation, not fame.
  (Example failure: `gpt-oss:20b` shipped 2025-08 and was overtaken by the Gemma 4
  and Qwen 3.5 generations within months.) The router's local model is a slow,
  free, opt-in fallback, so weight it toward a genuinely capable current model
  and give it a high latency so it never wins latency-sensitive intents by
  accident. This is SEPARATE from the memory-watcher model, which defaults to the
  SAME local model as the bridge (`qwen3.5:9b`) so Ollama keeps one model resident
  for both capture and inference (override to something smaller on a tight box).
- **Per-task capability (for the scorecard):** published benchmark numbers, one
  consistent benchmark per task_type:
  - `code` <- SWE-bench (Verified or Pro; pick one and stay consistent)
  - `reasoning` <- GPQA Diamond / Humanity's Last Exam (with tools) / AIME
  - `search` and `qa` <- interpolate from the agentic / grounding / instruction
    results, or a task-appropriate benchmark if one exists.

Capture the date and sources; you will cite them in the file `_note` and the
scorecard `source`.

## 2. Rewrite `models.json`

One entry per model you want routable. Rules:

- `id` MUST be the exact, fully-qualified API string (`anthropic/claude-sonnet-5`,
  not a guess like `anthropic/sonnet-5`). A wrong id fails at spawn, not at
  compile. `pi-stack agent ls` flags a pin that is not in the registry.
- Prices are the real list `$/Mtok`. Keep them current — `cost_usd` in the
  scorecard is a hand-computed per-task estimate (tokens x price), so a stale
  price silently poisons every cost-objective route.
- `local: true` for Ollama/free; `available: false` to hide a model without
  deleting it.
- Cover the tiers you actually route to: a frontier model, a workhorse, a cheap
  model, at least one cross-vendor model per adversarial role, and a local option.

## 3. Rewrite `scorecard.json`

One row per `(model, task_type)` for every model x {code, reasoning, search, qa}.

- `accuracy` (0..1): normalize the published benchmark for that task_type. Tag
  `source: "card"` (seeded from a model card / benchmark). There is no
  automated harness that later measures a real `source: "eval"` row —
  scores are hand-maintained; if you later have real usage data for a
  `(model, task_type)` pair, hand-edit the row and set `source: "eval"`
  yourself.
- `cost_usd`: a representative per-task estimate at list price. Use a fixed task
  shape so models are comparable (e.g. ~20k input + 6k output ->
  `0.02*in$ + 0.006*out$`).
- `latency_ms_p50`: a rough estimate (frontier/big = slower, flash/lite = fast,
  local = slowest).
- Keep the `_note` honest about what is measured vs interpolated vs estimated.

## 4. Re-check `policy.json` (only if needed)

The intents encode the POSTURE, not the models, so they usually do not change:
tier by leverage (frontier -> the one hard problem; workhorse -> code + advice;
cheap -> volume) and keep the adversarial roles CROSS-VENDOR via `providers`
allowlists (review off the author's vendor, red-team off a third). But the
`max_cost_usd` caps are absolute dollars, so if a tier re-priced, adjust the cap
so the intended tier still wins (e.g. a "workhorse under $X" cap must sit above
the workhorse's new per-task cost and below the next tier's). Do not pin models
in agents; let the intent resolve.

## 5. Compile and VERIFY

```bash
cd <repo>
pi-stack route compile --out routing.json     # or pi-stack-host route compile --out routing.json
pi-stack route show                            # registry + resolved intents
pi-stack agent ls                              # each agent's resolved model + WHY
```

Read the output like a reviewer, do not just run it:

- Is the crew **diverse** across tiers and vendors, or did everything collapse
  onto one model? A monoculture means a cap or the scorecard is off.
- Does each WHY make sense (`beat X` = a real contest; `sole fit` = a constraint
  chose it, which is intended for the vendor-locked workhorse tier)?
- Do the adversarial roles (`review`, `red-team`) land on DIFFERENT vendors from
  the author and each other?

There is no eval harness to keep in sync — `scorecard.json` is the single
source of truth and it is hand-maintained. If a card price/benchmark changes
later, come back and hand-edit the row, then re-run `pi-stack route compile`.

## 6. Ship

Config + baked-file changes reach new sandboxes only after `make load` on a DHI
host. Update `CHANGELOG.md`, and if the number/naming of models or intents
changed, the model mentions in `AGENTS.md` and `docs/design/routing.md`.

## Anti-patterns

- Naming a model from memory instead of a live source. It will be stale or
  mis-priced.
- A guessed or truncated model id (`sonnet-5` vs `claude-sonnet-5`). Verify
  against the vendor's current API model string.
- "Fixing" a registry inconsistency by downgrading to an older model to match
  other files. Upgrade the stale reference to the current model instead.
- Pinning `model:` in an agent to force a choice. Encode the intent + policy so it
  stays legible and re-levels automatically on the next refresh.
- Collapsing the crew onto one vendor. Keep the adversarial roles cross-vendor on
  purpose; that is the whole point of a multi-model crew.
