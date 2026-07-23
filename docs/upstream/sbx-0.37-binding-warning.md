# `sbx` prints "not injecting" for a credential it actually injects

**Version:** `sbx v0.37.0-rc1-218-g9da05c500` (macOS arm64)

## Summary

When a sandbox starts, `sbx` warns that it is **not** injecting a stored
credential — but it injects it anyway and the credential works. The warning is
wrong, and because it's printed once per stored key at every `sbx run`, it looks
like the whole run failed. New users reasonably hit Ctrl+C on a run that was
about to work fine.

## Steps to reproduce

1. Store a provider key:

   ```
   sbx secret set -g anthropic          # paste any valid Anthropic key
   ```

2. Use any kit that declares that credential (`kind: sandbox`, a
   `credentials[].apiKey` for `anthropic` injecting on `api.anthropic.com`, with
   `api.anthropic.com` in `caps.network.allow`).

3. Create the sandbox:

   ```
   sbx create <agent> . --kit ./kit --name repro
   ```

   `sbx` prints:

   ```
   credential for "anthropic" discovered but no domains allowed by your bindings; not injecting (edit ~/.config/sbx/credentials.yaml)
   ```

4. Make an authenticated call from inside the sandbox:

   ```
   sbx exec repro -- curl -s -o /dev/null -w '%{http_code}' \
     https://api.anthropic.com/v1/models -H 'anthropic-version: 2023-06-01'
   ```

## Expected

Either the credential is injected and there's no "not injecting" warning, or the
warning is correct and the call fails.

## Actual

The call returns **`200`** — the proxy *did* inject the key (the in-VM env var is
the `proxy-managed` sentinel and the proxy swapped in the real key). The warning
directly contradicts what `sbx` actually did. It fires once per stored provider
key on every `create`/`run`.

## Also worth fixing (same area, lower priority)

These came up while chasing the warning above; happy to split into separate
issues if you'd prefer.

- **The fix the message suggests doesn't work.** The message says to edit
  `~/.config/sbx/credentials.yaml`. Adding the documented binding for the
  service —

  ```yaml
  bindings:
    anthropic:
      discovery: []
      allowedDomains: [api.anthropic.com]
  ```

  — does not silence the warning (with `allowedDomains` matching the kit's inject
  domain, with `discovery` also populated, and after a `sbx daemon stop` + fresh
  start). So the one lever the message points at has no effect on rc1.

- **`op run` env-sourced credentials are never injected.** The 1Password guide
  documents an ephemeral pattern that keeps the key out of the sbx store:

  ```
  ANTHROPIC_API_KEY="op://vault/item/field" op run -- sbx run <agent>
  ```

  On rc1 this returns **`401`** — with no `sbx secret` stored and the value only
  in sbx's process env, the proxy has nothing to swap (tested under both
  `sbx create` and `sbx run`). The value must be persisted via `sbx secret set`.
  If Pattern 2 is meant to work, it's broken; if not, the guide is wrong.

## Notes

Reproduced on a custom `kind: sandbox` kit (kit-spec v2). Injection itself works
great — this report is only about the misleading warning and the two
docs/behavior mismatches above.
