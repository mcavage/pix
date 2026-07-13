# pi-stack — the "best stack, period" upgrade: master plan

Synthesis of a six-lens design pass (architect, security-lead, dx-consultant,
product-manager, ux-copywriter, growth-marketing). Source docs live beside this
file in `.pi-agent/upgrade/`. Scope is full, not MVP; phases are for sequencing,
not for cutting.

North star: pi-stack is the single most compelling reason to install Docker
`sbx`, and going deeper naturally pulls in Cloud MCP Gateway and hosted
sandboxes. The wedge (growth): **the only coding agent you can run fully
full-auto, because the disposable VM is the safety boundary — there's nothing on
the host for it to destroy.** Full-auto is safe here and nowhere else, so trying
pi-stack *is* adopting sbx.

---

## 1. The core architectural move: plugins at process-launch, not link, time

Today the only host extension seam is compile-time: overlay `overlay_*.go`
symlinked into `services/host/`, self-registering via `init()` into
`extraCommands`/`extraServiceFactories` (`main.go:30-35`, `serve.go:39-41`,
`make link-overlay`). That is the contradiction the earlier review named — a
prebuilt public binary can't "carry" private overlay code, because carrying =
linking.

**Re-base the host on `hashicorp/go-plugin`.** Each host capability becomes an
out-of-process plugin the supervisor launches over a local unix-socket gRPC,
never links. Three interfaces, each a 1:1 refactor of existing code:

- `MemoryStore` (Remember/Recall/Forget/Synthesize/Promotable/Stats/Health) ← `memory.go`
- `CredentialBroker` (`Mint(audience,scopes)` / `Check` / `Describe`) ← generalizes `gwstoken.go` + the `:11442` exec-proxy pattern
- `McpServer` (Info/ListTools/CallTool) ← `slack.go` + `util.go`

One `Handshake{ProtocolVersion, MagicCookie}` gates all three; a skewed plugin
is refused at launch. **Built-ins run via self-exec** (`pi-stack plugin memory`),
the pattern `subagents.ts` already uses, so no extra binaries ship by default and
reverting an override is a config flag, not a redeploy. `serve` becomes the
supervisor: launch → preflight `Check()` → health/backoff-restart →
`CleanupClients()` on shutdown.

Why this is the keystone: it dissolves the open-core contradiction (public binary
+ private sibling plugin, never linked), lets *anyone* override memory / creds /
MCP, and the open-core boundary gets **stronger** (private code = a separate
binary in a private repo, never a symlink in the public tree).

**Security gate on this move (non-negotiable, from the threat model):** plugins
are opt-in and pinned by absolute path + SHA-256 in an explicit manifest, no
auto-discovery from PATH/cwd/kit/network; signature-verified (cosign/minisign),
failure fatal; run as the user, never elevated; **spawned once at startup from
the pinned manifest, never in response to VM or network input**; RPC over
owner-only UDS (0600) or AutoMTLS. The honest caveat: "go-plugin everything"
partially reintroduces the daemon-spawns-child shape the project chose Go to
avoid — requirement "static, startup-time, signed-manifest spawns" is the single
thing that keeps it EDR-safe. If a plugin spawn ever becomes request-triggered,
the redesign has recreated the backdoor pattern and must stop.

**Credential-broker-as-plugin is the highest-risk element** and defensible only
under the full guardrail set — especially: brokers never emit raw secrets over
RPC, only short-lived minted tokens (`gwstoken.mint()`, `gwstoken.go:76`); broker
plugins on a stricter allowlist than feature plugins. Get this or the
request-time-spawn question wrong and the host trust boundary collapses.

## 2. One binary, one verb tree (repo-less consumer, dev still works)

`bin/pi-stack` is repo-relative so it can't just become the prebuilt binary.
Resolution: **one self-contained `pi-stack` launcher** (+ co-installed
`pi-stack-host`) in `~/.local/bin`, config in `~/.config/pi-stack/`, shelling out
to `sbx`. `--dev` becomes a *flag* that layers the old Mode-B repo-relative
behavior only when a checkout exists (cwd repo / `PI_STACK_DEV_ROOT` / config),
failing loud otherwise. The Makefile demotes to a contributor wrapper
(`make run: ; pi-stack run --dev .`). One product, one seam.

