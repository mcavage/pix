# pi-stack reference manual

This is the capability reference. Onboarding teaches you the first task and
gets out of the way; it doesn't walk you through everything pi-stack can do.
This doc does. Read the section you need, run the command, move on.

## 1. What pi-stack is

pi-stack is a multi-model coding agent (Claude, GPT, Gemini, plus local Ollama)
that runs inside a throwaway Docker sandbox. Nothing you do in a session
touches your host machine except through explicit mounts, git, and the sandbox
kit's network allowlist. When you're done, `sbx rm -f` and the sandbox is gone.
State that should persist (memory, packs, config) lives outside the sandbox on
the host, so it survives even though the sandbox doesn't.

```
pi-stack run [DIR]      # launch (default: current dir)
pi-stack                # status only, never launches
```

## 2. Memory

pi-stack learns from what you do, without you telling it to. A background
watcher looks at each exchange and decides what's worth keeping: durable facts,
preferences, corrections. You don't call a save function. You correct it by
acting differently next time, the same way you'd correct a person.

```
/recall <query>       # what memory would surface for this query
/remember <text>      # pin a fact immediately, no waiting on the watcher
/forget <id|query>     # drop a memory; id from /recall, or a query to drop its top match
/learnings [minFreq]  # recurring captured facts worth promoting into a skill (see the `promote` skill)
```

The agent has read-only access via **typed tools** (`memory_recall`,
`memory_stats`) that reach the host daemon directly, it never shells out to
`pi-stack` or `curl`. It reaches for `memory_recall`/`memory_stats` when you
ask what's remembered or how much is stored (`memory_recall` can return up to
100 rows, not an unbounded dump, see the truncation note below). Writing and
deleting are **human-driven slash commands** (`/remember`, `/forget`), not
agent tools, but that's a UX/safety choice on this tool surface, **not a
security boundary**: the memory daemon is unauthenticated and reachable (see
"Trust model" in [memory.md](memory.md)), so any sandbox code capable of an
HTTP POST could still write to it directly, independent of the agent's typed
tools.

**Privacy.** Extraction and embedding run on local Ollama and never leave your
machine, but recalled memory is not private from your model provider: once a
row is recalled, its content goes into the prompt sent to whichever model is
active (Claude, OpenAI, Gemini, or local Ollama). Never store secrets, tokens,
or credentials in memory.

Example: you tell pi three times across different sessions that your staging
DB is `postgres://staging.internal:5432`. The watcher notices the repetition
and remembers it without being asked. Next session, `/recall staging db` finds
it, or it surfaces unprompted in the system prompt when it's relevant.

**Durability.** A durable fact (preference, decision, convention) has no
automatic expiry. A watcher-captured **event** (time-bound status: what you're
doing right now) is perishable and expires after 7 days on its own. `/remember`
is the reliable explicit write, driven by the human, not the agent, though a
stable fact the watcher captures on its own can also land durable.

**What gets silently injected vs. what you can see.** Each turn, only a small
relevance-filtered subset is silently added to context, low-scoring
perishable hits are left out of that silent injection specifically, so a
noisy time-bound status doesn't compete for space in every turn. Durable hits
are never filtered by score. An explicit `/recall` or `memory_recall` skips
that score filter, but is still capped: a blank query (or `*`) returns up to
100 rows (with a truncation line if the store has more), not a true unbounded
dump.

