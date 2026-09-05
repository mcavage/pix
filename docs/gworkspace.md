# Google Workspace in an environment

This is an environment-authoring example. Users should follow their environment's
README and run `pix setup --env NAME`; they do not need to install or wire a
Google connector by hand.

Pix supplies the generic environment/setup machinery and a Google Workspace skill.
The environment supplies the connector, OAuth client, account choices, and setup
hooks. Google does not have a special host command in Pix.

## Declaration and credentials

The native `.sbxenv.yaml` owns the MCP server command. Prefer a pinned container
with an explicit state mount. This abbreviated example assumes a reviewed `gog`
build has already been installed on the host by environment setup:

```yaml
schemaVersion: "1"
agent: pix
mcp:
  servers:
    - name: google-workspace
      command: gog
      args: [--gmail-no-send, --wrap-untrusted, --readonly, mcp, --allow-tool, read]
```

Use the sidecar for credential and probe annotations:

```toml
schema = 1

[host.mcp.google-workspace]
env_keys = ["GOG_KEYRING_PASSWORD"]
plain_keys = ["GOG_ACCOUNT"]
probe_args = ["gog", "--readonly", "gmail", "labels", "list"]
```

`env_keys` resolve 1Password references; `plain_keys` identify non-secret values
such as an account email. Account names do not need to be stored as passwords.
The environment can label these fields with `[host.values.NAME]` and collect them
during setup. The native MCP entry has no per-server `env` map; Pix composes the
credential wrapper from the sidecar. The Gateway starts the resulting process.

For a containerized connector, use the same state mount, account, and keyring
settings in setup, the MCP command, and the probe. Do not point one at an unrelated
host keyring or blindly copy a platform-dependent storage path. A connector with
its own rotating OAuth grant need not declare an unrelated 1Password credential.

## Setup and verification

Declare install/authentication work as `[[setup]]` hooks in `pix.toml`, with every
companion input listed. Collect the OAuth client-file path with the account and
keyring details. Keep the client JSON outside the environment repository. The
hook imports it, completes browser authorization, and checks a real account read.
See [setup in the command reference](reference.md) for the hook schema.

```bash
pix setup --env NAME
pix env show NAME
pix run --env NAME
```

`pix env show` can run a declared probe for an approved environment. A server
being registered is a separate fact from its request succeeding. Check the live
MCP tool in a sandbox too; this proves the Gateway gives the connector the same
working credentials that setup used.

For gog, listing Gmail labels is a useful read probe. A static tool listing such
as `gog mcp --list-tools` proves no account access. A diagnostic that reports an
error in text but exits zero is also unsuitable as a setup success check. Probe
commands must fail when the real operation fails and must not open a browser.

## Access boundaries

Keep reading and document creation separate. A read connector should retain its
read-only/no-send flags and untrusted-content wrapping. If document creation is
needed, declare a separate write capability with the narrowest suitable grant.
A read-only connector must not gain write access just to support one workflow.

Returned messages and documents are untrusted content and may enter the active
model's context. Wrapping content reduces confusion with instructions but does
not eliminate prompt injection. The connector executes outside the agent sandbox
and can access the mounted account state. See [Security](../SECURITY.md).

Revoke OAuth grants with the account provider. Rerun environment setup to
reconnect, and restart the connector when rotating a credential held by an
existing process. Never put resolved keys, OAuth tokens, or client JSON in Git,
image layers, or a diagnostic transcript.
