# Google Workspace via `gog` (host MCP) — setup + migration

`gws` is gone. Google Workspace now runs as **`gog mcp`, a host-side MCP server**
the sbx gateway spawns (like `slack`). Your OAuth creds stay in `GOG_HOME` on the
host; only typed, read-only tool results cross into the sandbox. No token service,
no in-VM wrapper, no bearer forwarding.

## The one trap (read this first)
`gog auth` working in your Terminal proves **nothing** about the gateway. The
gateway spawns `gog mcp` with a bare, non-interactive env. If the keyring
password isn't in the env it inherits, the server starts and returns **zero tools,
silently** — the gws-style hours-in-circles trap. On macOS with the system
keychain, `gog` can unlock the stored token without a password and step 3 is
skipped; on other setups (file keyring, headless CI) step 3 is not optional.
`pi-stack doctor` probes the real headless path, not just `gog auth doctor`.

> **Note:** the sbx Cloud MCP Gateway (`SBX_MCP_URL`) is not yet publicly
> released. `gog` runs through it; without gateway access the `gworkspace`
> capability is unavailable regardless of gog setup.

## Host quickstart (6 steps, macOS)
```bash
# 1. Install
brew install gog                       # or see https://gogcli.sh/install.html

# 2. Register your OAuth client + authorize YOUR account (browser, once).
#    Use minimal READ-ONLY scopes (gmail.readonly, calendar.readonly, drive.readonly…).
gog auth add-client ~/Downloads/gog-oauth-client.json
gog --account you@example.com auth login

# 3. THE STEP EVERYONE SKIPS (file-keyring or headless setups only).
#    macOS system keychain users can skip this — gog unlocks the stored OAuth
#    token without a password. If you use a file keyring or need 1Password to
#    supply the keyring password, create the env-file at the XDG config path:
mkdir -p ~/.config/pi-stack
cat >> ~/.config/pi-stack/op-refs.env <<'EOF'
GOG_ACCOUNT=you@example.com
GOG_HOME=/Users/you/.config/gog
GOG_KEYRING_BACKEND=file
GOG_KEYRING_PASSWORD=op://Private/gog-keyring/password
EOF
#    (make-based flow uses config/op-refs.env in the repo instead of this path)

# 4. Prove it the way the gateway will — MUST print a non-empty tool list.
#    With op-refs: use op run. Without op-refs (system keychain), run directly.
op run --env-file=~/.config/pi-stack/op-refs.env -- gog --account you@example.com mcp --list-tools
# or (system keychain, no op):  gog --account you@example.com mcp --list-tools

# 5. Enable + register.
pi-stack config set gog_account you@example.com   # writes to ~/.config/pi-stack/config.toml
pi-stack config set mcp gog                       # adds gog to the mcp list in config.toml
pi-stack mcp register                              # registers the hardened read-only server
# The make-based flow reads the SAME config.toml (via pi-stack config get), so
# the two commands above configure make mcp-register too.

# 6. Launch with gog attached, then confirm.
make run          # attaches gog per the mcp list in config.toml; or: pi-stack run
pi-stack doctor   # gog group should be all green
```

The registered server is locked down by default:
`gog --account <you> --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read`
— read-only, can't send mail, returned Gmail/Doc bodies are fenced as untrusted
data. Add write tools later, per surface, only when you want them
(`--allow-write --allow-tool 'docs.*'`).

## Migrating off gws (teardown)
Do steps 1–5 above, then tear down the old path:
- `gws auth logout` on the host; stop anything on `:11441`.
- Remove any `GWS_TOKEN_AUTH` from your environment/secrets (the bearer is gone).
- `sbx rm -f pi-stack-pi-stack && make run` to recreate on the gws-free image
  (needs `make load` first to rebuild).

Four moving parts (token service + wrapper + bearer forwarding + Google allowlist)
collapse to one MCP registration authed entirely on the host.

## Security posture (why this is safe for full-auto)
Runs as **your** account (a throwaway account would be useless), but hardened:
minimal read-only OAuth scopes + gog `--readonly`/`--gmail-no-send`/
`--wrap-untrusted` + a revocable OAuth client. Residual risks to know: a
prompt-injected agent can still *read* your Google data and try to exfiltrate it
through some other channel (read-only stops writes, not reads); and the keyring
password in the gateway's process env unlocks standing OAuth — keep `GOG_HOME`
0700, the keyring 0600, single-user host, and rotate if exposed.