**Limits.** Memory runs as a host service (`pi-stack serve`, port 11435) and is
per-machine, not shared across your laptop and your desktop, and not shared
with teammates. It's SQLite plus FTS5 and embeddings on disk at
`~/.local/state/pi-stack` (or wherever your config points it). If the service
is down, the commands and tools above surface a clear error rather than
failing silently; only the silent per-turn auto-injection degrades quietly (no
memory gets added that turn, so a dead daemon never blocks the conversation).
Nothing here is a pack: memory is what pi-stack learned by watching, not what
you deliberately taught it (that's a pack, see §5); a pack can still scope
memory to itself via `memory_scope` (see [memory.md](memory.md)).

## 3. Skills (the flows)

Skills are named, tested workflows for the moments that recur. Each is a
`SKILL.md` with tight steps, invoked automatically when the conversation
matches its intent, or explicitly:

```
/skill:plan
/skill:build
```

`/help` lists every skill baked into your image with a one-line description.
The spine you'll use most:

| skill | what it does |
| --- | --- |
| `plan` | idea to eng-ready plan: discovery, PR/FAQ, PRD, design, architecture, with a review gate |
| `build` | ship a feature with the crew: story files, parallel worktrees, code review, QA, verification |
| `ship` | working tree to open PR: rebase, tests, lint, review, version bump, push |
| `debug` | root-cause-first: reproduce, form a falsifiable hypothesis, verify it, then fix |
| `code-review` | review the current diff, then get a cross-vendor second opinion |
| `tdd` | failing test first, watch it fail, minimal code to pass, refactor |
| `verify` | prove a claim by running the command before you say "done" |
| `qa` | drive a running app in the browser, report bugs with screenshots |
| `healthcheck` | is the harness working (keys, memory, MCP, skills) and is the code healthy (tests, lint, dead code) |
| `enrich` | write a durable fact into a shared knowledge bundle, gated by PR |
| `promote` | review what the memory watcher keeps repeating and graduate it into a skill or convention |

Example: say "let's fix this bug" and `debug` loads on its own; you don't
type `/skill:debug`. Say "review my diff before I push" and `code-review`
loads and hands off to a cross-vendor subagent automatically.

**Limits.** Skills are mechanism, not your personal config. A skill never
hardcodes your channel names or account IDs; that data lives in memory or a
pack (§5) and the skill reads it at runtime. A skill baked into the image is
read-only in a running sandbox; edit and version your own by adding it to a
pack instead (`pack add skill`).

## 4. The crew

pi-stack isn't one model. Four providers are in rotation (Claude, GPT, Gemini,
local Ollama), and specialist subagents (architect, engineer, qa-lead,
security-lead, and more) get dispatched for the parts of a task that need a
different lens. The point of cross-vendor review isn't ceremony: a different
vendor than the one that wrote the code checks it, so its blind spots don't
overlap with the author's.

```
/model              # open the model picker
Alt+P               # cycle models without leaving the keyboard
```

An agent (a subagent preset) declares an **intent**, not a pinned model:
`code`, `review`, `red-team`, `breadth`, `max-accuracy`, and more. The router
resolves intent to a model against cost, latency, and accuracy, so `review`
resolves to a different vendor than whichever one wrote the code, on purpose.

```
pi-stack agent ls                 # roster with each agent's resolved model and WHY
pi-stack route pick <intent>      # what the router would resolve for that intent
```

Example: `code-review` finishes its own pass, then dispatches the `review`
subagent, which the router resolves to GPT if your code was written by Claude.
You get an adversarial second opinion, not an echo.

**Limits.** Model routing is hand-maintained, not measured: scores in
`scorecard.json` come from published benchmarks, not a live eval harness. If
a model shipped last week, run `model-refresh` before trusting `agent ls`.
Subagents run headless (`pi --no-extensions`), so a child that gets stuck has
no UI to show you; a watchdog kills it after an idle or wall-clock timeout and
reports the failure instead of hanging forever.

## 5. Packs

A pack is a git repo that is your portable capability context: the skills you
taught pi-stack, an OKF knowledge bundle, the MCP servers and CLI wrappers
you're wired to, and config (which Google account, which model prefs). It's
the thing you'd `git diff`, as opposed to memory, which you'd only see in a
`/recall`. The default pack is active automatically; switching to another
context is switching the active pack.

