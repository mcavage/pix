# Slack credential model, and a safe onboarding path

Status: the credential model below is the accepted design, AND
`pix slack setup|status|disable` (`services/host/cmd/pix/slack.go`) is now
IMPLEMENTED on top of it — the "Future work" section further down describes
what actually shipped, not a proposal. Scope: `services/host/slack.go`,
`services/host/cmd/pix/slack.go`, `config/op-refs.env.example`,
`services/host/config/config.go`.

**What `pix slack setup` does NOT do, and why that still matters:** it never
implements the OAuth authorize/exchange dance itself — no fake OAuth. It
takes an `op://vault/item/field` reference to an `xoxp-` token you (or an
org-owned exchange service, below) already put in 1Password, verifies it
LIVE against Slack's `auth.test`, and wires it up. Obtaining that token in
the first place still needs one of the two paths this doc specifies: your own
Slack app's OAuth grant, or the org-owned callback/exchange service described
below. There is no shortcut around registering a Slack app and holding its
client secret somewhere — that is also true of Slack's own hosted remote MCP
server, which likewise needs a preregistered OAuth client before anyone can
authorize against it. `pix slack setup` only removes the NEED to hand that
client secret to every user; it does not remove the need for the app itself.

## The problem

Today, wiring up the `slack` MCP server means one person pastes a Slack token
into 1Password and points `SLACK_TOKEN` at it (`config/op-refs.env`, `pix
secret set SLACK_TOKEN op://...`). That token is read once, at spawn, by
`services/host/slack.go`, and used as the bearer credential for **every**
Slack Web API call the server makes, for every session, for every user who
happens to be sitting in front of a pix sandbox that has `slack` attached.

That is fine for a single operator wiring up their own Slack access. It stops
being fine the moment a second person uses the same sandbox setup, or the
token is handed around "so everyone can use the Slack MCP." Two facts make
that a real risk, not a hypothetical one:

1. **The token is a personal `xoxp-` user token, not a service credential.**
   Slack's `oauth.v2.access` issues `xoxp-` tokens scoped to the *authorizing
   user*, not the app. Whoever's token sits in `op://.../SLACK_TOKEN` is the
   identity every call runs as: `search.messages` searches what THAT person
   can see, `conversations.history` reads channels THAT person is in,
   `users.lookupByEmail` runs with THAT person's directory visibility. There
   is no bot identity underneath it to fall back to.
2. **Handing that token to a second person is impersonation, not sharing
   access.** Every message, search, or lookup that second person triggers
   shows up (to Slack, to audit logs, to anyone reading `auth.test`) as the
   original token owner doing it. It also means the token owner's access
   silently becomes the ceiling and floor for every user of the shared
   sandbox — nobody gets more or less than owner's own Slack permissions,
   with no per-user record of who actually asked what.

**Rule: `SLACK_TOKEN` is always a single named person's `xoxp-` user token.
It is never a shared "employee" or "team" token, never a bot token standing
in for the workspace, and it is never handed to a second person to reuse.**
Anyone else who wants Slack access through pix goes through their own OAuth
grant (below), not through someone else's `op://` ref.

## What "per-user, not shared" requires — and what it doesn't

A new user does NOT need direct access to the Slack app's client secret. The
Slack OAuth v2 code exchange (`https://slack.com/api/oauth.v2.access`, which
trades an authorization code for a token) needs the app's `client_id` AND
`client_secret` together. If every user who wants to authorize needs to run
that exchange themselves, either the client secret is copied out to every
laptop (defeats the whole point) or authorization never gets easier than "ask
the token owner to paste theirs."

The fix is an org-owned HTTPS callback and exchange service. Put the step that
needs the secret behind that service and never let the secret itself leave it.

```
 end user                         org-owned callback/exchange service        Slack
 --------                         ------------------------------------       -----
 1. opens authorize URL  -------->
                                    (this IS the Slack authorize endpoint,
                                     browser goes straight to Slack)   ------> 2. user approves scopes
                                                                        <----- 3. redirect to the
                                                                                exact registered
                                                                                redirect_uri with
                                                                                ?code=...&state=...
 4. browser lands on
    the org service's
    redirect_uri        -------->  5. service validates `state`,
                                     then calls oauth.v2.access with
                                     code + client_id + client_secret --------> 6. Slack returns
                                                                                {access_token: xoxp-...,
                                                                                 authed_user: {...}}
 7. service hands the user   <----  the freshly-issued xoxp- token
    their own xoxp- token           (one-time; never logged, never
    (or a one-time op:// write)     stored server-side beyond the exchange)
```

