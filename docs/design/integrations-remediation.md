# Integrations remediation (gog, Slack, BambooHR, Snowflake)

Status: **planned, not started.** Audit complete and reproduced on a live host
(2026-08-10). No code changed yet.

Scope: `pix` (this repo) and `gm-pix-pack` (`../gm-pix-pack`). This document is
the state of record for the whole remediation — findings, decisions, plan, and
open questions. Update the Status line and the per-phase checkboxes as work
lands; do not start a phase without reading "Decisions" first.

---

## 0. The one-paragraph version

`pix setup --pack gm-pix-pack` cannot succeed. Two of the pack's four setup
hooks call `pix` verbs that were deleted from the CLI, so their readiness
probes can never pass and their apply steps die on the first command. Pix's own
`docs/gworkspace.md` prescribes a third deleted verb and a verification step
that passes on a completely broken install. On the audited host, Google
Workspace works only because the operator built a fourth, undocumented
credential path (macOS Keychain → `~/.bashrc` → inherited gateway env) that pix
knows nothing about. `pix doctor` reports `✓ ready` throughout, because it
treats *registered with the gateway* as *working*.

---

## 1. Findings

Every claim below was reproduced on a live macOS host with `gog` v0.35.0, `sbx`
0.38 and gm-pix-pack at `b5a6594`. Re-verification commands are given so a
future agent can confirm rather than trust.

### 1.1 The pack calls verbs that do not exist

`gm-pix-pack/setup/google-workspace`:

| line | command | reality |
|---|---|---|
| 15 | `pix config get google_workspace_account` | exists |
| 16 | `pix config get google_workspace_access` | **key deleted** — exit 2 |
| 17 | `pix gworkspace status` | **verb deleted** |
| 31 | `pix gworkspace setup --create-docs` | **verb deleted** |

`gm-pix-pack/setup/slack`:

| line | command | reality |
|---|---|---|
| 16–17 | `pix config get slack.client_id` / `slack.redirect_uri` | **keys never existed** |
| 19, 35 | `pix slack status` | **verb deleted** |
| 26, 29 | `pix slack auth --yes` | **verb deleted** |

`workflow/pack/setup.go:52-66` runs `check`, then `apply` on failure, then
`check` again. For both hooks `check` can never pass and `apply` dies
immediately. **There is no input that reaches green.**

`gm-pix-pack/README.md:57-58` also tells users to verify with `pix gworkspace
status` and `pix slack status`.

```bash
pix config get google_workspace_access   # pix: unknown key … (exit 2)
pix gworkspace status                    # pix: no command named "gworkspace"
pix slack status                         # pix: no command named "slack"
```

`AGENTS.md:25` records the deletions as deliberate — "pix has no released
users, so a removed surface gets the ordinary unknown-command answer." The pack
is the released user.

### 1.2 Pix's own docs and skills are stale in the same direction

- `pix mcp register` → the verb is `pix mcp add` (`cmd/pix/mcp_cmd.go:54`;
  `register` was consolidated away, see `mcp_cmd.go:46-52`). Stale in
  `docs/gworkspace.md:64`, `skills/gworkspace/SKILL.md:10`,
  `skills/onboarding/SKILL.md:172`.
- `pix mcp load` → deleted. Stale in `skills/healthcheck/SKILL.md:100`,
  `skills/capability-routing/SKILL.md:124`.

### 1.3 `GOG_HOME` guidance is wrong on macOS

`docs/gworkspace.md:44`, `config/config.go:702` and
`config/op-refs.env.example` all say `GOG_HOME=$HOME/.config/gog`. The real
macOS root is `~/Library/Application Support/gogcli` (`gog --help` prints it;
`~/.config/gog` does not exist on the audited host). Following that line points
gog at an empty, unauthorized home.

### 1.4 The documented verification is a false positive

`docs/gworkspace.md:47-57` claims that without the keyring password the server
"starts and returns **zero tools, silently**," and prescribes `gog … mcp
--list-tools` as proof.

```bash
env -u GOG_KEYRING_PASSWORD gog --account you@x --readonly mcp --list-tools
# → full JSON tool list, exit 0
```

