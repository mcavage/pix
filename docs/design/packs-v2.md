# Packs v2 — portable capability context (DELIVERY PRD)

Status: BUILD TARGET (facets shipped; historical below — see note). North star is
`docs/design/packs.md` (the locked design); this is the delivery spec for the
facets v1 deferred, with a concrete owner use case as the acceptance test. v1
shipped a Tier-0 slice (skills + knowledge + reference-only integrations, one
auto-created personal pack). This closes the gap that IS the job-to-be-done:
**flip work↔personal as one portable context carrying my MCPs, my CLI wrappers,
and my config.**

**U08f note:** the facets below (MCP attach, `bin/` wrappers, knowledge
bundles) shipped and still work exactly as described; only the authoring UX
changed. `pix pack add mcp|proxy|knowledge <name>` cited throughout as HOW you
author a facet is retired — you write the `pack.toml` stanza (and any
`bin/<name>` script) by hand instead. See `docs/design/packs.md`'s retirement
callout for the full record.

## The job-to-be-done (owner, verbatim intent)

"Bring my whole agent context in one move, and flip work↔personal." Concretely:

- **Personal context:** the Fastmail MCP + a custom `platformio` CLI wrapper (the
  owner writes it) + personal config.
- **Work context:** work MCPs + a wrapped `warehouse` CLI + work config.
- **One command to switch.** Adding a capability is one command + one file.
- **Shareable at work:** hand a teammate the work pack; they put their OWN creds
  in (`op://` refs). Some knowledge travels with it (team truth); some stays
  private to the owner and is never in the shared pack.

If a first delivery does not let the owner flip between those two and have the
right MCPs + wrappers + knowledge live, it is not done.

## Scope of this build (the deferred facets)

Everything here is defined in packs.md; v1 punted it. This delivers it.

### F1 — MCP facet that ATTACHES (not just declares)
`pix pack add mcp <name>` records an MCP the pack needs + the ENV VAR names
(never values). On `pack use` / `run --pack`, its MCPs are registered with the
sbx gateway and enabled for the sandbox. Because registration ≠ in-session attach
(AGENTS.md), any add/switch that changes the MCP set prints "recreate the sandbox
to attach" in the same breath, and `pack use` on an active sandbox tells the user
the one command to recreate. Creds are solicited as `op://` refs at adoption.

### F2 — In-sandbox CLI-proxy wrappers (`bin/`)
`pix pack add proxy <name>` scaffolds a wrapper under the pack's `bin/`. On
launch, the active pack's `bin/` is mounted onto PATH in the sandbox (the proven
mixin-kit `files/` mechanism, made first-class). This covers the `warehouse` wrapper
(network-only, runs fenced in the sandbox — §9 "in-sandbox, safe by default").

### F3 — Host-mode CLI wrappers (the platformio case) — IN SCOPE
`platformio` needs a real device (`/dev/tty*`), which the sandbox structurally
cannot reach, so an in-sandbox `bin/` wrapper can't serve it. A pack therefore
carries a distinct **host-mode wrapper** facet: wrappers marked host-only are
installed into the host agent dir and put on PATH for `pix host` (never the
sandbox). This is host execution, so it is gated (see F5). `pack add proxy
<name> --host` marks a wrapper host-only.

### F4 — Work/personal switch swaps ALL facets
A single active pack IS the context (profiles are already deleted). `pix pack
use work|personal` (and `run --pack`) must swap, together: MCP set, `bin/`
wrappers (sandbox + host), config (gog_account, routing/model prefs), knowledge
scope, and the memory scope tag. Switching is one command. `pack ls` shows which
is active; `pack show` shows a pack's full facet inventory + host BoM.

