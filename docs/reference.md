# pix reference manual

This is the capability reference. Onboarding teaches you the first task and
gets out of the way; it doesn't walk you through everything pix can do.
This doc does. Read the section you need, run the command, move on.

## 0. Command map

The authoritative verb/flag list is `pix help --all` (and `pix help
<verb>`): it is generated from the dispatch tree, so it
cannot drift. This table says what each verb is FOR and where the reasoning
lives. It is the pointer target for `AGENTS.md`, which deliberately carries no
CLI reference of its own.

| Verb | For | Detail |
| --- | --- | --- |
| `run [DIR]` | launch (or re-attach to) the sandbox for a directory | §1, `docs/design/cli-redesign.md` |
| `resume SESSION [DIR]` | reopen the exact saved session named when pix exited | §1 |
| `status` / `ls` / `rm` | read-only control panel; list and remove `pix-*` sandboxes | §9 |
| `doctor` | probe host + sandbox health and print exact fixes | §9 |
| `setup` | the guided host+agent setup path (keys, memory, pack, identity) | `docs/design/onboarding.md` |
| `reset` | start over: config, data and runtime state moved aside, sandboxes removed | §9 |
| `serve` | the long-running host services (memory :11435) | `docs/design/serve-lifecycle.md` |
| `memory` (`mem`) | recall/remember/forget/stats from the host | §2, `docs/memory.md` |
| `pack` | the portable capability context: ls/show/use/rm (no authoring verb, edit `pack.toml`/`skills/` by hand) | §5, `docs/design/packs-v2.md` |
| `mcp` | `add`/`ls`/`auth` the MCP servers your pack declares, through the sbx gateway (the one door integrations come through) | §8 |
| `secret` | manage the 1Password `op://` refs (never the values) | §8 |
| `uat` | `status`/`browser bootstrap` for self-UAT | `docs/HOST-UAT.md` |
| `config` | `show`/`path`/`get`/`set`/`unset` the single runtime config | §1 |
| `task` | isolated parallel-work clones + sandboxes | `docs/design/worktree-tasks.md` |
| `models` | which models pix can use and how they are bound: bare status, `add` | `docs/design/ollama-inference.md` |
| `agent` | the subagent roster, read-only: `ls` only (`new`/`edit`/`rm`/`reassess` were removed) | §4 |
| `version`, `help` | stamped version; tiered help | (none) |

Removed verbs are simply gone: pix has no released users to keep a recovery
path for, so a deleted surface gets the ordinary unknown-command answer rather
than a curated migration notice. The live verb set is whatever `pix help --all`
lists. `pix state <backup|restore|reset>` is one of those removals and is not
coming back: `reset` returned as a top-level verb (§9), and the archive format
`backup`/`restore` spoke collapsed into `pix-host memory snapshot|restore`,
which is one sqlite file rather than an archive.

## 1. What pix is

pix is a multi-model coding agent (Claude, GPT, Gemini, plus local Ollama)
that runs inside a throwaway Docker sandbox. Nothing you do in a session
touches your host machine except through explicit mounts, git, and the sandbox
kit's network allowlist. When you're done, `sbx rm -f` and the sandbox is gone.
State that should persist (memory, packs, config) lives outside the sandbox on
the host, so it survives even though the sandbox doesn't.

```
pix run [DIR]               # launch (default: current dir)
pix resume SESSION [DIR]    # resume the exact session named on exit
pix                         # same as `pix run` at a terminal; status when piped
pix --dev                   # at a terminal: `pix run --dev`; scripts must use the explicit form
```

Dev mode's UAT MCP is fixed at sandbox creation. If that sandbox already exists
without its session UAT registration, `pix --dev` refuses rather than attaching
a session with missing tools. Run the exact recreate command it prints.

## 2. Memory

