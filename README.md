# Pix

Pix is an opinionated, Docker-sandboxed distribution of the
[pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent).
It combines disposable Docker Sandboxes, multi-model routing, cross-provider
review, local memory, and a host launcher into one repeatable development setup.

The sandbox is the safety boundary. Pix can build, test, review, and prepare a
pull request without asking for approval before every shell command.

## Install and run

The following is the supported first-run path on macOS. Run it in order.

<!-- PIX_PRIMARY_PATH_START -->

### 1. Install Docker Desktop

Install and start [Docker Desktop](https://www.docker.com/products/docker-desktop/).
Pix uses Docker Sandboxes rather than running the coding agent directly on your
host.

### 2. Install the nightly Docker Sandboxes CLI

```bash
brew install docker/tap/sbx@nightly
sbx login
```

Pix targets the nightly `sbx` CLI because its custom kit and MCP gateway support
are newer than the stable channel.

### 3. Install Ollama and the default local models

```bash
brew install ollama
brew services start ollama
ollama pull qwen3.5:9b
ollama pull nomic-embed-text
```

`qwen3.5:9b` powers local memory capture and the optional in-sandbox local model.
`nomic-embed-text` creates memory embeddings.

### 4. Install and sign in to 1Password CLI

```bash
brew install 1password-cli
op signin
```

Pix stores `op://` references, not resolved provider keys. `pix setup` requires
1Password CLI and reconciles Anthropic, OpenAI, and Google model credentials into
Docker Sandboxes.

### 5. Configure GitHub credentials for sandboxes

Install and authenticate GitHub CLI, then copy its token into the sandbox secret
store:

```bash
brew install gh
gh auth login
sbx secret set -g github -t "$(gh auth token)"
```

Git operations inside Pix use HTTPS. The sandbox proxy injects this credential;
`gh auth status` inside a sandbox may still say it is not logged in.

### 6. Install Pix

```bash
sbx settings set kit.allowedSources '["docker.io/","github.com/mcavage/"]'
curl -fsSL https://raw.githubusercontent.com/mcavage/pix/main/install.sh | sh
exec "$SHELL" -l
pix version
```

The installer places `pix` and `pix-host` in `~/.local/bin` without `sudo`.

### 7. Run setup

```bash
pix setup
```

Setup inventories the host, verifies 1Password references, reconciles sandbox
credentials, creates the default pack, verifies the result, and starts the
one-time agent onboarding handoff. It is safe to run again. For a host-only or CI
run with no sandbox handoff, use `pix setup --no-agent --yes`.

### 8. Verify readiness

```bash
pix doctor
```

The output is grouped by subsystem. These are literal examples of its rendered
shapes across healthy and unhealthy hosts:

```text
✓ model key    3 provider keys verified
✗ ollama       not installed (the configured memory service needs it for capture + recall)
? google-workspace sbx unavailable here; registration cannot be verified (check from the host)
⊘ google-workspace access denied by organization policy
⚠ pix: 2 items outstanding (optional, nothing blocking) — see the TODOs below.
```

`✓` is verified and ready. `✗` needs setup and includes a copy-paste fix. `?`
means Pix cannot check from the current environment and names where to retry.
`⊘` is a positive policy or permission block. The `⚠` headline means only
optional work remains, so normal use can continue. `pix doctor --json` provides
the same checks and exit verdict for automation.

### 9. Start Pix in a repository

```bash
cd /path/to/repository
pix run
```

`pix run` creates a named sandbox on first use and reattaches to it later. Use
`pix run --replace` when changing create-time kit or MCP settings.

### 10. Learn the workflows

Inside Pix, use `/help` for the capability map. On the host, use:

```bash
pix help
```

<!-- PIX_PRIMARY_PATH_END -->

## Optional: Google Workspace

Google Workspace is not part of default setup. After Pix works normally, install
the implementation dependency and opt in:

```bash
brew install openclaw/tap/gogcli
pix gworkspace setup --account you@example.com
pix gworkspace status
```

Setup guides Google Cloud project/API/OAuth configuration, proves the exact
headless read-only MCP process can authenticate and list tools, and only then
saves the Pix configuration. If the OAuth app remains in Testing, Google may
expire the token after seven days; `pix gworkspace status` reports publication
state and token age. Remove only Pix-owned registration and config with:

```bash
pix gworkspace disable
```

Your Google OAuth credentials are left untouched.

## Daily use

```bash
pix                         # fast status dashboard
pix run [DIR]               # create or reattach a sandbox
pix doctor                  # full readiness evidence
pix setup --no-agent --yes  # reconcile host state without an agent handoff
pix task new <name>         # isolated parallel task sandbox
pix task ls                 # list parallel tasks
pix task harvest <name>     # collect a task's artifacts
pix help --all              # complete command map
```

Host services start lazily when needed. Install them as a login service with
`pix serve install`; remove that service with `pix serve uninstall`. Runtime
config is managed by `pix config`, not by hand-editing TOML files.

## How it works

- **Isolation:** pi runs in a disposable, network-limited Docker Sandbox.
- **Models:** OpenAI orchestrates, Anthropic handles code and high-accuracy work,
  Google provides cross-vendor review and high-volume roles, and Ollama supplies
  local models.
- **Review:** code-producing workflows use a different model vendor for review.
- **Memory:** a host service stores durable facts in SQLite with FTS5 and vector
  search. Recall is appended to conversation context without rewriting the
  provider cache prefix.
- **Packs:** private skills, knowledge, MCP integrations, and wrappers live in a
  git-backed pack rather than this public repository.
- **Parallel work:** each `pix task` uses an isolated clone and sandbox so agents
  do not race in one working tree.

## Build from source

Normal users should use the installer. Maintainers need a DHI-entitled Docker
account to build the image:

```bash
make gate
make build
```

Image changes must be loaded into the Docker Sandboxes image store from the host.
See [AGENTS.md](AGENTS.md) and [docs/reference.md](docs/reference.md) for the
maintainer architecture and complete command reference.

## License

MIT. See [LICENSE](LICENSE).
