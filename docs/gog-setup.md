# Google Workspace via `gog` (host MCP) — setup + migration

`gws` is gone. Google Workspace now runs as **`gog mcp`, a host-side MCP server**
the sbx gateway spawns (like `slack`). Your OAuth creds stay in `GOG_HOME` on the
host; only typed, read-only tool results cross into the sandbox. No token service,
no in-VM wrapper, no bearer forwarding.

## The one trap (read this first)
`gog auth` working in your Terminal proves **nothing** about the gateway. The
gateway spawns `gog mcp` with a bare, non-interactive env. If the keyring
password isn't in the env it inherits, the server starts and returns **zero tools,
silently** — the gws-style hours-in-circles trap. So step 3 below is not optional,
and `pi-stack doctor` probes the real headless path, not just `gog auth doctor`.

## Host quickstart (6 steps, macOS)
```bash
# 1. Install
brew install gog                       # or see https://gogcli.sh/install.html

# 2. Register your OAuth client + authorize YOUR account (browser, once).
#    Use minimal READ-ONLY scopes (gmail.readonly, calendar.readonly, drive.readonly…).
gog auth add-client ~/Downloads/gog-oauth-client.json
gog --account you@example.com auth login

# 3. THE STEP EVERYONE SKIPS — headless keyring for the gateway spawn (one file).
#    Put these in config/op-refs.env (op:// refs resolved by `op run` at spawn):
cat >> config/op-refs.env <<'EOF'
GOG_ACCOUNT=you@example.com
GOG_HOME=/Users/you/.config/gog
GOG_KEYRING_BACKEND=file
GOG_KEYRING_PASSWORD=op://Private/gog-keyring/password
EOF

# 4. Prove it the way the gateway will — MUST print a non-empty tool list.
op run --env-file=config/op-refs.env -- gog --account you@example.com mcp --list-tools

# 5. Enable + register.
echo 'GOG_ACCOUNT = you@example.com' >> config/local.mk
echo 'MCP = gog'                     >> config/local.mk   # (add gog to the MCP list)
make mcp-register                                          # registers the hardened read-only server

# 6. Launch with gog attached, then confirm.
make run                    # (MCP=gog from config/local.mk attaches it)
pi-stack doctor             # gog group should be all green
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
