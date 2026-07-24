# Packs — design doc (FOR REVIEW)

Status: DRAFT, owner decisions locked (§14). Produced with the
crew (architect, product-manager, dx-consultant, security-lead, devrel) + the
`dx-impatient` reviewer. Goes hand in hand with `docs/design/onboarding-v2-spec.md`.

> **Update:** packs shipped (v2 — see `docs/design/packs-v2*.md`), and the
> build-time **overlay is now fully retired** — including the host-plugin half.
> Host-executing integrations ship as **containers** (a pack `[[integrations]]`
> `manifest`, run on the host by the sbx gateway) or as **host daemons**; **no
> `pi-stack-host` recompile is ever needed.** Where this doc calls the overlay the
> live "before" state or says the host-plugin half "stays distinct," read it as the
> design-time record. See [../OVERLAY.md](../OVERLAY.md).

Locked: name = `pack`; `profile` DELETED (active pack = context); single active
pack (no multi-pack) in v1; packs are 100% runtime (no compile-in); v1 is
Tier-0 + reference-only integrations with `op://` credential solicitation;
pack-shipped executables + trust gate + signing are v2; personal pack local-only
by default.

## 1. The idea

> A **pack** is the one folder, in git, that makes your setup yours and
> portable: the skills you taught pi-stack, the knowledge it uses, the tools it's
> wired to, and the context config, all versioned like code instead of trapped
> in one sandbox.

Today five git/config things are really facets of one: authored **skills**
(baked / overlay / `~/.local/share/pi-stack/skills`), an OKF **knowledge** bundle
(`knowledge_bundles`), **MCP servers**, **CLI proxies** (e.g. `snow` wrappers),
and the **overlay kit** — with a **profile** that selects a leaky subset of them.
Unify into ONE portable, git-backed **pack**. The overlay is the prototype;
make it first-class.

## 2. The three-way split (keep these distinct)

- **Memory** — what pi-stack *learns by watching*. Live, personal, never in git.
  You don't edit it; you correct it by acting differently. Stays separate.
- **Pack** — what you *deliberately taught it*. Versioned, shareable, a git repo.
- **Profile** — *which pack(s) you're wearing*. Not a third home for state, just a
  pointer. Test: **if you'd `git diff` it, it's a pack; if you'd only see it in a
  `recall`, it's memory.**

## 3. Problem

"Bring my whole agent context in one move" (new machine, hand to a teammate, flip
work<->personal) is smeared across 5 artifacts + a `profile` selector that only
indexes some of them. Adding one Slack-like proxy today touches five files across
three mechanisms (overlay `files/`, `capabilities.json`, MCP registration,
`op-refs.env`, a host `overlay_*.go`). That is a Golden Path failure.

## 4. Goals / non-goals

Goals: one portable unit; add a capability with one command and one file of
record; adopt a teammate's pack in one (gated) step; packs are runtime-swappable
(no image rebuild for the common cases); memory stays separate.

Non-goals (v1): a registry/marketplace; a signing PKI; pack-to-pack dependency
resolution; a templating DSL; a `pack push`/`publish` verb distinct from `git`; a
GUI/wizard; compiling a Go host plugin *shipped in* a pack.

## 5. Layout — one required file, grow on add

A pack is a git repo. The ONLY required file is `pack.toml` (name + `schema=1`).
Its presence is the entire "is this a pack" test. Everything else appears when
you add it — never pre-scaffold empty `skills/ mcp/ okf/ routing/` dirs (an empty
scaffold is a UI lie).

```
my-pack/
  pack.toml          # required: name, schema. Manifest of record.
  skills/<name>/SKILL.md
  knowledge/         # OKF bundle (what `knowledge init` scaffolds today)
  bin/               # CLI-proxy wrapper scripts
  capabilities.json  # capability routing FRAGMENT (colocated with what it routes)
  kit/files/…        # raw escape hatch for anything the schema can't express (100% overlay back-compat)
  pack.lock          # generated: resolved refs + plugin SHAs
```

**Pack creation adopts an existing repo OR inits a fresh one.** `pi-stack pack
new [PATH]` (and the onboarding lazy-create) resolves the target this way:
- PATH is already a git repo (or already has a `pack.toml`) -> USE it in place;
  just add a `pack.toml` if missing. Never re-init, never clobber history.
