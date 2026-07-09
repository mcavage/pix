# sbx: no shipped version works for a custom-kit + hosted-MCP setup

**Component:** Docker Sandboxes (`sbx`) + hosted MCP control plane · **Platform:** macOS arm64 · **Setup:** a custom `kind: agent` kit (pi-stack) that needs both cloud-provider API keys (Anthropic/OpenAI/Google) *and* hosted-catalog MCP servers (opine/granola/notion/atlassian).

## TL;DR

Two separate, version-boundary bugs box this setup in. There is **no shipped sbx version where both cloud-credential injection and hosted MCP servers work** for a custom kit:

| | cloud model creds | hosted-catalog MCP (opine/granola/notion/atlassian) | local stdio MCP (slack/bamboohr) + host svcs (gws/github/snow) |
|---|---|---|---|
| **0.33.0** (`d7da69cb`) | ✅ works | ❌ **control-plane provisioning 404s** (Bug B) | ✅ works |
| **0.34.0** (`2eae0c4f`) | ❌ **custom-kit credential gate** (Bug A) | ✅ likely (speaks current CP API; unverified) | ✅ works |

Pick 0.33 and lose every hosted MCP server; pick 0.34 and lose every cloud model. Local stdio MCP and host services work on both.

## Bug A — sbx 0.34: custom kits get no host-credential injection

On 0.34, `hasAgentCredentials` (sandboxlib/sandbox/sandbox.go) resolves credentials by **agent name** and `return false`s for any name not in the built-in set (`claude/codex/opencode/gemini/copilot`) or an embedded agent — *before* consulting the kit's declared `sources`/`values`. A custom `kind: agent` kit (e.g. `pi-stack`) therefore gets `hasHostCredentials=false` → `SBX_CRED_*_MODE=none` → the `proxy-managed` sentinel env vars are never set → pi registers zero cloud providers ("No models match pattern ...").

Positive control: the built-in `gemini` agent injects fine against the same secret store (`SBX_CRED_GOOGLE_MODE=apikey`), while a custom kit *and Docker's own reference `contrib/pi` kit* both get `none`. Built-in agent names can't be reused by a custom kit (`agent "codex" is already registered`), so there's no workaround. Full detail + repro: **`sbx-0.34-custom-kit-credentials.md`**.

## Bug B — sbx 0.33: hosted MCP control-plane provisioning is 404

On 0.33, every sandbox start that enables a hosted-catalog MCP server logs (10× across sessions, e.g. 2026-07-06 and 2026-07-08):

```
ERROR mcp: CP gateway provisioning failed  err="create environment: unimplemented: 404 Not Found"
```

The daemon then registers a gateway credential but the **environment never provisions**, so opine/granola/notion/atlassian never attach — a standup/refresh reports them "not in the gateway." `sbx mcp auth --all` succeeds and `sbx login` is fresh; the failure is the control plane answering `unimplemented: 404` to the `create environment` call that 0.33 makes — i.e., **the hosted control plane has dropped the API version 0.33 speaks.**

Effect is scoped to control-plane servers. Local stdio MCP (slack, bamboohr — run via `op run`) and host services (gws-token, snow, github) are unaffected, which matches the observed "chat/gworkspace/github live; crm/calls/docs/issues dark."

Log location: `~/Library/Application Support/com.docker.sandboxes/sandboxes/sandboxd/daemon.log`.

## Minor — daemon crashes

`~/Library/Application Support/com.docker.sandboxes/sandboxes/sandboxd/crashes/` holds 5 crash files (2026-06-22 through 2026-07-03), all **0 bytes** — a crash sentinel is written but no stack/goroutine dump is captured. So: the daemon has restarted ~5× over three weeks, but there's nothing to diagnose from. Worth (a) capturing an actual stack on crash, and (b) treating repeated daemon restarts as a possible contributor to state loss.

## The ask

A shipped sbx version (or a fix to 0.34) where a **custom `kind: agent` kit** gets **both**: host-credential injection for its declared providers, and hosted-catalog MCP servers. Concretely, either:
- fix Bug A on 0.34 (have `hasAgentCredentials` consult the kit's declared `sources`/`values` for non-built-in agents — the data is already passed in), so a custom kit on the current control-plane API gets both; or
- confirm the supported path for a custom multi-provider agent, if it's not "standalone `kind: agent` kit."
