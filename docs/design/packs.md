# Packs — design doc

Status: SHIPPED (v1 + v2). This is the design rationale behind the pack
architecture described for users in `docs/reference.md` §5 and built out
further in `docs/design/packs-v2.md` / `packs-v2-impl.md`.

Locked: name = `pack`; single active pack (no multi-pack) in v1; packs are
100% runtime (no compile-in); v1 shipped Tier-0 + reference-only integrations
with `op://` credential solicitation; pack-shipped executables + trust gate +
signing shipped in v2; personal pack is local-only by default.

## 1. The idea

> A **pack** is the one folder, in git, that makes your setup yours and
> portable: the skills you taught pix, the knowledge it uses, the tools
> it's wired to, and the context config, all versioned like code instead of
> trapped in one sandbox.

Five things are facets of one: authored **skills**, an OKF **knowledge**
bundle (`knowledge_bundles`), **MCP servers**, **CLI proxies** (e.g. `snow`
wrappers), and **context config** (`gog_account`, routing/model prefs,
memory scope). A pack unifies them into ONE portable, git-backed unit.

## 2. The three-way split (keep these distinct)

- **Memory** — what pix *learns by watching*. Live, personal, never in
  git. You don't edit it; you correct it by acting differently. Stays
  separate.
- **Pack** — what you *deliberately taught it*. Versioned, shareable, a git
  repo.
- **Active pack** — *which pack you're wearing*. Not a third home for state,
  just a pointer. Test: **if you'd `git diff` it, it's a pack; if you'd only
  see it in a `recall`, it's memory.**

## 3. Problem this solves

"Bring my whole agent context in one move" (new machine, hand to a teammate,
flip work<->personal) touches five separate artifacts, and adding one
capability (say a Slack-like proxy) means touching several files across
several mechanisms (a skills dir, `capabilities.json`, MCP registration,
`op-refs.env`). A pack collapses that into one command and one file of
record per capability.

## 4. Goals / non-goals

Goals: one portable unit; add a capability with one command and one file of
record; adopt a teammate's pack in one (gated) step; packs are
runtime-swappable (no image rebuild for the common cases); memory stays
separate.

Non-goals (v1): a registry/marketplace; a signing PKI; pack-to-pack
dependency resolution; a templating DSL; a `pack push`/`publish` verb
distinct from `git`; a GUI/wizard.

## 5. Layout — one required file, grow on add

A pack is a git repo. The ONLY required file is `pack.toml` (name +
`schema=1`). Its presence is the entire "is this a pack" test. Everything
else appears when you add it — never pre-scaffold empty
`skills/ mcp/ okf/ routing/` dirs (an empty scaffold is a UI lie).

```
my-pack/
  pack.toml          # required: name, schema. Manifest of record.
  skills/<name>/SKILL.md
  knowledge/         # OKF bundle (what `knowledge init` scaffolds today)
  bin/               # CLI-proxy wrapper scripts
  capabilities.json  # capability routing FRAGMENT (colocated with what it routes)
  web-search.json     # optional web discovery routing (provider/gateway/model)
  kit/files/…        # raw escape hatch for anything the schema can't express
  pack.lock          # generated: resolved refs + plugin SHAs
```

**Pack creation adopts an existing repo OR inits a fresh one.** `pix
pack new [PATH]` (and the onboarding lazy-create) resolves the target this
way:
- PATH is already a git repo (or already has a `pack.toml`) -> USE it in
  place; just add a `pack.toml` if missing. Never re-init, never clobber
  history.
- PATH exists but is not a repo -> `git init` it, add `pack.toml`.
- PATH absent -> create the dir, `git init`, add `pack.toml`.
- No PATH -> the personal-pack root (`~/.local/share/pix/skills`), same
  rules. So a user who already keeps a skills/dotfiles repo points setup at
  it and it becomes their pack; everyone else gets one initialized for them.

`pix pack add <kind> <name>` is the one authoring verb (kind =
`skill|mcp|proxy|knowledge`). It creates `pack.toml` implicitly on first use
(one-line stderr notice), writes the one file, and registers it — including
its routing entry in the SAME place, so removing the pack removes its
capability claim (no dangling `capabilities.json` pointing at an absent
provider). Target: under 30s to first artifact, matching the onboarding-v2
Stripe bar. No `pack new` wizard.

## 6. Composition

`BASE (public image) <- pack stack (0..n, ordered) <- personal pack (0..1)`,
deterministic later-wins. Per-facet merge: **union** (skills, kb, mcp,
proxies, net allowlist), **deep-merge** (`capabilities.json` — so stacking
>1 pack is coherent), **last-writer** (singular plugin slots, scalars).
Stacking is additive, like kits (`--pack A --pack B`); no resolver in v1.