```
pi-stack [DIR]                       Launch in DIR (bare = the 90% path)
pi-stack run [DIR] [flags] [-- ...]  --dev --skills DIR --kit --mcp --name --model
pi-stack setup                       Resumable, idempotent onboarding wizard
pi-stack doctor                      Health + copy-paste TODOs
pi-stack serve [SERVICE...]          Host services (plugin supervisor), foreground
pi-stack version | help [VERB]
pi-stack models pull | ls
pi-stack mcp register [NAME...] | ls | auth
pi-stack config show | edit | path
pi-stack secrets
pi-stack upgrade | uninstall
```

Daily surface first; `models`/`mcp`/`config` are the "wiring a data tool" tier
you meet only when `doctor` sends you there; `load`/`publish` stay
contributor-only in the Makefile.

- **Config to XDG** `~/.config/pi-stack/config.toml`: `version_pin`,
  `[kits].stack` (N stacked mixin kits), `[skills].paths` (consumer BYO skills,
  live-mounted + hot-reload without a clone), `[plugins]` slot→impl+sha. Survives
  upgrade/uninstall, never clobbered (seed only if absent).
- **`pi-stack setup`**: resumable/idempotent wizard, prompts only for what's
  missing (secrets via `sbx secret ls`, Ollama+models, gws auth, op/op-refs, MCP
  registration), reuses `doctor`'s copy-pasteable `TODO: <cmd>` grammar, writes
  only op:// refs / secret handles (never resolved secrets) at 0600.
- **`--skills DIR` live-mount** for consumers: "point at my skills, edit live"
  with no checkout and no Mode-B, composing predictably (baked < kit < live
  mount). *Verify:* does a later `--skill` root with a same-named `SKILL.md`
  shadow a baked one or error? (load-bearing unknown for the compose rule.)
- **Installer**: fetch a prebuilt `pi-stack`/`pi-stack-host` from GitHub
  Releases, verify published SHA-256 (ideally cosign) before exec, no sudo, no
  silent daemonize, must NOT broker 1Password. Ship `upgrade` + `uninstall`.

## 3. Version coupling by construction

`pi-stack run` wraps `sbx run` and pins the git kit to the binary's OWN embedded
release, so `pi-stack@X` launches image `X` — skew impossible, no `doctor`
after-the-fact check needed. **Load-bearing unknowns to verify in a Phase-0
spike** (confirmed real gaps): `publish.yml` never runs `git tag` (only commits a
version bump to `main`), and the git-kit URL only ever uses `#dir=pi-kit` with no
ref. Both must be built + verified; proven fallback is pinning the image via
`--template :<version>` (already how `make run` works).

## 4. Skills you can find and names that make sense

**In-agent discoverability** (`extensions/help.ts`): `/help` live-enumerates the
LOADED set via `pi.getCommands()` (source skill/extension/prompt) + the
agents-dir frontmatter parse `subagents.ts` already does + `capabilities.json`,
so it can never go stale. `/getting-started` is a guided first-run tutorial. A
one-time first-turn nudge fires on `session_start` via `ctx.ui.notify` (not
`sendMessage` — avoids the assistant-prefill 400), gated by a marker file.

**Naming taxonomy** (name for the user's intent verb; shared prefix only when 3+
skills need distinguishing; no `wf-`, no jargon). Proposed workflow renames — the
uncontentious ones are settled; the starred ones need your call:

