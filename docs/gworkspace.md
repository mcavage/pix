# Google Workspace via `gog` (host MCP)

Google Workspace runs as **`gog mcp`, a host-side MCP server** the sbx gateway
spawns, the same way it spawns `slack`. Your OAuth creds stay in `GOG_HOME` on
the host; only typed, read-only tool results cross into the sandbox. No token
service, no in-VM wrapper, no bearer forwarding.

## Guided setup (recommended)

One command drives the whole flow: installed-CLI check, OAuth client import,
read-only account authorization, headless verification against the exact
command the gateway will spawn, and gateway registration plus config, in that
order, each gated on the last one succeeding.

```bash
brew install gog   # or see https://gogcli.sh/install.html

pix gworkspace setup --account you@example.com --credentials ~/Downloads/gog-oauth-client.json
```

Omit either flag on a real terminal and it prompts for it. It never
authorizes without requesting read-only OAuth scopes, never registers a
server it hasn't just verified returns real tools, and never touches
`config.toml` until sbx registration has already succeeded. Run
`pix gworkspace setup -h` for exactly what each step checks and why.

Confirm it worked:

```bash
pix doctor    # gog group should read ready
pix run       # or: pix run --replace, to attach gog to a fresh sandbox
```

> **Note:** the sbx Cloud MCP Gateway is not yet publicly released. `gog` runs
> through it, so without gateway access the `gworkspace` capability is
> unavailable regardless of gog setup.

## The one trap the guided command exists to catch

A `gog auth` command working in your terminal proves nothing about the
gateway. The gateway spawns `gog mcp` with a bare, non-interactive
environment: if the keyring password isn't in the env it inherits, the server
starts and returns **zero tools, silently**. `pix gworkspace setup` probes the
real headless path, with the exact hardened flags the gateway will use, not
just `gog auth doctor`. On macOS with the system keychain, `gog` can unlock
the stored token without a password and this never bites you. On a file
keyring or headless host, it's the whole reason step 3 below exists.

## What the guided command automates (for troubleshooting)

If `pix gworkspace setup` fails, or you're diagnosing an existing setup, here's
what it does under the hood.

1. **Install gog and authorize your account.** Minimal read-only scopes
   (`gmail.readonly`, `calendar.readonly`, `drive.readonly`, ...). The exact
   gog subcommands vary by installed version; `pix gworkspace setup` detects
   and uses whichever your version supports.

2. **Supply the keyring password, if you need one.** Skip this on macOS with
   the system keychain. On a file keyring, or when 1Password should hold the
   password, add it to your op-refs file at the XDG config path:

   ```bash
   mkdir -p ~/.config/pix
   cat >> ~/.config/pix/op-refs.env <<'EOF'
   GOG_ACCOUNT=you@example.com
   GOG_HOME=/Users/you/.config/gog
   GOG_KEYRING_BACKEND=file
   GOG_KEYRING_PASSWORD=op://Private/gog-keyring/password
   EOF
   ```

3. **Verify the headless path directly**, the same probe `pix gworkspace setup`
   and `pix doctor` run. It must print a non-empty tool list:

   ```bash
   op run --env-file=~/.config/pix/op-refs.env -- gog --account you@example.com mcp --list-tools
   # system keychain, no op-refs needed:
   gog --account you@example.com mcp --list-tools
   ```

4. **Register with the gateway and enable it in config**, exactly what
   `pix gworkspace setup` does on success:

   ```bash
   pix config set gog_account you@example.com
   pix config set mcp gog
   pix mcp register
   ```

The registered server is locked down by default:

```
gog --account <you> --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read
```

Read-only, can't send mail, and returned Gmail/Doc bodies are fenced as
untrusted data. Add write tools later, per surface, only when you actually
want them (`--allow-write --allow-tool 'docs.*'`), by registering gog again
with the flags you want.

## Security posture (why this is safe for full-auto)

Runs as **your** account (a throwaway account would be useless), hardened
with minimal read-only OAuth scopes plus gog's
`--readonly`/`--gmail-no-send`/`--wrap-untrusted` flags and a revocable OAuth
client. Two residual risks worth knowing:

- **Prompt injection through returned content.** A prompt-injected agent can
  still *read* your Google data and try to exfiltrate it through some other
  channel. Read-only stops writes, not reads. `--wrap-untrusted` fences
  returned Gmail/Doc bodies so the agent treats them as data, not
  instructions, but that's a mitigation, not a guarantee.
- **Data transit to model providers.** Returned Google content is sent to the configured/selected model provider as part of the conversation. While OAuth credentials remain strictly host-side in `GOG_HOME` and tool access is limited to read-only, any retrieved Google content is sent to the external model provider to be processed as part of the LLM context.
- **The keyring password in the gateway's process env unlocks standing
  OAuth.** Keep `GOG_HOME` at `0700`, the keyring file at `0600`, and the host
  single-user. If that password or `GOG_HOME` is ever exposed, treat the
  OAuth grant as compromised.

To revoke or rotate access: revoke the grant from your Google Account's
[third-party access page](https://myaccount.google.com/permissions), then
either delete and recreate the OAuth client, or run `pix gworkspace setup`
again with a new credentials file. Rotating the keyring password means
updating `GOG_KEYRING_PASSWORD` in 1Password (or your op-refs file) and
re-running `pix mcp register` so the gateway picks up the change on its
next spawn.
