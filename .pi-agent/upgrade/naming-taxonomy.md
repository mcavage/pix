# Skill Naming Taxonomy

**Scope:** all 35 skills in `skills/*/SKILL.md`  
**Status:** proposal — renames need cross-ref updates (see Migration Cost)

---

## Convention

**Name for the user's verb, not the implementation.**

When someone picks a skill they say something to themselves: "I want to plan this", "I need to debug this", "let me ship it." The skill name should be that thing. Not `investigate` (what the agent does) but `debug` (what the user says). Not `wf-engineering` (an internal category tag) but `build-full` (what it does, qualified to distinguish it from `build`).

**Grouping rule:** use a shared prefix only when three or more related skills need to be distinguished from each other. A prefix buys you autocomplete clustering and a readable `/help` list. Use it for the `build-*` family (three skills at different scales) and nowhere else. Do not use `wf-`, `document-`, or other internal-jargon prefixes.

**No-prefix skills** should be the most-reached-for ones. If you type five characters and stop, you should be there: `plan`, `build`, `debug`, `ship`, `review`, `check`.

**Convention-reference skills** (api-conventions, git-conventions, etc.) auto-load; users rarely invoke them by name. They can keep their current compound form. The `-conventions` suffix is a useful signal that they are reference rules, not actions.

**The discoverability tool must itself be discoverable:** `check`, `setup`, `improve` — not `self-audit`, `setup-user`, `promote-learnings`.

---

## Overlap Resolution

Three contested pairs. Decision first, rationale after.

### autoplan vs wf-product → DIFFERENTIATE as `plan` and `product`

These are different in depth and agency:

- `plan` (was `autoplan`): one command, rough idea in, sequential review phases, auto-decisions at every gate, final approval from the user. Single agent. Under an hour. Think "quick sanity check before I start coding."
- `product` (was `wf-product`): full multi-role product process. Discovery, PR/FAQ, PRD, design review, architecture review, peer review, eng handoff. Subagents for product-manager, designer, architect. User approves at the PR/FAQ before the spec deepens. Think "we're building something real and need to think it through properly."

The name `autoplan` confused people because (a) "auto" implies the planning happens automatically, not that the gates are auto-approved, and (b) it sat next to `spec` and `wf-product` with no clear line between them. `plan` vs `product` is immediately readable: plan is quick, product is the real thing.

### spec vs wf-engineering → DIFFERENTIATE as `build` and `build-full`

- `build` (was `spec`): spec-first single-agent build loop. Turn a feature request into a PRD and lightweight architecture, shard it into self-contained stories, execute story-by-story. For a feature or change where one agent running sequentially is enough.
- `build-full` (was `wf-engineering`): full multi-role pipeline. Architect spec, impl plan, parallel execution across worktrees, cross-vendor code review, QA and security concurrent, verification gate, ship. For non-trivial work that needs the whole crew.

`spec` was misleading because it does both spec AND build. Users landing on it wanted "build this" and found "spec this" — accurate but not what they asked. `build` is what they say. The `build-full` qualifier signals it is the heavier version without needing to decode `wf-`.

### wf-prototype vs prototype-webapp → MERGE into `prototype`

These are the same job. `wf-prototype` is the orchestration wrapper (spec-lite, design+build, review, under 45 min). `prototype-webapp` is the how-to for building the web artifact. A user who wants a prototype does not choose between them; they type "prototype this." One skill, named `prototype`, absorbs both. Keep `wf-prototype`'s orchestration and time budget; keep `prototype-webapp`'s build guidance. The merged skill should note it is not for production code and should direct heavier work to `build-full`.

---

## Full Rename Table

### Dev Workflow