### F5 — Trust gate for host execution (Tier-1)
Adopting a pack that ships anything that RUNS on the host (an MCP command, a
host-mode wrapper, an external binary) halts at a bill-of-materials screen
enumerating exactly what will run on the host + net-egress domains, requires
explicit `[y/N]`, fails closed on non-TTY unless `--yes`. Any external host
binary is SHA-pinned and re-hashed before launch (mechanism exists in
`services/host/plugin`). Tier-0 packs (skills/knowledge/refs only) still adopt
with no prompt. Signing is deferred; v1-of-this trusts the git remote.

### F6 — Knowledge: shared vs private, bundle usable standalone (owner #3)
Knowledge bundles are first-class and standalone — indexed by the knowledge
service on their own, usable with or without any pack. A pack REFERENCES bundles
(does not only embed them), per entry, with a `shared` flag:

- `shared = true` (a git URL): travels with the pack; adopters pull the team
  bundle.
- `shared = false` (a local path): does NOT travel; when the pack is shared, the
  owner's private knowledge is simply absent from it. It still works locally
  because it is a standalone bundle.

Embedding (a literal `knowledge/` dir in the pack repo) remains supported as the
simplest "shared, travels-in-the-repo" case. `pack add knowledge <name> [--ref
<git-url|path>] [--private]` chooses embed vs reference and shared vs private.

## Acceptance criteria (the owner's 5/5)

1. `pix pack use work` → in a fresh sandbox: the `warehouse` wrapper is on
   PATH and the work MCPs are attached. `pix pack use personal` → the
   Fastmail MCP is attached and `platformio` is usable in host mode
   (`pix host`). One command flips between them.
2. Add any capability with one command + one file of record:
   `pack add mcp|proxy|skill|knowledge <name>`.
3. Every credential is an `op://` ref, never a value on disk or in the VM.
4. Sharing: the owner hands over the work pack; a teammate adopts it, is shown
   the host BoM, puts their own creds in, and gets the shared team knowledge —
   while the owner's `--private` knowledge never left the owner's machine.
5. Open-core guard holds: nothing company-specific is committed to the public
   tree; company packs live in the user's own git remotes.

## Non-goals (this delivery)

Registry/marketplace; signing PKI; multi-pack composition beyond additive
stacking; pack-to-pack dependency resolution; a `pack publish` verb (git is the
sync); a GUI. Host-executing integrations ship as containers or host daemons,
never a compiled-in Go extension; see `packs.md`.

## Phasing (owner: "whatever you want, not using it until done")

- **Phase 1 — the flip, sandbox facets:** F1 (MCP attach) + F2 (sandbox `bin/`) +
  F4 (switch swaps MCP/bin/config/knowledge-scope/memory-scope) + F6 (knowledge
  shared/private references) + Tier-0 adoption. DoD after P1: work-pack warehouse
  + both MCP sets + the work/personal flip all work in-sandbox.
- **Phase 2 — host execution:** F3 (host-mode wrappers → platformio) + F5 (Tier-1
  trust gate + SHA-pin + BoM screen). DoD after P2: platformio usable via
  `pix host --pack personal`; adopting a host-exec pack is gated.

## Parallel, separate tracks (not packs, but owed)

- **Onboarding B:** remove the completion checklist entirely; onboarding = a
  clean (anti-slopped, house-voice) opening + do the real task + teach a
  capability ONLY on a real trigger (repeat a preference → offer to save). No
  state machine, no forced coverage, packs/knowledge trigger-only. Retire the
  `onboarding_progress` tool + the checklist marker.
- **Reference manual (owner #1):** real user-facing docs — a capability reference
  for eager learners — since onboarding intentionally teaches little proactively.
  Covers memory, the skills/flows, the crew, packs (work/personal), knowledge,
  host mode, MCP. House voice, no slop.

## How this gets built (accountability)

Crew builds against THIS spec; the orchestrator validates every output against
the acceptance criteria before accepting (the v1 drift was designing against a
thinner target). Architect owns the genuinely hard design (F3 host-mode wrapper
install path + F5 trust gate + F4 atomic multi-facet switch); engineer implements
per phase; cross-vendor review + QA gate against the DoD each phase.
