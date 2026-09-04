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
installs the pinned `pix-agent` image and strict kit, creates and selects a
default environment when none exists, and reconciles this `PIX_HOME`'s own
memory container. It does NOT install `sbx`; a missing host tool is reported
with the exact install command, and setup is resumable, so you install it
and run setup again.

Every `PIX_HOME` is its own **stack**: a 16-hex id derived from that home's
canonical path names its sandboxes, its memory container, and its two
reserved MCP servers, so a second `PIX_HOME` on the same host runs alongside
the first without either one touching the other's resources. See
`docs/reference.md` §2 and §12 for what that means for memory and cleanup.

Setup never chooses how models should run. On an interactive first run it can
offer to record provider `op://` references; the explicit path is `pix secret
set` (references only, never values on disk). An already-credentialed backend
needs none. Local inference
(llmman or Ollama, over native or OpenAI-compatible transport) is authored
directly in an environment's own `pix.toml`, not chosen in setup. Your first
sandbox launches once a callable model is confirmed.

### Upgrades

`brew upgrade mcavage/tap/pix` replaces the binary and the release bundle
beside it. The next ordinary `pix` notices that this `PIX_HOME` is still on
the previous release and reconciles the stack artifacts Pix owns, printing
one line about it. `pix setup` remains the first-run and repair command,
not an upgrade chore.

A launch approves one thing on your behalf and says so here: `sbx env
create` renders its own plan and asks its own "Approve this plan?" for a
document Pix has already composed, fingerprinted, and put through its own
trust review. Pix answers that duplicate prompt internally, after its own
gate, and never displays the raw plan, because that text contains the
`pix-memory` URL with its bearer token in the query string. If a create
fails, you get the reason with every credential redacted.

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
pix rm pix-<stack-id>-some-repo-<digest>  # remove one (needs zero live holders)
pix rm --all --keep pix-<stack-id>-project-<digest>  # keep one sandbox in this stack
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
container, reached through the sbx Gateway, operated only through `/recall`,
`/remember`, `/forget`, and the `memory_*` MCP tools. A model is picked by
name (`pix run --model provider/id`, or an environment's `pix.toml`
`[models].main`): nothing resolves one for you.

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

If a tool has to be installed or logged into on your **host** before an
environment works, declare it as that environment's own setup hook and run
it explicitly:

```toml
# ~/.pix/envs/work/pix.toml
[[setup]]
id = "gh"
command = "./setup-gh"
check_args = ["check"]     # exit 0 = already ready, nothing runs
apply_args = ["login"]
required = true
kind = "auth"              # needs a terminal; install hooks do not
```

```bash
pix env trust work        # read and accept the exact argv + executable hash
pix setup --env work      # the ONLY thing that runs it
```

This is where a v1 pack's install/auth hook goes. It runs on the host, only
through that explicit command, only after trust, and never as a side effect
of `pix run`. See `docs/reference.md` §6 for the full grammar and rules.

## Common questions

**Is llmman or Ollama required?** No. Without one, memory still works but
degraded: recall falls back to keyword search (no vector ranking).
`/remember` is unaffected either way; it is an explicit store, not an
extraction. Automatic capture is a separate, opt-in setting in an
environment's `pix.toml` that pix does not turn on for you. Either backend,
when you do want one, is authored directly in that same `pix.toml`; there
is no setup interview and no machine-wide preference.

**Do I need a provider API key?** Only if your environment's inference is
not already credentialed. `pix secret set` is the one place a 1Password
reference is added for a direct key.

## Where to go next

- `docs/reference.md`: the full capability reference, one section per verb.
- `docs/memory.md`: the memory service, in detail.
- `AGENTS.md`: the harness's own memory; read it before extending pix itself.