`--list-tools` is a static registry dump; it never touches the keyring. The
claim is false and **the only check the docs give you passes on a broken
install.** The real failure appears on a tool *call*:

```bash
env -u GOG_KEYRING_PASSWORD gog --account you@x --readonly gmail labels list
# read OAuth client secret from keyring: … no TTY available for keyring file
# backend password prompt; set GOG_KEYRING_PASSWORD          (exit 1)
```

### 1.5 The probe that was missing all along

`gog auth doctor` is machine-readable and checks exactly the thing that breaks
— but it **exits 0 even when it reports `status error`**, which makes it useless
as a probe (pix judges a probe by its exit code). That was found by an
independent reviewer AFTER this document first prescribed it; the working probe
is a real read, `gog --readonly gmail labels list` (0 working, 2 broken). Left
here with the correction rather than quietly rewritten, because prescribing a
check that cannot fail is the exact mistake §1.4 documents:

```
ok    keyring.backend   file (source: config)
ok    keyring.password  GOG_KEYRING_PASSWORD is set
hint  keyring.password  keep this value identical across shell, service, and agent configs
ok    keyring.open      opened
ok    tokens            1 readable OAuth token of 1 stored token account
status ok
```

`gog auth setup <email> --credentials <client.json> --readonly --login` is the
guided flow ("Guide Google Cloud, OAuth client, and account setup"). Neither is
mentioned anywhere in pix or the pack. `docs/gworkspace.md` says "consult its
`--help`; pix does not drive this step."

Also verified: **`GOG_ACCOUNT` is read from the environment** (a bogus value
overrode stored config), so the account can travel through op-refs rather than
argv templating.

### 1.6 The audited host was green and working by accident

```
$ sbx mcp inspect google-workspace
Command:  /opt/homebrew/bin/gog --account … --gmail-no-send --wrap-untrusted
          --readonly mcp --allow-tool read          ← BARE, no op run wrapper
```

`op-refs.env`'s `GOG_KEYRING_PASSWORD` is not in play at all. It works because
`~/.bashrc:36-39` exports the passphrase from the macOS Keychain
(`security find-generic-password -s gogcli-keyring`) and the gateway inherits
it from the operator's shell.

Cause: `mcp/mcp.go:561` sets `GogUseOp = opReady && creds.GogKeyring`, and
`creds.GogKeyring` (`cmd/pix/mcp_credentials.go:39` → `secret/oprefs_find.go:80`)
requires `GOG_KEYRING_PASSWORD` to be a **filled `op://` ref at the moment
`pix mcp add` ran**. It wasn't. The operator later fixed the ref — and
**nothing re-registers on change and nothing warns.** A *literal* value there is
also silently downgraded to a bare registration rather than rejected, even
though `config/secretshape.go:16` explicitly recognises a literal
`GOG_KEYRING_PASSWORD` as a pasted secret.

The credential therefore lives in four places with no owner: 1Password (via
op-refs), macOS Keychain (via `.bashrc`), gog's own file keyring, and the
gateway's inherited process env — and only the last one is load-bearing.

### 1.7 Two registered MCP servers spawn commands that no longer exist

```
$ pix-host mcp --list
(empty)                    # main.go:5 — "local stdio servers: NONE"

$ sbx mcp inspect slack
Command: op run … -- /opt/homebrew/bin/pix-host mcp slack              ← dead

$ sbx mcp inspect google-docs-create
Command: op run … -- /opt/homebrew/bin/pix-host mcp google-docs-create ← dead
```

`sbx mcp ls` reports both `✓ ready`. `google-docs-create` is the retired
write-scoped companion MCP and is **actively attached to live sandboxes**
(`sbx run … --static-mcp google-docs-create`), despite
`docs/design/gworkspace-externalization.md` listing its removal as required
manual cleanup.

### 1.8 Slack was destroyed by `bce2b1b`, not designed away

```
bce2b1b  Rearchitect and harden the Pix host lifecycle stack (#65)
         783 files changed, 80280 insertions(+), 100084 deletions(-)
```

It deleted `services/host/slack.go`, `slack_plugin.go`, `cmd/pix/slack.go`,
`cmd/pix/slack_oauth.go` and the whole `services/host/slackoauth/` package
(manager, opstore, PKCE client, scopes, locking — ~20 files).

