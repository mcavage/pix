# Getting started

A first session, end to end. If you already ran `pix setup` and just want the
command reference, go to `docs/reference.md` instead: this doc is the guided
walk-through, not the catalogue.

## 1. Install and set up

```bash
brew install mcavage/tap/pix
pix setup
```

`setup` installs `sbx` if it is missing, asks how models should run (a direct
API key via 1Password is the default; an existing Ollama or a custom gateway
work without it), and launches your first sandbox once one callable model is
confirmed. It is resumable: if a host tool is missing, it prints the exact
install command and picks back up after you run it.

## 2. Your first sandbox

```bash
cd ~/code/some-repo
pix run
```

- No sandbox named for this directory yet → **create** it (Docker Sandbox +
  the pi coding agent, pointed at this checkout).
- A sandbox already exists → **reattach**, running or stopped, as-is.
- A bare `pix DIR` (no `run`) does the same thing **only from an interactive
  terminal**: it is shorthand for `run DIR`. From a script or pipe (no TTY)
  the same bare form refuses instead of silently creating or attaching a
  sandbox; a script needs the explicit verb: `pix run DIR`.

A sandbox is **ephemeral**: nothing you do inside it touches your host except
through explicit mounts, git, and the sandbox kit's network allowlist. There is
no `pix reset`: a broken sandbox is thrown away and recreated, not repaired in
place. See `docs/design/lifecycle-trust.md` for the full lifecycle.

## 3. Check what's there

```bash
pix                # at a terminal: `pix run` here. Piped/non-interactive: status.
pix ls              # every pix-* sandbox: name, state, dir
pix doctor          # full readiness evidence + exact fix commands
```

## 4. Clean up

```bash
pix rm pix-some-repo          # remove one, non-force (needs zero live references)
pix rm --all --keep pix-pix   # remove every pix-* sandbox but one
pix rm --orphans              # remove only pix-owned sandboxes nothing still holds
```

Removal is **never forced** by default: it needs a kernel-verified proof that
no shell still references the sandbox. The one forced seam is an explicitly
named `pix rm NAME --force`; `--all`/`--orphans` can never be forced. In the
common case you don't even need `rm`: the **last shell to exit a sandbox tears
it down by itself** (a non-force `sbx rm`, gated on that same zero-reference
proof), so tidy-up is usually nothing you do at all.

## 5. Parallel work on one repo

```bash
pix task new feature-x     # isolated checkout + branch + sandbox
pix task ls                 # every task: branch, dirty/clean, sandbox state
cd "$(pix task feature-x path)"
pix task rm feature-x       # persists the branch back, refuses a dirty/unpushed one without --force
```

Each task is its own clone (or `--worktree` for a linked worktree) with its own
branch, so two agents never race in one working tree. See
`docs/design/worktree-tasks.md`.

## 6. Memory, packs, and models

```bash
/recall <query>       # what memory would surface right now
/remember <text>      # pin a fact immediately
pix pack ls            # the active pack, if any
pix models ls           # which models are wired
pix agent ls            # the subagent roster: resolved model + why
```

Memory is a host service (`pix serve`, started lazily); packs are git-backed
capability bundles you activate with `pix setup --pack <url>` or `pix pack
use`; models resolve by **intent**, not a pinned name: see
`docs/design/routing.md`.

## 7. Slack and Google Workspace

Both are **external** integrations, not built into `pix-host`. Register
either the same way as any other MCP server:

```bash
pix mcp add
pix mcp ls
```

See `docs/gworkspace.md` and the `gworkspace` skill.

## Where to go next

- `docs/reference.md`: the full capability reference, one section per verb.
- `docs/design/lifecycle-trust.md`: how a sandbox's lifecycle and a pack's
  trust gate actually work, in one place.
- `AGENTS.md`: the harness's own memory; read it before extending pix itself.
