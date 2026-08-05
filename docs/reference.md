# pix reference manual

This is the capability reference. Onboarding teaches you the first task and
gets out of the way; it doesn't walk you through everything pix can do.
This doc does. Read the section you need, run the command, move on.

## 0. Command map

The authoritative verb/flag list is `pix help --all` (and `pix help
<verb>`) — it is generated from the dispatch tree, so it
cannot drift. This table says what each verb is FOR and where the reasoning
lives. It is the pointer target for `AGENTS.md`, which deliberately carries no
CLI reference of its own.

| Verb | For | Detail |
| --- | --- | --- |
| `run [DIR]` | launch (or re-attach to) the sandbox for a directory | §1, `docs/design/cli-redesign.md` |
| `status` / `ls` / `rm` | read-only control panel; list and remove `pix-*` sandboxes | §9 |
| `doctor` | probe host + sandbox health and print exact fixes | §9 |
| `setup` | the guided host+agent setup path (keys, memory, pack, identity) | `docs/design/onboarding.md` |
| `serve` | the long-running host services (memory :11435) | `docs/design/serve-lifecycle.md` |
| `memory` (`mem`) | recall/remember/forget/learnings/stats from the host | §2, `docs/memory.md` |
| `pack` | the portable capability context: ls/show/use/rm — no authoring verb, edit `pack.toml`/`skills/` by hand | §5, `docs/design/packs-v2.md` |
| `mcp` | register/list/load MCP servers through the sbx gateway — the one door integrations come through | §8 |
| `secret` | manage the 1Password `op://` refs (never the values) | §8 |
| `config` | `show`/`path`/`get`/`set`/`unset` the single runtime config | §1 |
| `task` | isolated parallel-work clones + sandboxes | `docs/design/worktree-tasks.md` |
| `monitor` | live-follow a sandbox's out-of-sandbox traffic | `docs/design/monitor.md` |
| `models` | which models pix can use, and the model router: ls/show/pick/route | `docs/design/routing.md`, `docs/design/models-cli.md` |
| `agent` | the subagent roster: ls/new/edit/rm/reassess | §4 |
| `version`, `help` | stamped version; tiered help | — |

Retired verbs (`slack`, `gworkspace`, `knowledge`, `host`, `upgrade`, `man`,
`backup`, `restore`, `reset`, `state backup|restore|reset`, `task harvest|gc`,
`route`, `onboard`, `evals`) answer with a `PIX_RETIRED` line naming the
replacement and exit 2, doing nothing else. `reset`/`state reset` moved state
aside as a reversible clean-slate; ephemeral sandboxes plus `setup`/`doctor` do
that job now, so recovery is manual and evidence-first: run `doctor` first,
back up whatever `config path` / `status --json` show, then `setup` — there is
no automated wipe. Every retirement is recorded, with its reason and
replacement, in `services/host/cmd/pix/corpus/retirement.jsonl`.

## 1. What pix is

pix is a multi-model coding agent (Claude, GPT, Gemini, plus local Ollama)
that runs inside a throwaway Docker sandbox. Nothing you do in a session
touches your host machine except through explicit mounts, git, and the sandbox
kit's network allowlist. When you're done, `sbx rm -f` and the sandbox is gone.
State that should persist (memory, packs, config) lives outside the sandbox on
the host, so it survives even though the sandbox doesn't.

```
pix run [DIR]      # launch (default: current dir)
pix                # status only, never launches
```

## 2. Memory

pix learns from what you do, without you telling it to. A background
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
`pix` or `curl`. It reaches for `memory_recall`/`memory_stats` when you
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

**Limits.** Memory runs as a host service (`pix serve`, port 11435) and is
per-machine, not shared across your laptop and your desktop, and not shared
with teammates. It's SQLite plus FTS5 and embeddings on disk at
`~/.local/state/pix` (or wherever your config points it). If the service
is down, the commands and tools above surface a clear error rather than
failing silently; only the silent per-turn auto-injection degrades quietly (no
memory gets added that turn, so a dead daemon never blocks the conversation).
Nothing here is a pack: memory is what pix learned by watching, not what
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
| `architecture-audit` | map and stress-test a whole system or major refactor with the full crew |
| `tdd` | failing test first, watch it fail, minimal code to pass, refactor |
| `verify` | prove a claim by running the command before you say "done" |
| `qa` | drive a running app in the browser, report bugs with screenshots |
| `healthcheck` | is the harness working (keys, memory, MCP, skills) and is the code healthy (tests, lint, dead code) |
| `enrich` | write a durable fact into a shared knowledge bundle, gated by PR |
| `promote` | review what the memory watcher keeps repeating and graduate it into a skill or convention |

