# Google Workspace via `gog` (host MCP)

Google Workspace runs as **`gog mcp`, a host-side MCP server** the sbx gateway
spawns, the same way it spawns `slack`. Your OAuth creds stay in `GOG_HOME` on
the host; only typed, read-only tool results cross into the sandbox. No token
service, no in-VM wrapper, no bearer forwarding.

There is **no built-in guided setup** for this integration. The `pix
gworkspace setup|status|disable` wizard that used to drive the whole flow
(installed-CLI check, OAuth import, headless verification, then gateway
registration) is retired, along with the separate `google-docs-create`
write-scoped companion MCP it could provision. gog is registered the SAME
generic way every other local stdio MCP server is: `pix mcp register`, or a
pack that carries the account. See `docs/design/gworkspace-externalization.md`
for what changed and why.

> **Note:** the sbx Cloud MCP Gateway is not yet publicly released. `gog` runs
> through it, so without gateway access the `gworkspace` capability is
> unavailable regardless of gog setup.

## Manual setup

```bash
brew install openclaw/tap/gogcli   # or see https://gogcli.sh/install.html
```

1. **Install gog and authorize your account.** Minimal read-only scopes
   (`gmail.readonly`, `calendar.readonly`, `drive.readonly`, ...). Follow the
   installed `gog` version's own auth command (`gog auth` or equivalent —
   consult its `--help`; pix does not drive this step).

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

3. **Verify the headless path directly** — the exact non-interactive
   environment the gateway spawns `gog mcp` with. A `gog auth` command working
   in your terminal proves nothing about the gateway: if the keyring password
   isn't in the env it inherits, the server starts and returns **zero tools,
   silently**. It must print a non-empty tool list:

   ```bash
   op run --env-file=~/.config/pix/op-refs.env -- gog --account you@example.com mcp --list-tools
   # system keychain, no op-refs needed:
   gog --account you@example.com mcp --list-tools
   ```

4. **Register with the gateway and enable it in config:**

   ```bash
   pix config set google_workspace_account you@example.com
   pix config set mcp google-workspace
   pix mcp register
   ```

Confirm it worked:

```bash
pix doctor    # Google Workspace group should read ready
pix run       # or: pix rm BOX && pix run, to attach google-workspace to a fresh sandbox
```

The registered server is locked down by default (baked into every
registration by `mcp.GogHardenedArgv`, not something you need to pass):

```
gog --account <you> --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read
```

Read-only, can't send mail, and returned Gmail/Doc bodies are fenced as
untrusted data. There is no built-in write-scoped or document-creation
profile; a pack can add one as its own MCP server if it needs one.

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
either delete and recreate the OAuth client and re-authorize (step 1 above),
or run the `gog` CLI's own re-auth command. Rotating the keyring password
means updating `GOG_KEYRING_PASSWORD` in 1Password (or your op-refs file) and
re-running `pix mcp register` so the gateway picks up the change on its next
spawn.

## Migrating off an old `pix gworkspace setup` install

If a host was set up before this externalization, clean up the stale pieces
the retired wizard could leave behind — see
`docs/design/gworkspace-externalization.md` for the full list.