It also collapsed the go-plugin capability map from four kinds to one:

```go
// git show bce2b1b^:services/host/plugin/handshake.go
var PluginMap = map[string]goplugin.Plugin{
    "memory":    &MemoryPlugin{},
    "knowledge": &KnowledgePlugin{},
    "broker":    &BrokerPlugin{},   // CredentialBroker: Mint/Check/Describe
    "mcp":       &McpPlugin{},      // McpServer: Info/ListTools/CallTool
}
```

`slack_plugin.go` implemented `plugin.McpServer`. The two interfaces that would
have solved this problem — a credential broker and a supervised MCP server —
existed and were removed in the same commit that left the dead registrations
behind.

Fossils confirming the strip was mechanical rather than designed:
`packinfo/service.go:52-57` still reserves the unit names `{"memory",
"knowledge", "broker", "serve"}`, and `[[services]] runtime = "container"` is
validated, consented and fingerprinted with **no consumer**
(`packinfo/service.go:32-34`).

### 1.9 The generic credential solicitor is switched off exactly when needed

`workflow/pack/pack.go:873-906` already prompts for an integration's `ig.Env`
op-ref and writes it via `secret.WriteOpRefQuiet`. Line 873 skips it whenever
`ig.Setup != ""` — i.e. the good path is disabled precisely when a setup hook
exists. That is why `setup/bamboohr` hand-rolls `ensure_ref`.

### 1.10 The non-secret allowlist is closed to packs

`config/config.go:665-669` hardcodes `NonSecretOpRefsKeys` to three `GOG_*`
names. A pack cannot extend it, so legitimate non-secret pack values show a
permanent red `✗ not an op:// ref` in `pix secret ls` (observed:
`SLACK_TEAM_ID`, `SLACK_USER_ID`).

### 1.11 Doctor cannot see any of this

`health/mcp.go` checks registration (`sbx mcp ls`) and, for remote servers,
control-plane auth. Nothing else. On the audited host — two dead commands, one
retired server, one accidentally-working server — the entire diagnosis was:

```
✓ mcp        optional  8 registered
```