pix remembers durable facts, preferences, and conventions across sessions,
so you don't re-teach the agent every time you open a new sandbox.
**Capture is explicit by default**: nothing is written unless a human or an
explicit command asks for it (`/remember`, `pix memory remember`, or the
agent's own explicit tools). There is no background watcher writing memory
out of the box.

```
/recall <query>       # what memory would surface for this query
/remember <text>      # pin a fact immediately
/forget <id|query>     # drop a memory; id from /recall, or a query to drop its top match
```

**Opt in to automatic capture** with `pix config set memory_capture
experimental-auto` if you want a background watcher to extract facts and
corrections from what you say, under one fixed daily budget (10 stored
rows/day). This only reaches a *new* sandbox: an already-running one keeps
the mode it launched with. A watcher-captured row is tagged with an `auto`
annotation on `/recall`/`memory_recall`, visibly distinct from an explicit
one, and `/forget <id>` is the feedback/undo mechanism for it, same as any
other row. See `docs/memory.md`'s "How capture works" for the full
admission rules, the secret filter, and the budget accounting.

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

Example: you tell pi your staging DB is `postgres://staging.internal:5432` and
say `/remember`. Next session, `/recall staging db` finds it, or it surfaces
unprompted in the system prompt when it's relevant. With `experimental-auto`
capture opted in, the watcher can extract and store the same kind of fact on
its own, without an explicit `/remember`.

**Durability.** A durable fact (preference, decision, convention) has no
automatic expiry, and every row pix writes is durable now, there is no
perishable/TTL write path any more. `/remember` is the reliable explicit
write, driven by the human, not the agent, though a stable fact the watcher
captures on its own can also land durable. A store that predates this is
retired once at startup rather than left holding rows nothing will ever
expire; see docs/memory.md's Legacy data section.

**What gets silently injected vs. what you can see.** Each turn, only a small
relevance-filtered subset is silently added to context; every row is durable,
so there is no score-based durability floor filtering anything out (a legacy
perishable-scoring floor existed here once, deleted along with the write-side
perishable/TTL path it policed). An explicit `/recall` or `memory_recall`
skips that ranking filter entirely, but is still capped: a blank query (or
`*`) returns up to 100 rows (with a
truncation line if the store has more), not a true unbounded dump.

**Limits.** Memory runs as a host service (`pix serve`, port 11435) and is
per-machine, not shared across your laptop and your desktop, and not shared
with teammates. It's SQLite plus FTS5 and embeddings on disk at
`~/.local/state/pix` (or wherever your config points it). If the service
is down, the commands and tools above surface a clear error rather than
failing silently; only the silent per-turn auto-injection degrades quietly (no
memory gets added that turn, so a dead daemon never blocks the conversation).
Nothing here is a pack: memory is personal, per-machine storage with no
versioning and no sharing (see Limits above); a pack is versioned, shared
capability context you'd `git diff` (see §5). A pack can still scope memory
to itself via `memory_scope` (see [memory.md](memory.md)).

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

An agent (a subagent preset) names a model, or inherits the session's. There is
no router: nothing resolves a model on your behalf, and nothing scores one.
Pointing `review` at a different vendor than the one that wrote the code is a
choice you make in the agent file, and it is worth making.

```
pix agent ls                 # roster with each agent's model and where it came from
pix models                   # the models this host can call, and their backends
```

`pix agent ls` is the only `agent` subcommand. `new`/`edit`/`rm`/`reassess`
were removed: an
agent is a hand-edited `agents/*.md` file, not a CLI mutation surface. To
change one, edit its frontmatter (or add a new file) directly.

Example: `code-review` finishes its own pass, then dispatches the `review`
subagent, which you have pointed at GPT because your code was written by
Claude. You get an adversarial second opinion, not an echo.

**Limits.** A model is only as good as the one you picked: pix measures none of
them and ranks none of them.
Subagents run headless (`pi --no-extensions`), so a child that gets stuck has
no UI to show you; a watchdog kills it after an idle or wall-clock timeout and
reports the failure instead of hanging forever.

## 4b. Your own skills and standing instructions

Personal context lives in one directory on the host, `~/.local/share/pix/context`
(`$XDG_DATA_HOME/pix/context`), and needs no pack:

```
~/.local/share/pix/context/
  AGENTS.md               # standing instructions, injected into every session
  skills/<name>/SKILL.md  # your own skills, alongside the baked ones
```

That directory is **bind-mounted read-write at the same path inside the
sandbox**, and it is mounted unconditionally, created if absent, so a session can
author its FIRST skill without going back to the host. Edits land on the host
immediately, so the whole directory can be a git repo you commit from either
side.

The two files have different lifecycles, deliberately:

- `skills/` is read live. Add or edit a `SKILL.md` and `/reload` picks it up in
  the running session.
- `AGENTS.md` is read ONCE at launch and inlined into a generated kit as
  `agentInstructions`. Editing it mid-session changes the file, not the session.
  The next sandbox picks it up. (Claude Code's `CLAUDE.md` behaves the same way.)

**None of this is enforcement.** Instructions are context a model reads and can
edit; a rule in `AGENTS.md` is not a fence. Enforcement is the sandbox boundary,
the kit's `permissions.network.allow`, and, for a certain refusal, a pi extension
hooking `tool_call` to return `{block: true, reason}`. The `guard` skill is a
reminder the agent is asked to honor, not a gate, and says so in its own text.

## 5. Packs

A pack is an explicit git repo containing portable capability context: skills,
an OKF knowledge bundle, the MCP servers and CLI wrappers you're wired to, and
config (model prefs, a memory scope, an inference gateway). It's the thing you'd
`git diff`, as opposed to memory, which you'd only see in a `/recall`. Packs are
opt-in and may be composed in order during setup.

```
pix pack use <path|git-url> # switch to another pack (config, knowledge, MCP set)
pix pack ls                 # show the active pack
pix pack show [PATH]        # inspect a pack's full facet inventory
pix pack rm                 # detach the active pack (files untouched)
```

There is no authoring verb. A pack is a directory you create and edit by
hand: a `pack.toml` (name + facets) plus `skills/`, `knowledge/`, and `bin/`
as needed: see docs/design/packs.md for the schema. Adding a capability is
one file and one `pack.toml` stanza: a `bin/warehouse` wrapper script plus a
`[[proxy]]` entry lands it on PATH inside the sandbox.

An MCP server is an `[[integrations]]` stanza, and it must declare **exactly
one transport**: `command` (a host binary the gateway spawns over stdio),
`image` (a container the gateway runs), `manifest` (an OCI server manifest), or
`url` (a remote endpoint the gateway OAuths). Pix ships no MCP servers of its
own and special-cases no vendor, so a name with no transport behind it is
refused at load rather than registering nothing: that shape is what left a dead
registration the gateway went on reporting as ready.

```toml
[[integrations]]
  name    = "Fastmail"
  mcp     = "fastmail"
  command = "fastmail-mcp"
  args    = ["serve", "--readonly"]   # LITERAL argv, never templated
  env     = "FASTMAIL_TOKEN"          # the op:// secret to solicit
  probe   = ["fastmail-mcp", "check"] # what doctor runs to prove it works
```

A remote server is the same stanza with `url = "https://..."` and no argv.
`env` names the credential `pack use` asks you to fill via 1Password; the value
never touches the pack or the VM. `env_keys` forwards additional env NAMES
(non-secret per-user values like an account id travel here, not in `args`), and
`probe` is the read-only argv `pix doctor` runs to establish the server can
actually do its job. Onboarding is declarative too: a `[[setup]]` block states
`[[setup.require]]` conditions (`bin`, `op-ref`, `probe`) and
`[[setup.apply]]` remediations (`interactive`, `exec`), so a pack can never
call a pix verb that no longer exists, because it never names one.

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

  MCP server (host):   fastmail → fastmail-mcp serve --readonly
                       Credential (name only, value stays in 1Password): FASTMAIL_TOKEN
  Host wrapper:        platformio (bin/platformio)
  Network access:      api.fastmail.com

Activate this pack and allow these integrations? [y/N]
```

The reviewed argv is exactly what the pack declared: the bare command plus its
literal args. Resolving that command to an absolute path is a property of *your*
PATH and deliberately not part of what you consent to.

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
corpus shipped in the public stack: the `knowledge` capability resolves to
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
rests on, so the two things it structurally cannot do (reach a real device and
rebuild the pix image itself) are done from your own shell, not through pix.

## 8. MCP and capabilities

External tools and data (a mailbox, a company wiki, an HR directory) wire in as
MCP servers, run through the sbx gateway. **Pix ships none of them.** Every
server pix can register is declared by the active pack (§5), in one of four
transports, and pix special-cases no vendor: there is no built-in Google
Workspace, no built-in Slack, and no `pix-host` subcommand that serves an MCP
server.

A credential an MCP server needs is a 1Password `op://` reference resolved at
spawn (§8 below, `pix secret`), never a value on disk and never baked into the
gateway registration. Where a server needs a user identity (a Slack `xoxp-`
user token is the canonical case), that identity is one named person's, never a
shared team token and never handed to a second person to reuse.

```
pix mcp add <name> --url <url>   # a hosted server, by URL
pix mcp add <name>               # one the active pack declares, with its
                                 # 1Password credential wrapper
pix mcp add                      # everything in the config mcp list
pix mcp auth <name>              # hosted-control-plane OAuth
pix mcp ls                       # list what's registered
```

Skills never hardcode a vendor. They ask for a **capability** (`chat`, `docs`,
`github`, `meeting-notes`) and `capabilities.json` maps that capability to a
concrete provider: an `mcp` server, a `cli` on PATH, an `http` service, a
`files` bundle on disk, or `none` if it isn't wired. Swap one JSON file and
every skill that reads that capability retargets at once. See the
`capability-routing` skill for the resolution and fan-out rules.

**Registration is not the same as being usable.** `pix mcp add` (or native
`sbx mcp add`) makes a server known to the gateway. It does not put that
server's tools in front of any running session. Each pack transport lands on one
sbx grammar: `command` and `image` become `--command`/`--args` (a host process
the gateway spawns, wrapped in `op run --env-file` when the server declares
credential names), `manifest` becomes `--local --url <manifest>` (a container the
gateway runs from an OCI manifest, credentials Docker-side), and `url` becomes
`--url` (a remote endpoint, OAuth'd host-side). `sbx mcp get <name>` shows you
exactly what's registered.

Two failures here are hard, not warnings, because both used to register
something broken and leave the gateway calling it ready:

- **A name no active pack declares** is an error naming the server, not a skip.
- **A `command` whose binary is not on PATH** is an error naming the server, the
  binary and the pack's install hint. It does not register.

A credential-free server is never wrapped in `op run`, deliberately: it must not
share fate with unrelated refs in your op-refs file.

**pix tolerates one sbx CLI grammar change at a time, never blindly.** A
container (manifest/remote-URL) registration runs a read-only `sbx mcp add
--help` once, up front, and picks the grammar that help text documents. It is
chosen there rather than after a failed attempt because a remote-URL
registration can open an interactive OAuth grant, and retrying that after a
failure risks a second, unwanted grant. It never loops or guesses past its one
known alternate; an unresolvable case reports the real failure, not an invented
one.

**`pix mcp add` never overwrites a server it does not own.** It fetches
registration evidence once (`sbx mcp ls`), classifies every name against it, and
only then acts: absent means add, registered at the same endpoint means leave it
alone, and registered under the same name at a DIFFERENT endpoint or kind fails
closed. Your `notion` is never replaced by the URL pix happens to know for that
name.

**Static preload, or explicit load, nothing else.** Every server in your
configured `mcp` list, and every integration an active or transient pack
carries, is passed to sbx as `--static-mcp <name>` when the sandbox is
CREATED, so its tools are in context from the start. There is no dynamic
discovery and no on-demand attach: a server you add or register after a
sandbox exists is not visible to it until you recreate: `pix rm BOX && pix run`
re-sends the full `--static-mcp` set.

The live-attach verb (`pix mcp load`) was removed. It existed to avoid a
recreate in a stack whose sandboxes are disposable and whose bare `pix`
recreates one in a second, and it wrote no receipt anyway, because "attached
once" is not the state of a live session. `pix status` and `pix doctor` never
poll a sandbox and never claim to know what it has attached; they report what
the HOST can check (see §9).

## 9. Status and doctor

`pix status` is a fast, read-only dashboard: services, provider keys, the
active pack, and, per configured MCP server, the things the host can actually
check, each tri-state (yes / no / unknown: never guessed):

- **declared**: some active pack says what this server *is*. A name nothing
  declares is a gap even when the gateway lists it as ready. That is a live host
  command nothing can vouch for, and the usual cause is a registration outliving
  the pack that created it.
- **registration**: `sbx mcp ls` says the server is known to the gateway
- **resolvable**: a declared `command` still exists on PATH. A registration
  pointing at a deleted binary lists exactly like a healthy one.
- **auth**: for a remote/OAuth server only (catalog or pack-remote), the
  hosted control plane's login state; a local stdio server has no
  control-plane auth to check
- **working**: the pack's declared `probe` argv exits clean, run through the
  same `op run` wrapper the gateway uses to spawn the server. Probing any other
  way inherits whatever you happen to have exported in your shell, which is
  exactly how a broken credential setup passes every check and then fails on
  first use.

**Attachment is deliberately not a third truth.** Nothing pix can run from
the host answers whether a RUNNING sandbox currently has a server's tools
loaded, so a registered server's note always carries the same caveat:
"host registration; attachment to a live session is not checkable from
here", instead of guessing `attached`/`not attached`. `pix mcp ls` prints
the identical caveat. The fix for a server that's registered but not (yet)
in your session is always the same regardless of history: `pix rm BOX && pix
run` to recreate and pick up the full `--static-mcp` set.

**Registered is not working, and doctor says which it proved.** A pack that
declared no `probe` gets "registered; no health probe declared, so working order
is unverified", not a tick. Absence of a check is never reported as a pass, and
a probe that cannot answer in time is `unverifiable`, not broken (if it runs
through `op run`, a locked 1Password vault is the usual reason, and doctor says
so, because a bare "could not run" sends people hunting the wrong problem).

`pix doctor` runs the same evidence through five verdicts per check:
**ready** (verified working), **todo** (a verified, fixable gap, with the
exact command), **unverifiable** (a probe timed out or the tool needed to
check isn't available; never treated as broken), **denied** (an explicit
policy or permission refusal, distinct from a setup gap), and **off**
(verified, optional, and intentionally not configured: no active pack, no
MCP servers, no supervised daemons, a disabled memory unit, zero local models.
Neither a gap nor a pass: no `Fix:` entry, never counted as an issue, and it
never turns the headline red). `doctor --json`
emits `schema_version` so a script can tell the shape apart from an older
run. Exit codes: **2** on a usage error, **1** only when a REQUIRED check is a
positively verified failure, **0** otherwise, including every optional or
unverifiable gap. Required, always: the **sbx CLI** being installed and at
least one resolved **provider key** (a single key for any one of Anthropic,
OpenAI, or Google satisfies it; you don't need all three): either one
failing alone is enough to fail doctor. A config file that fails to load
entirely is its own separate required gap: nothing else can be probed
without one, so it is reported alone. `pix-host serve`'s Suture-supervised
launchd unit and the pack system were both deleted in the Pix v2 cutover
(docs/design/pix-v2-architecture.md §14): there is no launchd/pack row left
to ever be required OR optional. The PIX_HOME-scoped `pix-memory` container's
health is a separate, always-run probe set (`CheckHome`), not part of this
required/optional model at all.

**sbx-missing exit codes are unified across every surface that shells to
sbx.** `pix ls`, `pix rm`, and every `pix mcp` verb that promises an operation
(`add`/`auth`, and read-only `ls`) all return the SAME detectable error when
`sbx` is not on PATH, so they exit and message this identically instead of
drifting into four different "sbx is missing" stories:

| surface | sbx absent -> exit | message names |
| --- | --- | --- |
| `pix ls` | 3 (`exitServiceDown`) | the exact install fix (`brew install docker/tap/sbx`) |
| `pix rm` | 3 (`exitServiceDown`) | the exact install fix |
| `pix doctor` | 1 (`ExitNotReady`) only if a REQUIRED check is a verified gap; sbx's own gap is `todo` with the exact install fix as its `Fix` | the exact install fix (`health.SbxInstallFix`) |

`pix mcp` is not part of the v2 CLI surface (Pix's own MCP registration/
administration was deleted, AC-16); the sbx Gateway is the only
sandbox-facing integration path.

The shared plumbing: `mcp.ErrSbxUnavailable` is the one sentinel every mutating
mcp verb and `ls`/`rm` wrap (`errors.Is`-detectable); the command layer maps it
to exit 3 (`sbxAwareFail` in `cmd/pix/root.go`, `mcpFailed` in
`cmd/pix/mcp_cmd.go`) and the install fix text is the ONE constant
(`health.SbxInstallFix`) doctor, ls, and rm all quote verbatim, never a
second paraphrase of "go install the CLI" that can drift out of sync with the
first.

### Starting over: `pix reset`

`doctor` fixes a host. `reset` abandons one. It is the "I want to be back where
I started" verb, and it is **reversible**: three directories are renamed to a
timestamped `<path>.bak-<unixts>` sibling, and renaming one back is a complete
undo.

| moved aside | holds |
| --- | --- |
| `~/.config/pix` | `config.toml`, `op-refs.env`, `pack-trust.json` |
| `~/.local/share/pix` | the memory store, adopted packs, personal context |
| `~/.local/state/pix` | daemon pid/lock/log, sandbox leases, **`pix task` checkouts** |

That last row is why `reset` contains no delete at all. The state dir looks like
pure ephemera and is not: `state/pix/tasks/<repo>/co/<name>` holds real git
checkouts, which can carry uncommitted work. A curated "these files are safe to
delete" list would have to be re-audited every time something new starts writing
under the state dir; a rename cannot eat anything, whatever ends up there.

Sandboxes are removed the same way `pix rm --all` removes them (non-forced,
each needing a kernel-verified zero-reference proof), because `reset` literally
calls it. There is no bulk force path. The MCP servers the (now moved-aside)
config declared are unregistered from the sbx gateway to match.

Left alone, deliberately: your **provider keys** (`sbx secret` / 1Password) and
your git repos. Re-entering keys is friction with no upside here.

```bash
pix reset                    # the whole stack, after a [y/N] confirmation
pix reset --keep-memory      # everything except the captured-memory store
pix reset --keep-sandboxes   # host state only; leave the pix-* boxes running
pix reset --yes              # no prompt (required on a non-interactive terminal)
```

Two refusals are load-bearing. Without `--yes`, a non-interactive terminal is
refused outright (exit 2, nothing touched) rather than reset by a script that
did not mean it. And a `pix-host serve` that cannot be proven **down** blocks
the data-dir move, because renaming a live sqlite writer's directory splits the
db from its wal; `--force` overrides that one and deliberately does not
override the state-dir move, since taking a live daemon's pidfile away orphans
it from `pix serve stop`. A daemon that was running is restarted on the clean
slate. Afterwards: `pix setup`.

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
   then `pix pack use <path>` to activate it. Run `/recall` first if you
   want to see what the watcher already captured on the topic.

That's the whole loop: run, work, let the parts introduce themselves, save
the repeat.