- PATH exists but is not a repo -> `git init` it, add `pack.toml`.
- PATH absent -> create the dir, `git init`, add `pack.toml`.
- No PATH -> the personal-pack root (`~/.local/share/pi-stack/skills`), same
  rules. So a user who already keeps a skills/dotfiles repo points setup at it and
  it becomes their pack; everyone else gets one initialized for them.

`pi-stack pack add <kind> <name>` is the one authoring verb (kind =
`skill|mcp|proxy|knowledge`). It creates `pack.toml` implicitly on first use
(one-line stderr notice), writes the one file, and registers it — including its
routing entry in the SAME place, so removing the pack removes its capability
claim (no dangling `capabilities.json` pointing at an absent provider). Target:
under 30s to first artifact, matching the onboarding-v2 Stripe bar. No `pack new`
wizard.

## 6. Composition

`BASE (public image) <- pack stack (0..n, ordered) <- personal pack (0..1)`,
deterministic later-wins. Per-facet merge: **union** (skills, kb, mcp, proxies,
net allowlist), **deep-merge** (`capabilities.json` — fixes today's overwrite so
stacking >1 pack is coherent), **last-writer** (singular plugin slots, scalars).
Stacking is additive, like kits today (`--pack A --pack B`); no resolver in v1.

## 7. Runtime plug-in — the honest scope

Packs are runtime-swappable for the common cases, NO recompile:
- **in-sandbox wrapper scripts** (`bin/`, kit `files/`) — fenced.
- **stdio MCP servers** registered with the sbx gateway — run on the HOST.
- **external SHA-pinned go-plugin binaries** for host capability slots
  (memory/knowledge/broker/mcp) — `serve` hashes-and-refuses on mismatch, fail
  closed, `sha` mandatory (the mechanism already exists in `services/host/plugin`).

For a PACK AUTHOR this means: **packs are 100% runtime, no build, ever.** The one
thing that still needs a compile — a Go plugin **compiled into** `pi-stack-host`
(`overlay_*.go`) — only extends pi-stack-host's OWN binary (a new subcommand or a
deeply-wired host service) and is a pi-stack-maintainer concern, not something a
pack does. So it is off the pack path entirely; don't mention it in pack docs
except to say packs never need it.

## 8. Profile is DELETED (owner decision)

With a single active pack, the pack IS your context — switching work<->personal is
switching the active pack, not editing four config keys. So `profile` and its
per-field override tables (`profiles.work.mcp`, `active_profile`, the
`.pi-stack/profile` file) are **removed**, not kept as a thin selector. What was
spread across a profile now lives in the pack: `gog_account`, mcp/integration
refs, knowledge, routing + model prefs (§8a), and the memory scope.

A pack **binds a memory scope tag** (written to `.pi-stack/memory.scope`,
wire-compatible with the existing `Profile` field in `plugin/interfaces.go`), so
switching packs switches memory scope — which also finally ships the
`profiles.md` memory-isolation fix. `active_pack` replaces `active_profile`.

## 8a. What "context config" in a pack means (routing vs model prefs)

A pack carries BOTH model layers, which are distinct:
- **Routing config** — intent -> model (the crew: `code`->sonnet, `review`->gpt,
  under cost/latency/accuracy). Pack-level overrides to `models.json` /
  `scorecard.json` / `policy.json`, recompiled to `routing.json`. A work pack can
  pin approved-vendor-only routing.
- **Model prefs** — specific choices: `ollama_bridge_model`, the default session
  model/intent. Plain scalars.

Both are part of a pack's context config, layered when the pack is active.

## 9. Trust model — split by whether the pack EXECUTES code

The common pack case is NOT shipping a custom binary. It is **referencing an
integration** (a remote gateway-catalog MCP server, gog, Slack) and **declaring a
credential need**. That ships NO executable code: the pack manifest lists the
capability + the ENV VAR names it needs, and adoption/onboarding solicits the
user's `op://` refs (`pi-stack secret set SLACK_TOKEN op://vault/item/field`).
Values stay in 1Password; nothing pack-authored runs. This is **v1-safe** and is
the primary credential-solicitation hook onboarding uses.

Only when a pack ships a binary to RUN (a stdio MCP command, an external
go-plugin) does host-execution trust apply — and that is **v2**. Execution
location is the boundary there:

- **In-sandbox** (wrappers, skills, extensions): constrained by DHI + the net
  allowlist. **Safe by default.**
