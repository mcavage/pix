# Proposal: pi-stack + OKF (Open Knowledge Format)

**Status:** proposal, for a decision.
**Author:** product-manager subagent.
**Scope:** how pi-stack should adopt OKF, at full scope.

## Recommendation in one line

**Augment, do not replace.** Memory and OKF do two different jobs. Keep the
self-learning memory loop exactly as it is (personal, ephemeral, auto-captured,
recency-weighted, private-to-me), and add OKF as a *second, separate grounding
surface* for curated, shareable, version-controlled domain knowledge. The two
feed the same context-injection point on `before_agent_start` but stay separate
stores with separate write paths, separate budgets, and clear source labels. OKF
arrives the pi-stack way: an org maintains an OKF bundle repo and ships it as a
private overlay (a git-mounted bundle wired through a new `knowledge` capability),
consumed read-only by default and enriched only through a gated, human-reviewed
write path.

## Why not replace memory with OKF (the core argument)

The instinct to consolidate is wrong here because the two systems disagree on
every axis that matters. They are not the same job stored twice; they are two
jobs that happen to both end in "text the agent should know."

| axis | memory (keep as-is) | OKF bundle (add) |
|---|---|---|
| **owner** | me, one person | an org / a team, many people |
| **authority** | inferred, best-effort, "treat as context" | curated, reviewed, authoritative |
| **write path** | automatic watcher, no human in the loop | human + enrichment agent, PR-reviewed |
| **lifecycle** | decays; recency-weighted; supersedes on contradiction | versioned in git; changes are commits, not decay |
| **scope** | my prefs, corrections, project facts | domain truth: catalog, schema, runbooks, conventions |
| **distribution** | host sqlite, single writer, never leaves the host | git-distributable, exchangeable across orgs |
| **privacy** | private-to-me by construction | shareable by construction |
| **structure** | flat scored bag of facts/learnings | a linked tree with progressive disclosure (index.md) |
| **retrieval** | scored recall (relevance x confidence x recency x freq x reward) | traversal + search over a stable directory |

Force personal memory *into* OKF and you lose the things that made the memory
rebuild work: recency weighting, the reward signal, automatic zero-ceremony
capture, and the privacy guarantee (my offhand corrections do not belong in a
git repo other people read). Force an OKF bundle *into* the sqlite store and you
lose the things OKF exists for: git-shipping, human authoring, cross-org
exchange, PR review, and the linked-tree progressive disclosure. They are
complementary. Ship both, labeled, side by side.

## Jobs OKF does that memory cannot (JTBD)

The job memory does: *"When I'm working, remember what I told you and what we
learned, so I never repeat myself."* That is personal and continuous. OKF covers
four jobs memory structurally cannot:

| # | Job (JTBD) | Why memory can't | OKF fit |
|---|---|---|---|
| **J1** | "When I join a domain, ground me in the shared truth that predates me and outlives my sessions (data catalog, schemas, internal API surface)." | Memory is per-me and starts empty; shared domain truth has no owner to capture it. | A curated bundle authored once, read by everyone. |
| **J2** | "When knowledge changes, let a team review and version it, and let another org consume it." | Memory has no review, no diff, no export; a watcher-inferred fact is not auditable. | Git-native: PRs, history via `log.md`, cross-org exchange. |
| **J3** | "When you answer, ground it in human-reviewed authoritative facts with citations, not auto-inferred guesses." | Memory is explicitly best-effort ("treat as context, say if stale"). | OKF frontmatter `type` + `# Citations` = provenance and authority. |
| **J4** | "When the domain is large, let me traverse it top-down (start at the index, drill into what's relevant) instead of getting a flat scored dump." | Memory returns a small scored working set, not a navigable structure. | `index.md` progressive disclosure + bundle-relative cross-links. |

If a proposed OKF use maps to none of J1-J4, it probably belongs in memory, not
OKF. Use this list as the scoping test.

## The mixin-kit + enrichment/consumption design

The user's hypothesis is right: an OKF bundle wants a maintainer and a repo, and
pi-stack already has the exact shape for that. This is the private-overlay model
from `docs/OVERLAY.md`, reused.

### Distribution: a git-mounted bundle, pointed at by the overlay

- An org maintains an **OKF knowledge repo** (its own private git repo): a
  directory tree of markdown, `index.md` + `log.md`, frontmatter per file. This
  is the source of truth. Humans and enrichment agents write to it via PRs.