| Current | Proposed | Action | Rationale / Description update |
|---------|----------|--------|---------------------------------|
| `brainstorm` | `brainstorm` | keep | Clear verb. Description is fine. |
| `challenge` | `challenge` | keep | Clear verb. "Poke holes, force alternatives, pre-mortem before any major decision." |
| `autoplan` | `plan` | rename | "Rough idea → reviewed plan, hands-free. Auto-gates through idea shaping, scope, arch, and product review. Final approval from you." |
| `wf-product` | `product` | rename | "Full multi-role product process: discovery, PR/FAQ, PRD, design, arch review, peer review, eng handoff. User gate at the PR/FAQ." |
| `spec` | `build` | rename | "Spec-first build loop for a feature. Writes the plan, shards it into stories, executes them. Single agent, sequential." |
| `wf-engineering` | `build-full` | rename | "Full multi-role build pipeline: architect + engineer + QA + reviewer, parallel worktrees, verification gate, ship. For non-trivial work." |
| `wf-prototype` | `prototype` | rename + merge | "Working thing fast, under 45 min. Spec-lite, design+build, review. Not for production; use build-full for that." |
| `prototype-webapp` | (merge into `prototype`) | merge | Redundant with wf-prototype. Absorbed. |
| `tdd` | `tdd` | keep | TDD is the term. Description is correct. |
| `investigate` | `debug` | rename | "Root-cause-first debugging. Reproduce, investigate, form a falsifiable hypothesis, verify it, then fix." Users say "debug this", not "investigate this". |
| `code-review` | `review` | rename | "Review the diff for correctness and safety, then get a cross-vendor second opinion." Users say "review this" or "review my code". |
| `review-gate` | `peer-review` | rename | "Cross-model peer review for docs, plans, decisions — not code diffs. Use before sending anything important." Distinguishes from review (code diffs). |
| `verify` | `verify` | keep | Clear. "Prove a claim before you make it." |
| `health` | `health` | keep | Clear. "Code-quality dashboard." |
| `ship` | `ship` | keep | Clear verb. |
| `qa` | `qa` | keep | Industry-standard abbreviation. |
| `design-review` | `design-review` | keep | Clear compound verb. |
| `document-release` | `docs-sync` | rename | "After code ships, compare the release diff to the doc set, draft updates, batch for approval." Users say "sync the docs" not "document the release". |

### Writing

| Current | Proposed | Action | Rationale / Description update |
|---------|----------|--------|---------------------------------|
| `anti-slop` | `anti-slop` | keep | Memorable. Does what it says. |
| `write-like-mark` | `write-like-mark` | keep | Persona skill. Name is intentionally specific. |
| `write-one-pager` | `one-pager` | rename | "Write an executive one-pager that drives a specific ask: approval, budget, priority call." The `write-` prefix is redundant for an action skill. |
| `microcopy-patterns` | `microcopy` | rename | "UI copy patterns for buttons, errors, empty states, confirmations, tooltips, form labels." The `-patterns` suffix is noise. |
| `docs-standards` | `docs-standards` | keep | Convention reference that auto-loads. Rarely invoked by name. |

### Knowledge and System

| Current | Proposed | Action | Rationale / Description update |
|---------|----------|--------|---------------------------------|
| `ingest` | `ingest` | keep | Clear verb. |
| `promote-learnings` | `improve` | rename | "Review patterns the memory watcher has captured and graduate the recurring ones into skills or conventions." Users ask "what can we improve" or "let's improve the system" — not "promote learnings". |
| `setup-user` | `setup` | rename | "Guided onboarding: seeds identity and context into memory, probes connected tools, gets the agent doing useful work." The `-user` suffix is redundant. |
| `self-audit` | `check` | rename | "Is everything working? Confirms keys, the memory service, the full agent roster, and all skills load and respond." Users say "check if things are working". `self-audit` sounds like an accounting exercise. |
| `competitive-analysis` | `competitive` | rename | "Structured competitive brief on a named competitor or category: landscape, gaps, positioning." Shorter, still unambiguous. |

### Reference / Convention (auto-loading)

These skills auto-load from context. Users rarely invoke them directly. Keep existing names.

| Current | Proposed | Action | Rationale |
|---------|----------|--------|----|
| `conventions` | `conventions` | keep | |
| `api-conventions` | `api-conventions` | keep | |
| `git-conventions` | `git-conventions` | keep | |
| `design-system` | `design-system` | keep | |
| `capability-routing` | `capability-routing` | keep | Internal plumbing. |
| `delegation-guide` | `delegation-guide` | keep | Internal plumbing. |
| `guard` | `guard` | keep | Clear. |

---

## Migration Cost

A rename touches:

1. **The skill directory itself:** `skills/<old-name>/` becomes `skills/<new-name>/`.
2. **Frontmatter `name` field** inside `SKILL.md` — must match the directory.
3. **Cross-references inside other SKILL.md files:** `wf-engineering` is referenced three times inside `wf-prototype/SKILL.md` (lines 13, 84, 91, 94). `self-audit` is referenced in `setup-user/SKILL.md` (lines 22, 103). `design-review` is referenced in `microcopy-patterns/SKILL.md`. Grep for each old name across `skills/**/*.md` before cutting.
4. **`AGENTS.md`:** the skills table section lists `ship · code-review · investigate · spec · qa · design-review · tdd · verify` (line 20) and the "dev spine" entry references `spec`. The `wf-*` prefix is called out in `conventions/SKILL.md` line 26 ("the wf-* skills").
5. **`README.md`:** mentions `code-review`, `investigate`, `spec`, `qa`, `design-review`, `capability-routing` by name (lines 114-118, 135-136).
6. **`prompts/*.md`:** three prompt templates — check for skill name references.
7. **`docs/design/self-learning-loop.md`:** references `self-audit` and `setup-user` by name.

