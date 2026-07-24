# Google Workspace via `gog` (host MCP)

Google Workspace runs as **`gog`, a host-side MCP server** the sbx gateway
spawns on demand, the same pattern as `slack`. Your OAuth credentials stay in
`GOG_HOME` on the host; only typed, read-only tool results cross into the
sandbox. There is no token service, no in-VM wrapper, no bearer forwarding.

`pi-stack gog setup` is the guided path: one command checks `gog` is
installed, imports your OAuth client, authorizes your account, verifies the
gateway can actually call it headlessly, and registers it. It is an
**orchestrator**, never a credential store: it never reads or prints your
OAuth client JSON's contents, and no organization client is bundled here;
bring your own.

## Data disclosure

Gmail, Drive, Docs, Sheets, and Calendar content that `gog` returns is **not
private from your model provider.** Once a tool call fetches a message, a doc,
or an event, its text goes into the prompt/context sent to whichever model is
active for this session: Claude, OpenAI, Gemini, or a local Ollama model.
This is the same disclosure as [memory](memory.md#the-shape): only the OAuth
tokens and the network call stay host-side; the *content* a tool returns is
visible to your cloud provider like any other context. **Never point `gog` at
an account whose email you wouldn't want summarized, quoted, or reasoned
about by that provider.**

If your organization has enterprise/zero-data-retention terms with a model
provider, that governs what the provider does with prompts it receives;
pi-stack makes no compliance claim of its own and does not verify your
provider agreement. Check your provider contract before pointing `gog` at
regulated or sensitive mailboxes.

## Quickstart

```bash
pi-stack gog setup --account you@example.com --credentials ~/Downloads/gog-oauth-client.json
```

On a real terminal, omit either flag and you'll be prompted for it. Add
`--yes` to fail instead of prompting (CI). Re-running a healthy setup is safe
and idempotent: it re-verifies and re-registers deterministically.

## What it does

1. Checks the `gog` CLI is installed. If not, it prints the install command
   (`brew install gog`, or see https://gogcli.sh/install.html) and stops.
2. Validates `--credentials` points at a TRUE regular file (a FIFO, socket,
   or device is rejected, even though it "exists and isn't a directory"; a
   symlink to a genuine regular file is still fine). The contents are never
   read or printed; only the path is passed to `gog` as an argument.
3. Probes `gog auth --help` once and picks the **first supported route** for
   the installed version (see below). It then probes that SELECTED route's
   own subcommand help (e.g. `gog auth setup --help`), not just the
   top-level subcommand names, and confirms every flag the route needs is
   advertised, **including `--readonly` on whichever step actually performs
   the OAuth grant.** `pi-stack gog setup` always passes `--readonly` at
   grant time; if the installed `gog` cannot advertise that flag for the
   selected route, the command fails with upgrade guidance rather than
   authorizing without it or trying an older, unguarded route.
4. **Preflights every remaining predictable hard requirement before any
   authorization happens:** `sbx` must be on PATH (it registers gog with
   the gateway; missing it is a hard failure, never a silent "would
   register" success), your `pi-stack` config must load cleanly, and
   whatever gog registration already exists must be **confirmed**, either
   confirmed absent, or confirmed present with a readable command. This
   confirmation is bounded and genuinely three-way, not a yes/no: if the
   registration listing itself is momentarily unavailable, or gog is listed
   but its registered command can't be parsed back, that counts as
   **unknown**, never as absent, and `pi-stack gog setup` refuses to
   authorize or overwrite anything until it's readable, so an existing
   registration this command can't confidently read is never silently lost
   to a same-run rollback. Only once every one of these is confirmed does
   it import the client and authorize `--account` by running the route
   interactively; it inherits this terminal's stdin/stdout/stderr, so a
   browser or device-code flow works normally.
5. Verifies interactive auth (`gog --account <you> auth doctor --check`),
   **then** verifies the headless path the sbx gateway will actually use
   (see "the one trap" below), through the `op run` wrapper when
   1Password/op-refs.env are set up, or the **bare hardened command
   directly** when they aren't (this always runs; it is never skipped, not
   even when the OS keychain usually makes interactive auth alone look
   sufficient). A healthy interactive login with zero headless tools
   **fails the command** with the exact fix, rather than claiming you're
   ready; a probe that times out or can't exec is unverifiable and is
   likewise never reported as success.
6. On success: it registers gog with the sbx gateway FIRST, and only once
   that genuinely succeeds does it save `gog_account` and add `gog` to your
   configured MCP set. A registration failure never touches your persisted
   config; if saving the config fails AFTER a successful registration, it
   rolls the sbx-side registration back to exactly the step 4 snapshot
   (restoring whatever was registered before, or removing the new one)
   rather than leaving config and the gateway to drift apart.

### Current and fallback `gog` CLI routes

Different `gog` releases expose different auth subcommands. `pi-stack gog
setup` probes `gog auth --help` and uses the first of these it finds, in
order:

1. **current, one-shot:** `gog auth setup <account> --credentials <path> --login --readonly`
2. **current, two-step:** `gog auth credentials <path>` then `gog auth add <account> --readonly`
3. **older, legacy:** `gog auth add-client <path>` then `gog --account <account> auth login --readonly`

If your installed `gog` advertises none of these top-level subcommands, OR
the selected route's OWN subcommand help doesn't advertise every flag it
needs (including `--readonly` on the grant step, a syntax change under an
unchanged subcommand name), the command prints the installed version and an
upgrade hint (`brew upgrade gog`) instead of guessing at an obsolete or
unsafe command. You never need to know which route applies, or which flags
it takes: this is exactly what the probing is for.

### Expected output

A healthy run looks like:

```
Importing your OAuth client + authorizing you@example.com (gog auth route: setup, read-only scopes requested)...
This may open a browser for you to sign in.
  running: gog auth setup you@example.com --credentials /path/to/client.json --login --readonly
interactive auth OK for you@example.com
headless tools OK (verified the same host-side path the sbx gateway/doctor use)

gog is dynamically discoverable by default (lean context): the in-VM agent finds + calls it on demand.
Existing sandbox? attach it live: pi-stack mcp load gog
```

## Google OAuth caveats

`gog` authorizes through a **Desktop-app OAuth client** you create yourself in
Google Cloud Console (bring your own; no org-wide client is bundled here).
A few things about that client and the consent screen are worth knowing
before you run `pi-stack gog setup`, because they are Google's rules, not
pi-stack's, and they can change:

- **Internal vs External consent screen.** If your Google Cloud project is on
  a Google Workspace organization, you can usually set the OAuth consent
  screen to **Internal**, which skips Google's verification review entirely
  for accounts inside that org. A personal Gmail account, or an External
  consent screen, does not get that exemption.
- **Testing / unverified apps.** An External consent screen normally starts
  in **Testing** status: it only works for test users you explicitly list in
  Cloud Console, and Google shows an "unverified app" warning that the user
  has to click through (Advanced -> Go to app). The number of test users
  allowed, and exactly when Google requires verification, are **Google
  policy details that change over time**: pi-stack does not hardcode or
  guarantee a specific cap here. Check the current limits in [Google Cloud
  Console](https://console.cloud.google.com/apis/credentials/consent) and
  [Google's OAuth verification
  guidance](https://support.google.com/cloud/answer/9110914) before relying
  on a number you read somewhere else, including this doc.
- **Gmail readonly is a restricted scope.** `https://www.googleapis.com/auth/gmail.readonly`
  (what `--readonly` requests) is one of Google's **restricted scopes**. For a
  personal/test setup this is fine as-is. Rolling this out to an entire
  Workspace organization, or moving the consent screen out of Testing, may
  require Google's **app verification and, for restricted scopes, a
  security assessment**, a real process with real lead time. Don't promise a
  team-wide rollout timeline without checking Google's current requirements
  for your case first.
- **This is a caveat list, not a compliance claim.** pi-stack does not verify
  your Cloud project's consent-screen status, verification state, or scope
  grants on your behalf; `pi-stack doctor` and `gog setup` verify that the
  headless path actually returns tools, which is a functional check, not a
  policy one.

## The one trap (read this first)

`gog auth doctor` working in your interactive shell proves **nothing** about
the gateway. The sbx gateway spawns `gog` headless, in a bare,
non-interactive environment. If the keyring password isn't in the env the
gateway gives it, the server starts and returns **zero tools, silently**: the
classic hours-in-circles trap.

`pi-stack gog setup` always probes the real headless path, whether or not
1Password is set up: through the `op run` wrapper when `op` and
`op-refs.env` are both present, or the **bare hardened command directly**
when they aren't. On macOS with the system keychain, `gog` usually unlocks
the stored token without a password, so this bare probe usually comes back
healthy on its own. On a file keyring or a headless/CI host, it doesn't: you
need to supply the keyring password via 1Password references in your
`op-refs.env`:

```bash
pi-stack config path op-refs   # prints the exact file to edit
```

Add (or let `pi-stack secret set` add) these keys; they're on the documented
non-secret allowlist except the password itself:

```
GOG_ACCOUNT=you@example.com
GOG_HOME=/home/you/.config/gog
GOG_KEYRING_BACKEND=file
GOG_KEYRING_PASSWORD=op://Private/gog-keyring/password
```

`pi-stack gog setup` and `pi-stack doctor` both probe the **real** headless
path: `op run --env-file=<op-refs.env> -- gog --account <you> ... mcp
--list-tools` when 1Password is set up, or the same hardened command run
BARE when it isn't, not just `gog auth doctor`. This probe always runs (it
is never skipped): a clean pass means the gateway will actually get tools;
a clean zero-tool result fails outright with the exact fix; and a timeout or
exec error is unverifiable and is never reported as a pass either way.

## Verification

Run `pi-stack doctor` any time; its gog group probes the exact command the
sbx gateway registered (account, op-refs path, and binaries as-registered),
not a reconstruction, whenever sbx can report it. A green gog group means the
gateway will get tools; a `⚠` means unverifiable (for example, no `op`
installed to check with), never a silent false pass. `pi-stack gog setup`
run again is the same check, plus it re-registers if anything changed.

## Dynamic discovery vs. eager attach

By default, `gog` is registered but **dynamically discoverable**: it isn't in
a fresh sandbox's context at creation, and the in-VM agent finds and calls it
on demand (mcp-find/mcp-exec). Pin it eager instead with:

```bash
pi-stack config set mcp_static gog
```

so a freshly created sandbox has it in context from the start. Either way,
attach it to an **already-running** sandbox live, no recreate needed:

```bash
pi-stack mcp load gog
```

## Security posture (why this is safe for full-auto)

Runs as **your** account (a throwaway account would be useless to you), but
hardened in two independent layers:

1. **At OAuth grant time**, `pi-stack gog setup` always passes `--readonly`
   on the auth command that actually performs the grant, and refuses to
   proceed at all if the installed `gog` can't advertise that flag for the
   selected route (see above). This is a REQUEST for read-only scopes, made
   explicitly rather than left to gog's defaults: `gog` exposes no stable,
   parseable scope-inspection command pi-stack can check the actual granted
   scopes against, so this is honestly what it is: a guaranteed request, not
   an independently verified grant.
2. **At runtime, regardless of what was granted**: gog's own
   `--gmail-no-send --wrap-untrusted --readonly --allow-tool read` flags,
   passed every time the sbx gateway spawns gog (mcp.go), block write calls
   at the tool layer. This is the actual backstop: it holds even if a grant
   somehow carried broader scopes than requested.

Plus a revocable OAuth client. Residual risks worth knowing: a
prompt-injected agent can still *read* your Google data and try to
exfiltrate it through some other channel (read-only stops writes, not
reads), and the keyring password in the gateway's process env unlocks
standing OAuth: keep `GOG_HOME` at `0700`, the keyring file at `0600`, this
a single-user host, and rotate the client if it's ever exposed.

## Rotation and revocation

`gog` grants are host-side and long-lived by design (that's what makes
headless calls work at all), so know how to shut one off before you need to.
There is a **named owner** for this: whoever controls the Google account
`--account` names, and whoever has access to the Google Cloud project the
OAuth client lives in. On a shared host, write down who that is.

If a grant, client, or keyring password may have leaked (lost laptop,
committed `client.json`, leaked keyring password), do all of these, in
order:

1. **Revoke the user's OAuth grant.** In the Google account that ran
   `--account`, go to [Google Account security → Third-party
   access](https://myaccount.google.com/permissions) and remove the OAuth
   client's access. This kills the standing refresh token immediately,
   independent of anything below.
2. **Rotate/replace the OAuth client in Google Cloud Console.** Don't just
   delete-and-recreate a client with the same name; go to APIs & Services →
   Credentials, delete the compromised Desktop client, and create a fresh
   one with a new client ID/secret. A leaked `client.json` is a leaked
   client secret: revoking the user grant (step 1) stops it from acting as
   *you*, but the old client credentials themselves stay valid for issuing
   NEW grants until you delete the client.
3. **Rerun the guided setup with the new client:** `pi-stack gog setup
   --account you@example.com --credentials <path-to-new-client.json>`. This
   re-imports the client, re-authorizes, re-verifies the headless path, and
   re-registers with the gateway in one step; don't hand-edit config or the
   sbx registration to swap the client in.
4. **Rotate the keyring password too, if it was ever exposed** (checked into
   git, pasted somewhere, shared over a channel you don't fully trust): pick
   a new value, update it in 1Password (`GOG_KEYRING_PASSWORD` op:// ref),
   and re-run `pi-stack gog setup` so the new password is what the headless
   probe actually verifies against.
5. **Verify with `pi-stack doctor`.** Its gog group probes the exact command
   the gateway will run, with the new client/grant/password in place, a
   green result is your evidence rotation actually worked, not just that you
   ran the commands.

## Already authorized `gog` yourself?

`pi-stack gog setup` is safe to run even if you've already authorized the
account by hand: it re-runs the auth route's commands (safe to repeat; `gog
auth setup`/`auth login` refresh, not duplicate, an existing grant), then
verifies and registers exactly as a first run does. You don't need a separate
manual path; the guided command IS the supported one.
