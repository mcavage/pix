# Packs v2 — implementation design (BUILD-READY)

Status: BUILD SPEC. Validated against the real tree (`pack.go`, `config/config.go`,
`sbxargs.go`, `hostrun.go`, `mcp.go`, `knowledge.go`, `run.go`, `serve_plugin.go`,
`extensions/memory-recall.ts`). North star: `docs/design/packs.md`. Delivery PRD:
`docs/design/packs-v2.md`. This is the doc the owner validates before the crew builds.

Frameworks used: ADRs (Nygard) for the 4 hard calls, a C4 component sketch for the
switch path, fitness functions for the boundaries that must not regress, and
lightweight ATAM tradeoff notes on each hard decision.

---

## AS-SHIPPED ADDENDUM (supersedes the `pack.lock`-based trust design below)

Both phases shipped and passed cross-vendor review + security 5/5. The build
hardened the trust model well past this original spec; where they conflict, THIS
addendum is authoritative (the sections below are the pre-review design intent):

- **Trust acceptance AND activation provenance live in a launcher-owned host-state
  store, NOT in the pack's `pack.lock`.** A pack-supplied `pack.lock` is
  attacker-controlled (a downloaded/ZIP'd pack), so nothing security-relevant is
  ever read from the payload. The store is `<config-dir>/pack-trust.json`
  (`packtruststore.go`): symlink-refused on read AND write, atomic, and every
  read-modify-write is serialized by a cross-process **flock** (so `pack use`
  racing `pix host` can't clobber state or orphan a host wrapper). `pack.lock`
  remains only a local hint, never trusted; a pack-supplied one is scrubbed on
  adoption; a one-time migration lifts a NON-adopted (local/authored) Phase-1
  `pack.lock` activation into the store.
- **The Tier-1 gate keys on a canonical, injective host-exec FINGERPRINT** (sorted
  structured JSON over: each MCP's resolved argv, each host-proxy's script
  content-sha, each `[[bin]]` name+sha+host, egress, cred names) — not name-only
  coverage. Any host-exec change (incl. a mutated host-proxy script or a changed
  `gog_account` argv) re-gates; acceptance is keyed by stable identity
  (remote-without-commit, or canonical path) so a same-fingerprint new commit does
  NOT re-prompt.
- **Host proxies are content-pinned like `[[bin]]`** (re-hashed at install and
  every host launch; a mutated script refuses to launch until re-accepted).
  Install is a staged + atomic dir swap under the store lock; strict/launch fails
  closed on any skipped/failed/never-accepted item (no half-installed set, no
  orphan on PATH).
- **Local-vs-remote MCP classification fails CLOSED:** an unknown/unprobeable
  classification is treated as host-exec (Tier-1, gated), never silently Tier-0.
  A remote gateway-catalog reference (and gog) stays Tier-0.
- **All `<workspace>/.pix/*` writes AND removes are symlink-safe**
  (`workspacestate.go`): refuse a symlinked `.pix` dir, atomic temp+rename
  that replaces a symlinked destination rather than following it.
- **Known residual (deliberate):** a hard kill (SIGKILL) in the tiny window
  between the atomic activation-store write and the atomic `cfg.Save` can
  over-retain a pack's own contribution (no user data removed; lock-only removal
  protects hand-added entries). Recovered by re-activating or a manual config
  edit. Ordinary failures (read-only dir, disk full) roll back cleanly.

User-facing map: `docs/reference.md`.

---

## 0. What already exists (do not rebuild)

v1 shipped the Tier-0 slice. These are the seams v2 extends, not replaces:

| Seam | File | v2 role |
| --- | --- | --- |
| `packManifest` / `loadPack` | `pack.go` | add facets to the struct + loader |
| `applyPackToLaunch(cfg,*runOpts,env)` | `pack.go` | the per-launch mount point; grows bin + mcp |
| `runPackUse` | `pack.go` | the switch verb; grows the atomic swap + trust gate |
| `buildSbxArgs` | `sbxargs.go` | `--kit`, `--mcp`, `--skill`, workspace; append the pack kit |
| `registerServers` / `mcpRegistrar` | `mcp.go` | already builds `sbx mcp add … op run`; drive it from a pack |
| `AddKnowledgeBundle` / `RemoveKnowledgeBundle` | `config.go` | pack knowledge swap already uses these |
| `verifyPluginSHA(spec)` | `serve_plugin.go` | the SHA-pin + re-hash primitive for F5 |
| `hostAgentDir` / `provisionHostAgentDir` / `runHostSetup` / `hostChildEnv` | `hostrun.go` | F3 host wrapper install + PATH |
| `.pix/profile` scope file | `memory-recall.ts` L82, `memory-capture.ts` | the memory scope tag mechanism (F4) — already read in-VM; run.go currently deletes it |
| `.pix/knowledge.scope` | `run.go` `wireKnowledgeScope` | knowledge scope, unchanged |
| sparse `Save()` | `config.go` | every new config key MUST default-omit |

Two facts that shape everything:

1. **`--mcp` and `--kit` are create-only.** `buildSbxArgs` emits them; `buildReattachArgs`
   does not. A running sandbox cannot gain an MCP or a `bin/` mount without a recreate.
   This is not a bug to fix; it is the sbx model. Every facet that lands in the sandbox
   is therefore "recreate to attach."
2. **The memory scope tag already works in-VM.** `memory-recall.ts` reads
   `<cwd>/.pix/profile`; the only reason switching packs doesn't switch memory scope
   today is that `run.go` *deletes* that file. F4 is mostly "write it again, keyed on the pack."

---

## 1. Component sketch (C4 level 3 — the switch path)

```
                       pix pack use <path|git-url>
                                   │
              ┌────────────────────┼───────────────────────────────┐
              ▼                    ▼                                ▼
      loadPack(root)        computeHostBoM(pack)              solicitPackCredentials
      (pack.toml +          (F5: mcp cmds, host              (F1/existing: op:// refs
       facets)               wrappers, ext bins,              → op-refs.env, values in 1Password)
                             SHAs, egress)
              │                    │
              │             Tier-0? skip.  Tier-1? BoM screen → [y/N], non-TTY fail-closed
              ▼                    ▼
      ┌─────────────────── packSwap (single transaction) ───────────────────┐
      │  cfg.Pack = root                                                     │
      │  swap MCP set:   remove oldPack.mcp from cfg.MCP; add newPack.mcp    │
      │  swap knowledge: RemoveKnowledgeBundle(old); AddKnowledgeBundle(new) │
      │  swap config:    gog_account, ollama_bridge_model, routing overrides │
      │  cfg.Save()  ── ONE write, source of truth                          │
      └─────────────────────────────────────────────────────────────────────┘
              │                              │                         │
              ▼                              ▼                         ▼
   registerServers(cfg, pack.mcp)   propagateServeConfig       "recreate the sandbox
   (sbx mcp add … op run)           (reindex knowledge)         to attach: pix run
   HOST, best-effort                 daemon restart              --replace" (create-only facets)

                       next `pix run` (create):
   buildSbxArgs → --kit <pack-kit>(bin/) --mcp <pack.mcp…> --skill <pack/skills>
   run.go → write .pix/profile (memory scope) + .pix/knowledge.scope
```

The **commit point for host-side facets** (config, gateway registration, knowledge index,
memory scope for the *next* run) is `cfg.Save()`. The **commit point for in-sandbox facets**
(bin, mcp attach, skills) is the sandbox recreate. Keeping those two commit points explicit
is what makes the "switch" honest instead of a lie about what is live.

---

## 2. `pack.toml` v2 schema (complete)

Additive to v1. Every array is `omitempty`. A Tier-0 pack (skills/knowledge/refs only) has
none of the executing facets and adopts with no prompt — exactly as today.

```toml
name   = "work"
schema = 1                       # unchanged; bump only on a breaking field rename

ollama_bridge_model = "qwen3.5:9b"   # v1, unchanged

# ── F4 context config (all optional, layered when the pack is active) ──
gog_account = "you@company.com"      # swapped into cfg.GogAccount on `pack use`
memory_scope = "work"                # → .pix/profile; default = pack name; "default" = shared
[routing]                            # optional pack-level model routing override (P2/stretch)
  policy    = "routing/policy.json"      # repo-relative; recompiled to routing.json when active
  scorecard = "routing/scorecard.json"

# ── F1 reference-only integrations (v1 shape, now ATTACHES) ──
[[integrations]]
  name   = "Fastmail"
  mcp    = "fastmail"            # MCP server name to attach (registered host-side)
  env    = "FASTMAIL_TOKEN"      # op:// ref var name solicited at adoption; value NEVER in pack
  static = true                  # preloaded at sandbox CREATE (--static-mcp) so the pack's
                                 # skills have its tools in context from turn one. A server not
                                 # preloaded is reachable via `pix mcp load <name>` on an
                                 # existing sandbox, or by recreating with `run --replace`.

# ── F2 in-sandbox proxy wrappers (bin/, fenced) ──
[[proxy]]
  name = "warehouse"            # scaffolds bin/warehouse; mounted → /usr/local/bin in the VM
  # host = false (default)      # sandbox-only
  # egress = ["warehouse.example"]  # declared net domains → BoM + kit allowlist hint

# ── F3 host-mode wrappers (host exec, gated) ──
[[proxy]]
  name = "platformio"
  host = true                   # installed into the host agent dir; on PATH for `pix host` ONLY
  # egress = []                 # host wrappers still declare egress for the BoM

# ── external host binary (Tier-1, SHA-pinned; rare, P2) ──
[[bin]]
  name = "fastmail-mcp"
  path = "bin/fastmail-mcp"     # repo-relative
  sha  = "9f2c…"                # MANDATORY; re-hashed before launch (reuses verifyPluginSHA)
  host = true

# ── F6 knowledge references (vs embedded knowledge/) ──
[[knowledge]]
  name   = "team-runbooks"
  source = "https://github.com/acme/runbooks.git#ref=main"  # git URL → travels
  shared = true
[[knowledge]]
  name   = "my-notes"
  source = "~/notes/okf"        # local path → does NOT travel
  shared = false                # == --private; absent from a shared pack, still works locally
```

Go struct additions in `pack.go` (`packManifest`):

```go
GogAccount   string           `toml:"gog_account,omitempty"`
MemoryScope  string           `toml:"memory_scope,omitempty"`
Routing      *packRouting     `toml:"routing,omitempty"`
Integrations []packIntegration `toml:"integrations,omitempty"` // exists
Proxies      []packProxy      `toml:"proxy,omitempty"`
Bins         []packBin        `toml:"bin,omitempty"`
Knowledge    []packKnowledge  `toml:"knowledge,omitempty"`
```

```go
type packProxy    struct{ Name string; Host bool `toml:"host,omitempty"`; Egress []string `toml:"egress,omitempty"` }
type packBin      struct{ Name, Path, SHA string; Host bool `toml:"host,omitempty"` }
type packKnowledge struct{ Name, Source string; Shared bool `toml:"shared,omitempty"` }
```

Parser hardening (extend the existing symlink/`safeArtifactName` posture): reject any
`Path`/`Source` that escapes the pack root (`..`, absolute-into-pack, symlink); reject a
`Name` that isn't `[A-Za-z0-9._-]`; `packBin` with empty `SHA` is a load error (fail closed).
Unknown TOML keys are already tolerated by the decoder — that is fine; the *typed allowlist*
(§9.4 of packs.md) is enforced because only these typed fields ever reach an exec path.

---

## 3. `config.toml` keys

**No new persisted keys are needed for the common path** — the whole point of "active pack IS
the context" is that pack facets project *into the existing keys* on `pack use`:

- `pack` (exists) — active pack root. `active_pack` in packs.md == this key. Keep the name `pack`.
- `mcp` (exists) — pack MCPs are added here on `pack use`, removed on switch (mirrors the
  knowledge-bundle swap already in `runPackUse`).
- `knowledge_bundles` (exists) — pack `[[knowledge]]` references resolve here.
- `gog_account`, `ollama_bridge_model` (exist) — overwritten from the pack, restored on switch.

**One new state file, not a config key:** to make the swap reversible we must know *which*
mcp/bundle entries a pack contributed (so switching removes exactly those and not a user's
manually-added ones). Store a small provenance record beside the pack, not in config.toml:

- `<PacksDir or PackDir>/<pack>/pack.lock` — generated, git-ignored-by-default, records the
  resolved contributions of the *last activation*: `mcp = [...]`, `knowledge = [<canonical ids>]`,
  `bin = [<name→sha>]`. `pack use` reads the *previous* active pack's `pack.lock` to compute the
  removal set, writes the new one. This is the `pack.lock` packs.md deferred, scoped down to
  "activation provenance," which is exactly what the atomic swap needs. It is not a
  dependency-resolver lockfile.

Rationale for not adding `active_pack` etc. as new keys: sparse `Save()` + the existing keys
already give a single unambiguous selection surface. Adding parallel keys would reintroduce the
"two authoritative places" bug packs.md §13 explicitly warns against.

---

## 4. The facets

### F1 — MCP attach

**Schema:** `[[integrations]]` (exists) with `mcp` + `env`. No new fields.

**Mechanism.** `runPackUse` gains, after `cfg.Save()`:

1. For each `integration.mcp`, `cfg.AddMCP(name)` (persisted, sparse). Record it in `pack.lock`.
2. Call the existing `registerServers(cfg, env, out, packMcpNames, findHostBinary)` — it already
   partitions local vs gateway-catalog vs gog, wraps in `op run --env-file`, and fails closed on
   an unresolvable local set. **No new registration code.** Reuse it verbatim; it already reads
   `SBX_MCP_URL` and degrades when the gateway is off.
3. `solicitPackCredentials` (exists) prompts for missing `op://` refs. Already TTY-gated, already
   op-installed-gated, already writes only refs. **No change.**
4. Print the recreate line **in the same breath** (PRD F1 + packs.md §13 must-fix):
   > pack `work` attached 2 MCP server(s) to the gateway. They attach to a sandbox at CREATE
   > only — recreate to pick them up:  `pix run --replace`

**`applyPackToLaunch` change.** Today it only *warns* about `integration.mcp` (it deliberately
does not auto-enable). In v2 the enabling happened at `pack use` (into `cfg.MCP`), so
`buildSbxArgs`' existing `--mcp` loop attaches them on the next create automatically — nothing
new in the arg builder. `applyPackToLaunch` keeps the missing-credential warning only.

**Switch removal.** On `pack use` of a *different* pack, remove the previous pack's `mcp` entries
(read from its `pack.lock`) with `cfg.RemoveMCP`, and `sbx mcp rm <name>` best-effort. A name that
another active source still needs is protected because removal is scoped to the old pack's lock.

**New/changed files:** `pack.go` (`runPackUse` + a `packMcpNames(*packInfo)` helper + `pack.lock`
read/write). `mcp.go` unchanged (reused). `config.go` unchanged (`AddMCP`/`RemoveMCP` exist).

**Security boundary.** An MCP server *runs on the host* (gateway-spawned). So declaring one is a
Tier-1 host-exec fact and feeds the F5 BoM. The credential is an `op://` ref only — the pack never
carries a value, the VM never sees it (`op run` resolves at gateway spawn, exactly as `slack`/`gog`
do today). A gateway-catalog remote MCP (notion/atlassian) is *referenced*, not run by the pack —
it is Tier-0 for the pack (nothing pack-authored executes) even though a remote server exists.

---

### F2 — in-sandbox `bin/` wrappers

**Schema:** `[[proxy]]` with `host` unset/false. `pack add proxy <name>` scaffolds `bin/<name>`
(0755, a `#!/usr/bin/env bash` shim template) and appends the `[[proxy]]` entry.

**Mechanism — reuse the proven mixin-kit `files/` mount, made first-class.** A pack's `bin/` is
not itself a kit, and mounting it as a bare workspace would not put it on PATH. So synthesize an
**ephemeral mixin kit** at launch that drops the sandbox wrappers into the image's existing PATH
dir:

- New `packKitDir(pack) string` = `<StateDir>/pix/pack-kits/<pack-name-hash>/`.
- New `synthesizePackKit(pack)`:
  - writes `spec.yaml` (`kind: mixin`),
  - for each non-host `[[proxy]]`, copies `bin/<name>` → `files/usr/local/bin/<name>` (0755).
    Copy, not symlink (the pack loader already refuses symlinks; sbx mounts a real tree).
  - returns the kit dir path.
- `applyPackToLaunch` calls it (create-time only, gated by the existing `willCreate` guard in
  run.go) and appends the dir to `o.Kits`. `buildSbxArgs` already loops `o.Kits` into `--kit`
  *before* `cfg.Kits.Stack`, so the pack kit stacks under any configured `cfg.Kits.Stack`, later-wins per packs.md §6.

`/usr/local/bin` is already on PATH in the DHI image and writable per AGENTS.md ("`/usr/local/bin`
may not exist → `mkdir -p`" — the mixin `files/` tree handles creation). So the wrapper is on PATH
with zero in-VM shim.

**Why an ephemeral kit and not `pack.kit/files/`.** packs.md §5 keeps `kit/files/…` as a raw
escape hatch. F2 wants `bin/` first-class (`pack add proxy` writes one file, no kit knowledge).
Synthesizing the kit from `bin/` at launch gives the first-class UX while *reusing the one proven
mount mechanism*. See ADR-2.

**Recreate UX.** Because `--kit` is create-only, changing `bin/` (adding a proxy, or switching
packs) requires a recreate. `pack add proxy` and `pack use` print the same recreate line as F1.

**New/changed files:** `pack.go` (`synthesizePackKit`, `packKitDir`, `runPackAdd` gains the `proxy`
kind + shim template). `run.go`/`pack.go` `applyPackToLaunch` appends to `o.Kits`. `sbxargs.go`
unchanged.

**Security boundary.** In-sandbox = constrained by DHI + the net allowlist. Safe by default
(packs.md §9). A wrapper that needs egress declares `egress = [...]`; that surfaces in the BoM and
tells the user which domains to add to the kit allowlist (a pack cannot silently widen the fence —
the sbx `network.allowedDomains` is kit-level and still requires a deliberate allow). A sandbox
`bin/` wrapper is **not** a host-exec fact and does **not** raise the trust tier.

---

### F3 — host-mode wrappers (platformio)

**Schema:** `[[proxy]]` with `host = true` (a wrapper script under `bin/`) or `[[bin]]` with
`sha` (an external binary). `pack add proxy <name> --host` sets `host = true`.

**Mechanism — install into the host agent dir, on PATH for `pix host` only.**

- New `hostPackBinDir()` = `filepath.Join(hostAgentDir(), "bin")` (state-flavored, rebuildable —
  matches the existing symlink posture in `provisionHostAgentDir`).
- New `installHostPackWrappers(pack)`:
  - for each `host = true` `[[proxy]]`, copy `bin/<name>` → `hostPackBinDir()/<name>` (0755);
  - for each `host = true` `[[bin]]`, **`verifyPluginSHA`-equivalent check first** (re-hash the
    file against `sha`; refuse on mismatch), then copy;
  - wrappers from a *previous* pack are cleared first (read old `pack.lock`), so the host `bin/`
    only ever holds the active pack's host wrappers (atomic swap, F4).
- `runHostLaunch` (`hostrun.go`) prepends `hostPackBinDir()` to `PATH` in the child env. Add to
  `hostChildEnv` (or just before exec): `"PATH=" + hostPackBinDir() + ":" + os.Getenv("PATH")`.
  This is the *only* place the host wrapper reaches PATH — never the sandbox, never the login shell.
- Wire the install into `runHostSetup` (so `pix host setup` lays them down) **and** re-run it
  on `runHostLaunch` from the active pack (so a `pack use` since last setup takes effect without a
  re-setup). Keep it idempotent, exactly like `provisionHostAgentDir`.

**Interaction with `runHostSetup`.** `runHostSetup` today symlinks harness dirs + installs pi
extensions. Add one step: `installHostPackWrappers(activePack)` after `provisionHostAgentDir`,
guarded so a missing/Tier-0 pack is a no-op. It must not fail setup if a wrapper copy fails —
print the TODO, exactly like the pi-extension install loop already does.

**Why host mode and not the sandbox:** platformio needs `/dev/tty*`, structurally unreachable from
the DHI VM (AGENTS.md). This is the one case that justifies host exec. It is gated by
`host.enabled` (already off by default) **and** the F5 trust gate at adoption.

**New/changed files:** `hostrun.go` (`hostPackBinDir`, `installHostPackWrappers`, PATH prepend in
`hostChildEnv`/`runHostLaunch`, call from `runHostSetup`). `pack.go` (`runPackAdd` `--host` flag).

**Security boundary.** Host wrapper = full host access, no fence, real creds. Never safe by
default. Two gates in series: (1) `host.enabled` machine-level opt-in (exists); (2) F5 adoption
BoM. An external `[[bin]]` is additionally SHA-pinned and re-hashed at *every* host launch (not
just install) via the `verifyPluginSHA` primitive — a swapped binary refuses to run.

---

### F4 — atomic work/personal switch

**What swaps together, and where it commits:**

| Facet | Swap action | Commits at | Live or recreate |
| --- | --- | --- | --- |
| active pack | `cfg.Pack = root` | `cfg.Save()` | live |
| MCP set | remove old `pack.lock.mcp`, add new; `registerServers` | `cfg.Save()` + gateway | **recreate** (attach is create-only) |
| sandbox `bin/` | synthesized pack kit | next create | **recreate** |
| host `bin/` | `installHostPackWrappers` (clear old, install new) | immediately | live (next `pix host`) |
| config (`gog_account`, `ollama_bridge_model`, routing) | overwrite from pack | `cfg.Save()` | live for host-side; recreate for in-VM ollama model file |
| knowledge scope | `RemoveKnowledgeBundle(old)` + `AddKnowledgeBundle(new)`; `[[knowledge]]` refs | `cfg.Save()` + `propagateServeConfig` | live (daemon reindexes) |
| memory scope tag | `pack.memory_scope` (default = pack name) → written to `.pix/profile` at run/host launch | next run | live in-VM once written |

**Implementation.** `runPackUse` becomes a single transaction:

1. `loadPack(new)`; `prev := loadPack(cfg.Pack)` (best-effort) and read `prev`'s `pack.lock`.
2. Compute removal set (prev's mcp + bundles) and addition set (new's).
3. Mutate the in-memory `cfg` fully (Pack, MCP ±, knowledge ±, gog_account, ollama_bridge_model).
4. **One** `cfg.Save()` — the atomic commit for all host-side/config facets. If Save fails,
   nothing is half-written to config (the pre-Save cfg was never persisted).
5. Post-Save, best-effort side effects (each already idempotent): `registerServers`,
   `installHostPackWrappers`, `propagateServeConfig`, `solicitPackCredentials`, write `pack.lock`.
6. Print the recreate line for the create-only facets.

**Memory scope tag — the smallest real change.** `run.go` currently *deletes*
`<workspace>/.pix/profile` (leftover from profile removal). Replace that deletion with a
write of the active pack's `memory_scope` (default: pack name; empty pack or `"default"` → the
shared scope). `memory-recall.ts` and `memory-capture.ts` already read this file — **no extension
change**. Do the same in `runHostLaunch` (it already calls the shared launcher machinery). The
in-store scope column is retained-dormant per packs.md §8; recall sees `{scope} ∪ {default}`.

**`active_pack` storage:** the existing `cfg.Pack` key. `pack ls` shows it (exists); extend
`pack show` to print the full facet inventory + host BoM (F5's `computeHostBoM` rendered).

**New/changed files:** `pack.go` (`runPackUse` transaction, `pack.lock` I/O), `run.go` (swap the
profile-delete for a `writeMemoryScope(workspace, pack)`), `hostrun.go` (same write on host launch).

**Security boundary.** The switch never *widens* trust silently: adopting a pack with new host-exec
facets re-triggers the F5 gate. Switching *between* already-adopted packs does not re-prompt (trust
was granted at adoption), but a changed external-bin SHA still refuses at launch.

---

### F5 — Tier-1 trust gate

**Trigger.** On `pack use` (and `run --pack <new>`), compute the host bill-of-materials. Tier-0
(no `integration.mcp`, no host `[[proxy]]`, no `[[bin]]`) → no prompt, adopt as today.
Tier-1 (any host-exec facet) → the BoM screen.

**`computeHostBoM(pack) hostBoM`** (new, pure, testable) enumerates:

- MCP commands that will run on the host: for each `integration.mcp`, the resolved
  `mcpRegistrar.serverCmd(name)` (reuse `mcp.go`) — the exact argv the gateway will spawn.
- Host wrappers: each `host = true` `[[proxy]]` (script path).
- External host binaries: each `[[bin]]` with `path` + `sha`.
- Net egress: the union of every facet's `egress`.
- Credentials solicited: the `env` var names (never values).

**Screen + gate:**

```
This pack runs code on your host (not just in the sandbox):

  MCP servers (host):   fastmail   → op run -- pix-host mcp fastmail
  Host wrappers:        platformio (bin/platformio)
  External binaries:    fastmail-mcp  sha256:9f2c…  [re-hashed before every launch]
  Network egress:       api.fastmail.com
  Credentials (op://):  FASTMAIL_TOKEN   (you supply your own; never in the pack)

Adopt this pack and allow the above to run on your machine? [y/N]
```

- `[y/N]` default No. Non-TTY: **fail closed** unless `--yes` (mirror `pix onboard --yes`).
- SHA-pin: `[[bin]]` with empty `sha` fails `loadPack` (never reaches the gate). At install and at
  every launch, re-hash via the `verifyPluginSHA` primitive (extract the hashing core from
  `serve_plugin.go` into a shared helper, or duplicate the ~10 lines — the launcher is a separate
  dependency-light package, same pattern as `canonicalizeKnowledgeBundle`; see ADR-4).
- Provenance: v1-of-this trusts the git remote (packs.md §9.5, §14.3). Signing is deferred. Record
  the adopted remote + commit in `pack.lock` so a later `pack show` can display provenance.

**New/changed files:** `pack.go` (`computeHostBoM`, `hostBoM` type, the gate in `runPackUse`,
`--yes` flag), a shared `hashFileSHA256` helper (new tiny file or in `pack.go`).

**Security boundary.** This is *the* boundary for pack-shipped execution. Fail-closed on non-TTY is
non-negotiable (a CI/script adoption must not silently run host code). The typed schema is the
allowlist: only `integration.mcp`, host `[[proxy]]`, and `[[bin]]` reach an exec path, so there is
no arbitrary-shell field a malicious `pack.toml` could smuggle a command through.

---

### F6 — knowledge shared vs private, standalone bundles

**Schema:** `[[knowledge]]` with `source` + `shared`. Three cases:

1. Embedded (`knowledge/` dir in the pack, no `[[knowledge]]` entry) — v1 behavior, travels in the
   repo. Still discovered by convention in `loadPack` (`p.KnowledgeDir`). Simplest shared case.
2. `shared = true`, `source = <git-url>` — a reference. On `pack use`, resolve via the existing
   `resolveBundleRef(source, knowledgeCacheDir(), out)` (clone/pull into the cache), then
   `AddKnowledgeBundle(resolved)`. Travels: an adopter pulls the same team bundle.
3. `shared = false`, `source = <local-path>` — a reference. Resolve to abs, `AddKnowledgeBundle`.
   Does **not** travel: it is only a path on the owner's machine; when the pack repo is shared the
   local path is simply absent for the adopter (and harmless — it is a standalone bundle they never
   had). Still works locally because bundles are indexed independently of any pack.

**Standalone-ness.** `knowledge_bundles` in config is already pack-independent; the daemon indexes
whatever is listed. `pack use` adds the pack's resolved bundles; switching removes the previous
pack's (via `pack.lock`), exactly as `runPackUse` already does for the embedded `KnowledgeDir`.
`pix knowledge` verbs keep working on bundles with or without a pack — no change.

**What travels on share vs stays local.** Travels: `pack.toml`, `skills/`, embedded `knowledge/`,
`bin/`, `capabilities.json`, `[[knowledge]] shared=true` references (the URL, not the content —
the adopter pulls it). Stays local: `[[knowledge]] shared=false` (the owner's private bundle
content and path), `pack.lock` (activation provenance, git-ignore it), `op://` credential *values*
(always in 1Password), the cloned cache under `knowledge-cache/`.

**`pack add knowledge <name> [--ref <git-url|path>] [--private]`:** no `--ref` → embed (scaffold
`knowledge/` as today). `--ref` → write a `[[knowledge]]` entry with `source`; `--private` sets
`shared = false`. Extend the existing `runPackAdd` `knowledge` case.

**New/changed files:** `pack.go` (`packKnowledge` handling in `loadPack` + `runPackUse` resolve
loop + `runPackAdd` flags). `knowledge.go` unchanged (`resolveBundleRef` reused).

**Security boundary.** A `shared = true` git URL is fetched with the existing `safeGitURL` /
`resolveBundleRef` guards (no `ext::`/`file::` transports). Knowledge is markdown content, not
executed — Tier-0. Private bundles never enter the pack repo, so `git push` of a shared pack cannot
leak them (fitness function below asserts this).

---

## 5. The hard decisions (ADRs)

### ADR-1 — MCP set lives in `cfg.MCP`, swapped via `pack.lock`; not a new per-pack config table
**Context.** The switch must add a pack's MCPs and remove the previous pack's, without clobbering an
MCP the user added by hand.
**Options.** (a) A new `[packs.<name>.mcp]` config table (reintroduces per-pack override tables —
the exact thing profiles-deletion killed). (b) Recompute the whole MCP set from the active pack
every launch (loses user-added MCPs). (c) Project pack MCPs into `cfg.MCP` and track *what the pack
contributed* in `pack.lock`, remove exactly that on switch.
**Decision: (c).** One authoritative selection surface (`cfg.MCP`), reversible switch, user-added
MCPs preserved. Cost: a generated `pack.lock` (bounded scope — activation provenance only).
**Rejected (a)** because packs.md §13 flags "two authoritative places" as a known bug;
**(b)** because it silently drops manual config.

### ADR-2 — `bin/` mounts via a synthesized ephemeral mixin kit, not a bare workspace mount
**Context.** `bin/` must land on the sandbox PATH; skills mount as `--skill` + workspace, which does
not touch PATH.
**Options.** (a) Mount `bin/` as a workspace and ship an in-VM extension that prepends it to PATH
(new in-VM moving part, PATH-injection timing risk). (b) Require the pack author to hand-write
`kit/files/usr/local/bin/` (defeats `pack add proxy` first-class UX). (c) Synthesize a mixin kit at
launch from `bin/`, copy into `files/usr/local/bin/`, `--kit` it.
**Decision: (c).** Reuses the one proven mount path (mixin kit → `/usr/local/bin`, already
on PATH), keeps `pack add proxy` to one file, no new in-VM code. Cost: a per-pack kit dir under the
state dir, regenerated on change; create-only (recreate to attach) — consistent with MCP, honest
about sbx's model. **Rejected (a)** for added in-VM surface; **(b)** for UX.

### ADR-3 — Two explicit commit points (config `Save` vs sandbox recreate), surfaced loudly
**Context.** Some facets go live on `pack use` (config, knowledge index, host wrappers, gateway
registration); others cannot until a sandbox recreate (mcp attach, sandbox `bin/`, in-VM model file)
because `--mcp`/`--kit` are create-only.
**Options.** (a) Auto-recreate the sandbox on `pack use` (surprising, destroys a running session,
violates the re-attach model in `planSandboxLaunch`). (b) Pretend it is all live (the "tool lies"
risk packs.md §13 calls out). (c) Commit host-side immediately, print the exact recreate command
for the rest, let the user choose when.
**Decision: (c).** Matches the existing `pix run` re-attach lifecycle (`--replace` is the
recreate). The recreate line is printed by `pack use`, `pack add mcp`, and `pack add proxy` in the
same breath as the change. **Rejected (a)** as too destructive; **(b)** as dishonest.

### ADR-4 — Reuse the `verifyPluginSHA` hashing core for pack external binaries
**Context.** `[[bin]]` external host binaries need mandatory SHA-pin + re-hash-before-launch; the
identical guarantee already exists for go-plugin slots in `serve_plugin.go`.
**Decision.** Extract the sha256 core into a tiny shared helper (`hashFileSHA256(path) (string,
error)`) used by both `verifyPluginSHA` and the pack loader/launcher; or, if a package boundary
makes sharing awkward (the launcher is dependency-light and already duplicates
`canonicalizeKnowledgeBundle` for that reason), duplicate the ~10 lines with a comment pointing at
the canonical one. Do **not** invent a second checksum scheme. Fail closed on empty/mismatch,
identically to plugins.

---

## 6. Phasing (per the PRD)

**Phase 1 — the flip, sandbox facets (no host exec):**
- F1 MCP attach (register + `cfg.MCP` + recreate line; MCP servers already run host-side via the
  gateway, but the *pack* ships no binary → Tier-0 adoption for reference-only integrations).
- F2 sandbox `bin/` (synthesized kit).
- F4 switch: MCP set + sandbox bin + config + knowledge scope + memory scope tag + `pack.lock`.
- F6 knowledge shared/private references.
- Tier-0 adoption (no prompt).
- **DoD:** `pack use work` → fresh sandbox has the warehouse wrapper on PATH + work MCP set
  attached; `pack use personal` → Fastmail MCP attached; one command flips; memory + knowledge
  scope follow.

**Phase 2 — host execution:**
- F3 host-mode wrappers → platformio on PATH for `pix host` only.
- F5 Tier-1 trust gate: BoM screen, `[y/N]`, non-TTY fail-closed `--yes`, SHA-pin + re-hash for
  `[[bin]]`, provenance in `pack.lock`.
- **DoD:** platformio usable via `pix host --pack personal`; adopting a host-exec pack is
  gated; a tampered external bin refuses to launch.

Note on F1's tier: a pack that only *references* a host-provided MCP (`integration.mcp` +
`op://` ref) ships no executable and is Tier-0 (packs.md §9). A pack that ships an external MCP
*binary* (`[[bin]]`) is Tier-1 → P2. This keeps P1 gate-free, matching the PRD.

---

## 7. Fitness functions (must not regress — wire into CI)

1. **Open-core guard.** Extend `scripts/check-open-core.sh`: no `pack.toml`, `bin/`, or company
   pack fixture is ever tracked in the public tree; a pack with company-specific names lives only
   in the user's git remotes. (packs.md §12 acceptance #5.)
2. **Private knowledge never travels.** Unit test: `git archive`/`ls-files` of a pack repo with a
   `shared = false` `[[knowledge]]` entry contains neither the private bundle path contents nor the
   resolved cache — only the `pack.toml` reference line (which, being a local path, is inert for an
   adopter).
3. **No secret on disk / in VM.** Test: after `pack use` with credentials solicited, `op-refs.env`
   holds only `op://` refs (reuse the existing op-refs parser assertions); the pack repo contains
   no value; no facet writes a token to `.pix/*`.
4. **SHA-pin fail-closed.** Test: `[[bin]]` with empty sha fails `loadPack`; a mismatched sha
   refuses at both install and launch (mirror `TestExternalMcpRefusesOnSHAMismatch`).
5. **Trust gate fail-closed on non-TTY.** Test: Tier-1 `pack use` on a non-TTY without `--yes`
   exits non-zero and registers nothing; Tier-0 adopts silently.
6. **Switch is reversible.** Table test: `pack use A` then `pack use B` then `pack use A` yields the
   same `cfg.MCP` / `knowledge_bundles` as the first `pack use A` (no accumulation), and a
   user-added MCP present before any `pack use` survives every switch.
7. **Recreate line always printed.** Any operation that changes the sandbox facet set
   (`pack use`, `pack add mcp|proxy`) emits the recreate instruction (packs.md §13 must-fix).

---

## 8. Risks / call-outs for the owner

- **`pack.lock` is new surface.** It is generated activation provenance, not a resolver lockfile.
  If you'd rather avoid a lockfile entirely, the alternative is recomputing the removal set from the
  *previous* pack's live `pack.toml` — but that breaks if the previous pack's manifest changed
  between activations. `pack.lock` is the robust choice; it is git-ignored by default.
- **Ephemeral pack-kit dir lifecycle.** Synthesized kits accumulate under
  `<StateDir>/pix/pack-kits/`. Add a `pack gc` step or clean-on-switch (cheap: keyed by pack
  name hash, overwrite in place). Recommend overwrite-in-place + a `pix state reset` sweep.
- **Host PATH prepend ordering.** Prepending the pack `bin/` in host mode means a pack wrapper
  shadows a real host tool of the same name. That is the intent (a wrapped `warehouse`), but it is a
  footgun for a careless pack. The BoM screen names every host wrapper so the user sees the shadow
  before adopting.
- **Gateway registration is best-effort and out-of-band.** `registerServers` needs `SBX_MCP_URL`;
  when the gateway is off, `pack use` still updates config and prints the enable hint (existing
  behavior). The MCP is not attached until both the gateway is on *and* the sandbox is recreated —
  surface both in the recreate line so it is not a silent miss.
- **Known residual: crash-window over-retention (deliberate, crash-only).** The activation commit
  is two atomic file writes that can't be one transaction: `pack.lock` first, then `cfg.Save`. Both
  ordinary-failure sides are now consistent: a lock-write *failure* aborts before Save (round-4 F1
  — config is never committed without its attribution), and a `cfg.Save` *failure* (read-only
  config dir, disk full) rolls the lock back — `commitPackActivation` snapshots the prior
  `pack.lock` bytes before writing the new one and restores them atomically before returning the
  error, so lock and config never diverge on a plain error. The only window left is a true hard
  kill (SIGKILL/power loss) in the milliseconds *between* the atomic lock rename and the atomic
  config rename during a switch/reactivation: the new (narrower) lock lands beside the old config,
  so an MCP/bundle the fresh lock no longer attributes stays in config *with no attribution* — a
  later `pack use`/`pack rm` will NOT remove it (removal is deliberately scoped to lock
  attribution ONLY), so it stays until removed by hand (`pix config`). That scoping is the
  chosen safe side of the lock-only-removal design — it can never remove a user's manually-added
  entry (the worse bug, finding #2). Manifest-based reconciliation would reopen that.
  Over-retention is safe (an extra entry, never a lost one) and recoverable; do NOT add
  manifest-driven removal to "fix" it.

---

## 9. File-change summary (for the engineer)

| File | Change | Phase |
| --- | --- | --- |
| `pack.go` | schema structs (`packProxy/packBin/packKnowledge/packRouting`, fields on `packManifest`); `loadPack` facet parse + hardening; `runPackAdd` gains `proxy` (+`--host`) and `knowledge --ref/--private`; `runPackUse` becomes the atomic swap + `pack.lock` I/O + recreate line; `synthesizePackKit`/`packKitDir`; `computeHostBoM` + Tier-1 gate + `--yes`; `packMcpNames` helper | P1 (schema, F1/F2/F4/F6), P2 (F5 gate, host facets) |
| `run.go` | replace the `.pix/profile` *delete* with `writeMemoryScope(workspace, pack)`; `applyPackToLaunch` appends the synthesized pack kit to `o.Kits` (create-only guard already present) | P1 |
| `hostrun.go` | `hostPackBinDir`, `installHostPackWrappers` (clear-old + SHA-check + copy), PATH prepend in `hostChildEnv`/`runHostLaunch`, call from `runHostSetup`; write memory scope on host launch | P2 |
| `config.go` | none required for keys (facets project into existing `pack`/`mcp`/`knowledge_bundles`/`gog_account`/`ollama_bridge_model`); optionally a `hashFileSHA256` is *not* here (launcher pkg) | — |
| `serve_plugin.go` | optionally extract `hashFileSHA256` from `verifyPluginSHA` for reuse | P2 |
| `mcp.go` | none (reused) | — |
| `knowledge.go` | none (`resolveBundleRef` reused) | — |
| `extensions/memory-recall.ts`, `memory-capture.ts` | none (already read `.pix/profile`) | — |
| `scripts/check-open-core.sh` | extend for pack fixtures + private-knowledge guard | P1 |

No new config.toml keys, no new in-VM extension, no new MCP registration code, no second checksum
scheme. Everything hangs off seams that already exist.