## 7. Runtime plug-in — the honest scope

Packs are runtime-swappable for the common cases, NO recompile:
- **in-sandbox wrapper scripts** (`bin/`, kit `files/`) — fenced.
- **stdio MCP servers** registered with the sbx gateway — run on the HOST.
- **external SHA-pinned go-plugin binaries** for host capability slots
  (memory/knowledge/broker/mcp) — `serve` hashes-and-refuses on mismatch,
  fail closed, `sha` mandatory (the mechanism already exists in
  `services/host/plugin`).

For a PACK AUTHOR this means: **packs are 100% runtime, no build, ever.**
The only host-side extension point in `pix-host` is the generic,
SHA-pinned `[plugins.*]` external-process mechanism, and it is entirely off
the pack path — packs never need it. There is no compile-in extension seam
of any kind: extending `pix-host`'s own binary (a new subcommand or a
deeply-wired host service) is a pix-maintainer concern handled by
editing `services/host/` directly and shipping a new release, not something
a pack can carry.

## 8. Active pack = your context

With a single active pack, the pack IS your context — switching
work<->personal is switching the active pack, not editing several config
keys. Everything that makes up "which context am I in" lives in the pack:
`gog_account`, mcp/integration refs, knowledge, routing + model prefs (§8a),
and the memory scope.

A pack **binds a memory scope tag** (written to `.pix/profile`,
wire-compatible with the `Profile` field in `plugin/interfaces.go`), so
switching packs switches memory scope. `pack` is the config key that
tracks which pack is active.

## 8a. What "context config" in a pack means (routing vs model prefs)

A pack carries BOTH model layers, which are distinct:
- **Routing config** — intent -> model (the crew: `code`->sonnet,
  `review`->gpt, under cost/latency/accuracy). Pack-level overrides to
  `models.json` / `scorecard.json` / `policy.json`, recompiled to
  `routing.json`. A work pack can pin approved-vendor-only routing.
- **Model prefs** — specific choices: `ollama_bridge_model`, the default
  session model/intent. Plain scalars.

Both are part of a pack's context config, layered when the pack is active.

## 9. Trust model — split by whether the pack EXECUTES code

The common pack case is NOT shipping a custom binary. It is **referencing an
integration** (a remote gateway-catalog MCP server, gog, Slack) and
**declaring a credential need**. That ships NO executable code: the pack
manifest lists the capability + the ENV VAR names it needs, and
adoption/onboarding solicits the user's `op://` refs (`pix secret set
SLACK_TOKEN op://vault/item/field`). Values stay in 1Password; nothing
pack-authored runs. This is **v1-safe** and is the primary
credential-solicitation hook onboarding uses.

Only when a pack ships a binary to RUN (a stdio MCP command, an external
go-plugin) does host-execution trust apply — this shipped in v2. Execution
location is the boundary there:

- **In-sandbox** (wrappers, skills, extensions): constrained by DHI + the
  net allowlist. **Safe by default.**
- **On-host** (stdio MCP commands, external go-plugin binaries): full host
  access. **Never safe by default.**

Safeguards:
1. **Host bill-of-materials gate.** `pack use <url>` halts at a review
   screen enumerating exactly what will run on the host (MCP commands,
   plugin binaries + SHAs, net-egress domains) and requires explicit
   `[y/N]`. Non-TTY fails closed unless `--yes`.
2. **Mandatory SHA-pin** for any external host binary; `serve` re-hashes
   before every launch and refuses on mismatch.
3. **1Password credential boundary.** A pack declares only ENV VAR *names*,
   never values; adoption prompts `pix secret set <VAR> op://…`; `op
   run` resolves at launch. The pack never carries secrets; the VM never
   sees them.
4. **Typed allowlist.** Host execution only through the typed schema
   (arrays of MCP commands, strict slot defs). Unknown keys / arbitrary
   shell injection -> the parser rejects the whole pack.
5. **Provenance.** Signed commits / sigstore; an unsigned or unknown-signer
   pack elevates the host-exec gate to a typed override. v1 trusts the git
   remote (like `npm install` from a GitHub URL); signing is v2 hardening.

Adoption tiers (progressive disclosure of trust):
- **Tier 0** (skills + routing only, no host exec): `pack use` just works,
  no prompt — skills are markdown the agent reads, not executed.
- **Tier 1** (any host exec present): the BoM gate above, one yes/no.

## 10. Verbs

- `pix pack add <skill|mcp|proxy|knowledge> <name>` — author (implicit
  create).
- `pix pack use <path|git-url>` — adopt (Tier gate); writes `packs =
  [...]` (sparse-diff config, like every other key).
