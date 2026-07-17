# Model routing — cost / latency / accuracy tradeoff selection

**Status:** built (v2). The router replaces hand-pinned model ids on every agent
with a declared *intent* (a hard constraint) that resolves to a concrete model
from measured scores. Evals feed the scores; the resolver picks; nobody toggles
models by hand.

**v2 reshape (agent lifecycle + promptfoo).** v1 shipped the engine but read as
"a router." v2 reframes it as what it is for: **agents are first-class objects
you create, eval, and manage** (`pi-stack agent ls|new|edit|rm|reassess`), and
routing is the engine that makes each one pick its model. Two concrete changes
from v1: (1) evals moved from a hand-rolled Go harness to **promptfoo** (one
legible home in `evals/`), the Go side only *imports* results into the
scorecard; (2) an `agent` CLI makes the default-with-override behavior legible
(`agent ls` shows each agent's resolved model and why). The resolver, data
model, and `routing.json` contract are unchanged from v1. See the PR/FAQ +
arch review in `.pi-agent/plan/agent-lifecycle/` for the full rationale.

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

- **Frontier** — `max-accuracy` (`deep`) → Claude Fable 5 (the one genuinely-hard
  problem, no cap); `strategy` (`architect`, `product-manager`) → Claude Opus 4.8
  (accuracy-critical judgment under an Opus-tier cap).
- **Workhorse (Sonnet 5)** — `code` (`engineer`, `designer`) and `advisory` (the
  specialist crew: sre, dx, finance, legal, devrel, growth, ux, enterprise-admin,
  enrich): strong, cost-effective, kept on the primary production vendor.
- **Cross-vendor adversaries** — `review` → OpenAI GPT-5.6 Sol and `red-team`
  (`security-lead`) → Google Gemini 3.1 Pro. Their job is to independently check
  Claude-authored work, so they are pinned OFF Anthropic (and off each other's
  vendor) via `providers:` allowlists, so their blind spots genuinely differ.
- **Cheap / high-volume** — `breadth` (`fanout`) → Gemini 3.1 Flash-Lite and
  `verify` (`qa-lead`) → Claude Haiku 4.5.
- **Local** — `ollama/gemma4:31b` is registered and evaluable: the current open-weight leader (Apache 2.0), free + private,
  but slower, so it wins nothing by default and serves as an offline fallback.

A crew task fans out across three cloud vendors plus a local option, matched to
the leverage of the role — not a wall of one model. The registry/scorecard are
seeded from LIVE model cards + pricing (see the `model-refresh` skill), not from
training-data guesses; retarget any of it by editing `policy.json`/`scorecard.json`
and re-running `route compile`; no agent files change. `pi-stack agent ls` prints
a WHY for each pick (objective, the winner's accuracy/$/latency, and what it beat
or whether a constraint left a sole fit).

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

## Eval harness (promptfoo)

Evals live in `evals/` as **promptfoo** (the one legible home). The Go side no
longer scores anything; it imports promptfoo's results into the scorecard.

- **`evals/promptfooconfig.yaml`** — the candidate model list (as pi providers)
  and which suites to run.
- **`evals/providers/pi.js`** — a custom provider that invokes each model
  through `pi` (hermetic: `--no-tools --no-context-files --no-session
  --no-extensions`, throwaway cwd, wall-clock timeout). Credentials stay
  proxy-managed; every provider `pi` reaches is evaluable with no extra code.
  This is v1's `piRunner` insight, re-homed.
- **`evals/suites/*.yaml`** — human-readable cases. Each carries
  `metadata.task_type` (the scorecard key) and an assertion promptfoo scores:
  `contains`/`icontains`/`icontains-any`/`regex` (JS regex — NO `(?i)` inline
  flags), an external `javascript` grader under `evals/asserts/` (the mechanical
  build/test grader), or `llm-rubric` (judge). Per-agent suites live under
  `suites/agents/`.
- **`pi-stack evals run`** shells `promptfoo eval`, then imports the results
  (`routing.ImportPromptfoo`) into the scorecard keyed by `(model, task_type)`:
  accuracy = mean promptfoo score, latency = p50, cost = mean. Rows whose model
  invocation errored (`response.error`) are excluded so a transient blip can't
  overwrite a good score; a failed *assertion* is a legitimate 0. `--budget`
  evaluates one model at a time and stops before the cap; `--dry-run` calls
  nothing; `--save` writes the scorecard; then `route compile`.
- **`pi-stack evals import <results.json>`** folds an existing promptfoo run in.

Deliberately NOT run unattended: evals cost real money, which is the exact thing
this feature exists to control. It is a one-command sweep the user runs on a new
model release. The importer's schema is pinned by a real `results.json` fixture
(`services/host/routing/testdata/`).

**Scope + prerequisites.** The `evals` and `agent` commands are **repo-rooted**
(run from the repo so `evals/` and `agents/` resolve; override with
`$EVALS_CONFIG` / `$PI_STACK_AGENTS_DIR`) and require **promptfoo on the host**
(`npm i -g promptfoo`). They are maintainer/power-user tools, not part of the
repo-less consumer install. What the eval measures is a **model's capability at
a `task_type`** (which sets an agent's default model), not the exact wording of
one agent's prompt. Every requested model must have a `providers:` entry in
`promptfooconfig.yaml`, or the run errors rather than silently scoring nothing.

## CLI

Host (`pi-stack-host`): `route pick <intent>`, `route compile`, `route show`,
`models`, `evals run|import|show|ls`.
Launcher (`pi-stack`): `agent ls|new|edit|rm|reassess`, `route`, `evals`
(passthrough to the host binary), and `run --intent <name>` resolves the
interactive session model.

## Agent lifecycle

An agent is a first-class object (`agents/<name>.md` frontmatter): identity,
`description` (used for auto-selection), prompt, `tools`, an **`intent`** (not a
pinned model), an optional provider constraint, an advisory `budget_usd`, and an
eval suite under `evals/suites/agents/`.

- **`agent ls`** shows each agent's resolved model and WHY (its intent, and
  whether the resolver fell back). This is what makes "sensible default with
  override" legible — you never pick a model per task.
- **`agent new`** scaffolds the md + a starter suite; **`agent new
  --interactive`** runs the `agent-new` skill (powered by the `authoring` intent
  → Opus) to author the prompt + real eval cases conversationally, run them, show
  the tradeoff table, and set the default.
- **`agent edit` / `agent rm`** manage an agent without hand-editing frontmatter.
- **`agent reassess`** re-levels the roster: `--model NEW` evaluates the new
  model across every suite and recompiles; without it, re-resolves under the
  current policy (zero spend — the new-user-budget flow). Prints the routing diff.

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

## Hardening notes and known limitations (be honest)

A cross-vendor review shaped these; some are fixed, some are deliberate scope.

**Fixed / enforced**

- **The evaluated model can't touch the host.** The pi provider runs each model
  with `--no-tools --no-context-files --no-session --no-extensions --no-skills
  --no-prompt-templates --no-themes` in a throwaway cwd, with a wall-clock
  timeout (pi has no read timeout) and a bounded output buffer. The DEFAULT suite
  uses only non-executing assertions (contains/regex/llm-rubric). The mechanical
  exec grader (`evals/asserts/`) is OPT-IN, wired into no default suite, and
  documented as unsafe for untrusted output (see below).
- **Spend is bounded and honest.** `--budget` is a per-model cap: `evals run`
  evaluates one model at a time and does not START a new model once spend has hit
  the cap (the in-flight model's matrix runs whole, so the cap is advisory at
  model granularity). Cost comes from pi's own `usage.cost.total` (cache-aware),
  not registry guesses. Invocation errors (`response.error`) are excluded from
  aggregates, so a transient outage can't overwrite a good score with a spurious
  0. A requested model with no `providers:` entry errors before spending.

**Deliberate design choices**

- **The router never returns "no model."** When no candidate satisfies the hard
  constraints, it dispatches the intent's `fallback` (a cheap/sane model) and
  sets `constraints_met=false` with a reason. A crew task should degrade, not
  fail to launch. The flag is surfaced for auditing; the fallbacks in
  `policy.json` are chosen to be economical.
- **The opt-in mechanical (exec) grader executes model output on the host** with
  your full environment. It is meant for TRUSTED graders (build/test/lint) and is
  deliberately NOT in any default suite. Do not point one at untrusted model
  output without wrapping it in a disposable container/VM (no network, no
  secrets, unprivileged, read-only workspace). The default suite avoids execution
  entirely.

**Known limitations (future work)**

- The resolver treats a score as a point estimate and does not yet weight by
  sample count (`n`) or freshness (`updated`). A one-sample eval and a
  hundred-sample eval are equally authoritative, and scores never expire. Add a
  min-sample / staleness policy before trusting this at scale.
- Constraints are historical **averages** (mean cost, p50 latency), not runtime
  ceilings. A long multi-turn agent can cost more than its short eval average.
  The router picks well; it does not enforce a per-run budget mid-flight.
- Registry prices are user-maintained estimates. Keep them current, or rely on
  the eval-measured `cost_usd` (which uses pi's reported cost).
