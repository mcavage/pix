# Google Workspace via `gog` (host MCP)

Google Workspace runs as **`gog`, a host-side MCP server** the sbx gateway
spawns on demand, the same pattern as `slack`. Your OAuth credentials stay in
`GOG_HOME` on the host; only typed, read-only tool results cross into the
sandbox. There is no token service, no in-VM wrapper, no bearer forwarding.

`pi-stack gog setup` is the guided path: one command checks `gog` is
installed, imports your OAuth client, authorizes your account, verifies the
gateway can actually call it headlessly, and registers it. It is an
**orchestrator**, never a credential store — it never reads or prints your
OAuth client JSON's contents, and no organization client is bundled here;
bring your own.

## Quickstart

```bash
pi-stack gog setup --account you@example.com --credentials ~/Downloads/gog-oauth-client.json
```

On a real terminal, omit either flag and you'll be prompted for it. Add
`--yes` to fail instead of prompting (CI). Re-running a healthy setup is safe
and idempotent — it re-verifies and re-registers deterministically.

## What it does

1. Checks the `gog` CLI is installed. If not, it prints the install command
   (`brew install gog`, or see https://gogcli.sh/install.html) and stops.
2. Validates `--credentials` points at a regular file. The contents are never
   read or printed — only the path is passed to `gog` as an argument.
3. Probes `gog auth --help` once and picks the **first supported route** for
   the installed version (see below), then imports the client and authorizes
   `--account` by running that route interactively — it inherits this
   terminal's stdin/stdout/stderr, so a browser or device-code flow works
   normally.
4. Verifies interactive auth (`gog --account <you> auth doctor --check`),
   **then** verifies the headless path the sbx gateway will actually use (see
   "the one trap" below). A healthy interactive login with zero headless
   tools **fails the command** with the exact fix, rather than claiming
   you're ready.
5. On success: saves `gog_account`, adds `gog` to your configured MCP set,
   and registers it with the sbx gateway.

### Current and fallback `gog` CLI routes

Different `gog` releases expose different auth subcommands. `pi-stack gog
setup` probes `gog auth --help` and uses the first of these it finds, in
order:

1. **current, one-shot:** `gog auth setup <account> --credentials <path> --login`
2. **current, two-step:** `gog auth credentials <path>` then `gog auth add <account>`
3. **older, legacy:** `gog auth add-client <path>` then `gog --account <account> auth login`

If your installed `gog` advertises none of these, the command prints the
installed version and an upgrade hint (`brew upgrade gog`) instead of
guessing at an obsolete command. You never need to know which route applies —
this is exactly what the version probe is for.

### Expected output

A healthy run looks like:

```
Importing your OAuth client + authorizing you@example.com (gog auth route: setup)...
This may open a browser for you to sign in.
  running: gog auth setup you@example.com --credentials /path/to/client.json --login
interactive auth OK for you@example.com
headless tools OK (verified the same host-side path the sbx gateway/doctor use)

gog is dynamically discoverable by default (lean context) — the in-VM agent finds + calls it on demand.
Existing sandbox? attach it live: pi-stack mcp load gog
```

## The one trap (read this first)

`gog auth doctor` working in your interactive shell proves **nothing** about
the gateway. The sbx gateway spawns `gog` headless, in a bare,
non-interactive environment. If the keyring password isn't in the env the
gateway gives it, the server starts and returns **zero tools, silently** — the
classic hours-in-circles trap.

On macOS with the system keychain, `gog` usually unlocks the stored token
without a password, so this never bites. On a file keyring or a headless/CI
host, it does — you need to supply the keyring password via 1Password
references in your `op-refs.env`:

```bash
pi-stack config path op-refs   # prints the exact file to edit
```

Add (or let `pi-stack secret set` add) these keys — they're on the documented
non-secret allowlist except the password itself:

```
GOG_ACCOUNT=you@example.com
GOG_HOME=/home/you/.config/gog
GOG_KEYRING_BACKEND=file
GOG_KEYRING_PASSWORD=op://Private/gog-keyring/password
```

`pi-stack gog setup` and `pi-stack doctor` both probe the **real** headless
path (`op run --env-file=<op-refs.env> -- gog --account <you> mcp
--list-tools`), not just `gog auth doctor` — so a pass from either command
means the gateway will actually get tools, not just that your login worked.

## Verification

Run `pi-stack doctor` any time — its gog group probes the exact command the
sbx gateway registered (account, op-refs path, and binaries as-registered),
not a reconstruction, whenever sbx can report it. A green gog group means the
gateway will get tools; a `⚠` means unverifiable (for example, no `op`
installed to check with) — never a silent false pass. `pi-stack gog setup`
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
hardened: minimal read-only OAuth scopes plus gog's own
`--gmail-no-send --wrap-untrusted --readonly --allow-tool read`, and a
revocable OAuth client. Residual risks worth knowing: a prompt-injected agent
can still *read* your Google data and try to exfiltrate it through some other
channel (read-only stops writes, not reads), and the keyring password in the
gateway's process env unlocks standing OAuth — keep `GOG_HOME` at `0700`, the
keyring file at `0600`, this a single-user host, and rotate the client if it's
ever exposed.

## Already authorized `gog` yourself?

`pi-stack gog setup` is safe to run even if you've already authorized the
account by hand: it re-runs the auth route's commands (safe to repeat — `gog
auth setup`/`auth login` refresh, not duplicate, an existing grant), then
verifies and registers exactly as a first run does. You don't need a separate
manual path — the guided command IS the supported one.
