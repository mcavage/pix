# Pix

Pix runs the [pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
inside a Docker Sandbox. You type `pix` in a directory, you get an agent
session scoped to that directory, you exit, and your host is untouched.

The sandbox is the boundary, so the agent does not stop to ask permission for
each command. It can build, test, review its own work with a second model,
and open a pull request in one run.

macOS only.

## 1. What you need first

Pix installs none of these. Install them yourself, then check each one.

| Thing | Required? | Install | Check |
| --- | --- | --- | --- |
| Homebrew | required | [brew.sh](https://brew.sh) | `brew --version` |
| `sbx`, Docker Sandboxes, plus a signed-in Docker account | required | `brew install docker/tap/sbx` | `sbx diagnose` |
| `op`, the 1Password CLI | required to add a direct provider key | `brew install 1password-cli` | `op --version` |
| `gh`, the GitHub CLI | required to push or open PRs from a sandbox | `brew install gh` | `gh auth status` |
| llmman or Ollama | optional (see section 5) | [ollama.com](https://ollama.com) | `ollama --version` |

`sbx diagnose` prints a checklist. Every line must be a green tick, including
**Authentication**. If it is not authenticated, run `sbx login`.

A sandbox has no GitHub credential of its own. Give one every environment
uses, once:

```bash
pix secret set GITHUB_TOKEN op://vault/item/field
```

Without it the agent can commit inside the sandbox but cannot push or open a
pull request. Like every other credential this is a reference, not a value:
each run resolves it into that run's own sandbox-scoped sbx secret. A
host-global `sbx secret set github` is ignored by Pix (and never removed by
it).

Pix stores provider keys as 1Password references, never as values on disk.
That is the only reason `op` is on the required list. If your environment
already carries credentialed inference, you never add a key and `op` stays
optional.

### Coexisting installations

One `PIX_HOME` = one **stack**, identified by a 16-hex id derived from the
canonical `PIX_HOME` path and carried by every Pix-owned resource:
sandboxes (`pix-<id>-…`), the memory container (`pix-memory-<id>`), and the
two reserved MCP servers (`pix-memory-<id>`, `pix-session-<id>`). Two homes
run side by side with their own containers, their own loopback memory ports,
and their own namespaced entries in the host-global sbx MCP registry.
Cleanup only ever reaches the current stack.

Provider keys are `op://` references in `$PIX_HOME/secrets.env`. `pix setup`
creates that file; every run resolves the refs into **sandbox-scoped** sbx
secrets, so a rotated 1Password item takes effect on the next run.
Host-global sbx secrets are ignored and never removed automatically.

## 2. Install

<!-- PIX_PRIMARY_PATH_START -->
```bash
brew install mcavage/tap/pix
pix setup
```
<!-- PIX_PRIMARY_PATH_END -->

Use the `mcavage/tap/` prefix. Bare `brew install pix` matches no formula,
and Homebrew will suggest `pixi`, which is a different tool.

`pix setup` is repeatable and idempotent, not interactive: it checks Docker
and `sbx`, initializes `PIX_HOME` (default `~/.pix`) as a Git repository,
installs the pinned `pix-agent` image and strict kit, creates and selects a
default environment if none exists, and reconciles the `pix-memory`
container. It never interviews you for a model provider or a local inference
backend; a provider key is added with `pix secret set`, and local inference
(if any) is authored directly in an environment's own `pix.toml` (section 5).
A gap it cannot repair is printed with the exact command that fixes it.

## 3. How to tell it worked

```bash
pix doctor
```

Doctor is read-only. Every check reports what it proved: docker and sbx
availability, environment trust state, model reachability, `op://` reference
resolution, sbx Gateway MCP registration, and the memory container's health.
Every failing row names the owning system and one exact next action. Exit
codes: `0` when nothing required is verifiably broken, a nonzero operational
failure otherwise, `2` on a usage error.

## 4. Do I need a model provider API key?

Usually yes, for a direct key. One key for any one of Anthropic, OpenAI, or
Google is enough; you do not need all three. `pix setup` is the one place a
1Password reference is solicited, and it proves the key with a live request.
If your environment's backends carry their own auth (a credentialed gateway,
for example), doctor reports no provider key needed and means it.

## 5. Is llmman or Ollama required?

No. Pix supports both, reached over Ollama's native transport or an
OpenAI-compatible one (llmman, or any other OpenAI-compatible endpoint). There
is no setup interview for either: you author a backend and its models
directly in the environment's own `pix.toml`:

```toml
[inference.backends.ollama]
driver = "ollama"
base_url = "http://host.docker.internal:11434/v1"
auth = "none"

[[inference.models]]
id = "ollama/qwen3.5:9b"
backend = "ollama"
upstream_id = "qwen3.5:9b"
```

`pix run` merges that declaration over machine config for the session it
launches; `pix setup --env NAME` and `pix doctor` validate what an
environment declares. Neither ever silently prefers or migrates one backend
over another. Without a declared backend:

| Capability | With a local backend | Without one |
| --- | --- | --- |
| Memory recall | vector ranking plus keyword search | keyword search only |
| Automatic fact capture (opt-in) | a watcher model extracts facts | unavailable, no watcher model |
| `/remember` and `/forget` | work | work (an explicit store, not an extraction) |
| A local model in the session | available, loaded on demand | cloud models only |

Pix does not install model weights during an ordinary run.

## 6. Daily use

```bash
cd ~/code/my-project
pix
```

That is the loop. `pix` launches the sandbox for this directory, or
reattaches to an existing one, running or stopped, from an interactive
terminal only; piped or scripted, the same bare form never launches
anything.

| Command | Does |
| --- | --- |
| `pix run [DIR]` | the explicit launch, safe in a script |
| `pix ls` | your `pix-*` sandboxes: environment, project, holder count |
| `pix rm NAME` | remove one sandbox (needs zero live holders, unless `--force`) |
| `pix doctor` | full readiness evidence, with exact fix commands |
| `pix task new NAME` | an isolated clone plus branch plus sandbox, for parallel work |
| `pix reset` | remove every `pix-*` sandbox and the memory container, then rename `PIX_HOME` aside |

A normal sandbox is removed after its last holder exits; a **holder** is one
live node (the interactive session, or a running child agent) that still
depends on it. `pix reset` is reversible: it renames `PIX_HOME` to a
timestamped `.bak-` sibling rather than deleting it, and leaves your provider
keys and Git repos alone.

Removal is never forced by default; it requires proof that no reference
lock still names the sandbox. `pix rm NAME --force` is the one explicit
override, and it never widens the `pix-*` namespace.

## 7. Environments

An environment is a directory under `~/.pix/envs/<name>/`, declaring a
native `.sbxenv.yaml` and an optional `pix.toml` sidecar. There is no
registration database and no `add`/`edit`/`use`/`forget` verb: create,
clone, edit, and remove one with ordinary filesystem and Git tools.

```bash
pix env                 # list environments, the default, and trust state
pix env NAME --effective # the exact sandbox declaration a new launch would use
pix env trust NAME       # read and accept what NAME runs on your host
```

An environment that runs host code or handles a credential must be approved
with `pix env trust NAME` before a launch will use it. Approval is recorded
outside the environment directory and rechecked on every launch: a changed
fact (a kit, a mount, an MCP command or URL, a secret destination) refuses
launch and reprints the same review, defaulting to No.

## 8. MCP servers and integrations

The only MCP path into a sandbox is the sbx Gateway. An environment declares
its servers directly in `.sbxenv.yaml` (native `mcp.servers` grammar); Pix
does not run a second registry and ships no MCP servers of its own.

```bash
sbx mcp auth <name>   # OAuth a Gateway-registered server, native to sbx
```

`pix.toml` may annotate a declared server with the 1Password reference name
it needs and a `pix doctor` probe. See `docs/gworkspace.md` for a worked
example and `docs/reference.md` section 11 for the full model.

## 9. Memory

Memory is a separate Docker container, `pix-memory`, speaking MCP over
Streamable HTTP through the sbx Gateway. Pix has no top-level `memory`
command: everything happens through `/recall`, `/remember`, `/forget`, and
the `memory_*` MCP tools a model can call directly. Capture is explicit by
default; see `docs/memory.md`.

## 10. What actually constrains the agent

`AGENTS.md`, skills, and an environment's `context/` are guidance a model
reads. They are not enforcement. The agent can edit those files, and a model
can decline to follow an instruction. Do not write a rule there and consider
a dangerous action blocked.

The things that hold:

- **The sandbox.** The agent cannot touch your host except through the
  directories you mounted. That is why it needs no permission prompts.
- **The network allowlist.** A domain absent from the kit's
  `permissions.network.allow` is unreachable from inside. Credentials never
  enter the sandbox; the host proxy swaps a sentinel for the real key on the
  way out.
- **A `tool_call` gate, if you write one.** A pi extension that hooks
  `tool_call` and returns `{block: true, reason}` refuses an action before it
  runs. Pix ships no such extension. An extension is a single `.ts` file in
  `~/.pi/agent/extensions`; pi's `docs/extensions.md` has
  `permission-gate.ts` and `protected-paths.ts` examples.

Host-native MCP servers are a second thing to know: they run on the host,
outside the sandbox, with your host-user privileges, and content they return
can be included in the conversation sent to your model provider. See
[SECURITY.md](SECURITY.md).

## 11. Working on Pix itself

Maintenance is `make`, not the CLI:

```bash
make gate         # the fast test gate
make build-agent  # build the pix-agent sandbox image
make load         # build and load it into the sandbox image store
```

## Where to go next

- [docs/getting-started.md](docs/getting-started.md): a first session, end to end.
- [docs/reference.md](docs/reference.md): the full command reference, one section per verb.
- [docs/memory.md](docs/memory.md): how memory captures, ranks, and backs up.
- [AGENTS.md](AGENTS.md): the architecture, if you are extending Pix.

## License

MIT. See [LICENSE](LICENSE).
