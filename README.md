# Pix

Pix is an opinionated, Docker-sandboxed distribution of the
[pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent).
It combines disposable Docker Sandboxes, multi-model routing, cross-provider
review, local memory, and a host launcher into one repeatable development setup.

The sandbox is the safety boundary. Pix can build, test, review, and prepare a
pull request without asking for approval before every shell command.

## Install and run

<!-- PIX_PRIMARY_PATH_START -->

```bash
brew install mcavage/tap/pix
pix setup
pix run
```

`pix setup` installs the two core host tools (`sbx` and `op`) with one Homebrew
consent when needed, configures one model provider from 1Password,
and launches the first sandbox. One provider is enough. Additional providers,
Ollama, GitHub CLI, and Google Workspace are optional capabilities that can be
added later. When a required host tool is absent, setup prints the exact install
command and resumes safely after it is run. Do not use the bare `brew install pix`: Homebrew does not
know a formula by that name and may suggest `pixi` instead. The `mcavage/tap/`
qualifier is required.

Activate an advanced work pack explicitly through the same setup flow:

```bash
pix setup --pack 'git+https://github.com/your-org/work-pack.git#ref=main'
```

Packs can declare required setup probes and idempotent actions. Pix reviews and
fingerprints host-executing hooks, runs only required hooks, verifies them after
execution, and resumes past completed work on the next setup run. A pack can also
declare optional setup hooks. Run one explicitly with `--with`, for example:

```bash
pix setup --pack 'git+https://github.com/your-org/work-pack.git#ref=main' --with hr
```

Without `--with`, the pack is still activated and all required hooks run. Only
the named optional hook is skipped. Repeat `--with` to select more than one.

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

## Optional: Slack

Slack integration is optional and absent by default. Configure your Slack App's Client ID with:

```bash
pix config set slack.client_id <client_id>
```

Then run guided setup to authorize access via local PKCE OAuth (public client, no client secret required):

```bash
pix slack setup
```

This opens the OAuth grant in your browser, verifies your identity, and stores the rotating credential in a 1Password document (`Private` vault by default). Twelve-hour access tokens refresh automatically on use; you do not authorize every 12 hours. Renew the roughly monthly interactive grant with `pix slack auth`. Verify health with `pix slack status` and revoke access with `pix slack disable`. A static fallback (`pix slack setup --token-ref op://...`) is also available for pre-issued tokens.

## Daily use

```bash
pix                         # fast status dashboard
pix run [DIR]               # create or reattach a sandbox
pix doctor                  # full readiness evidence
pix task new <name>         # isolated parallel task sandbox
pix task ls                 # list parallel tasks
pix task harvest <name>     # collect a task's artifacts
pix help --all              # complete command map
```

Host services start lazily when needed. Install them as a login service with
`pix serve install`; remove that service with `pix serve uninstall`. Runtime
config is managed by `pix config`, not by hand-editing TOML files.

`pix run` and `pix task new` add Pix workspace state to the repository's local
`.git/info/exclude`, so generated `.pix` files stay out of `git status` in every
repository Pix touches. The optional `.pix/knowledge` pointer remains trackable.

## How it works

- **Isolation:** pi runs in a disposable, network-limited Docker Sandbox.
- **Models:** Pix runs with any one configured cloud provider. Additional
  providers improve role specialization and cross-vendor review. Ollama adds
  optional local models.
- **Review:** code-producing workflows use a different model vendor for review.
- **Memory:** a host service stores durable facts in SQLite with FTS5 and vector
  search. Recall is appended to conversation context without rewriting the
  provider cache prefix.
- **Packs:** private skills, knowledge, MCP integrations, and wrappers live in a
  git-backed pack rather than this public repository.
- **Parallel work:** each `pix task` uses an isolated clone and sandbox so agents
  do not race in one working tree.

## Build from source

Normal users should install with Homebrew. Maintainers need a DHI-entitled Docker
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