- `pix pack ls | show` — list / inspect (incl. host BoM).
- `pix pack rm <name>` — unregister (keep the clone, like sandbox
  re-attach).
- **No `pack sync`/`push`** — a pack is a git repo; `git` is the sync,
  unmediated. `knowledge init/use/sync` are subsets of `pack` (a pack
  *contains* a bundle).
- `run --pack <path|git-url>` — attach for one session (additive, like
  kits).

## 11. Onboarding integration

Onboarding is task-first, no menu. The capture-artifact flow lands writes
in a pack:

- First task runs. Mid-task, a capture-worthy moment surfaces. On confirm:
  if no personal pack exists, `git init` it at the default path silently
  (no "let's set up your pack" detour), write the artifact, **commit it**.
  Name it once: "That's in your pack now — `git log` any time." Close
  reports pack path + commit count.
- **Personal pack** = where you capture (created lazily on first write, no
  remote required). **Work pack** = where you contribute (inherited when
  provisioned). Rule taught once when relevant: *personal pack = capture;
  work pack = contribute.*
- Core after any pack-touching onboarding: you have a pack, it's a git
  repo, one thing is committed. Deferred: authoring external plugins,
  baking a pack into an image, sharing/publishing, multi-pack precedence.

See `onboarding-v2-spec.md` for the host-state fields this feeds and
`skills/onboarding/SKILL.md` for the shipped onboarding flow.

## 12. v1 scope vs v2 (what shipped when)

**v1 (shipped, safe, instant, sub-60s):** `pack.toml`; `pack add
skill|knowledge`; `pack use` (Tier 0 — no prompt, nothing to trust); `pack
ls|show|rm`; `run --pack`; local + git-pinned; the onboarding
lazy-personal-pack flow with the commit GUARDED (falls back to
staged-not-committed if `user.email` is unset — never fails mid-onboarding);
a single active pack is your whole context (memory scoped per pack); and
**reference-only integrations + credential solicitation** — a pack declares
a capability it uses (remote/existing MCP, gog) + the ENV VAR names it
needs, and adoption/onboarding walks the user through `pix secret set
<VAR> op://…` (no pack code runs, so this was v1-safe).

**v2 (shipped, see `packs-v2.md` / `packs-v2-impl.md`):** an MCP facet that
ATTACHES (not just declares); in-sandbox CLI-proxy wrappers (`bin/`);
host-mode CLI wrappers for devices the sandbox can't reach; the
work/personal switch swapping ALL facets atomically; the Tier-1 host
bill-of-materials trust gate + SHA-pin for pack-shipped binaries; and
knowledge bundles that are shared-vs-private and usable standalone.

Still deferred: registry/marketplace; signing PKI; pack-to-pack dependency
resolution; a `pack
publish` verb (git is the sync); a GUI.

## 13. Open questions — resolved (owner)

1. **Ordered active pack stack.** `pix setup --pack A --pack B` composes
   collections by union and scalar declarations by last writer, while the
   host-owned activation ledger retains per-pack removal attribution.
2. **Provenance/signing: deferred past v2.** Tier-0 packs are markdown +
   refs (no pack code runs), so trust there is the git remote, like `npm
   install` from a GitHub URL. Host-executing packs get the Tier-1 gate
   (§9) now; signing is future hardening on top of that.
3. **Personal pack is local; the user pushes to git themselves.**
   pix's involvement ends at `git init` — it never adds a remote,
   never pushes, and never even OFFERS to. Remotes, cross-machine sync, and
   backup are the user's own git workflow, unmediated (matches "skills
   belong in git, the user checks them in").
4. **Name is `pack`.** Retire user-facing "kit"/"bundle" overlap (kit = sbx
   sandbox image config; a pack *contains* an OKF bundle).
5. **The only host-side extension point is the generic, SHA-pinned
   `[plugins.*]` external-process mechanism** (§7) — packs never need it,
   and extending `pix-host`'s own binary is a maintainer concern
   outside the pack path entirely.

## 14. Prior art (borrow / avoid)

devcontainers (borrow declarative+composable features; avoid
rebuild-on-change — a pack edit must be instant via dev-mode live-mount).
dotfiles/chezmoi (borrow "it's just git"; avoid a templating DSL). VS Code
profiles (borrow one-action import/export + runtime switch; avoid opaque
non-diffable blobs). Nix home-manager (borrow single source of truth; avoid
the learning curve — no new declarative language before writing a skill).
oh-my-zsh (borrow "drop a file in a folder, add a line, it's active"; avoid
eager-loading every plugin — connect MCP/proxies lazily, preserving the
MCP-opt-in default).
