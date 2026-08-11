# Google Workspace externalization (W2/U02B)

Status: **SUPERSEDED — historical record only.** This unit shipped while still
marked merge-blocked, and the half-finished rip it left behind is the subject of
`docs/design/integrations-remediation.md`, which is the state of record now. Read
this only for what was deleted and why; do not follow any command in it. In
particular the successor to the deleted wizard is `pix mcp add`, not the verb
named below, and gog is no longer known to pix at all: it is declared by a pack
like any other MCP server (`docs/gworkspace.md`).

## What this deletes

W1 (`589231b`) already retired the `pix gworkspace` CLI verb (dispatch answers
`PIX_RETIRED` → `pix mcp register`) and shrank doctor's Google Workspace group
to the same two facts every other MCP server's group renders. That left the
leaf packages it stopped calling still in the tree. This unit deletes them:

- `services/host/workflow/gworkspace/` — the built-in `pix gworkspace
  setup|status|disable` OAuth wizard: credential-file import, interactive
  authorization, headless-spawn verification against the exact hardened
  command the gateway would run, register-then-save-with-rollback.
- `services/host/google_docs_create_plugin.go` (+ its wiring in
  `mcp_bridge.go`) — the separate, write-scoped `google-docs-create` host MCP
  the wizard's `--create-docs` profile could provision (one tool,
  `google_docs_create`, no document-id input).
- The config surface that existed only to drive those two:
  `Config.GoogleWorkspaceAccess` / `SetGoogleWorkspaceAccess` /
  `google_workspace_access` (get-only key), `config.GWDocsCreateServerName`,
  `mcp.BuildGogRegistrar` / `mcp.RegisterGogRegistrar` /
  `mcp.RegisterDocsCreateRegistrar` / `mcp.GogRegisteredArgv`.
- The doctor probe surface the wizard was the only caller of:
  `doctor.RegisteredGogCommand`, `GogHardenedFlags`, `GogMissingHardenedFlags`,
  `GogSpawnCheck`, `GogHeadlessOK`, `GogAccount`, and the `sbx mcp
  inspect/get`-parsing helpers (`parseGogCommandLine/Table/JSON`,
  `gogCommandComplete`) — all dead once their one caller (the wizard) is gone.
- Tests exercising only the above: `cmd/pix/doctor_gogseam_test.go`,
  `cmd/pix/gworkspace_antidrift_test.go` (the retired-guided-CLI naming
  contract), `mcp.TestBuildGogRegistrarWrapsOnlyForTheKeyringRef`,
  `doctor.TestGogParse_RejectedWrapperFallsThrough`, and the elaborate
  headless-probe fixtures in `doctor_test.go` (`gogGreen`/`gogConfirmed` kept
  as identity no-ops for their many call sites rather than a mechanical
  rewrite; see that file's comment).

## What this preserves — the generic external gog MCP support

gog is **not** served by the `pix-host mcp <name>` bridge (that switch only
ever had `slack`; `google-docs-create` is now gone too). It has always been
registered directly against the sbx gateway with its own hardened argv, and
that stays:

- `mcp.GogHardenedArgv` (services/host/mcp/mcp.go) is the ONE place the
  hardened invocation is built, and it is unconditional — every `pix mcp
  register`/pack registration of gog gets `--readonly --gmail-no-send
  --wrap-untrusted mcp --allow-tool read`, never opt-in:

  ```
  gog --account <acct> --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read
  ```

- `mcp.RegisterServers` (the engine behind `pix mcp register`) special-cases
  gog the same way it always did: resolve the binary, require an account,
  build the hardened argv, op-wrap it only when an explicit
  `GOG_KEYRING_PASSWORD` ref is present. No wizard involved.
- `doctor.gogGroup` (services/host/workflow/doctor/gog.go, now ~60 lines) still
  renders gog's registration + sandbox-attachment facts — identical treatment
  to every other MCP server's group.
- `config.NonSecretOpRefsKeys` / `OpRefsTemplate` still document
  `GOG_ACCOUNT`/`GOG_HOME`/`GOG_KEYRING_BACKEND` as the non-secret op-refs.env
  allowlist gog's headless keyring needs.
- `config.GWServerName` ("google-workspace") and `GWInstallCmd` are unchanged.

## Migration reference data

A pack, or a manually-configured host, that wires gog after this change MUST
satisfy the following. This is reference data for that migration, not a new
mechanism — every item is already enforced by `mcp.GogHardenedArgv` in code;
this table exists so the requirement survives independent of anyone reading
that function.

| requirement | where it's enforced | why |
|---|---|---|
| **Pinned artifact** | not enforced by pix — the OPERATOR must pin an exact `gog` build/version (brew formula `openclaw/tap/gogcli`, or a pack-declared version) | pix no longer installs, upgrades, or version-checks `gog`; an unpinned "whatever's on PATH" is the operator's supply-chain risk to own, not pix's |
| `--readonly` | `mcp.GogHardenedArgv`, baked into every registration | no write path can ever be exposed to the agent through the ordinary server |
| `--gmail-no-send` | `mcp.GogHardenedArgv` | Gmail sending is off regardless of OAuth scope grants |
| `--wrap-untrusted` | `mcp.GogHardenedArgv` | returned Gmail/Doc/Drive content is fenced as untrusted data (prompt-injection guard) — see the `gworkspace` skill |
| `GOG_HOME` host-side only | `config.NonSecretOpRefsKeys`; never listed in any sandbox env allowlist | OAuth credentials never enter the VM; only typed tool results cross the gateway |
| **Stale registration cleanup** | operator/pack action, not automatic | see below |

### Stale registration cleanup

A host set up before this change may carry leftovers the retired wizard
created. None of these auto-clean on upgrade:

1. `sbx mcp ls` may still list `google-docs-create` from an old
   `--create-docs` setup. Remove it: `sbx mcp rm google-docs-create`.
2. `~/.config/pix/config.toml` may still carry `google_workspace_access =
   "create-docs"` from that setup. It is no longer read by anything; drop the
   line (`pix config` has no unset for it since it was never settable through
   `pix config set` — hand-remove it, config.toml is otherwise
   launcher-managed).
3. Re-run `pix mcp register` (or `make mcp-register`) so the surviving
   `google-workspace` registration is rebuilt from the current
   `mcp.GogHardenedArgv`, not a copy a since-changed wizard wrote historically.
4. `pix doctor` should read the Google Workspace group as registration +
   attachment only — no hardened-flags/headless-spawn checks remain to fail
   or hide a problem; the readback is inherently thinner than before, by
   design.

## What this change does NOT prove

**No live `gog` binary was available in this environment to execute against.**
Every claim above about `--readonly`/`--gmail-no-send`/`--wrap-untrusted`/
`GOG_HOME` is a claim about what `mcp.GogHardenedArgv` and the surrounding Go
code construct and unit-test as a string/argv — it is not evidence that an
actual `gog` process, given that argv, enforces read-only access, refuses to
send mail, or correctly fences untrusted content at the OAuth/API level. That
is the pinned external artifact's job, and this change ships without one to
verify against.

Do not read this document, its tests, or the commit it lands in as guardrail
proof. **This unit is merge-blocked pending that external `gog` artifact** —
a pinned, runnable build to register, probe (`pix doctor`, `pix mcp
register`), and exercise end-to-end before anyone relies on the read-only/
no-send/wrap-untrusted properties in production. Until then, treat the table
above as a specification to hold a future integration test to, not a result.
