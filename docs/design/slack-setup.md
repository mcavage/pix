# Slack credential model, and PKCE OAuth setup

Status: the credential model below is the accepted design, AND
`pix slack setup|auth|status|disable` (`services/host/cmd/pix/slack.go`) is
IMPLEMENTED on top of it. Scope: `services/host/slack.go`,
`services/host/cmd/pix/slack.go`, `services/host/cmd/pix/slack_oauth.go`,
`services/host/slackoauth/`, `config/op-refs.env.example`,
`services/host/config/config.go`.

## Overview and credential model

`pix slack setup|auth|status|disable` provides guided setup for the `slack` MCP
server (`services/host/slack.go`). It supports two setup paths:

1. **Local PKCE OAuth grant (`pix slack setup`, no `--token-ref`)**: The
   primary setup path. Runs a local PKCE OAuth grant flow against Slack's
   `oauth/v2/authorize` endpoint. PKCE (S256 code challenge/verifier) operates
   as a public client: **no client secret is ever held, requested, or sent**.
2. **Static `--token-ref` fallback (`pix slack setup --token-ref op://vault/item/field`)**:
   Wires an existing `xoxp-` user token already held in 1Password.

### The core problem and safety rule

When `slack` is attached to a sandbox, every call it makes runs as a personal
`xoxp-` user token.

1. **The token is a personal `xoxp-` user token, not a service credential.**
   Slack's `oauth.v2.access` issues `xoxp-` tokens scoped to the *authorizing
   user*, not the app. Whoever's credential backs `slack` is the identity
   every call runs as: `search.messages` searches what that person can see,
   `conversations.history` reads channels that person is in, and
   `users.lookupByEmail` runs with that person's directory visibility.
2. **Handing a token to a second person is impersonation, not sharing access.**
   Every message, search, or lookup that second person triggers shows up in
   audit logs as the original token owner. Sharing credentials collapses
   per-user authorization boundaries.

**Rule: `SLACK_TOKEN` or Slack OAuth credentials are always a single named person's
user credential. It is never a shared "employee" or "team" token, never a bot
token standing in for the workspace, and it is never handed to a second person
to reuse.** Anyone else who wants Slack access through Pix runs their own OAuth
grant.

## PKCE OAuth architecture (no client secret)

PKCE (Proof Key for Code Exchange, RFC 7636) allows public clients to execute
OAuth 2.0 authorization flows securely without embedded client secrets.

### Configuration

Set configuration in `~/.config/pix/config.toml` via `pix config set`:

- **Client ID**: Set via `pix config set slack.client_id <id>` (or pass
  `--client-id <id>` to `pix slack setup`).
- **Redirect URI**: Set via `pix config set slack.redirect_uri <uri>` (or pass
  `--redirect-uri <uri>`). Defaults to `http://localhost:17373/slack/callback`.
  The URI must use `http://localhost:<fixed port>/slack/callback`.

### Setup flow (`pix slack setup`)

1. **Preflight check**: Verifies `sbx` is available and any existing `slack`
   MCP registration is the canonical Pix host command.
2. **Callback listener and PKCE parameter generation**:
   - Binds `127.0.0.1:17373` (or the configured port) before opening the
     browser so a port conflict fails immediately.
   - Generates cryptographically secure random `state` (32 bytes) and PKCE
     `code_verifier` (64 bytes) via `crypto/rand`.
   - Computes PKCE S256 `code_challenge` (`base64url(sha256(verifier))`).
3. **Browser authorization**:
   - Opens `https://slack.com/oauth/v2/authorize?client_id=<id>&user_scope=search:read,channels:read,...&redirect_uri=http://localhost:17373/slack/callback&state=<state>&code_challenge=<challenge>&code_challenge_method=S256` in the user's browser (and prints the URL as fallback).
   - Requests minimal read-only `user_scope`s.
4. **Callback processing**:
   - Listens for a single GET request on `/slack/callback`.
   - Compares `state` using constant-time comparison.
   - Rejects non-GET requests, requests carrying an `Origin` header (CSRF
     guard), or mismatched state. Times out after 5 minutes.
5. **Code exchange**:
   - Exchanges the authorization code via `slackoauth.Client` using PKCE (`code`
     + `code_verifier` + `client_id` + `redirect_uri`).
   - No `client_secret` is sent or required.
   - Returns a rotating credential pair: a 12-hour rotating access token
     (`xoxe.xoxp-...`) and a 30-day refresh token (`xoxe-1-...`).
6. **Live identity verification**:
   - Calls `auth.test` with the newly minted access token to confirm the live
     identity (`team_id`, `user_id`) matches the team and user reported by
     the exchange.
7. **1Password document persistence**:
   - Prompts for user confirmation (skipped with `--yes`).
   - Writes the entire credential blob (`accessToken`, `refreshToken`,
     `accessExpiresAt`, `grantExpiresAt`, `teamID`, `userID`, `scopes`) to a
     1Password document using `slackoauth.OPStore` via `op` CLI.
   - Content is passed over stdin only (never on command-line arguments or disk).
   - Uses the `Private` vault by default (or `--vault <name>` /
     `slack.oauth_vault_id`).
   - Saves `oauth_vault_id`, `oauth_document_id`, and cached
     `oauth_grant_expires_at` to `config.toml`.
   - Writes non-secret identity pins `SLACK_TEAM_ID` and `SLACK_USER_ID` to
     `op-refs.env` and removes any legacy static `SLACK_TOKEN` ref.