```
pi-stack pack use default        # return to the default pack
pi-stack pack use <path|git-url> # switch to another pack (config, knowledge, MCP set)
pi-stack pack new [PATH]         # adopt an existing repo, or git-init a fresh one
pi-stack pack add skill <name> [PACK]
pi-stack pack add knowledge <name> [PACK] [--ref <git-url|path>] [--private]
pi-stack pack add proxy <name> [PACK] [--host]
pi-stack pack add mcp <name> [PACK] [--env VAR]
pi-stack pack ls                 # show the active pack
pi-stack pack show [PATH]        # inspect a pack's full facet inventory
pi-stack pack rm                 # detach the active pack (files untouched)
```

Adding a capability is one command and one file. `pack add proxy snowflake`
scaffolds `bin/snowflake`, a wrapper script that lands on PATH inside the
sandbox. `pack add mcp fastmail --env FASTMAIL_TOKEN` declares an MCP server
the pack needs plus the env var name it'll ask you to fill via 1Password;
the value never touches the pack or the VM.

**MCP servers and `bin/` wrappers attach at sandbox CREATE, not live.** If you
switch packs or add an MCP inside a running sandbox, it's registered on the
host but the running sandbox doesn't have it yet:

```
pi-stack run --replace     # recreate the sandbox to pick up the new MCP/bin set
```

**Host-mode wrappers** (`pack add proxy platformio --host`) are for tools that
need something the sandbox structurally can't reach, a `/dev/tty*` serial
device is the canonical case. They install to the host, not the sandbox, and
only run under `pi-stack host` (§7), never inside the VM.

**Sharing a pack:** push the git repo, a teammate runs `pi-stack pack use
<url>` and supplies their own `op://` credential refs at adoption. Credentials
in a pack are always 1Password references, never values, on disk or in the
sandbox.

**The trust gate.** Adopting a pack that ships anything that runs on the host
(an MCP server command, a host-mode wrapper, an external binary) stops at a
bill-of-materials screen:

```
This pack runs code on your host (not just in the sandbox):

  MCP servers (host):   fastmail   -> op run -- pi-stack-host mcp fastmail
  Host wrappers:        platformio (bin/platformio)
  Network egress:       api.fastmail.com
  Credentials (op://):  FASTMAIL_TOKEN   (you supply your own; never in the pack)

Adopt this pack and allow the above to run on your machine? [y/N]
```

Default is No. On a non-TTY (CI, a script), it fails closed unless you pass
`--yes`. A pack that ships only skills and knowledge, nothing that executes,
adopts with no prompt.

**Limits.** One active pack at a time (no multi-pack stacking of two packs
simultaneously). External binaries in a pack are SHA-pinned and
re-hashed before every launch; a tampered binary refuses to run rather than
warning you. Switching packs is reversible for what the pack itself
contributed (tracked in a generated `pack.lock`), but it will never remove an
MCP or knowledge bundle you added by hand outside any pack.

## 6. Knowledge

A knowledge bundle is an OKF (Open Knowledge Format) directory of domain
facts: markdown, indexed with SQLite FTS5 and embeddings, searchable
independent of any pack.

```
pi-stack knowledge init [DIR]      # scaffold a bundle
pi-stack knowledge use <path|url>  # point at one
pi-stack knowledge query <text>    # search it (from the host, no sandbox needed)
pi-stack knowledge sync            # commit + push, opens a PR branch by default
```

A pack references bundles rather than only embedding them, and the reference
carries a `shared` flag:

- `shared = true`, a git URL: the reference travels with the pack. An
  adopter pulls the same bundle.
- `shared = false`, a local path: it does not travel. When you share the
  pack, your private knowledge is simply absent from what the teammate gets,
  and it still works for you because the bundle is standalone.

Example: your `work` pack references `shared=true`
`https://github.com/acme/runbooks.git` (the team bundle everyone gets) and
`shared=false ~/notes/okf` (your own scratch notes, which stay yours even if
you hand the pack to a teammate).