| Current | Proposed | Note |
|---|---|---|
| `investigate` | `debug` | clear win |
| `document-release` | `docs-sync` | clear win |
| `write-one-pager` | `one-pager` | clear win |
| `microcopy-patterns` | `microcopy` | clear win |
| `competitive-analysis` | `competitive` | clear win |
| `promote-learnings` | `improve` | clear win |
| `wf-prototype` + `prototype-webapp` | `prototype` | MERGE — same job |
| `wf-product` | `product` | full multi-role, user gate at PR/FAQ |
| `autoplan` | `plan` | light, auto-gates |
| `setup-user` | `setup` | ⚠ collides with the `pi-stack setup` CLI verb — decide |
| `spec` | `build` * | ⚠ `spec` is a well-understood term; "build" may be worse |
| `wf-engineering` | `build-full` * | ⚠ build/build-full distinction is weak |
| `code-review` | `review` * | ⚠ collides with the `review` AGENT — confusing |
| `review-gate` | `peer-review` * | pairs with above |
| `self-audit` | `check` * | ⚠ "check" is very generic |

Migration touches cross-refs in AGENTS.md, README, several SKILL.md files,
prompts/, docs/design/ — enumerated in the naming doc; do them atomically per
rename.

## 5. OKF: augment memory, don't replace it