- **On-host** (stdio MCP commands, external go-plugin binaries): full host access.
  **Never safe by default.**

Safeguards:
1. **Host bill-of-materials gate.** `pack use <url>` halts at a review screen
   enumerating exactly what will run on the host (MCP commands, plugin binaries +
   SHAs, net-egress domains) and requires explicit `[y/N]`. Non-TTY fails closed
   unless `--yes`.
2. **Mandatory SHA-pin** for any external host binary; `serve` re-hashes before
   every launch and refuses on mismatch.
3. **1Password credential boundary.** A pack declares only ENV VAR *names*, never
   values; adoption prompts `pi-stack secret set <VAR> op://…`; `op run` resolves
   at launch. The pack never carries secrets; the VM never sees them.
4. **Typed allowlist.** Host execution only through the typed schema (arrays of
   MCP commands, strict slot defs). Unknown keys / arbitrary shell injection ->
   the parser rejects the whole pack.
5. **Provenance.** Signed commits / sigstore; an unsigned or unknown-signer pack
   elevates the host-exec gate to a typed override. (v1 may start at "trust the
   git remote, like `npm install` from a GitHub URL"; signing is the v2 hardening.)

Adoption tiers (progressive disclosure of trust):
- **Tier 0** (skills + routing only, no host exec): `pack use` just works, no
  prompt — skills are markdown the agent reads, not executed.
- **Tier 1** (any host exec present): the BoM gate above, one yes/no.

## 10. Verbs

- `pi-stack pack add <skill|mcp|proxy|knowledge> <name>` — author (implicit
  create).
- `pi-stack pack use <path|git-url>` — adopt (Tier gate); writes `packs = [...]`
  (sparse-diff config, like every other key).
- `pi-stack pack ls | show` — list / inspect (incl. host BoM).
- `pi-stack pack rm <name>` — unregister (keep the clone, like sandbox re-attach).
- **No `pack sync`/`push`** — a pack is a git repo; `git` is the sync, unmediated.
  `knowledge init/use/sync` become subsets of `pack` (a pack *contains* a bundle).
- `run --pack <path|git-url>` — attach for one session (additive, like kits).

## 11. Onboarding integration (goes hand in hand)

Onboarding-v2's shape is unchanged (task-first, no menu). What changes: the §7.4
capture artifact now lands in a pack.

- First task runs. Mid-task, a capture-worthy moment surfaces. On confirm: if no
  personal pack exists, `git init` it at the Q1 path silently (no "let's set up
  your pack" detour), write the artifact, **commit it**. Name it once: "That's in
  your pack now — `git log` any time." Close reports pack path + commit count.
- **Personal pack** = where you capture (created lazily on first write, no remote
  required). **Work pack** = where you contribute (inherited; the `provisioned`
  short-circuit means a work pack is present). Rule taught once when relevant:
  *personal pack = capture; work pack = contribute.*
- Core after any pack-touching onboarding: you have a pack, it's a git repo, one
  thing is committed. Defer: authoring external plugins, baking a pack into an
  image, sharing/publishing, multi-pack precedence.

Amendments to fold into `onboarding-v2-spec.md`: truth file gains a `pack`
object `{path, git_initialized, commits}`; §7.4 write target is "your pack
(created on first write) and committed"; §7.7 receipt names the pack + commit;
§9 provisioned = a work pack is present (orthogonal to whether a personal pack
exists); §12 Q1 resolves — that path IS the personal pack root and "wiring" means
`git init`; §13 replaces the personal-skills-dir item with the lazy-pack flow.

## 12. Migration (zero-touch)

Absent packs -> an implicit anonymous pack byte-identical to today (guarded by an
open-core fitness test extending `check-open-core.sh`). `OVERLAY=… make run`
keeps working with no `pack.toml`. The overlay's **sandbox half is superseded by
packs**; only the **host-plugin half** (`overlay_*.go`, compiled-in Go) stays
distinct (packs are runtime-loaded; those are compile-time). `pi-stack pack
migrate` proposes a diff from today's overlay + `knowledge_bundles` +
personal-skills-dir into a pack under a `[Y/n]` gate.

## 13. v1 scope vs deferred (revised per the `dx-impatient` review)

The reviewer's verdict on the first cut was **FRICTION bordering BAIL**: v1 tried
to land the noun AND migrate four subsystems AND solve host-code trust at once.
The highest-leverage fix: **make v1 Tier-0-only — a pack carries skills, routing,
and knowledge CONTENT, but cannot execute host code.** That single constraint
deletes the trust gate, SHA-pin, the per-var `op://` prompt chain, the BoM
screen, and the "`pack add mcp` can't attach in-session" lie — every place adopt
friction and "the tool lies" risk lived. So:

**v1 (safe, instant, sub-60s):** `pack.toml`; `pack add skill|knowledge`; `pack
use` (Tier 0 — no prompt, nothing to trust); `pack ls|show|rm`; `run --pack`;
local + git-pinned; the onboarding lazy-personal-pack flow with the commit
GUARDED (fall back to staged-not-committed if `user.email` is unset — never fail
mid-onboarding); **delete `profile`** (active pack replaces active_profile,
memory scoped per pack); and **reference-only integrations + credential
solicitation** — a pack declares a capability it uses (remote/existing MCP, gog)
+ the ENV VAR names it needs, and adoption/onboarding walks the user through
`pi-stack secret set <VAR> op://…` (no pack code runs, so this is v1-safe). Single
active pack.

**Deferred to v2 (everything where a pack SHIPS + RUNS code, or a subsystem
migration):** authoring a pack-shipped stdio-MCP/proxy *binary*; external
go-plugin SHA-pin; the Tier-1 host bill-of-materials adoption gate for
pack-shipped binaries; `pack.lock`; folding
(NOTE: reference-only integrations + `op://` credential solicitation are v1 —
only pack-AUTHORED executables are deferred here.)
`knowledge init/use/sync` into `pack` (leave the working subsystem alone; a pack
just *contains* a `knowledge/` dir); the `profile`->pack migration + `Resolve()`
rewrite (answer open question 1 FIRST); deep-merged capabilities (only needed
with multi-pack composition, open question 2); registry/marketplace; signing PKI;
dependency resolution; per-pack memory isolation beyond the scope tag; a
remote-by-default personal pack.

Two v1 correctness must-fixes the reviewer flagged regardless of scope:
- **`pack add mcp`/`proxy` (when it lands in v2) must print "recreate the sandbox
  to attach" in the same breath** — registration ≠ in-session attachment (AGENTS.md).
- **The single pack-selection surface must be unambiguous:** if `profile`
  survives, `packs = [...]` is just "the default profile's stack", documented as
  such — not a parallel authoritative key.

## 14. Open questions — RESOLVED (owner)

1. **Delete `profile` entirely** — not a thin selector. Single active pack = your
   context; `active_pack` replaces `active_profile`. (§8)
2. **Single active pack for v1** — no multi-pack composition. (Revisit if a real
   personal+company-at-once need shows up.)
3. **Provenance/signing: deferred to v2** with host execution. v1 is markdown +
   refs (no pack code runs), so trust = the git remote, like `npm install` from a
   GitHub URL.
4. **Personal pack is local; the user pushes to git themselves.** pi-stack's
   involvement ends at `git init` — it never adds a remote, never pushes, and
   never even OFFERS to. Remotes, cross-machine sync, and backup are the user's
   own git workflow, unmediated (matches "skills belong in git, the user checks
   them in").
5. **Name is `pack`.** Retire user-facing "kit"/"bundle" overlap (kit = sbx
   sandbox image config; a pack *contains* an OKF bundle).
6. **Compile-into-`pi-stack-host` is NOT a pack concern** — packs are 100%
   runtime (external subprocess / stdio MCP / in-sandbox wrapper). Compile-in
   (`overlay_*.go`) only extends pi-stack-host's own binary and stays a
   maintainer/advanced concern, off the pack path entirely.

## 15. Prior art (borrow / avoid)

devcontainers (borrow declarative+composable features; avoid rebuild-on-change —
a pack edit must be instant via dev-mode live-mount). dotfiles/chezmoi (borrow
"it's just git"; avoid a templating DSL). VS Code profiles (borrow one-action
import/export + runtime switch; avoid opaque non-diffable blobs). Nix
home-manager (borrow single source of truth; avoid the learning curve — no new
declarative language before writing a skill). oh-my-zsh (borrow "drop a file in a
folder, add a line, it's active"; avoid eager-loading every plugin — connect
MCP/proxies lazily, preserving the onboarding-v2 MCP-opt-in fix).