- pi-stack **mounts the bundle** into the sandbox at a known path (recommend a
  read-only `/knowledge` mount, cloned/pulled at sandbox create). The overlay's
  mixin kit ships the *pointer and wiring* (the `capabilities.json` entry, the
  mount config), **not** the bundle content baked into `files/`. Rationale: a
  git-mounted bundle updates with a `git pull`; a baked-in copy needs a
  `make load` image rebuild every time the catalog changes. Ship the pointer,
  mount the content.
- Multiple bundles are fine and expected (an org bundle + a team bundle). That
  is exactly what capability-routing's fan-out-and-merge is built for.

### Consumption agents (READ) — the default, and most of the value

The running pi agent is a **consumption agent** in OKF's terms. Consumption is
"permissive": tolerate unknown `type`s, missing fields, broken links. Two consume
paths, one on-demand and one automatic (see Phases):

- **On-demand traversal** (a skill / `/knowledge` command): read `index.md`,
  follow bundle-relative links, pull the concepts relevant to the task. This is
  progressive disclosure, and it is the honest first step because it reinvents
  nothing.
- **Automatic grounding** (recall injector, Phase 2): a host `knowledge` plugin
  indexes the bundle and the recall injector merges knowledge hits into the same
  `before_agent_start` injection as memory, clearly labeled and citing the
  bundle-relative path.

### Enrichment agents (WRITE) — gated, later, and never the watcher

OKF distinguishes enrichment (writes the bundle). In pi-stack this is:

- **An `enrich-knowledge` skill + a subagent role.** It reads a wired data source
  (a warehouse, an internal API, chat, docs) *via capability-routing*, and
  **writes OKF markdown** (correct frontmatter, `index.md` entry, `log.md` line,
  `# Citations`) into the bundle repo, then opens a **PR**. It never commits to
  the bundle directly. This mirrors the memory loop's promotion ethos: durable
  shared truth is always gated behind human review, never auto-written.
- **A promotion bridge from memory.** `/learnings` already surfaces recurring
  personal learnings. When one is genuinely *domain* knowledge (not a personal
  pref), the promotion step can propose graduating it into an OKF concept as a
  bundle PR. That is the one sanctioned path across the personal → shared
  boundary, and it stays gated.

The watcher must **never** write OKF. Auto-inferred, best-effort, private-to-me
is the opposite of curated-shared-reviewed. Keep that wall.

## Capability-routing integration

**Yes, `knowledge` is a new capability.** It slots into `capabilities.json`
exactly like `docs` or `chat`, and the fan-out-and-merge semantics already handle
multiple bundles.

- **Public profile:** `"knowledge": [{ "provider": "none" }]` — no bundle wired,
  degrade cleanly (the consuming skill says "no knowledge bundle wired, using web
  + files" once, plainly). Nothing company-specific enters the public repo.
- **Overlay profile (Phase 1):** a bundle mounted at a path. This needs a small,
  honest addition to capability-routing: a **`files` (bundle) provider kind**
  alongside `mcp`/`cli`/`http`/`none`, e.g.
  `{ "provider": "files", "path": "/knowledge", "about": "org OKF data catalog" }`.
  The consuming skill reads `index.md` at that path and traverses.
- **Overlay profile (Phase 2+):** upgrade the provider to
  `{ "provider": "http", "url": "http://host.docker.internal:11442", "about": "..." }`
  pointing at the `pi-stack-host knowledge` plugin, which serves search/get over
  the indexed bundle. `:11442` is already the port the overlay docs reserve for a
  host service, and http is already a supported provider kind, so Phase 2 needs
  no capability-routing change.

Skills ask for the `knowledge` capability, never for a vendor or a path. Swap the
overlay's `capabilities.json` and every knowledge-aware skill retargets at once.

## Should memory and OKF share the recall path?

**Shared injection point, separate stores, separate budgets, distinct labels.**

- **Shared** where it counts: both feed the `before_agent_start` system-prompt
  injection, so domain grounding is as automatic as memory recall. The user does
  not run a command to be grounded.
- **Separate** everywhere else: two stores (sqlite memory vs the indexed OKF
  bundle), two host plugins (`memory` :11435, `knowledge` :11442), two write
  paths. Do not merge the databases.
- **Distinct labels in the prompt.** Memory injects under "From memory (recalled
  for this task) ... treat as context, say if stale." Knowledge injects under a
  separate heading like "From the knowledge base (authoritative)" and **cites the
  bundle-relative path** (`/tables/orders.md`) so the agent can drill in and so
  the user can trust the provenance. Authority differs; the labels must make that
  visible.