`docs/design/gworkspace-externalization.md` admits the thinning ("the readback
is inherently thinner than before, by design") and marks itself
**"merge-blocked pending that external gog artifact."** It shipped anyway.

### 1.12 NEW: the test suite writes into the real user trust store

`~/.config/pix/pack-trust.json` on the audited host:

```
adopted total: 226
  test-fixture entries: 225      ← /var/folders/…/TestClonePack_…/pack-4be9fe27fc868d2f
  real entries:          1
  remotes: 150 × https://example.com/attacker/pack.git
            75 × https://example.com/attacker/pack2.git
```

`TestClonePack_MarksAdoptionDurablyBeforeReturn` is not hermetic: it records
durable adoption marks for attacker-shaped remotes in the operator's production
trust file. This is the 50 KB. Not known to be exploitable (temp paths are
random and gone), but the trust store is the security boundary and tests must
not be able to write to it.

### 1.13 What actually works

| | mechanism | verdict |
|---|---|---|
| **BambooHR** | `[[integrations]]` + `image` → op-run-wrapped `docker run` | **correct.** Probes real state (`docker image inspect` + secret ref). |
| **Snowflake** | `[[proxy]]` + host daemon on `:11442` | **works, best-designed probe.** `check` is `curl /health` — a behavioural probe of the thing that must work. |

BambooHR nits: `check_ready` greps `pix secret ls`'s **human-readable output**
for the literal string `✓ BAMBOOHR_API_KEY = op:// ref` (any wording change
silently flips it to "not ready" forever); the image is a local tag from a local
`docker build`, unpinned by digest.

Snowflake nits: `apply` clones and executes `connection-setup.sh` +
`install.sh` from a private repo with no pin or fingerprint, outside the Tier-1
gate that covers the setup hooks themselves; the resulting LaunchAgent daemon is
entirely outside pix's supervision.

---

## 2. Root causes

1. **The pack↔pix contract is unversioned and unenforced.** `pack.toml` versions
   its schema but setup hooks are opaque shell calling the pix CLI with no
   compatibility guarantee.
2. **`registered` is reported as `working`.** The single most load-bearing
   defect: it is what allowed every other item here to hide.
3. **Credentials have four homes and no owner**, and the op-refs path silently
   no-ops when its precondition fails.
4. **Registration is a snapshot, not a reconciliation.** `GogUseOp` is decided
   once at `pix mcp add` time; later edits change nothing and warn about nothing.
5. **Setup checks read config back instead of exercising the integration.**
   `pix config get X == Y` proves you typed something.

---

## 3. Decisions

Settled with the maintainer on 2026-08-10. Do not relitigate without saying so.

| # | Decision | Rationale |
|---|---|---|
| D1 | **gog is ripped out of pix core entirely.** Not a public feature. Moves to gm-pix-pack. | The rip was started in `04eed17` and left half-done. |
| D2 | **Slack becomes a container MCP image**, declared `[[integrations]] image=…`, exactly like BambooHR. | The one path proven end-to-end on a real host. |
| D3 | **Plugin kinds stay at one (`memory`).** `McpServer` and `CredentialBroker` stay deleted. | Accept `#65`'s collapse rather than restore 100k lines. |
| D4 | **One credential home: `~/.config/pix/op-refs.env`.** 1Password is assumed present. | Kills the four-way ambiguity in §1.6. |
| D5 | **pix first, then gm-pix-pack** against the settled contract. | The pack cannot be fixed correctly against a moving target. |
| D6 | *(assumed — confirm)* **Setup steps become declarative TOML.** Pack declares what must be true; pix owns how. | With D3 ruling out a Setup plugin kind, the alternative keeps the stringly-typed CLI coupling that broke. See §6 Q1. |

### Consequence of D3 worth stating plainly

The go-plugin seam supervises *long-running processes pix owns*. gog, Slack and
BambooHR are short-lived stdio servers the **sbx gateway** spawns — pix only
hands it an argv. So "make the integrations plugins" is a category error under
D3. The one genuine fit is **Snowflake's daemon** (§Phase 6).

---

## 4. Migration hazard: the Tier-1 trust fingerprint

**This is the single thing most likely to go wrong during Phase 1, and it will
present to users as a scary security prompt or a hard launch failure.**

### What the fingerprint is

Every pack that touches the host gets a **bill of materials** — resolved MCP
argv, host wrappers, `[[bin]]`s, `[[services]]`, setup hooks (hashed, never
run), inference gateways, the egress union, and credential variable *names*.
`ComputeHostBoM` (`workflow/pack/trust.go:148`) builds it; it is serialised to a
canonical JSON document (`fpDoc`, currently `V: 6`) and sha256'd into a single
fingerprint. You review that BoM once; the fingerprint records what you agreed
to. Change any input and the fingerprint changes, which is the point: a pack
cannot silently grow a new host surface after you consented to the old one.

### Why removing gog breaks it

`ComputeHostBoM` takes `cfgGogAccount` and threads it in:

```go
account := strings.TrimSpace(p.Manifest.GogAccount)
if account == "" { account = strings.TrimSpace(cfgGogAccount) }
if account == "" { account = "<gog_account>" }
reg := mcp.McpRegistrar{Gog: "gog", Account: account, HostBin: "pix-host"}
…
case isLocalMCP(name):
    b.MCP = append(b.MCP, hostBoMMCP{Name: name, Argv: reg.ServerCmd(name)})
```

The gog account is **inside the hashed argv**. Delete `cfgGogAccount`,
`GogHardenedArgv` or `GWServerName` and every already-accepted fingerprint
becomes wrong.

### What the user sees

`requireAcceptedFingerprint` (`workflow/pack/truststore.go:220-231`) is called
from three places — launch (`trust.go:117`), setup (`setup.go:99`) and service
admission (`unitview.go:57`) — and it **returns an error**, not a warning:

```
pack gm-pix-pack host-exec surfaces are not accepted (or changed since
acceptance) — run `pix pack use /Users/…/gm-pix-pack` to review them
```

So after a naive Phase 1, every existing user's next `pix run` **hard-fails**
and sends them to a full trust re-review. From their side that is
indistinguishable from "my pack changed its host surface behind my back" — the
exact alarm the mechanism exists to raise. Raising it for a pix-side refactor
trains people to click through it, which is worse than the bug.

### The fix, and there is already precedent

`fpDoc` carries a version (`V: 6`) and the codebase already documents the
additive-migration pattern:

> `Services` is ADDITIVE with `omitempty` on purpose: a pack with no
> `[[services]]` keeps its exact prior encoding, so every already-accepted
> fingerprint stays valid.

Options, in order of preference:

1. **Encoding-preserving removal.** Make the gog contribution `omitempty`-absent
   for packs that never declared it, so their `V: 6` document is byte-identical
   before and after. Packs that *did* declare gog re-gate — correctly, since
   their host surface genuinely changed.
2. **Versioned migration.** Bump to `V: 7`, and on a `V: 6` mismatch recompute
   the old encoding; if it matches the accepted record, silently re-accept under
   `V: 7`. One-time, invisible, and auditable.
3. **Accept and announce.** Bump the version, force re-consent, and lead the
   release note with it plus a `pix pack use` one-liner. Only acceptable if 1
   and 2 are impractical — and never without the release note.

**Acceptance for Phase 1:** a host with an accepted pre-change fingerprint runs
`pix run` after the change with **no trust prompt and no error**, proven by a
test that pins a `V: 6` record and asserts it still verifies.

While in this area, fix §1.12 (tests writing to the real trust store) — the same
files, and a trust store the test suite can write to is not a trust boundary.

---

## 5. Plan

### Phase 0 — Stop the bleeding (host-side, no code) ☐

Two dead servers are registered and one is attached to every sandbox.

```bash
sbx mcp rm google-docs-create
sbx mcp rm slack
pix config unset mcp google-docs-create
pix rm <box> && pix run
```

Do **not** yet remove the `~/.bashrc` `GOG_KEYRING_PASSWORD` export — it is what
makes Workspace work today. Phase 2 replaces it; drop it only after the op-run
registration is verified.

### Phase 1 — pix: finish the gog rip ☐

Delete, per §Decisions D1:

- `mcp/mcp.go`: `GogHardenedArgv:172`, `ServerCmd` case `:207`, `ExecArgv`
  special case `:427`, the `wantGog` path `:471-603`,
  `McpRegistrar.Gog/Account/GogUseOp:142-148`, `Credentials.GogKeyring:64`
- `config/config.go`: `GogAccount:88`, `SetGogAccount:579`, `GWServerName:650`,
  `GWInstallCmd:654`, the `GOG_*` entries in `NonSecretOpRefsKeys:665`, the gog
  block in `OpRefsTemplate:698-704`
- `config/secretshape.go:16` — the `GOG_KEYRING_PASSWORD` case
- `cmd/pix/mcp_credentials.go:39` — `gogKeyring`
- `workflow/provision/config.go:16,41,70-79`, `provision/onboarding.go:65,95`,
  `workflow/doctor/mcp.go:64`, `workflow/launch/hoststate.go:117`,
  `workflow/pack/trust.go:129,160`
- `packinfo/pack.go:44` (`gog_account`) and the `PriorGogAccount` rollback at
  `workflow/pack/pack.go:190-193, 427-431, 672-673, 790`
- `docs/gworkspace.md`; `skills/gworkspace/SKILL.md` moves to the pack

**Gated on §4.** Do not merge without the fingerprint-stability test.

### Phase 2 — pix: one generic local-command MCP integration ☐

The single new capability replacing the gog special case:

```toml
[[integrations]]
  name       = "Google Workspace"
  mcp        = "google-workspace"
  bin        = "gog"                                  # [[bin]] (SHA-pinned) or PATH
  args       = ["--gmail-no-send", "--wrap-untrusted", "--readonly",
                "mcp", "--allow-tool", "read"]
  env        = ["GOG_KEYRING_PASSWORD"]               # op:// secrets
  env_names  = ["GOG_ACCOUNT", "GOG_HOME"]            # non-secret allowlist
  setup      = "google-workspace"
```

Rules:

- **Literal `args` only, no templating.** Per-user values travel in op-refs;
  `GOG_ACCOUNT` is read from env (verified §1.5).
- **`env_names` replaces the hardcoded `NonSecretOpRefsKeys`** (fixes §1.10).
- **Always op-run wrap** when op-refs exists and the integration declares `env`.
  Delete the `GogUseOp` conditional; a bare registration for a server that
  declares credentials is the §1.6 bug.
- **Reject a literal value** for any name in `env` at register time.
  `LooksSecretShaped` already detects it — fail loudly instead of degrading.
- Fold Slack in as an `image` integration (D2) in the same phase.

### Phase 3 — pix: make doctor honest ☐

Highest-leverage phase. Until doctor distinguishes *registered* from *working*,
every other fix is unverifiable.

1. **Resolve the argv** — confirm the binary and subcommand exist. `pix-host mcp
   <name>` against an empty `pix-host mcp --list` is unambiguous and catches
   both dead registrations in §1.7.
2. **Drift detection** — diff the registered argv against what current config
   would produce; report *"registration is stale — run `pix mcp add <name>`."*
   This is the §1.6 gap.
3. **Honest health probe** — run a pack-declared `probe` argv (`gog auth
   doctor`) and show its output. **Never `--list-tools`** (§1.4).
4. **Orphan sweep** — name registered-but-unconfigured servers with the
   `sbx mcp rm` command.

**Acceptance:** on a host in the pre-Phase-0 state, `pix doctor` must not print
`✓ mcp optional 8 registered`.

### Phase 4 — pix: declarative setup steps ☐

Replace `SetupStep{Path, CheckArgs, ApplyArgs}` (`packinfo/pack.go:91`):

```toml
[[setup]]
  id = "google-workspace"
  description = "Google Workspace authorization"

  [[setup.require]]
    kind = "bin"; name = "gog"; min_version = "0.35.0"
    install = "brew install openclaw/tap/gogcli"
  [[setup.require]]
    kind = "op-ref"; env = "GOG_KEYRING_PASSWORD"
  [[setup.require]]
    kind = "probe"; argv = ["gog","auth","doctor"]; expect = "exit0"

  [[setup.apply]]
    kind = "interactive"
    argv = ["gog","auth","setup","--readonly","--login"]
    explain = "Opens Google Cloud to create a Desktop OAuth client (~2 min, one time)."
```

A pack can never call a nonexistent verb because it never names one. `op-ref`
steps route through the existing solicitor — **delete the `ig.Setup != ""` skip
at `workflow/pack/pack.go:873`** (§1.9). Keep `kind = "exec"` with a SHA-pinned
script as the escape hatch for genuinely bespoke logic (Snowflake's installer),
fingerprinted like `[[bin]]`.

Note: the fingerprint's `fpSetup` struct hashes `CheckArgs`/`ApplyArgs`, so this
phase changes the encoding again — same §4 discipline applies.

### Phase 5 — pix: docs and skills truth pass ☐

Independent of everything else; hours of work; ship early.

- `pix mcp register` → `pix mcp add` (§1.2, 3 files)
- `pix mcp load` → deleted (§1.2, 2 files)
- Delete the `GOG_HOME=$HOME/.config/gog` claim (§1.3, 3 files)
- Delete `docs/gworkspace.md:47-57` (§1.4) — it is false and it is why a broken
  install looked fine
- Remove the `{"knowledge","broker"}` fossils from `packinfo/service.go:52-57`;
  wire or delete `runtime = "container"`
- `docs/design/gworkspace-externalization.md` is still marked "merge-blocked"
  and shipped — close it out honestly or retract it

### Phase 6 — gm-pix-pack ☐

- Rewrite all four setup hooks declaratively (Phase 4 contract)
- Carry gog as `[[bin]]` + local-command integration; carry the `gworkspace`
  skill; replace the `--list-tools` guidance with a real read
- Ship Slack as an OCI image (D2)
- Move Snowflake's daemon to `[[services]] runtime = "go-plugin"` with a SHA
  pin — a long-running host daemon installed by an unpinned shell script into a
  LaunchAgent, currently outside every trust seam. **The one genuine plugin fit.**
- Fix `pack.toml:135-146`, which documents a `services/host/slack.go` and
  `pix slack auth` PKCE flow that no longer exist
- Replace BambooHR's `pix secret ls` output-grep with a machine-readable query

### Phase 7 — guards, so it cannot rot again ☐

1. **`pix pack verify <dir>`** — validate every declared step kind, binary and
   probe against the running pix. Runs in both repos' CI.
2. **Pack contract CI job** — build pix from `main`, run every gm-pix-pack
   `require` step. The current breakage is one grep from being caught.
3. **CLI-surface test** — every command string in `docs/`, `skills/` and any
   pack must exist in `pix help --all`. `AGENTS.md:25` already treats the
   generated verb tree as source of truth; make it enforceable.
4. **`pix_api = N`** in `pack.toml`, validated before anything runs, so the next
   deletion sweep fails at load instead of silently at apply.

### Sequencing

```
Phase 0  ─────────────────────────────────────────────  now, by hand
Phase 1 (gog rip, gated on §4) ──┐
Phase 3 (doctor honesty)         ├── independent, parallel
Phase 5 (docs truth pass)      ──┘
                                 └─▶ Phase 2 (generic integration)
                                        └─▶ Phase 4 (declarative setup)
                                               └─▶ Phase 6 (pack)
                                                      └─▶ Phase 7 (guards)
```

---

## 6. Open decisions

- **Q1 — Setup seam shape (D6 is assumed, not confirmed).** Declarative TOML
  (assumed) vs. shell hooks bound to a frozen machine contract (`pix … --json`,
  frozen exit codes, `pix_api = N`). Declarative is the recommendation; confirm
  before Phase 4.
- **Q2 — Fingerprint migration strategy.** §4 options 1/2/3. Decide before
  Phase 1 merges.
- **Q3 — Does a public, pack-less Workspace path survive?** D1 says gog is not
  public. Confirm no public user is expected to reach Google Workspace without a
  pack, since Phase 1 removes the only route.
- **Q4 — Slack OAuth ownership.** D2 makes Slack a container, but the PKCE grant
  against org client `2217531547.10627346157236` has to live somewhere: inside
  the image, in a Phase-4 setup step, or as a static token in op-refs. The
  original `slackoauth/` package rotated credentials into each employee's
  Private 1Password vault; if that behaviour is still wanted, a container has to
  reimplement it.
- **Q5 — `[[services]] runtime = "container"`:** wire it (Phase 6 could use it)
  or delete it. It is currently validated, consented and fingerprinted with no
  consumer.
- **Q6 — Snowflake installer trust.** Phase 6 pins the daemon, but
  `connection-setup.sh` / `install.sh` still execute unpinned from a cloned
  repo. Bring them under `[[bin]]`-style SHA pinning, or accept and document.
- **Q7 — `~/.bashrc` Keychain export.** Once Phase 2 lands, is the shell export
  removed (single path, D4) or kept as a deliberate fallback? Keeping it
  reintroduces the ambiguity D4 exists to kill.

---

## 7. Re-verification commands

For a future agent confirming this document rather than trusting it:

```bash
# §1.1 dead verbs
pix config get google_workspace_access; pix gworkspace status; pix slack status

# §1.4 the false verification, then the true failure
env -u GOG_KEYRING_PASSWORD gog --account "$ACCT" --readonly mcp --list-tools | head
env -u GOG_KEYRING_PASSWORD gog --account "$ACCT" --readonly gmail labels list

# §1.5 the probe that should have been used — and the two that must NOT be.
# Both of these exit 0 on a completely broken install; only the read call fails.
gog auth doctor                                  # exits 0 even on `status error`
gog --readonly mcp --list-tools                  # exits 0 with no credentials
gog --readonly gmail labels list                 # 0 working, 2 broken  <-- use this

# §1.6 / §1.7 what is actually registered
sbx mcp ls
for s in google-workspace slack google-docs-create bamboohr; do sbx mcp inspect $s; done
pix-host mcp --list          # empty

# §1.8 what #65 deleted
git show bce2b1b^:services/host/plugin/handshake.go | sed -n '/PluginMap/,/^}/p'
git show bce2b1b^:services/host/plugin/interfaces.go | grep '^type.*interface'

# §1.12 trust-store pollution
python3 -c "import json;d=json.load(open('$HOME/.config/pix/pack-trust.json'));\
print(len(d['adopted']),'adopted;',sum('/var/folders/' in k for k in d['adopted']),'from tests')"
```
