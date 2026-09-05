# pix-memory

Standalone memory MCP service. Streamable HTTP `/mcp` plus a non-MCP
`/healthz`, backed by one sqlite store. See
`docs/design/pix-v2-architecture.md` §9 for the product contract and
`docs/design/pix-v2-surface.md` §8.1/§9 for how it fits into Pix v2.

This is its own Go module (`pix-memory`), independent of
`services/host`'s `pix/host` module. It was copied out of
`services/host/{memory.go,memembed.go,memory_snapshot.go,
memory_secretfilter.go,memory_capture_mode.go}` (pix-v2 U2): same schema,
same migration, same recall/dedupe/filter/embed/capture/snapshot/restore
semantics, config from environment variables instead of
`~/.config/pix/config.toml`. `services/host`'s copy is untouched; this is a
copy, not a cutover.

## Layout

- `store/` — schema, migration, remember/recall/forget/stats, the
  Ollama-backed embedder and watcher capture, the secret filter, and
  snapshot/restore. No dependency on `services/host`.
- `server/` — the MCP server: eight tools with accurate annotations, wired
  to a Streamable HTTP handler plus `/healthz`.
- `cmd/pix-memory/` — the binary entrypoint.

## Tools

`memory_recall`, `memory_stats`, `memory_remember`, `memory_forget`,
`memory_observe`, `memory_status`, `memory_snapshot`, `memory_restore`.

## Env

| var | default | purpose |
| --- | --- | --- |
| `MEMORY_BIND` | `0.0.0.0` | listen address |
| `MEMORY_PORT` | `8080` | listen port |
| `MEMORY_DATA_DIR` | `/data` | mounted state dir |
| `MEMORY_DB` | `<data-dir>/memory.db` | sqlite path override |
| `OLLAMA_HOST` | `http://127.0.0.1:11434` | embed/watcher backend |
| `MEMORY_EMBED_MODEL` | `nomic-embed-text` | |
| `MEMORY_WATCHER_MODEL` | `qwen3.5:9b` | |
| `MEMORY_CAPTURE_MODE` | `explicit` | `explicit` or `experimental-auto` |

## Build and test

```console
cd services/memory
go build ./...
go test ./...
```

## Container

```console
docker build -f services/memory/Dockerfile -t pix-memory:dev services/memory
docker run --rm -p 127.0.0.1:8080:8080 -v pix-memory-data:/data pix-memory:dev
```

The default `BUILDER_IMAGE`/`RUNTIME_IMAGE` build args point at DHI tags this
repo cannot itself resolve to a digest without a credentialed registry
session (same posture as the repo-root `Dockerfile`); pass public
substitutes if you have no DHI entitlement. Untested against a real
registry/daemon in this environment — no `docker` available in this
sandbox.
