# Replace `gws` with `gog` (gogcli.sh)

## Why (the pain, named)
`gws` runs **inside the sandbox**, so Google auth has to be smuggled in: the host
keeps the OAuth refresh creds, `services/host/gwstoken.go` mints a short-lived
bearer on :11441, `bin/gws` fetches it and injects `GOOGLE_WORKSPACE_CLI_TOKEN`,
and the per-sandbox broker bearer (`GWS_TOKEN_AUTH`) has to be forwarded into the
VM (the unresolved "host-verify #2"). Four moving parts, one of them unverifiable
in kit-spec v1. That is the nightmare.

`gog` ships **`gog mcp`** — a stdio MCP server that the sbx gateway runs **on the
host** (same pattern as `slack`), authed with the host's own `gog` credentials.
The agent calls typed tools (`gmail_search`, `docs_get`, `calendar_events`, …)
through the gateway. **No CLI in the VM, no token service, no wrapper, no bearer
forwarding, no Google endpoints in the VM allowlist.** It also fits pi-stack's
existing MCP infra exactly, and is safer for full-auto (read-only by default,
`--allow-write` gated, per-command allowlists, `--wrap-untrusted`, `--gmail-no-send`).

## Recommended architecture: gog as a host-side MCP server (the `slack` pattern)
`sbx mcp add gog --command gog --args --account --args you@x --args mcp --args
--allow-tool --args read` → the gateway spawns `gog … mcp` on the host; the VM
reaches its tools through the gateway. Identical mechanism to how `slack` is
registered/run today (`make mcp-register`, the `MCP` list, the `mcp <name>` path).

### Delete (this is most of the work — removal)
- `bin/gws` (in-VM wrapper) and the `_gws` binary + its install in the `Dockerfile`.
- `services/host/gwstoken.go` (the :11441 token service) + its `runGwsToken`
  dispatch in `main.go`.
- `services/host/gws_plugin.go` + `gws_plugin_test.go` (the built-in
  `CredentialBroker` gws impl) and the `gws`/`gws-token` service slot +
  `gwsTokenCheck` preflight in `serve.go`.
- The Google API entries in `pi-kit/spec.yaml` `network.allowedDomains`
  (`www.googleapis.com`, `gmail/calendar/drive/docs/sheets/people/oauth2`
  googleapis, and `host.docker.internal:11441`). The VM no longer talks to Google.
- `GWS_TOKEN_AUTH` broker-bearer plumbing **for the built-in case**: gws was its
  only built-in consumer, so removing gws dissolves host-verify #2. Keep the
  generic broker-bearer only if an overlay broker still needs it (dormant otherwise).
- `gws-token-serve` (Makefile), `gws` from `SERVICES`, and gws checks in
  `cmd/pi-stack/{setup,doctor}.go` + `config/config.go`.

### Add
- **Register `gog`** with the gateway (extend `LOCAL_STDIO_MCP`/`MCP` +
  `make mcp-register`), read-only by default:
  `gog --account <acct> --gmail-no-send mcp --allow-tool read` (add
  `--allow-write --allow-tool 'docs.*'` etc. only where you want writes).
- **`capabilities.json`**: point `gworkspace` (and/or split into
  `mail`/`calendar`/`docs`) at provider **mcp: gog** instead of **cli: gws**.
  `capability-routing` then resolves those to the gog MCP tools.
- **A `gworkspace` skill** (or let gog generate one from `gog schema --json`) so
  agents know the tool surface + the read-only-by-default posture.
- **Host setup** (macOS): `brew install` gog, `gog auth` once (store an OAuth
  client + authorize the account), and — because the gateway spawns it
  non-interactively — set `GOG_KEYRING_BACKEND=file` + `GOG_KEYRING_PASSWORD` in
  the gateway/service environment (the `op run` env-file already used for MCP
  creds is the natural home). Verify with `gog --account <a> auth doctor --check`
  and `gog … mcp --list-tools`.

## Migration (phased, low-risk)
1. **Additive:** add gog registration + capability route + skill; leave gws in
   place. Prove `gog mcp` tools work through the gateway.
2. **Flip:** switch `capabilities.json` `gworkspace` → gog; smoke-test
   gmail/calendar/docs reads via the agent.
3. **Delete:** rip out gws (wrapper, token service, broker, allowlist, Dockerfile
   install, Makefile, setup/doctor/config refs). Rebuild image (thinner) + kit
   (smaller allowlist).

## The one caveat + fallback
gog-as-MCP rides the **sbx MCP gateway** — the same thing that 502'd on you. That
502 is a gateway auth/session issue (`make mcp-auth`), not gog-specific, and it's
the sanctioned path (`slack` uses it). If gateway reliability is a dealbreaker,
the fallback is **Option B: gog CLI in the VM + a host broker** minting a Google
*access token* (gog supports direct access tokens) — smaller conceptual change,
but it keeps a token-forwarding step (the thing we're trying to kill). Recommend A;
fall back to B only if the gateway proves too flaky for daily use.

## Open decisions
1. **Write access:** ship read-only (`--allow-tool read`) and add writes per
   surface, or allow `docs.*`/`sheets.*` writes from the start? (Recommend
   read-only first; it's the safe full-auto default.)
2. **Capability granularity:** one `gworkspace` capability, or split into
   `mail`/`calendar`/`docs`/`sheets` so routing + skills are finer-grained?
3. **A vs B:** commit to gog-as-host-MCP (A), or keep a CLI-in-VM option (B) for
   when the gateway is down?