**Limits.** `knowledge` resolves to `none` in the public stack by default;
there's no corpus shipped. It only has content once you `init` or `use` one.
A bundle is markdown, not executable, so it never triggers the pack trust
gate (§5), even when it's part of a pack that does.

## 7. Host mode

`pi-stack host` execs pi directly on your host machine, no sandbox, no
network fence. It exists for the two things a Docker sandbox structurally
cannot do: reach a real device (`/dev/tty*` for platformio) and rebuild the
pi-stack image itself (you can't `make load` from inside the VM you're
building).

```
pi-stack config set host.enabled true   # gate is off by default; you turn it on
pi-stack host [DIR]
pi-stack host setup                     # provision the host agent dir first
```

**Say this plainly: host mode is not a security boundary.** pi has no
built-in sandbox or permission prompts of its own; the sandbox *is* pi-stack's
safety model, and host mode steps outside it. What you get instead is a set
of guardrails against accidents, not against a compromised session:
subagents are disabled entirely (no headless child can run unsandboxed),
credentials are sourced just-in-time via 1Password and never persisted to
disk, and the session prints a visible red banner so you can't mistake it for
a normal run. Use it narrowly, for the two cases above, not as a default
runtime.

`pi-stack setup` sources cloud keys from 1Password, and it's mandatory: the
`op` CLI must be installed and signed in, or setup fails with the exact fix.
1Password is the only provider-key source — the old `--use-sbx-keys` /
`--use-1password` flags and the persisted `provider_key_mode` are gone (both
flags now error). setup validates one `op://` ref per provider, mirrors them
into `hostmode.env`, and reconciles them into `sbx`. `pi-stack onboard` never
provisions provider keys.

Host mode reaches cloud models through the same `op://` refs in `hostmode.env`
that setup writes; real validation happens again at every `pi-stack host`
launch via `op run --env-file`. Cloud keys are reported "validated this run"
because setup just resolved them via `op read`.

**Limits.** No sandbox means no network fence, no throwaway teardown, and no
subagent fan-out. If you find yourself reaching for `pi-stack host` as your
everyday driver, that's the failure mode the design explicitly warns against.

## 8. MCP and capabilities

External tools and data (Slack, GitHub, Google Workspace, a company wiki) wire
in as MCP servers, registered with the sbx Cloud MCP Gateway and either
pre-active in a session or discoverable on demand.

```
pi-stack config set mcp <name>     # add a local stdio server to the launch set
pi-stack mcp register              # register configured stdio servers with the gateway
pi-stack mcp ls                    # list what's registered
```

Skills never hardcode a vendor. They ask for a **capability** (`chat`, `docs`,
`github`, `meeting-notes`) and `capabilities.json` maps that capability to a
concrete provider: an `mcp` server, a `cli` on PATH, an `http` service, a
`files` bundle on disk, or `none` if it isn't wired. Swap one JSON file and
every skill that reads that capability retargets at once. See the
`capability-routing` skill for the resolution and fan-out rules.

**Limits.** Registering a stdio MCP server with the gateway is not the same
as attaching it to a running sandbox; like packs, a new MCP needs a sandbox
recreate to show up. Local stdio servers aren't surfaced by the gateway's
dynamic discovery tool, only servers your sandbox was created with `--mcp`
show up.

## 9. Your first hour

1. `pi-stack run` in a real project directory. Not a toy repo, the thing you
   actually need to get done today.
2. Do the task. Say what you want in plain language. A skill will load on its
   own when the conversation matches one (`debug` on a bug report, `build` on
   "implement X").
3. Let the crew show up uninvited. If you ask for a code review, a
   cross-vendor subagent checks it without you naming a model.
4. When you catch yourself repeating a preference or a wrapper script across
   sessions, that's the trigger, not before: `pi-stack pack new` to make it a
   pack, `pack add skill <name>` to save what worked as a reusable flow. Run
   `/learnings` first if you want to know what the watcher already noticed
   repeating.

That's the whole loop: run, work, let the parts introduce themselves, save
the repeat.