Example: say "let's fix this bug" and `debug` loads on its own; you don't
type `/skill:debug`. Say "review my diff before I push" and `code-review`
loads and hands off to a cross-vendor subagent automatically. Use
`architecture-audit` when the question is whether the system or a major
refactor is sound, not whether one diff has a defect.

**Limits.** Skills are mechanism, not your personal config. A skill never
hardcodes your channel names or account IDs; that data lives in memory or a
pack (§5) and the skill reads it at runtime. A skill baked into the image is
read-only in a running sandbox; edit and version your own by adding a
`skills/<name>/SKILL.md` file to a pack directly.

## 4. The crew

pix isn't one model. Four providers are in rotation (Claude, GPT, Gemini,
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
pix agent ls                 # roster with each agent's resolved model and WHY
pix models pick <intent>     # what the router would resolve for that intent
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

A pack is an explicit git repo containing portable capability context: skills,
an OKF knowledge bundle, MCP servers, CLI wrappers,
you're wired to, and config (which Google account, which model prefs). It's
the thing you'd `git diff`, as opposed to memory, which you'd only see in a
`/recall`. Packs are opt-in and may be composed in order during setup.

```
pix pack use <path|git-url> # switch to another pack (config, knowledge, MCP set)
pix pack ls                 # show the active pack
pix pack show [PATH]        # inspect a pack's full facet inventory
pix pack rm                 # detach the active pack (files untouched)
```

There is no authoring verb. A pack is a directory you create and edit by
hand: a `pack.toml` (name + facets) plus `skills/`, `knowledge/`, and `bin/`
as needed — see docs/design/packs.md for the schema. Adding a capability is
one file and one `pack.toml` stanza: a `bin/warehouse` wrapper script plus a
`[[proxy]]` entry lands it on PATH inside the sandbox; an `[[integrations]]`
stanza with `mcp = "fastmail"` and `env = "FASTMAIL_TOKEN"` declares an MCP
server the pack needs plus the env var name `pack use` will ask you to fill
via 1Password — the value never touches the pack or the VM.

**MCP servers and `bin/` wrappers attach at sandbox CREATE, not live.** If you
switch packs or add an MCP inside a running sandbox, it's registered on the
host but the running sandbox doesn't have it yet:

```
pix rm BOX && pix run # recreate the sandbox to pick up the new MCP/bin set
```

**Host-mode wrappers** (a `[[proxy]]` entry with `host = true`) are for tools
that need something the sandbox structurally can't reach, a `/dev/tty*` serial
device is the canonical case. They install to the host, not the sandbox, and
run on the host, not in the sandbox (§7), so a sandboxed session cannot use
them at all.

**Sharing a pack:** push the git repo, a teammate runs `pix pack use
<url>` and supplies their own `op://` credential refs at adoption. Credentials
in a pack are always 1Password references, never values, on disk or in the
sandbox.

**The trust gate.** Adopting a pack that ships anything that runs on the host
(an MCP server command, a host-mode wrapper, an external binary) stops at a
bill-of-materials screen:

```
This pack runs code on your host (not just in the sandbox):

  MCP servers (host):   fastmail   -> op run -- pix-host mcp fastmail
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

Knowledge is pack-delivered context: an OKF (Open Knowledge Format) directory
of domain facts a pack points at. There is no launcher verb for it and no
corpus shipped in the public stack — the `knowledge` capability resolves to
`none` until a pack wires one, and `pix pack use <path|url>` is how it arrives.

A pack references bundles rather than only embedding them, and the reference
carries a `shared` flag:

- `shared = true`, a git URL: the reference travels with the pack. An
  adopter pulls the same bundle.
- `shared = false`, a local path: it does not travel. When you share the
  pack, your private knowledge is simply absent from what the teammate gets.

A bundle is markdown, not executable, so it never triggers the pack trust gate
(§5), even when it's part of a pack that does.

## 7. Leaving the sandbox

There is no unsandboxed run mode. The sandbox is the boundary the whole design
rests on, so the two things it structurally cannot do — reach a real device and
rebuild the pix image itself — are done from your own shell, not through pix.

## 8. MCP and capabilities

External tools and data (Slack, GitHub, Google Workspace, a company wiki) wire
in as MCP servers, run through the sbx gateway.

A credential an MCP server needs is a 1Password `op://` reference resolved at
spawn (§8 below, `pix secret`), never a value on disk and never baked into the
gateway registration. Where a server needs a user identity — a Slack `xoxp-`
user token is the canonical case — that identity is one named person's, never a
shared team token and never handed to a second person to reuse.

