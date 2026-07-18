# pi-stack-host

The single compiled **Go** binary for everything that runs on the **host** (outside
the sandbox).

**Convention:** host code is Go (one static binary); in-sandbox code (pi
extensions, in-box MCP) is TypeScript. Why Go on the host: a single binary is
saner to ship, and a Node/Python interpreter that listens on a socket and spawns a
child process from network input is backdoor-shaped — endpoint security / EDR
flags exactly that. A compiled Go binary doing the same work runs unflagged.

## Subcommands

```
# non-MCP host HTTP services (run by `make serve`, reached over host.docker.internal):
pi-stack-host memory        memory store, JSON-RPC         (:11435)
pi-stack-host serve         run the enabled services together (SERVICES)

# MCP servers (stdio, run by the sbx gateway via `sbx mcp add` / `make mcp-register`):
pi-stack-host slack         Slack read/search MCP
pi-stack-host gog           Google Workspace read MCP (Gmail/Drive/Docs/Sheets/Calendar)
```

- **memory** — the self-learning store: JSON-RPC over HTTP, pure-Go sqlite + FTS5,
  embeddings + capture watcher via Ollama. Env: `MEMORY_*`, `OLLAMA_HOST`.
- **gog** — Google Workspace stdio MCP server. Like `slack`: NOT an HTTP daemon,
  NOT in `make serve`; the gateway runs it on the host once registered. Creds stay
  on the host in `GOG_HOME` (never in the VM). **Read-only + `--gmail-no-send` by
  default** — typed read tools (`gmail_search`, `gmail_get_message`, `drive_search`,
  `drive_get`, `docs_get`, `sheets_read_range`, `calendar_events`); write tools are
  gated/off. Returned Gmail/Doc content is **wrapped as untrusted** (prompt-injection
  guard). Registered via `make mcp-register`, attached at sandbox creation.
- **slack** — stdio MCP server. NOT an HTTP daemon, NOT in `make serve`; the MCP
  gateway runs it on the host once registered. `sbx mcp add` (local stdio) has no
  `--env`, so creds come from 1Password: the registered command is
  `op run --env-file=config/op-refs.env -- pi-stack-host slack` (see
  `make mcp-register`), and `op` resolves the refs at spawn time — nothing in the
  registration or the VM. Reads `SLACK_TOKEN`/`SLACK_TEAM_ID` at startup; declare
  the refs in `config/op-refs.env`.

**Private overlay.** Company-specific subcommands (e.g. a warehouse exec-proxy, an
HR-directory MCP) live in gitignored `services/host/*.go` files that **self-register**
into the binary via `init()` (`extraCommands` / `extraServiceFactories` in
`main.go`). The binary builds and runs identically with or without them; the public
tree ships none.

The MCP stdio transport is newline-delimited JSON (what the gateway speaks);
`mcpStdio` also tolerates Content-Length framing on input.

## Build / run

```bash
make serve            # builds pi-stack-host + runs `serve` (the `services` list from config.toml)
# or directly:
cd services/host && go build -o pi-stack-host . && ./pi-stack-host serve
```

Deps: `modernc.org/sqlite` (pure-Go sqlite + FTS5, so the binary stays single and
static) and `github.com/google/uuid`. The binary is gitignored.

In-sandbox code (pi extensions, e.g. `extensions/memory-recall.ts`) stays
TypeScript and talks to these over HTTP.

## Security note: host service trust boundary

The host HTTP services bind to `127.0.0.1` and are **unauthenticated by default** —
any process on the host (including any sandbox reaching `host.docker.internal`) can
drive them (e.g. read/write the memory store). This is the
deliberate single-user assumption: your machine, your disposable VMs, your data.
It's bounded by loopback binding. To require a shared secret on a service, set its
`*_AUTH` env var (the sandbox wrapper sends the matching value). Do not bind these
to a routable interface or run them on a shared host without an auth proxy.
