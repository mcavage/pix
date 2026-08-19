# Getting started

A first session, end to end. If you already ran `pix setup` and just want the
command reference, go to `docs/reference.md` instead: this doc is the guided
walk-through, not the catalogue.

## 1. Install and set up

```bash
brew install mcavage/tap/pix
pix setup
```

`setup` installs three things: the launchd agent for host services, any pack you
asked for, and local Ollama weights (only with `--pull-models`). It does NOT
install `sbx`. A missing host tool is reported with the exact install command to
run, and setup is resumable, so you install it and run setup again.

It also asks how models should run. A direct API key via 1Password is the
default; an existing Ollama or a credentialed gateway work without one. Your
first sandbox launches once a callable model is confirmed.

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
through explicit mounts, git, and the sandbox kit's network allowlist. A broken
sandbox is thrown away and recreated (`pix rm <box>`, then `pix run`), never
repaired in place. `pix reset` is the bigger hammer for the HOST side of the
stack (config, memory, packs, runtime state and every sandbox at once), and it
moves things aside rather than deleting them, so it is reversible. See
`docs/design/lifecycle-trust.md` for the full lifecycle.

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

## 7. Slack, Google Workspace, and every other integration

Pix ships **no** MCP servers. Each one is declared by the active pack, so the
order is: adopt a pack that declares it, then register what it declared.

```bash
pix pack use <path|git-url>   # Tier-1 review of what runs on your host
pix mcp add
pix mcp ls
pix doctor                    # registered vs actually working
```

See `docs/gworkspace.md` for the Workspace case worked through, and the
`gworkspace` skill for its tools and the untrusted-content rule.

## Common questions

**Is Ollama required?** No. Without it, memory still works but *degraded*:
recall falls back to FTS5 keyword search (no vector ranking). `/remember` is
unaffected either way: it is an explicit store, not an extraction. Automatic
capture is a **separate, opt-in** setting (`pix config set memory_capture
experimental-auto`) that pix does not turn on for you: capture stays
`explicit` (off) by default whether or not Ollama is installed. Ollama merely
determines whether `experimental-auto`, if you opt into it, has a watcher
model available to extract facts from; without Ollama there is no watcher
model, so `experimental-auto` has nothing to run. `pix doctor` reports the
`models` row as optional. Install Ollama and pull the two models to get
semantic recall and a usable `experimental-auto`; see `docs/memory.md`.

**Can I still launch the image with sbx directly?** Yes, through the kit:

```bash
sbx run pix --kit "git+https://github.com/mcavage/pix.git#dir=pi-kit"
```

That is the supported consumer path and what `make run-published` runs. Note it
is `sbx run pix`, the agent name from the kit (which pins the image version),
not an image reference.

What you give up by skipping `pix run` is everything the launcher does on the
HOST: MCP servers registered from your config, the trusted host-state handed to
the agent, memory autostarted, and the active pack's `bin/` wrappers and skills.
A raw kit run gets you the sandbox and pi; it does not get you your integrations.

**Do I need a provider API key?** Only if your inference is not already
credentialed. On a host whose configured backends carry their own auth (a pack
with an authenticated gateway, for example), `pix doctor` says `no provider key
needed` and means it. Keys are added with `pix models add <provider>`, which is
the one place a 1Password reference is solicited.

## Where to go next

- `docs/reference.md`: the full capability reference, one section per verb.
- `docs/design/lifecycle-trust.md`: how a sandbox's lifecycle and a pack's
  trust gate actually work, in one place.
- `AGENTS.md`: the harness's own memory; read it before extending pix itself.
