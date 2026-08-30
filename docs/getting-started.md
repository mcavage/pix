# Getting started

A first session, end to end. If you already ran `pix setup` and just want the
command reference, go to `docs/reference.md` instead: this doc is the guided
walk-through, not the catalogue.

## 1. Install and set up

```bash
brew install mcavage/tap/pix
pix setup
```

`setup` initializes `PIX_HOME` (default `~/.pix`) as a Git repository,
installs the pinned `pix-agent` image and strict kit, creates a default
environment when none exists, wires a model backend, and reconciles the
`pix-memory` container. It does NOT install `sbx`; a missing host tool is
reported with the exact install command, and setup is resumable, so you
install it and run setup again.

It also asks how models should run. A direct API key via 1Password is the
default; an already-credentialed backend needs none. Your first sandbox
launches once a callable model is confirmed.

## 2. Your first sandbox

```bash
cd ~/code/some-repo
pix run
```

- No sandbox for this directory yet -> **create** it (a Docker Sandbox
  running the pinned pi coding agent, pointed at this checkout).
- A sandbox already exists -> **reattach**, running or stopped, as-is.
- A bare `pix DIR` (no `run`) does the same thing **only from an interactive
  terminal**. From a script or pipe (no TTY) the same bare form refuses
  instead of silently creating or attaching a sandbox; a script needs the
  explicit verb: `pix run DIR`.

A sandbox is **ephemeral**: nothing you do inside it touches your host
except through explicit mounts, git, and the sandbox kit's network
allowlist. A broken sandbox is thrown away and recreated (`pix rm NAME`,
then `pix run`), never repaired in place.

## 3. Check what's there

```bash
pix ls        # every pix-* sandbox: name, environment, project, holders
pix doctor    # full readiness evidence + exact fix commands
```

## 4. Clean up

```bash
pix rm pix-some-repo          # remove one, non-force (needs zero live holders)
pix rm --all --keep pix-pix   # remove every pix-* sandbox but one
pix rm --orphans              # remove only pix-owned sandboxes nothing still holds
```

Removal is **never forced** by default: it needs proof that no reference
lock still names the sandbox. The one forced seam is an explicitly named
`pix rm NAME --force`; `--all`/`--orphans` can never be forced. In the
common case you don't even need `rm`: the sandbox is removed automatically
once its last holder (the interactive session, or a running child agent)
exits.

## 5. Parallel work on one repo

```bash
pix task new feature-x     # isolated checkout + branch + sandbox
pix task ls                 # every task: branch, dirty/clean, sandbox state
cd "$(pix task path feature-x)"
pix task rm feature-x       # requires zero live holders; refuses a dirty/unpushed checkout without --force
```

Each task is its own `git clone --local` checkout with its own branch, so
two agents never race in one working tree. See
`docs/design/worktree-tasks.md`.

## 6. Environments, memory, and models

```bash
/recall <query>       # what memory would surface right now
/remember <text>      # pin a fact immediately
pix env                # environments under ~/.pix/envs, the default, trust state
```

An environment is a plain directory (`.sbxenv.yaml` plus an optional
`pix.toml`); there is no registration command. Memory is a separate Docker
container (`pix-memory`) reached through the sbx Gateway, operated only
through `/recall`, `/remember`, `/forget`, and the `memory_*` MCP tools. A
model is picked by name (`pix run --model provider/id`, or an environment's
`pix.toml` `[models].main`): nothing resolves one for you.

## 7. MCP servers and integrations

Pix ships no MCP servers of its own. An environment declares each one
directly in its `.sbxenv.yaml` (native sbx grammar); the sbx Gateway owns
registration, attachment, and OAuth.

```bash
pix env NAME --effective   # confirm what MCP servers this environment declares
sbx mcp auth <name>         # OAuth a declared server
pix doctor                  # registered vs actually working
```

See `docs/gworkspace.md` for the Google Workspace case worked through, and
the `gworkspace` skill for its tools and the untrusted-content rule.

## Common questions

**Is llmman or Ollama required?** No. Without one, memory still works but
degraded: recall falls back to keyword search (no vector ranking).
`/remember` is unaffected either way; it is an explicit store, not an
extraction. Automatic capture is a separate, opt-in setting in an
environment's `pix.toml` that pix does not turn on for you.

**Do I need a provider API key?** Only if your environment's inference is
not already credentialed. `pix setup` is the one place a 1Password reference
is solicited for a direct key.

## Where to go next

- `docs/reference.md`: the full capability reference, one section per verb.
- `docs/memory.md`: the memory service, in detail.
- `AGENTS.md`: the harness's own memory; read it before extending pix itself.