Keep the memory loop as-is; add OKF as a **second, separate grounding surface**.
They share the `before_agent_start` injection point (both ground the agent
automatically) but stay separate stores, write paths, token budgets, and labels
("from memory: context, may be stale" vs "from the knowledge base: authoritative,
cited to `/tables/orders.md`"). Two commands: `/recall` (memory) and `/knowledge`
(bundle). They can't merge because they disagree on every axis that matters:
owner (me vs org), authority (inferred vs curated), write path (auto watcher vs
gated PR), lifecycle (decay vs git history), privacy (private vs shareable).

OKF's four jobs memory structurally can't do: (J1) ground me in shared domain
truth that predates me; (J2) let a team review/version knowledge and another org
consume it; (J3) answer from human-reviewed authoritative facts with citations;
(J4) traverse a large domain top-down (index → drill in). Scoping test: if a use
maps to none of J1–J4, it belongs in memory.

Shape: `knowledge` is a new capability in `capabilities.json` (public profile =
`none`; an overlay wires a git-mounted bundle — ship the pointer, not the
content, so `git pull` updates it with no image rebuild). Consumption = the
running agent (permissive read). Enrichment = a gated `enrich-knowledge`
skill/subagent that opens PRs; the watcher never writes OKF. Three phases:
(1) read-only consume skill + a `files` provider kind, no host code; (2) a
`pi-stack-host knowledge` plugin indexing into FTS5 + embeddings, merged into the
recall injector; (3) gated write-back + a `/learnings`→OKF promotion bridge.

## 6. Growth: the Docker flywheel

```
demo GIF / Show HN (title = the wedge sentence)
  → engineer installs sbx to try pi-stack        (sbx is mandatory = adoption)
  → daily use → wants Slack/Linear/Notion        → wires Cloud MCP Gateway
  → shares a mixin kit with the team             → teammates install sbx
  → team wants shared memory + portability       → hosted sandboxes + cloud
```

Top 3 moves: (1) **record the demo GIF** — `ship` end-to-end, tests → GPT
argues against Claude's diff → PR opens, zero prompts; it's the only thing
missing and it turns the claim into evidence. (2) **`/getting-started`** that
completes one real `investigate → ship` loop in <10 min (fixes the post-install
"what do I type" drop-off). (3) **publish one polished public mixin kit**
(`pi-stack-kit-oss`: issue triage, changelog, contributor messages) aimed at OSS
maintainers — staff+ engineers who work in public, so each "I use this" reaches
the exact ICP, and each install requires sbx. Extensibility (plugins + kits +
OKF) is the ecosystem/distribution loop: each published kit is an ad for pi-stack
+ sbx. Anti-patterns: no landing page, no vanity metrics, no fake community, no
no-keys free tier.

---

## Review-gate verdict: RESEQUENCE (folded in below)

A cross-vendor adversarial review (gpt-5.6-sol) refuted the maximalist framing.
Keep the ambition; fix the order. Its four load-bearing corrections, all
accepted:

1. **Don't "go-plugin everything."** The compile-time `init()` overlay
   (`main.go:33-35`, `serve.go:36-37`, `docs/OVERLAY.md:57-89`) is ~30 lines,
   proven, and fine for Mark's private case. go-plugin only earns its cost
   (gRPC, handshake, cosign, manifest, supervisor, restart) where a THIRD PARTY
   is actually demanded to override a slot — and there's zero evidence anyone but
   Mark writes host plugins yet (the growth doc's own ecosystem test is "5+
   external kits in 90 days," i.e. unproven). Keep `init()` overlays; add
   go-plugin to ONE slot (memory) behind a flag, only after a real ask.
2. **The 12 security non-negotiables are correct FOR a public plugin ecosystem,
   and a shipping trap if required for the launcher.** Split them: mandatory now
   = enforce loopback binds + refuse non-loopback without auth + make gws-token
   auth easy. Mandatory *before third-party credential plugins* = the full 12.
   NOT a prerequisite for the repo-less launcher = cosign, plugin manifest,
   broker allowlists.
3. **`--template` is NOT equivalent to version coupling.** It pins the image
   only; the kit (`agentContext`, network allowlist, credentials, entrypoint,
   capabilities, skills, extensions in `pi-kit/spec.yaml:59-190`) still drifts on
   `main`. If sbx can't pin a git-kit ref, the real fallback is ship the matching
   `pi-kit` WITH the release and run it as a local kit — never fetch kit files
   from floating `main` and call it coupled.
4. **The blind spot all six specialists shared: none of this makes the agent
   better at CODING** — the actual job. It's all harness/distribution/naming/
   growth. Before a demo GIF + Show HN, the core loop (`ship`, `code-review`,
   `investigate`, patch quality, test execution, failure recovery) has to be
   tight enough to survive scrutiny. A flashy GIF of a brittle harness backfires.
   **Add a workstream that hardens the coding loop, and gate the growth launch on
   it.**

Net: this is really three projects (repo-less productization / host-plugin
architecture / knowledge+ecosystem+growth) that don't share a critical path.
Sequence them; don't ship them fused.

## Critical path (resequenced)

1. **Verify sbx kit-ref behavior** (Phase 0 spike). If unsupported, build the
   release-local-kit fallback (not `--template`).
2. **Repo-less `pi-stack`**: binary + `run` (version-pinned per #3) + `serve` +
   `doctor` (lead verdict) + `setup` + XDG `config.toml` + signed installer +
   `upgrade`/`uninstall`. The one thing users actually need.
3. **`/help` + `/getting-started`** (`extensions/help.ts`) + first-turn nudge.
4. **Harden the coding loop** (the blind-spot workstream): tighten `ship` /
   `code-review` / `investigate` / `tdd` / `verify`, measure patch + review
   quality, failure recovery. This is what makes it "the best stack, period."
5. **Record the demo GIF** — only after step 4 makes the loop demo-worthy.
6. **Then, on proven demand:** one memory go-plugin behind a flag; skill renames
   (atomic, deferred churn); OKF consume→plugin→write-back; public OSS kit +
   launch.

The minimal loopback-auth hardening (security #7/#8, downscoped) rides along in
step 2; the full 12 non-negotiables are gated to step 6's plugin work.

## Original phasing (superseded by the critical path above; kept for detail)

- **Phase 0 (spike, unblocks everything):** verify git-kit ref-pinning + add CI
  `git tag` per release; prototype one `go-plugin` slot (memory) with the
  handshake + self-exec built-in behind a config flag (reversible).
- **Phase 1 (repo-less consumer):** prebuilt binary + signed installer +
  `upgrade`/`uninstall`; XDG `config.toml`; `pi-stack` verb tree; `setup` wizard;
  `doctor` with a lead verdict; `pi-stack run` version-coupled launch.
- **Phase 2 (discoverability + naming):** `extensions/help.ts` (`/help` +
  `/getting-started` + first-turn nudge); execute the settled skill renames
  atomically (hold the starred ones for your call).
- **Phase 3 (plugin-ize the rest):** memory/creds/MCP all go-plugin under the
  full security guardrails; `--skills` live-mount; published-mixin-kit stacking
  from `config.toml`.
- **Phase 4 (knowledge + ecosystem):** OKF consume → plugin → gated write-back;
  publish `pi-stack-kit-oss`; record the demo GIF and launch.

## Decisions — LOCKED (2026-07-12)

1. **go-plugin from the get-go**, and the **credential-broker is a pluggable
   slot** (not built-in-only). Justification the reviewer said was missing: Mark
   uses a Snowflake proxy at work but not at home — a real, demanded per-site
   broker override. The existing `:11442` overlay exec-proxy (the `snow` broker,
   `pi-kit/spec.yaml:104-108`, `AGENTS.md:132,153`) becomes the **reference
   `CredentialBroker` plugin**, which also validates the interface against a real
   implementation. This overrules the review gate's "no demand for plugins" and
   its "keep init() overlay" recommendation — go-plugin is the architecture.
2. **Request-time spawn: confirmed** hard rule — plugins spawn ONLY at startup
   from the signed/pinned manifest, never in response to VM or network input.
   This is the single invariant that keeps the design EDR-safe.
3. **Per-sandbox bearer auth: brokers only.** Require a bearer on credential
   brokers (gws-token, snow, any `CredentialBroker` plugin) — they cross to real
   external creds and are the prompt-injection exfil target. Memory stays
   unauthenticated for now (local, disposable, low-stakes). Token mechanism:
   `serve` writes a per-sandbox token to `~/.config/pi-stack/`, `run` injects it
   as a sandbox secret, the in-sandbox wrapper sends it as the bearer.
   **HARD CONSTRAINT: zero friction — the user never sees, types, or manages this
   token.** It is minted, injected, and sent entirely by the launcher + wrapper.
   Every launch path (`pi-stack run`, `make run`, `bin/pi-stack`) MUST route
   through one shared token-injection helper so a broker call just works; a
   missing token is a launcher bug, never a user-facing auth prompt.
4. **Skill renames: deferred** until the architecture is locked, then done
   atomically. (Uncontentious ones settled; starred ones still open.)
5. **OKF bundle mount: git-mount** at sandbox create (needs an allowlist entry),
   so `git pull` updates it with no image rebuild.
6. **Version coupling: git-ref, CONFIRMED viable.** sbx kit URLs support
   `#ref=<tag>&dir=pi-kit` (Docker kit docs). `pi-stack run` pins the kit to its
   own embedded release tag; CI must add `git tag v0.0.<n>` at release
   (`publish.yml` currently only commits the bump). No `--template` exposed.
   Empirical check still owed on Mark's sbx build: `sbx run pi-stack
   --kit "git+...#ref=main&dir=pi-kit" .` — if it boots, tags work; if not,
   fallback is a release-local `pi-kit/` + `--template :version` under the hood.

## (historical) Decisions that were open

1. **Credential-broker-as-plugin:** ship it under the full guardrail set, or keep
   the credential broker built-in-only (not override-able) and make *only*
   memory + MCP override-able? (Security says broker is the highest-risk seam.)
2. **Request-time spawn:** confirm the hard rule "plugins spawn only at startup
   from a signed manifest, never on VM/network input." Everything EDR-safe hinges
   on this.
3. **Local auth on host services:** require a per-sandbox bearer for memory +
   gws-token even over loopback (security wants yes; adds setup friction)?
4. **The starred skill renames** (spec→build, wf-engineering→build-full,
   code-review→review, self-audit→check, setup-user→setup): approve, tweak, or
   keep current?
5. **OKF bundle mount:** git-clone-at-create (live, needs allowlist entry) vs
   baked into the kit (static, rebuild to update)? PM leans git-mount.
6. **Verify (Phase 0):** does sbx honor a ref in the git-kit URL, and does a
   later `--skill` root shadow a same-named baked skill or error?