- **Separate token budgets.** Knowledge grounding must not crowd out personal
  memory (or vice versa). Give each its own small budget in the injector.
- **Surfaces:** keep `/recall` for memory; add `/knowledge <query>` for the
  bundle. Two commands, because they answer two different questions ("what do you
  remember about me/this project" vs "what does the org's catalog say").

## What NOT to do

- **Do not reinvent OKF's format.** No custom schema, no bespoke frontmatter, no
  parallel "pi-stack knowledge format." Consume and emit standard OKF markdown.
- **Do not force personal memory into OKF.** Privacy, recency weighting, the
  reward signal, and zero-ceremony auto-capture do not survive the trip. Memory
  stays in sqlite.
- **Do not let the watcher write OKF.** Auto-inferred is not curated. Enrichment
  is human-gated, PR-reviewed, always.
- **Do not build storage/query infrastructure that competes with git.** OKF
  explicitly disclaims storage/query infra. The git bundle is the source of
  truth; our `knowledge` index (FTS5 + embeddings) is a **disposable cache** of
  it, rebuilt from the bundle, never authoritative.
- **Do not build a schema registry or a fixed taxonomy.** OKF's non-goals are our
  non-goals. Tolerate unknown `type`s and broken links; degrade, don't validate.
- **Do not make knowledge decay like memory.** Knowledge changes via commits, not
  recency. No TTL, no supersede-on-contradiction; that is what git history is for.
- **Scope-creep watch:** resist "let's also make the bundle editable live from the
  agent," "let's add a knowledge graph DB," "let's sync memory into the bundle
  automatically." Each breaks one of the walls above.

## Phased deliverables (sequencing, not MVP hedging)

Each phase is independently shippable and independently valuable.

### Phase 1 — Consume (read-only, reinvent nothing)

- Add the `knowledge` capability to `capabilities.json` (public: `none`).
- Add a `files` (bundle) provider kind to the `capability-routing` skill.
- A consumption skill + `/knowledge` command: read `index.md`, traverse
  bundle-relative links, pull relevant concepts on demand. Permissive parsing.
- Overlay wiring: mount an OKF bundle at `/knowledge`, point the overlay's
  `capabilities.json` at it.
- **Ships:** on-demand domain grounding (J1, J4) with zero new host code.

### Phase 2 — Ground automatically (the recall merge)

- New host plugin `pi-stack-host knowledge` (Go, `:11442`): index the bundle into
  FTS5 + optional Ollama embeddings; serve `search` / `get` / `reindex`. The
  index is a cache; the bundle is truth.
- Extend the recall injector to query knowledge in parallel with memory and merge
  into `before_agent_start`, under a separate authoritative-labeled heading, with
  bundle-relative citations, on its own token budget.
- Overlay upgrades the `knowledge` provider from `files` to `http`.
- **Ships:** automatic, cited domain grounding on every turn (J3), same
  zero-ceremony feel as memory.

### Phase 3 — Enrich (gated write-back, closes the org loop)

- `enrich-knowledge` skill + subagent role: read a wired source via
  capability-routing, emit correct OKF markdown (frontmatter, `index.md`,
  `log.md`, `# Citations`), open a PR to the bundle repo. Never a direct commit.
- Promotion bridge: `/learnings` can propose graduating a recurring *domain*
  learning into an OKF concept as a gated bundle PR (the one sanctioned
  personal → shared crossing).
- **Ships:** the human-reviewed, versioned, cross-org write path (J2) and a real
  connection between the personal learning loop and the shared knowledge base.

## Open questions for the decision

1. **Bundle mount mechanism.** Git-clone-at-create into a read-only `/knowledge`
   mount (updates via pull, needs the bundle repo in the network allowlist) vs
   baked into the mixin kit's `files/` (static, needs a rebuild to update). I lean
   git-mount for the pointer-not-content reasons above; this needs a kit/network
   decision.
2. **Cross-boundary promotion governance.** Who reviews the PR when a personal
   learning is proposed as a shared OKF concept, and how do we keep genuinely
   personal facts from leaking into a shared repo? Phase 3 is blocked on a clear
   answer here.
3. **Injection budget split.** Fixed per-surface token budgets vs one shared
   budget that ranks memory and knowledge hits together. I lean fixed-per-surface
   so authoritative grounding is never starved by personal chatter, but it is a
   tuning call worth a bake-off.
