# Onboarding v2 — product spec (FOR REVIEW, rev 2)

Status: DRAFT for owner review. Not implemented. Supersedes the in-session
identity-Q&A flow in `skills/onboarding/SKILL.md` and extends (does not replace)
the trust-plane architecture in `docs/design/onboarding.md`.

Produced with the crew (product-manager, dx-consultant, designer, devrel) and a
new adversarial reviewer, **dx-impatient** ("the impatient developer", holds the
Stripe bar), plus owner direction. Rev 2 folds in the owner's comments and the
impatient reviewer's verdict, which was FRICTION bordering BAIL on rev 1.

## 1. Problem

First-run onboarding asks generic identity Q&A (tone, peeves, values). It is
ceremony that spends the highest-intent minute on feelings, not leverage. Worse,
the fenced agent narrates host state it cannot see (it told the owner "no
knowledge base" when one was configured). The owner wants onboarding to get out
of the way and deliver value fast, teaching by doing, leaving real artifacts
only when the moment is real.

## 2. The bar (rev 2, corrected)

Rev 1 targeted "5-8 min to first task" and metered "artifacts per onboarding".
Both were wrong — they reward a guided march. The bar is the **Stripe bar:
under 60 seconds from `pi-stack setup` to a real, useful result.**

- North star: **time-to-first-real-verb** (target <60s) and **did the user run a
  second command unprompted** (retention).
- Explicitly NOT: artifact count. One real task done and a happy bail is a
  success, not a failure.

## 3. The load-bearing inversion (highest-leverage fix)

**Land in a real task FIRST; everything else is surfaced inside or after it, or
not at all.** The moment the keys gate passes, drop the user into a context-picked
verb (`healthcheck` on this repo is the safe default). Identity, memory, the one
relevant track, and the autonomy dial appear at the moment they become real, or
never. This is the single change that clears most of the bar.

Consequences (what rev 1 got wrong, now cut):
- No opening "what pi-stack is" paragraph. Nobody reads it. Teach by doing.
- No up-front track menu. A menu of abstract nouns punts the design onto the
  user. Offer AT MOST ONE context-picked track, after the aha.
- No identity form up front. Derive name/email from git/gh; let the watcher learn
  tone/preferences from the real work; ask a single field inline only if a task
  actually needs it.
- No printed host-state block. The truth file is good plumbing (below) but the
  user wants to work, not read a status readout. Surface a host problem only when
  it blocks, and only inside an error.
- No autonomy-dial teaching moment. Default `review` silently; surface it the
  first time the user says "just build it."
- No crew tour as a step. Point the crew out retroactively after a fan-out
  actually happens.

## 4. Principles

1. **Keys are a hard gate; the rest is invisible until it blocks.** No model key
   = no agent = no onboarding. Fix printed in the blocking error (see §7).
2. **Host owns config; the agent never guesses it.** Host writes a truth file
   (§6); the agent states facts from it, never probes.
3. **Task before teaching.** §3.
4. **At most one track, context-picked, after the aha.** Never a menu.
5. **Confirm before every write** (memory, skill file, `onboarding.json`).
6. **Defer is cheaper than decline** (`later` vs `skip`, distinct).
7. **Resume, don't restart** — and **a pre-provisioned env is not onboarded at
   all** (§9).
8. **Never demo what will fail** — enforced against MCP specifically (§8).

## 5. Host secrets and keys (owner comment 1)

Today the host phase only checks that sbx has secrets set and moves on. That is
wrong: it can pass while pi-stack has no usable key, so the first task 401s and
the tool has taught the user it lies (the impatient reviewer's unforgivable
finding).

Decision (owner): **1Password holds the secret VALUES; pi-stack owns the
`op://` REFERENCES, never the secrets.** At launch pi-stack resolves the refs via
`op` and injects them; values never touch pi-stack's config or the VM.

- This is NOT new infra — it is the exact pattern the repo already uses for MCP
  credentials: `config/op-refs.env` + `op run --env-file=... -- ...`, managed by
  `pi-stack secret status|edit|check`. v2 EXTENDS that same mechanism to the
  provider keys (anthropic/openai/google), so there is one credential model for
  everything, and 1Password stays the single store.
- The keys gate resolves the refs (can `op` read them?) instead of checking
  "does sbx have some secret". This closes the false-pass -> post-gate 401 hole.
  On a miss, the blocking error points at `pi-stack secret edit` with the missing
  ref named. "Check sbx has secrets and move on" is deleted.
- 1Password is the first provider; the ref-resolution seam stays pluggable for a
  future secrets backend. Injection target (proxy vs sbx secret vs direct) is an
  implementation detail behind the ref model, not a user-facing choice.

## 6. Host->agent truth file

Host writes `<workspace>/.pi-stack/host-state.json` at launch (sibling of the
existing `profile`, `ollama-bridge.model`, `knowledge.scope` files). The agent
READS it and states facts; it never probes host config. It is NOT printed as an
onboarding block (§3) — it feeds the agent and gates the short-circuit (§9).

```json
{
  "provisioned": false,
  "keys":      { "resolved": true, "source": "1password" },
  "memory":    { "up": true, "port": 11435 },
  "knowledge": { "bundles": ["/path/acme-kb"], "seeded": true, "service_up": true },
  "gog":       { "enabled": false, "account": "" },
  "mcp":       { "enabled": false, "servers": [] },
  "overlay":   { "kit": "acme", "skills": 7, "tools": ["snow"] },
  "models":    { "watcher": "osmosis-structure:0.6b", "embed": "nomic-embed-text" }
}
```

Net-new host work, on the critical path. Generalizable: any skill can read it.

## 7. First-run journey (rev 2)

`pi-stack setup` -> host phase (non-interactive: resolve keys via §5, ensure
memory, opt-in MCP off by default, write the truth file) -> launch. Then:

1. **If provisioned (§9): one line + first task. Done.** No onboarding.
2. **Keys unresolved:** the launch is blocked BEFORE the session with the exact
   fix command. No silent 401 later.
3. **Otherwise:** the agent opens by DOING — a context-picked real verb
   (`healthcheck` default, or `code-review` if a diff exists, `debug` if
   something's broken). No paragraph, no menu, no form first.
4. **Inside / right after that task**, and only if real:
   - memory recall demonstrated (state a fact, act on it next turn);
   - ONE context-picked track: KB-seed if the repo has capture-worthy docs
     (propose 3-5 pre-judged candidates, one Enter to seed via `enrich`, skip if
     already seeded); OR a custom skill if a gap actually surfaced;
   - identity captured passively (git/gh + watcher), a single inline ask only if
     needed.
5. **Autonomy dial:** default `review` silently; surfaced on first "just build
   it".
6. **MCP / proxies:** never in first-run; a one-line default-no offer to explain
   the "why" later, naming `gworkspace` + `pi-stack doctor`.
7. **Close:** a one-line receipt of anything written, then straight into work.

## 8. MCP opt-in (owner comment 2)

Today MCP (the sbx gateway) errors every startup on non-Docker-employee laptops.
Fix: **MCP is opt-in, default OFF.**

- When MCP is off: no `--mcp`, no gateway registration, no gateway-connect
  attempt, and NO startup error. The truth file carries `mcp.enabled=false`,
  `servers=[]`.
- Enable explicitly (`pi-stack config set mcp.enabled true` or a `setup --mcp`
  flag). Only then does pi-stack wire the gateway + servers.
- §4-P8 ("never demo what fails") is enforced here specifically: a fresh install
  never shows an MCP error during the aha.

## 9. Overlay kits + pre-provisioned short-circuit (owner comment 3)

An overlay kit can ship a fully-configured environment: skills, MCP servers, and
arbitrary command-proxy tools (e.g. `snow`). A user who inherits a complete kit
must NOT be re-onboarded from scratch (the impatient reviewer's overlay-user
BAIL).

- The kit (or the truth file's `overlay` + `provisioned` fields) marks the env as
  provisioned: keys resolved, KB seeded, skills present, tools wired.
- **Provisioned => onboarding collapses to one line + first task.** No menu, no
  identity ask, no KB-seed offer for an already-seeded bundle. The overlay owner
  already paid the setup cost.
- Onboarding must treat overlay-provided skills/MCP/tools as first-class present
  (read from the truth file), not re-propose them.
- Spec dependency: define how a kit declares "provisioned" and how the host
  reflects overlay-shipped skills/tools into the truth file.

## 10. Success metrics (rev 2)

- North star: time-to-first-real-verb (<60s); second-command-unprompted rate
  (retention).
- Guardrails: keys-gate false-pass rate = 0 (no post-gate 401s); zero MCP startup
  errors on a fresh non-Docker laptop; provisioned users see 0 onboarding
  prompts.

## 11. Ollama models for RAG/memory (July 2026)

Ollama is RAG/memory only here (no coding). Optimize cold-start + predictable
latency, and use Ollama's native structured outputs (v0.32+, JSON-schema
constrained) to make the watcher reliable regardless of model.

- **Watcher (fact extraction -> structured facts):** replace `qwen3.5:9b` (the
  90s-timeout cause; 9b cold-load). Recommend a purpose-built tiny extraction
  model:
  - **`gemma4:e4b-mlx`** — DEFAULT on Apple Silicon. Gemma 4 (effective 4B) on
    the MLX backend: fast/lean on a Mac, and a general 4B extracts higher-quality
    facts than a tiny specialist (bad facts pollute memory). Far lighter than the
    9b that caused the 90s timeout.
  - **`smolstruct:1.7b`** or **`osmosis-structure:0.6b`** — minimal-RAM /
    non-Apple-Silicon fallbacks, or if gemma4 extraction proves flaky.
  - Pair the chosen model with Ollama JSON-schema structured output +
    warm-on-start + a longer first-call budget.
- **Embeddings:** keep **`nomic-embed-text`** (still the practical CPU default).
  Optional quality bump: `qwen3-embedding:0.6b` or `bge-m3` if recall quality
  bugs us. No urgent change.

## 12. Decisions (owner)

- **Q1 (where authored skills live): SUPERSEDED by packs.** Skills live in the
  active pack's `skills/`; the personal-pack root is the
  `~/.local/share/pi-stack/skills` path, now a git repo (`git init` + first
  commit, guarded if `user.email` is unset). Team-shared skills live in a work
  pack. See `docs/design/packs.md`.
- **Q2 (default autonomy mode): `review`.**
- **Q3 (guided path skippable): yes** — and rev 2 makes it <60s to value, not
  5-8 min.
- **Q4 (build truth file first): yes.**
- **Q5 (key model): DECIDED — 1Password holds values, pi-stack owns the `op://`
  references** (extend the existing `op-refs.env` / `pi-stack secret` mechanism to
  provider keys). See §5.

## 13. Net-new work this depends on

1. `host-state.json` writer (host) + reader (skill). Critical path.
2. DONE. `pi-stack secret sync` resolves provider-key `op://` refs -> sbx
   secrets (sandbox proxy store); host mode already resolves them via `op run
   --env-file`. Keys gate points at `pi-stack secret sync`; setup runs it
   best-effort; host-state reports `keys.source`. Live `op` run is a host test
   (op is a host tool; the sandbox can't reach 1Password).
3. MCP opt-in (default off, no startup error) (§8).
4. Overlay "provisioned" marker + reflecting overlay skills/tools into the truth
   file (§9).
5. Personal skills dir `~/.local/share/pi-stack/skills` wiring (Q1).
6. Watcher model default -> `gemma4:e4b-mlx` (Apple Silicon) + warm-on-start +
   longer cold budget + Ollama structured outputs.

## 14. Crew note

Added **`agents/dx-impatient.md`** ("the impatient developer") to the roster: an
adversarial DX reviewer that holds any developer-facing surface to the Stripe bar
(60-second time-to-value, defaults over questions, no walls of text, nothing that
breaks on first contact). Its rev-1 review drove this revision. Reusable on any
CLI/flow/error/doc going forward.