8. **Gateway registration and config**:
   - Registers `slack` with the sbx gateway (`sbx mcp add slack ...`) and adds
     `slack` to the `mcp` list in `config.toml`.

### Token lifecycle and 30-day reauthorization

- **Rotating 12-hour access tokens**: Access tokens carry the `xoxe.xoxp-`
  prefix and expire in 12 hours. The runtime manager (`slackoauth.Manager`)
  automatically reads and refreshes tokens from 1Password using the refresh
  token.
- **Monthly grant expiry (30 days)**: The underlying OAuth grant expires after
  30 days (`grant_expires_at`).
- **Automatic access-token refresh**: The Slack MCP runtime refreshes the
  12-hour access token when less than ten minutes remain. No interactive
  command or always-running Pix daemon is needed; the next Slack request runs
  the locked refresh and atomically replaces the 1Password document.
- **Reauthorization**: When the roughly monthly PKCE grant expires, run
  `pix slack auth` to execute a fresh browser grant and update the 1Password
  document. This is the interactive consent boundary, not the 12-hour token
  rotation.

## Static fallback setup (`pix slack setup --token-ref`)

For environments where an `xoxp-` user token is already stored in 1Password,
`pix slack setup --token-ref op://vault/item/field` is supported as a fallback:

1. Requires `--token-ref` to be an `op://vault/item/field` reference.
2. Resolves the reference via `op read` and verifies the token starts with `xoxp-`.
3. Performs a live `auth.test` call to verify identity and asks for confirmation.
4. Writes `SLACK_TOKEN`, `SLACK_TEAM_ID`, and `SLACK_USER_ID` refs to `op-refs.env`.
5. Registers `slack` with the sbx gateway and saves `config.toml`.

## Status verification (`pix slack status`)

`pix slack status` executes live probes based on the active mode in `config.toml`:

### OAuth mode (`slack.client_id`, `slack.oauth_vault_id`, `slack.oauth_document_id` set):
- Reports mode and non-secret 1Password document locators (`vault`, `document_id`).
- Reports cached grant expiry countdown (warns when 7 days or fewer remain;
  returns exit code 1 / todo when expired).
- Obtains or refreshes a live access token via `slackoauth.Manager` reading
  the 1Password document (using the same runtime component as `services/host/slack.go`).
- Performs `auth.test` against Slack to confirm live identity and verifies
  it matches `SLACK_TEAM_ID` and `SLACK_USER_ID` pins.
- Checks sbx gateway registration and notes sandbox attachment state.

### Static mode (`--token-ref`):
- Checks `SLACK_TOKEN` ref in `op-refs.env`.
- Resolves token and runs live `auth.test`.
- Compares live identity against `SLACK_TEAM_ID`/`SLACK_USER_ID` pins.
- Checks sbx gateway registration.

## Disable and revocation (`pix slack disable`)

`pix slack disable` removes Slack integration cleanly:

### OAuth mode:
1. **Revokes token at Slack first**: Calls `auth.revoke` with the live access
   token and requires Slack confirmation (`ok: true`, `revoked: true`).
2. **Archives 1Password document**: Archives the credential document (`op document delete --archive`).
3. **Removes gateway registration and refs**: Unregisters `slack` from the sbx
   gateway (`sbx mcp rm slack`), clears `slack` from `mcp` in `config.toml`, and
   removes `SLACK_TEAM_ID`/`SLACK_USER_ID` refs from `op-refs.env`.
4. **Clears OAuth locators**: Clears `oauth_vault_id`, `oauth_document_id`, and
   `oauth_grant_expires_at` from `config.toml`, while **retaining `client_id` and
   `redirect_uri`** so subsequent setup reuses the app configuration.

### Static mode:
- Unregisters `slack` from the gateway, clears `slack` from `config.toml`, and
  removes `SLACK_TOKEN`/`SLACK_TEAM_ID`/`SLACK_USER_ID` refs. (Does not revoke
  at Slack because no client credentials are held).

## Registration vs. attachment

- **Registration**: `pix slack setup` registers the host MCP server with the sbx
  gateway (`sbx mcp add slack`).
- **Sandbox attachment**: Registration makes the server known to the gateway, but
  **running sandboxes do not see the server until they are recreated**
  (`pix run --replace`) or attached live (`pix mcp load slack`). `pix slack status`
  reports registration and attachment as separate properties.

## Minimal read-only scopes

`services/host/slack.go` calls these Slack Web API methods, requiring only minimal read-only `user_scope`s:

| Method | Tool(s) | User scope needed |
| --- | --- | --- |
| `auth.test` | `health` | none beyond a valid token |
| `search.messages` | `search_messages` | `search:read` |
| `conversations.list` | `list_channels` | `channels:read`, `groups:read`, `im:read`, `mpim:read` (per `types` requested) |
| `conversations.history` | `read_channel` | `channels:history`, `groups:history`, `im:history`, `mpim:history` |
| `conversations.replies` | `read_thread` | same `*:history` scopes as above |
| `users.info` | `get_user`, name resolution | `users:read` |
| `users.list` | `search_users`, bulk name resolution | `users:read` |
| `users.lookupByEmail` | `get_user` (by email) | `users:read.email` |

No write scopes (`chat:write`, `channels:write`, …) appear anywhere in `slack.go` — the server is read-only by construction.
