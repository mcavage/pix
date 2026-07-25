# Model routing — cost / latency / accuracy tradeoff selection

**Status:** built (v2). The router replaces hand-pinned model ids on every agent
with a declared *intent* (a hard constraint) that resolves to a concrete model
from a scorecard. Scores are seeded from published benchmarks and pricing and
are **hand-maintained** (edit `scorecard.json` directly); the resolver picks,
nobody toggles models by hand.

**v2 reshape (agent lifecycle).** v1 shipped the engine but read as "a
router." v2 reframes it as what it is for: **agents are first-class objects
you create and manage** (`pi-stack agent ls|new|edit|rm|reassess`), and routing
is the engine that makes each one pick its model. The concrete change from v1:
an `agent` CLI makes the default-with-override behavior legible (`agent ls`
shows each agent's resolved model and why). The resolver, data model, and
`routing.json` contract are unchanged from v1. An earlier revision of this
system ran an automated harness to re-measure scores by calling every candidate
model; it was torn out as a fragile host dependency the router never needed —
the resolver only reads the scorecard, regardless of how its numbers got there.
See the PR/FAQ + arch review in `.pi-agent/plan/agent-lifecycle/` for the full
rationale.

## The problem

Every subagent preset hard-codes a `model:`. Thirteen of eighteen were pinned to
`anthropic/claude-opus-4-8`, so any crew task fired a wall of Opus in parallel.
That is the bill. Worse, the pin is a guess: no one measured whether Opus is
actually better than Sonnet or a local model *for that specific task*, and there
was no way to react to a new model (a new Sonnet, a GPT variant, a Gemini
release, a local `gpt-oss`) except hand-editing eighteen files.

## The posture today

The policy is not "pick the single best model." It is a deliberate **tiered,
multi-vendor crew**, encoded in `policy.json` (see `pi-stack agent ls` for the
live map):

- **Orchestrator** — `overlord` (the top-level interactive session) → OpenAI
  GPT-5.6 Sol. Pinned OFF Anthropic on purpose: Claude is the weak writer, and a
  same-vendor orchestrator shares its authors' blind spots. Opt-in per host via
  the shipped `run_intent` default (`pi-stack config set run_intent <intent>` to
  change it, e.g. `strategy` for Opus on an Anthropic-only host; `none` opts out
  to pi's own default model).
- **Frontier** — `strategy` (`architect`, `product-manager`) and `max-accuracy`
  (`deep`) → Claude Opus 5 (the 2026-07-24 insta-upgrade from Opus 4.8; an Opus-tier
  cost cap keeps the pricier Fable out of both). Fable 5 is no longer a general
  frontier pick — it is reserved for the one role that earns it (security, below).
- **Workhorse (Sonnet 5)** — `code` (`engineer`, `designer`): the production code
  vendor, best value under a per-task cap.
- **Advisors (Opus 5)** — `advisory` (finance, legal, sre, dx-consultant,
  enterprise-admin): high-leverage judgment where a wrong recommendation is
  expensive, so the Anthropic flagship, not the workhorse.
- **Prose (Gemini)** — `writing` (`ux-copywriter`, `devrel`, `growth-marketing`,
  `enrich`) → Gemini 3.6 Flash. Pinned OFF Anthropic because Claude is the weak
  writer; cheapest Google model over a quality floor (bump to Gemini 3.1 Pro for
  higher-stakes prose).
- **Cross-vendor reviewer** — `review` (`review`, `dx-impatient`) → Google
  Gemini 3.1 Pro. A THIRD vendor, distinct from both the OpenAI orchestrator and
  the Anthropic authors it checks, so its blind spots genuinely differ.
- **Security frontier** — `red-team` (`security-lead`) → Claude Fable 5. The one
  role that earns the frontier model regardless of cost: getting a security review
  wrong is the most expensive miss in the crew.
- **Cheap / high-volume** — `breadth` (`fanout`) → Gemini 3.1 Flash-Lite and
  `verify` (`qa-lead`) → Gemini 3.6 Flash (Haiku was the dumb pick for fast QA).
- **Local** — `ollama/qwen3.5:9b` is registered and evaluable: a current Apache-2.0 all-rounder that fits a 16GB machine (~6.6GB), free + private,
  but slower, so it wins nothing by default and serves as an offline fallback.

A crew task fans out across three cloud vendors plus a local option, matched to
the leverage of the role — not a wall of one model. Vendor spread after the
2026-07-24 Opus 5 reshape: **OpenAI** orchestrates (overlord), **Anthropic** does
code/strategy/advisory/security, **Google** does review/writing/verify/breadth. The registry/scorecard are
seeded from LIVE model cards + pricing (see the `model-refresh` skill), not from
training-data guesses; retarget any of it by editing `policy.json`/`scorecard.json`
and re-running `route compile`; no agent files change. `pi-stack agent ls` prints
a WHY for each pick (objective, the winner's accuracy/$/latency, and what it beat
or whether a constraint left a sole fit).

Any agent is measurable on three axes: **cost**, **latency**, **accuracy**. Cost
and latency are cheap to derive from published pricing and model cards.
Accuracy is the hard axis — it comes from published benchmarks and is
hand-entered into the scorecard, not measured by an in-repo harness.

## Shape (mirrors `capabilities.json`)

`capabilities.json` already established the pattern: declare an abstract need,
resolve it in one file, swap the file to retarget everything. The router is the
same idea for models.

```
HOST (Go, tested, owns the truth)            SANDBOX (TS, reads one file)
┌───────────────────────────────┐            ┌──────────────────────────┐
│ models.json    (registry+price)│            │ routing.json (resolved)  │
│ scorecard.json (hand-edited)   │  compile   │  intent -> model + why   │
│ policy.json (intents)          │ ─────────▶ │                          │
│      │                         │            │ subagents.ts reads it:   │
│  Resolve() constrained-opt     │            │  intent: -> model id     │
│      ▲                         │            │  (explicit model: wins)  │
│  hand-edit scorecard.json      │            └──────────────────────────┘
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
  Hand-maintained: seeded from published benchmarks + pricing at launch, and
  hand-edited in `scorecard.json` whenever a model's numbers change or a new
  model is added.
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

## Scoring the scorecard (hand-maintained)

There is no automated measurement harness in this repo. `scorecard.json` is a
plain data file (`services/host/routing/defaults/scorecard.json`) you edit by
hand: one `(model, task_type)` row per line, with accuracy/latency/cost pulled
from published model cards, vendor benchmarks, and pricing pages (see the
`model-refresh` skill, which re-grounds the registry + scorecard against LIVE
model cards rather than training-data guesses). This replaced an earlier harness
that called every candidate model to re-measure scores automatically — useful
in principle, but a fragile external host dependency for a router that only
ever reads the scorecard, never the harness that produced it. After editing
`scorecard.json`, run `pi-stack route compile` to bake the change into
`routing.json`.

## CLI

Host (`pi-stack-host`): `route pick <intent>`, `route compile`, `route show`,
`models`.
Launcher (`pi-stack`): `agent ls|new|edit|rm|reassess`, `route` (passthrough to
the host binary), and `run --intent <name>` resolves the interactive session
model.

## Agent lifecycle

An agent is a first-class object (`agents/<name>.md` frontmatter): identity,
`description` (used for auto-selection), prompt, `tools`, an **`intent`** (not a
pinned model), an optional provider constraint, and an advisory `budget_usd`.

- **`agent ls`** shows each agent's resolved model and WHY (its intent, and
  whether the resolver fell back). This is what makes "sensible default with
  override" legible — you never pick a model per task.
- **`agent new`** scaffolds the md; **`agent new --interactive`** runs the
  `agent-new` skill (powered by the `authoring` intent → Opus) to author the
  prompt conversationally and set the default.
- **`agent edit` / `agent rm`** manage an agent without hand-editing frontmatter.
- **`agent reassess`** re-resolves the roster under the current policy/scorecard
  (zero spend) and recompiles. `--model NEW` no longer measures anything
  automatically — it points you at hand-editing `scorecard.json` and re-running
  without `--model`. Prints the routing diff either way.

## Sandbox integration

`subagents.ts` model resolution order:

1. explicit `model:` frontmatter (back-compat — always wins),
2. `intent:` frontmatter → `routing.json` resolved map → model id,
3. neither → inherit the parent model (current default).

Agent presets migrate from a hard-coded `model:` to an `intent:`. `routing.json`
is baked at `~/.pi/agent/routing.json` next to `capabilities.json`.

## Adding a model later (the whole point)

1. Add one entry to `models.json` (`id`, `provider`, prices, `available:true`).
2. Hand-add its scores to `scorecard.json` (from published benchmarks/model
   cards).
3. `pi-stack route compile` and `make load`.

No agent files change. The router reconsiders every intent against the new model
automatically.

## Hardening notes and known limitations (be honest)

A cross-vendor review shaped these; some are fixed, some are deliberate scope.

**Deliberate design choices**

- **The router never returns "no model."** When no candidate satisfies the hard
  constraints, it dispatches the intent's `fallback` (a cheap/sane model) and
  sets `constraints_met=false` with a reason. A crew task should degrade, not
  fail to launch. The flag is surfaced for auditing; the fallbacks in
  `policy.json` are chosen to be economical.

**Known limitations (future work)**

- The resolver treats a score as a point estimate and does not yet weight by
  sample count (`n`) or freshness (`updated`). A benchmark cited once and one
  cited a hundred times are equally authoritative, and scores never expire. Add
  a min-sample / staleness policy before trusting this at scale.
- Constraints are **averages** (mean cost, p50 latency) pulled from published
  benchmarks, not runtime ceilings. A long multi-turn agent can cost more than
  its benchmark average. The router picks well; it does not enforce a per-run
  budget mid-flight.
- Registry prices and scorecard numbers are user-maintained estimates. Keep
  them current by hand (see `model-refresh`), since nothing in the repo
  re-measures them automatically.
