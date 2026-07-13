# Handoff: repo-less + go-plugin + OKF upgrade (macOS host steps)

Everything buildable/verifiable inside the sandbox is done, on branch
`feat/repo-less-plugin-upgrade` (opened as a PR). Full acceptance gate is green:
`go build/vet/test -race`, `gofmt`, `tsc`, `check-open-core.sh`, shell syntax,
allowlist consistency. Two adversarial cross-vendor reviews + fix loops, and
three UATs (fresh-user flow included) all passed. What remains needs **your
macOS host** (Docker + `sbx` + your accounts), which the sandbox can't touch.

## What shipped (summary)
- **Skill taxonomy**: 35 → 31 verb-named skills (merges: `plan`←autoplan+wf-product,
  `build`←spec+wf-engineering, `healthcheck`←health+self-audit; renames; dropped
  prototype→a `build` mode; added `enrich`). Both allowlists (`.gitignore` +
  `.dockerignore`) synced.
- **go-plugin host**: `services/host/plugin/` (MemoryStore / CredentialBroker /
  McpServer / KnowledgeStore over net/rpc, handshake, SHA-pinned override).
  `serve` is now a supervisor; built-ins run in-process, overrides launch as
  signed subprocesses at startup only.
- **Launcher** `pi-stack` (`services/host/cmd/pi-stack`): verb tree
  (run/serve/doctor/setup/config/version + stubs), version-coupled `sbx run`
  (`#ref=v<version>`), XDG `~/.config/pi-stack/config.toml`.
- **Google Workspace via `gog`**: the read-only `gog` host MCP server the sbx
  gateway spawns (the slack pattern). Creds resolve from `config/op-refs.env` at
  spawn; Workspace runs through the gateway, not as a standalone host service.
  Setup: `docs/gog-setup.md`.
- **Host services**: memory (:11435) [+ knowledge (:11436) when configured].
- **OKF**: reader + built-in `knowledge` service (:11436, sqlite+FTS5+embeddings),
  `knowledge` capability (default `none`), consume extension
  (`extensions/knowledge-recall.ts`, cited + budgeted), gated `enrich` skill/agent.
- **/help + /getting-started** extension (live capability map).
- **Installer**: `install.sh` (checksum-verified, no-sudo, atomic, `--uninstall`)
  + a `release-binaries` CI job + a per-release `git tag v<version>` step.

## Host steps (do these on your Mac)

### 0. Review + merge the PR (your call)
It stops at PR — never auto-merged. On merge, CI stamps `0.0.<run>`, builds the
image, pushes it, commits the bump, **tags `v0.0.<run>`**, and uploads darwin/linux
binaries + `SHA256SUMS` to the release.

### 1. Bake the image for sandboxes (once, needs DHI login)
The new baked skills/extensions/host aren't in a sandbox until the image rebuilds:
```bash
docker login dhi.io        # DHI-entitled account
make load                  # build + load into sbx (or just wait for CI publish on merge)
```

### 2. HOST-VERIFY #1 — confirm sbx honors the kit git-ref pin
The version-coupled launcher builds `--kit "git+…#ref=v<version>&dir=pi-kit"`.
Confirm your `sbx` build accepts `#ref` (docs say yes; your local sbx-0.34 has
quirks):
```bash
sbx run pi-stack --name reftest --kit "git+https://github.com/mcavage/pi-stack.git#ref=main&dir=pi-kit" .
# boots → tags work. rejects #ref → see the fallback note in cmd/pi-stack/sbxargs.go
sbx rm -f reftest
```

### 3. HOST-VERIFY #2 — register + smoke-test the `gog` Workspace MCP server
Google Workspace runs through the sbx gateway as the read-only `gog` MCP server
it spawns — not as a host service.
Register it and confirm the gateway actually gets tools (a headless keyring/
op-refs mismatch is the classic 0-tools trap; `pi-stack doctor` now probes the
exact gateway spawn and prints which account + op-refs it verifies). Full setup:
`docs/gog-setup.md`.
```bash
make mcp-register        # registers gog (needs GOG_ACCOUNT + config/op-refs.env)
sbx mcp ls               # gog listed
pi-stack doctor          # gog check: account + op-refs it's verifying, headless spawn OK
```

### 4. Install the launcher + host binary
After a release exists:
```bash
curl -fsSL https://raw.githubusercontent.com/mcavage/pi-stack/main/install.sh | sh
```
Or from a checkout right now: `make install` (symlinks `bin/pi-stack`), and build
the Go launcher with `make launcher` (→ `out/pi-stack`).

### 5. Run the host services as a launchd agent
```bash
cp scripts/macos/com.pi-stack.serve.plist ~/Library/LaunchAgents/
sed -i '' "s#/Users/CHANGEME#$HOME#g" ~/Library/LaunchAgents/com.pi-stack.serve.plist
launchctl load -w ~/Library/LaunchAgents/com.pi-stack.serve.plist
# logs: ~/Library/Logs/pi-stack-serve.{out,err}.log
```

### 6. Wire your accounts
```bash
pi-stack setup      # seeds ~/.config/pi-stack/config.toml, prompts for missing secrets/ollama/gog/mcp
pi-stack doctor     # confirm: keys, ollama+models, memory :11435, gog MCP (see docs/gog-setup.md), mcp
```

### 7. (Optional) Private overlay for work
Your snow/warehouse broker + private memory/OKF live in your overlay peer repo, NOT
here. Ship them as `[plugins.broker]` / `[plugins.knowledge]` overrides in
`config.toml` (SHA-pinned; `docs/OVERLAY.md` §4 has the pattern). An OKF knowledge
bundle is a git repo you mount and point `knowledge_bundles` at.

## The two things I could not verify (both flagged above)
1. sbx `#ref` kit pin on your exact sbx build (step 2).
2. The `gog` Workspace MCP server registered + returning tools on YOUR gateway
   (step 3) — the headless keyring/op-refs path is host-specific; `pi-stack doctor`
   probes it and `docs/gog-setup.md` has the fix for the 0-tools trap.