```
pix config set mcp <name>     # add a local stdio server to the launch set
pix mcp register              # register configured stdio servers with the gateway
pix mcp ls                    # list what's registered
```

Skills never hardcode a vendor. They ask for a **capability** (`chat`, `docs`,
`github`, `meeting-notes`) and `capabilities.json` maps that capability to a
concrete provider: an `mcp` server, a `cli` on PATH, an `http` service, a
`files` bundle on disk, or `none` if it isn't wired. Swap one JSON file and
every skill that reads that capability retargets at once. See the
`capability-routing` skill for the resolution and fan-out rules.

**Registration is not the same as being usable.** `sbx mcp add` (or `pix
mcp register`) makes a server known to the gateway. It does not put that
server's tools in front of any running session. A native server is added one
of three ways: `--command`/`--args` (a local process the gateway spawns
host-side), `--url` (a remote endpoint, OAuth'd host-side), or `--local --url
<manifest>` (a container the gateway runs from an OCI manifest). `sbx mcp get
<name>` shows you exactly what's registered.

**Static preload, or explicit load, nothing else.** Every server in your
configured `mcp` list, and every integration an active or transient pack
carries, is passed to sbx as `--static-mcp <name>` when the sandbox is
CREATED, so its tools are in context from the start. There is no dynamic
discovery and no on-demand attach: a server you add or register after a
sandbox exists is not visible to it until you either recreate
(`pix rm BOX && pix run`, which re-sends the full `--static-mcp` set) or
attach it live:

```
pix mcp load <name> [DIR]      # attach an already-registered server to the
                                    # RUNNING sandbox for DIR (default cwd), no recreate
pix mcp auth [args...]         # hosted-control-plane OAuth for remote catalog
                                    # servers (auth --all, auth status --all, auth rm)
pix mcp bundle                 # register the shipped catalog (notion/
                                    # atlassian/granola) in one step
```

`pix mcp load` resolves to `sbx mcp load <name> --sandbox <box>` and
records a receipt only after that attach succeeds; `pix status` and
`pix doctor` read that receipt back, they do not poll the gateway live
(see §9).

## 9. Status and doctor

`pix status` is a fast, read-only dashboard: services, provider keys,
knowledge bundles, and, per configured MCP server per running sandbox, one of
five states drawn from launcher receipts, not a live gateway probe:

- **preloaded**: the sandbox's create receipt says this server shipped as
  `--static-mcp`, and it's still registered
- **loaded**: a later `pix mcp load` receipt attached it, and it's
  still registered
- **registered-not-attached**: registered, but neither receipt covers this
  sandbox; the fix is `pix mcp load <name> <dir>`
- **not-registered**: `sbx mcp ls` positively lacks the server
- **unverifiable**: an old or externally created sandbox with no launcher
  receipt, or the registration/sandbox listing itself failed; status never
  guesses a state it can't back with a receipt

`pix doctor` runs the same evidence through four verdicts per check:
**ready** (verified working), **todo** (a verified, fixable gap, with the
exact command), **unverifiable** (a probe timed out or the tool needed to
check isn't available; never treated as broken), and **denied** (an explicit
policy or permission refusal, distinct from a setup gap). `doctor --json`
emits `schema_version` so a script can tell the shape apart from an older
run. Exit codes: **2** on a usage error, **1** only when a core requirement
(a model provider key, or the config file itself) is a positively verified
failure, **0** otherwise, including every optional or unverifiable gap. A
single resolved key for any one of Anthropic, OpenAI, or Google satisfies the
provider check; you don't need all three.

## 10. Your first hour

1. `pix run` in a real project directory. Not a toy repo, the thing you
   actually need to get done today.
2. Do the task. Say what you want in plain language. A skill will load on its
   own when the conversation matches one (`debug` on a bug report, `build` on
   "implement X").
3. Let the crew show up uninvited. If you ask for a code review, a
   cross-vendor subagent checks it without you naming a model.
4. When you catch yourself repeating a preference or a wrapper script across
   sessions, that's the trigger, not before: write a `pack.toml` and a
   `skills/<name>/SKILL.md` by hand to save what worked as a reusable flow,
   then `pix pack use <path>` to activate it. Run `/learnings` first if you
   want to know what the watcher already noticed repeating.

That's the whole loop: run, work, let the parts introduce themselves, save
the repeat.