The user's browser talks to Slack directly for the authorize step (no secret
involved there — that leg only needs `client_id`, which is not sensitive).
Only step 5, the code-for-token exchange, needs `client_secret`, and that
step runs entirely inside the org-owned service. The end user never sees or
handles the client secret; they only ever see the `xoxp-` token that comes
back addressed to them.

### The authorization URL

```
https://slack.com/oauth/v2/authorize
  ?client_id=<app client id>              (public identifier — not the secret)
  &user_scope=search:read,channels:read,channels:history,
              groups:read,groups:history,
              im:read,im:history,
              mpim:read,mpim:history,
              users:read,users:read.email
  &redirect_uri=<exact, pre-registered HTTPS callback URL>
  &state=<opaque, single-use, server-generated token bound to this attempt>
```

Notes that matter:

- **`user_scope`, not `scope`.** The `scope` param requests bot-token scopes;
  this flow only ever wants a user token, so every scope goes on
  `user_scope`. Anything that shows up under `scope` in a walkthrough for
  this app is a sign the app is misconfigured for this model.
- **`redirect_uri` must match, byte for byte, one of the URIs registered on
  the Slack app**, including scheme, host, and path. Slack rejects anything
  else at the authorize step, which is the whole point: it's the mechanism
  that stops a state/code pair from being redeemable anywhere but the
  service that requested it.
- **`state` is generated per attempt, single-use, and checked on the way
  back.** It is not a secret in the `client_secret` sense (it doesn't grant
  anything by itself), but it is what stops a stale or replayed callback
  from being accepted, and it is how the callback correlates "this code" to
  "this user's onboarding attempt."

### Code exchange (server-side, inside the org service)

The callback handler:

1. Confirms `state` matches an attempt it issued and hasn't already
   consumed it.
2. Calls `oauth.v2.access` with `client_id`, `client_secret` (both held only
   by the service, e.g. as its own `op://` refs — never shipped to a
   sandbox or a laptop), `code`, and the same `redirect_uri`.
