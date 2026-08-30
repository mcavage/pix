# Google Workspace comes from your environment

Google Workspace is **not a pix feature**. Pix ships no MCP servers, installs
no `gog`, stores no Google account, and special-cases no vendor. There is no
`gworkspace` verb.

What pix has is the `gworkspace` **capability** (`capabilities.json`) and the
`gworkspace` **skill** (read tools, the untrusted-content rule). Both resolve
to an MCP server named `google-workspace`. Something has to *provide* that
server, and in v2 that is your environment's native `.sbxenv.yaml`, wiring
the same host command any other MCP integration uses.

## What the environment declares

An `mcp.servers` entry in `.sbxenv.yaml`, in native sbx grammar, wires a
host-executed command over stdio:

```yaml
mcp:
  servers:
    google-workspace:
      command: gog
      args:
        - --gmail-no-send
        - --wrap-untrusted
        - --readonly
        - mcp
        - --allow-tool
        - read
      env:
        GOG_KEYRING_PASSWORD: ${op://vault/item/field}
```

`pix.toml` may add the annotations Pix itself needs, without touching the
native declaration: a doctor probe and any extra non-secret env names.

```toml
[host.mcp.google-workspace]
env_keys = ["GOG_ACCOUNT"]
probe = ["gog", "--readonly", "gmail", "labels", "list"]
```

`.sbxenv.yaml` owns the command, argv, and credential binding; `pix.toml`
only annotates what pix can check. A host command that runs on your machine
must be approved once with `pix env trust NAME` before a launch will use it,
the same gate that covers any other host-executing configuration.

## Wiring it up

```bash
pix env trust NAME             # review and accept the host command, once
pix run                          # launch; the Gateway registers what the
                                  # environment declared
pix doctor                       # registered, or actually working?
pix rm BOX && pix run            # a declaration reaches a session at CREATE only
```

## Verifying it

`pix doctor` distinguishes **registered** from **working**. For a declared
command it checks the binary resolves on PATH, then runs the declared
`probe` through the same wrapper the Gateway uses to spawn the server, so a
credential that only works because it happens to be exported in your shell
fails here, which is the point. A server with no declared probe is reported
as *unverified*, never as healthy.

For `gog` itself the probe has to be a real READ: `gog --readonly gmail
labels list`, which exits 0 when the keyring opens and a token is readable,
and nonzero when it cannot. Two obvious-looking alternatives verify nothing,
both measured on gog v0.35.0: its MCP tool-listing flag prints the full tool
list with no credentials at all, and its auth self-diagnosis prints `status
error` on a dead keyring and still exits 0. Pix judges a probe by its exit
code, so either would pass on a completely broken install.

**`gog mcp --list-tools` proves nothing.** It dumps a static tool registry
without touching the keyring: it prints the full list and exits 0 with no
credentials at all. It passes on a completely broken install. Do not use it
as a check, and do not trust a doc that tells you to.

**Do not copy a `GOG_HOME` out of any document, including this one.** gog's
root is platform-dependent, so no path written down here can be right for
you. Run `gog auth status`; it prints the home it is actually using. Setting
the variable to a path you read somewhere points gog at an empty,
unauthorized home.

## Security posture

A host-declared MCP server runs **on the host, outside the sandbox, with
your host-user privileges**, and everything it returns lands in the
conversation sent to your model provider. That is the trade for reaching
your real mailbox at all.

- **Prompt injection through returned content.** Anyone can send you an
  email or share a doc. A read-only server stops writes, not reads: an
  injected agent can still read your Google data and try to exfiltrate it
  elsewhere. gog's `--wrap-untrusted` fences returned bodies as data rather
  than instructions, a mitigation, not a guarantee. The `gworkspace` skill
  carries the rule the agent is asked to hold.
- **Writing documents** is out of scope for the base read-only capability. A
  separate `docs-write` capability, if you wire one, resolves to a narrowly
  scoped write server without altering the read-only boundary above. If
  `docs-write` resolves to none, the agent will plainly state that writing
  docs is not wired.
- **The keyring password unlocks standing OAuth.** Whatever process env
  holds it can read your mail. Keep gog's home and keyring file owner-only
  and the host single-user; if the password leaks, treat the OAuth grant as
  compromised.
- **Revoking** is a Google-side action: your account's
  [third-party access page](https://myaccount.google.com/permissions), then
  re-authorize. Rotating the password means updating the `op://` item and
  re-running `pix env trust NAME` if the reviewed fingerprint changed, or
  recreating the sandbox (`pix rm BOX && pix run`) so the next spawn picks it
  up.

See `../SECURITY.md` for the trust boundary this sits outside of.
