# The Pix launcher

This Go module builds `pix`, the only user-facing host binary. It resolves a named
environment, composes a native sbx document, launches or attaches Pi, and manages
Pix-owned session/task lifetime. The separate [memory module](../memory/README.md)
runs in a Docker container; integrations are reached through the sbx MCP Gateway.

Read [AGENTS.md](../../AGENTS.md) for code ownership and safety invariants, and
[the architecture](../../docs/design/pix-v2-architecture.md) before changing a
boundary. There is no host daemon, plugin framework, model router, or private
memory RPC to extend.

## Entry points

- `cmd/pix/root.go`: command dispatch; `pix help --all` is generated from it.
- `workflow/env/`: shared environment preview/launch composition and fingerprints.
- `workflow/launch/`: sandbox creation, attachment, session locks, and teardown.
- `workflow/provision/` and `container/`: runtime preparation and memory lifecycle.
- `envinfo/`, `envsetup/`, `hosttrust/`: sidecar parsing, approved snapshots, trust.
- `inference/` and `secret/`: named model bindings and scoped credential delivery.
- `pixhome/` and `stack/`: authoritative storage paths and stack resource names.

## Build and test

From this directory:

```bash
go build ./...
go test ./...
go vet ./...
```

From the repository root, `bash scripts/gate.sh` adds the cross-language and
architecture checks. Use `make load` for a complete host-UAT bundle, not a binary
copied away from its matching runtime archive and manifest. A unit test that
imports a helper does not replace an integration test of its real CLI caller.

All user-owned state is under `PIX_HOME`, default `~/.pix`. Resolve it through
`pixhome`, and derive resource identities through `stack`. Host-global sbx secrets
are ignored; provider and GitHub references are resolved for each sandbox.