3. Reads `authed_user.access_token` (the `xoxp-` token) and `authed_user.id`
   off the response. Discards the bot-scoped fields entirely — this flow has
   no use for `access_token` at the top level (that's the bot token) or
   `authed_user.scope` beyond a sanity check against what was requested.
4. Hands the user their token back exactly once — either directly to their
   terminal (for `pix secret set`, below) or by writing it straight to a
   1Password item the service creates for them, whichever future
   `pix slack setup` lands on. The service does not keep a copy after that.

### Personal token storage: `op://` ref, same as today

Whatever the exchange hands back is stored exactly the way every other pix
credential is: as a 1Password item, referenced from `config/op-refs.env` as
`SLACK_TOKEN=op://<vault>/<item>/<field>`, resolved by the gateway at spawn
time via `op run --env-file`. The token never touches disk in the sandbox and
never touches the repo. This part of the model doesn't change — the only
thing that changes is that the item the ref points at now belongs to the
person who ran the OAuth grant, not to whoever set up Slack first.

### Identity verification

`services/host/slack.go`'s `health` tool already calls `auth.test` and
returns `team`, `team_id`, `user`, and `user_id` straight from Slack's
response (`slackToolHandlers()["health"]`, `slack.go:206-212`). That is the
ground truth for "whose token is this," and it is cheap (no scopes beyond an
already-valid token) and idempotent — call it after every setup, and any
time you need to confirm which person's Slack identity a running server is
acting as. A future `pix slack status` reuses this exact call; see below.

### Rollback and revocation

If a token is compromised, shared by mistake, or the person leaves:

1. **Revoke at Slack first.** The token owner (or a workspace admin) revokes
   the app's authorization for that user from Slack's "Apps" management
   page, or the org service calls `auth.revoke` with the token. This is the
   step that actually invalidates it — deleting the `op://` item alone
   leaves a still-valid token sitting in whatever cached env a running
   sandbox already resolved it into.
2. **Remove the ref**: `pix secret rm SLACK_TOKEN` (or delete the 1Password
   item directly) so a future host-side Slack MCP process cannot resolve it.
3. **Restart the Slack MCP process or recreate the sandbox if needed.** The
   token stays on the host and never enters the sandbox, but a gateway-managed
   Slack process that is already running may retain the bearer it resolved at
   spawn. Revocation in step 1 is the immediate security boundary.
4. **Re-verify with `health`/`auth.test`**: after rotation, confirm the new
   token resolves to the right identity before trusting it.

### Minimal scopes actually used

`services/host/slack.go` calls exactly these Slack Web API methods (see
`slackToolHandlers()`), which is the scope list the authorize URL above
requests and nothing wider:

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

No write scopes (`chat:write`, `channels:write`, …) appear anywhere in
`slack.go` — the server is read-only by construction, the same posture as
gog's Gmail/Drive read tools.

## `pix slack setup|status|disable` (implemented)

Implemented in `services/host/cmd/pix/slack.go`, following the shape `pix
gworkspace setup|status|disable` already established:

- **`pix slack setup --token-ref op://vault/item/field [--yes]`** takes an
  EXISTING token (never a pasted one — a non-`op://` value is rejected
  outright, without ever echoing it back), resolves it via `op read`,
  requires the resolved value start with `xoxp-`, and verifies it LIVE via
  `auth.test` before anything is written. It prints the resolved team/user
  and asks for confirmation (skipped with `--yes`). Only after that does it
  write `SLACK_TOKEN` (through the existing `pix secret set` primitive) plus
  the non-secret `SLACK_TEAM_ID`/`SLACK_USER_ID` identity pins, register
  `slack` with the sbx gateway (via the existing `registerServers`, which
  hard-fails if `sbx` is absent rather than silently reporting success), and
  save config. A registration failure or a save failure after a successful
  registration leaves things exactly as documented below in "Rollback".
- **`pix slack status`** reports, from probes: whether `SLACK_TOKEN` is a
  filled ref, the identity a fresh `auth.test` resolves it to RIGHT NOW,
  whether that still matches the `SLACK_TEAM_ID`/`SLACK_USER_ID` pin recorded
  at setup (a mismatch means the token was swapped underneath the pin, which
  is exactly the silent-drift case this doc opened with), and whether `slack`
  is registered with the sbx gateway — reported as a separate line from
  attachment, per the invariant below.
- **`pix slack disable`** removes the gateway registration, the `slack` mcp
  config entry, and the `SLACK_TOKEN`/`SLACK_TEAM_ID`/`SLACK_USER_ID` refs. It
  says explicitly that this does NOT revoke the token at Slack, and that a
  gateway-managed process that already resolved the old token at spawn may
  keep using it until restarted — see "Rollback and revocation" above.

The two invariants this shape depends on:

- **Registration is not the same as attachment.** `pix slack setup` running
  the OAuth dance and writing `SLACK_TOKEN` to `op-refs.env` only makes the
  server *registerable* (`pix mcp register` / `sbx mcp add`). A sandbox
  already running does not see it until it's recreated
  (`pix run --replace`) or explicitly attached (`pix mcp load slack`) — same
  rule as every other MCP server (`docs/reference.md` §8–9). `pix slack
  status` must report these as separate facts (registered? attached to
  THIS sandbox? both?), not collapse them into one boolean.
- **Status proves the authenticated identity, not just presence of a
  token.** `pix slack status` should call `health` (`auth.test`) and print
  the `team`/`user` it gets back, the same "prove it, don't assert it"
  posture as every other `pix status`/`doctor` check
  (`services/host/cmd/pix/doctor.go`). Reporting "SLACK_TOKEN is set" without
  confirming whose identity it resolves to is exactly the gap that lets a
  shared/stale token go unnoticed.
- **`pix slack disable`** removes the ref and the registration (mirrors
  rollback step 2 above); it does not by itself recreate running sandboxes,
  and should say so.

## What `pix slack setup|status|disable` deliberately does not do

- It does not implement the OAuth authorize/exchange dance. It accepts an
  already-issued token (as an `op://` ref) and verifies it live; it never
  talks to Slack's authorize endpoint, never holds a `client_id`/
  `client_secret`, and never mints a token itself. No fake OAuth.
- It does not add write tools or broaden `services/host/slack.go`'s API
  surface. The runtime now requires the resolved credential to be an `xoxp-`
  personal token and, when `SLACK_TEAM_ID`/`SLACK_USER_ID` pins are present,
  calls `auth.test` once at process startup and refuses data-bearing calls on
  a mismatch. The legacy `SLACK_USER_TOKEN`/`SLACK_BOT_TOKEN` env lookup
  fallbacks remain, but a bot token is rejected by the personal-token check.
- It does not stand up the org-owned callback/exchange service. That's
  infrastructure a specific org provisions; this doc specifies the contract
  it must satisfy (holds the client secret, validates `state`, hands back a
  personal token) so `pix slack setup --token-ref` has a token to point at
  once it exists. The same is true of Slack's own hosted remote MCP server:
  it also needs a preregistered OAuth client before anyone can authorize
  against it — there is no path that skips app registration entirely.
