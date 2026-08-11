# Google Workspace comes from a pack

Google Workspace is **not a pix feature**. Pix ships no MCP servers, installs
no `gog`, stores no Google account, and special-cases no vendor. The
`gworkspace` verb is deleted and is not coming back.

What pix has is the `gworkspace` **capability** (`capabilities.json`) and the
`gworkspace` **skill** (read tools, the untrusted-content rule). Both resolve to
an MCP server named `google-workspace`. Something has to *provide* that server,
and the only thing that can is the active pack.

## What the pack declares

One `[[integrations]]` stanza in the pack's `pack.toml`, with exactly ONE
transport — `command` (a host binary over stdio), `image` (a container the
gateway runs), `manifest` (an OCI server manifest), or `url` (a remote endpoint
the gateway OAuths):

```toml
[[integrations]]
  name    = "Google Workspace"
  mcp     = "google-workspace"
  command = "gog"
  args    = ["--gmail-no-send", "--wrap-untrusted", "--readonly",
             "mcp", "--allow-tool", "read"]   # LITERAL argv, never templated
  env      = "GOG_KEYRING_PASSWORD"           # the op:// secret
  env_keys = ["GOG_ACCOUNT"]                  # extra env NAMES forwarded
  probe    = ["gog", "auth", "doctor"]        # health probe
  setup    = "google-workspace"               # a [[setup]] block, declarative
```

The pack owns the hardened flags, the credential names, the health probe, and
the install/authorize steps (`[[setup.require]]` / `[[setup.apply]]`). Pix
registers what the pack declared and probes it; it contributes no flags of its
own. Full schema: `docs/reference.md` §5.

Adopting a pack that declares a host command halts at the Tier-1
bill-of-materials review, which prints that exact argv before anything runs.

## Wiring it up

```bash
pix pack use <path|git-url>   # Tier-1 review, then it registers what it declared
pix mcp add google-workspace  # re-register by hand (after rotating a credential)
pix doctor                    # registered, or actually working?
pix rm BOX && pix run         # a registration reaches a session at CREATE only
```

## Verifying it

`pix doctor` distinguishes **registered** from **working**. For a declared
command it checks the binary resolves on PATH, then runs the pack's `probe`
through the same `op run` wrapper the gateway will use to spawn the server — so
a credential that only works because it happens to be exported in your shell
fails here, which is the point. A server with no declared probe is reported as
*unverified*, never as healthy. A registered server no active pack declares is
reported as a gap even though the gateway lists it.

For gog itself the probe is `gog auth doctor`: it checks the keyring backend,
the password, and the stored tokens, and exits non-zero when any of that is
broken.

**`gog mcp --list-tools` proves nothing.** It dumps a static tool registry
without touching the keyring: it prints the full list and exits 0 with no
credentials at all. It passes on a completely broken install. Do not use it as
a check, and do not trust a doc that tells you to.

**Do not copy a `GOG_HOME` out of any document, including this one.** gog's root
is platform-dependent, so no path written down here can be right for you. Run
`gog auth status`; it prints the home it is actually using. Setting the variable
to a path you read somewhere points gog at an empty, unauthorized home — which is
exactly what the guidance this file replaced did.

## Security posture

A `command`-transport MCP server runs **on the host, outside the sandbox, with
your host-user privileges**, and everything it returns lands in the
conversation sent to your model provider. That is the trade for reaching your
real mailbox at all.

- **Prompt injection through returned content.** Anyone can send you an email or
  share a doc. A read-only server stops writes, not reads: an injected agent can
  still read your Google data and try to exfiltrate it elsewhere. gog's
  `--wrap-untrusted` fences returned bodies as data rather than instructions —
  a mitigation, not a guarantee. The `gworkspace` skill carries the rule the
  agent is asked to hold.
- **The keyring password unlocks standing OAuth.** Whatever process env holds
  it can read your mail. Keep gog's home and keyring file owner-only and the
  host single-user; if the password leaks, treat the OAuth grant as compromised.
- **Revoking** is a Google-side action: your account's
  [third-party access page](https://myaccount.google.com/permissions), then
  re-authorize through the pack's setup step. Rotating the password means
  updating the `op://` item and re-running `pix mcp add google-workspace` so the
  next spawn picks it up.

See `../SECURITY.md` for the trust boundary this sits outside of.