Do not do renames piecemeal. Grep every old name first, then rename the directory and update every hit in one commit per skill. The merge of `prototype-webapp` into `prototype` also needs the merged SKILL.md content reviewed before the old directory is deleted.

---

## `/help` — Live Capability Map

```
Skills — type /skill:<name> or just describe what you need

BUILD
  plan          Rough idea → reviewed plan, no interruptions
  product       Idea → eng-ready spec, full product process (PR/FAQ, PRD, design, arch)
  build         Spec-and-build loop for a feature: plan it, shard it, execute it
  build-full    Full multi-role pipeline: architect + engineer + QA + reviewer, parallel
  prototype     Working thing fast — under 45 min, not for production

DEVELOP
  debug         Root-cause-first. Find it, prove it, then fix it
  tdd           Write the failing test first. Watch it fail. Then write the code
  review        Review the diff — correctness, safety, cross-vendor second opinion
  peer-review   Cross-model review for docs, plans, decisions (not code diffs)
  verify        No "done" without proof. Runs the command, reads the output
  ship          Tests + code-review + PR. Done to open in one command

QUALITY
  qa            Browser-based QA of a running app, screenshot evidence
  design-review Visual audit: hierarchy, spacing, type, consistency. Score + fixes
  health        Code-quality dashboard — scores categories, ranks fixes by impact

WRITE
  brainstorm        Shape a fuzzy idea into something buildable, no pitch theater
  challenge         Poke holes, force alternatives, pre-mortem before you commit
  competitive       Structured competitive brief: landscape, gaps, positioning
  one-pager         Executive one-pager that drives a specific ask
  microcopy         UI copy patterns: buttons, errors, empty states, confirmations
  docs-sync         Sync docs to a release — compare diff, draft updates, batch
  anti-slop         Kill AI tells: banned words, em-dashes, filler, passive voice
  write-like-mark   Mark's voice — direct, concrete, no slop
  docs-standards    Documentation standards for tutorials, APIs, READMEs (auto)

SYSTEM
  setup    First-time onboarding: identity, memory, connected tools
  check    Is everything working? Keys, extensions, agents, skills
  ingest   Read a doc, extract facts, store them with source tracking
  improve  Graduate memory patterns into skills or conventions

REFERENCE (auto-load on relevant tasks)
  conventions        Working-dir layout, scratch rules, output formats
  api-conventions    REST API design and review patterns
  git-conventions    Git discipline, branches, worktrees, gh CLI
  design-system      React/Tailwind/shadcn component conventions
  capability-routing Map an abstract capability → concrete provider
  delegation-guide   Context-passing rules for subagent orchestration
  guard              Guardrails for destructive commands and edit-scope locks
```

---

## `/getting-started` — First-Run Tutorial Script

```
Welcome to pi-stack. Here are five things to try.

─────────────────────────────────────────────────────
1. Set yourself up

  /skill:setup

  Takes about 5 minutes. Seeds your identity and working context into memory
  so the agent knows who you are, what you're building, and which tools are
  connected. Do this once. After that, it just knows.

─────────────────────────────────────────────────────
2. Build something

  "Build me a login page with email + password, using our existing auth service"
  /skill:build

  This is the main loop: spec it, shard it into stories, execute them. For
  bigger work with parallel worktrees and a full review crew, use /skill:build-full.
  For a quick prototype to see what something looks like: /skill:prototype.

─────────────────────────────────────────────────────
3. Debug something broken

  "Why is the auth callback returning 401 on the second request?"
  /skill:debug

  Reproduce first, form a hypothesis, verify it, then fix. Never patches a
  symptom without a confirmed root cause. If you've been chasing a bug in
  circles, start here.

─────────────────────────────────────────────────────
4. Ship it

  /skill:ship

  Runs tests, code-reviews the diff (with a cross-vendor second opinion),
  bumps the version, writes the changelog entry, opens the PR. You approve
  the PR; pi does not merge.

─────────────────────────────────────────────────────
5. Check that everything's working

  /skill:check

  Confirms keys, the memory service, extensions, the full agent roster, and
  all skills respond. Run this after a config change or when something feels
  off. Takes about 30 seconds.

─────────────────────────────────────────────────────

A few other things worth knowing right away:

The agent has a memory that persists across sessions. It captures useful things
automatically. You can also tell it to remember something explicitly:
  /remember we deploy to fly.io, not Railway

Type /model to see what model you're on and switch. Alt+P cycles through them.
The cheaper models (haiku, flash) are fine for most tasks; save the heavy ones
(opus, gpt-5) for genuinely hard problems.

Type /help to see all skills grouped by what they do.
```

---

*Voice call: Mark's — direct, concrete, no em-dashes, no AI slop. Numbers where possible. Opinions stated, not hedged.*
