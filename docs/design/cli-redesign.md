# pix CLI redesign — make it usable by a new human

Status: IMPLEMENTED (Shape B shipped — `state` grouping noun, tiered `help` / `help --all` with per-noun `help <verb>`, staged `setup`; all legacy verb spellings retained as aliases)
Authors: pi + crew (dx-consultant, product-manager, architect, ux-copywriter), reviewed by cross-vendor `review`

## The problem

The `pix` launcher has grown to ~19 top-level verbs in a flat namespace.
The complaint, verbatim intent:

- "It's a monstrosity, a zillion commands."
- "The man page is really not helpful, it doesn't explain how to set anything up."
- "The setup flow is garbage, no explanation of 1Password, just did something
  but I don't even know what."
- "This has no taste."

The diagnosis is not that the surface is *wrong*. Every verb earns its keep and
the mechanics (idempotent config writes, `--json`, reversible resets) are solid.
The surface is **flat and unnarrated**: 19 peer verbs with no grouping, four of
them destructive, three one-time, and a setup screen that dumps every jargon term
(`op-refs.env`, `op://vault/item/field`, sbx gateway, MCP stdio, OKF bundle) at
once with no "you can stop here." A new human has no path through it.

## The taste we are borrowing (Mitchell Hashimoto)

Terraform, Vagrant, Packer, Ghostty share a shape:

1. **A tiny set of daily verbs** you hold in your head. Everything else lives one
   level down under a noun (`terraform state list`, not 19 flat verbs).
2. **Progressive disclosure.** The common path is dead simple; power lives one
   level down. Default help shows the handful you need, not everything.
3. **The tool teaches you as you use it.** Every screen ends with an obvious
   next step. Errors say what happened AND what to do.
4. **Opinionated defaults, explicit escape hatches.** Optional things are
   clearly optional and deferrable, never blocking.
5. **No surprises.** The bare command is safe and read-only; destructive things
   are explicit and reversible. (pix already gets this right.)
6. **Jargon is defined the first time it appears, or hidden until needed.**

## What does NOT change

- The bare `pix` command stays read-only status, never launches. Correct
  and Hashimoto-consistent; keep it.
- Every mechanic: idempotent config writes, `--json` shapes, exit codes (2 usage,
  3 daemon-down), reversible `reset`, the `--man` global, ports.
- **Full back-compat.** Every one of the ~19 current verb spellings keeps
  working as an alias. This is a *visibility* reclassification, not a removal.
  A back-compat test invokes all legacy spellings and expects success.

## The golden path (the north star)

A new human should reach a working agent with as few steps as possible, reading
no docs:

```
1. curl -fsSL https://raw.githubusercontent.com/mcavage/pix/main/install.sh | sh
2. pix setup      # narrates; checks keys + sbx; offers to skip the rest
3. sbx secret set -g anthropic -t "sk-..."   # only if setup found no key
4. pix run        # launches the sandbox; you are in the agent
```

**Honesty about step 3 (review finding, P0).** `setup` does NOT set provider
keys itself; keys are `sbx` secrets injected by the proxy, and `setup` today only
*prints* the `sbx secret set` command. So the path is not a clean three commands
for a keyless user. Two hard requirements fall out of this:

- `setup` must not print "you are ready" unless at least one provider key is
  actually present AND `sbx` is on PATH. If neither, it ends on the exact
  `sbx secret set` line as the required next step, and says so plainly.
- `run` must fail loud and helpful when no key is present (name the missing key,
  print the `sbx secret set` line, exit non-zero) rather than launch a dead agent.

Everything not on this path is deferred or hidden.

## Command tiers

Default `pix help` shows the **Core** tier only. `pix help --all`
(and teachable per-noun `pix help <noun>`) reveals the rest. Every verb
stays fully working and self-documenting regardless of tier.

The tier name is **Core**, not "daily" (review finding P1-7): `setup` is
one-time, `doctor` is troubleshooting, and `serve` is process management, not
daily work. But all three belong in the default view because they are what a new
or stuck human reaches for. Default help is organized by intent, not frequency:

### Core (the whole default help)
| Section | Verb | Job |
|---------|------|-----|
| Work | `run [DIR]` | Launch a sandbox and enter the agent. The main event. |
| Work | `status` (`st`) | Read-only: what is up, down, next. Also the bare command. |
| Getting started | `setup` | Guided first-run; re-run to fix anything missing. |
| Getting started | `serve` | Start the host services (memory, knowledge). |
| Troubleshooting | `doctor` | Diagnose and print the exact fix commands. **Stays visible.** |
| Meta | `help [verb]` | Teaches; ends with a Next step. |

`serve` is listed but the goal is to make it *not* daily toil: today it is a
foreground process, so "just run it" is friction (see Open decision 2). Keep it
visible until there is a background/service-manager mode.

### Occasional (shown in `help --all` / `help <noun>`)
`memory`, `knowledge`, `config`, `pack`, `backup` (routine data safety, not
expert), and integration setup.

### Rare / expert (hidden from default help; `help --all` only)
`mcp`, `secret`, `restore`, `reset`, `uninstall`, `man`, `version`. `reset` and
`uninstall` stay hidden but `doctor` surfaces them as a last resort when relevant.

## Command tree (target)

**Recommendation: adopt Shape B as the baseline.** All example copy in this doc
uses Shape B spellings (`secret`, `pack`, `state` top-level). Shape A is
recorded below as a possible future, but the review flagged real problems with it
(P1-8), so it is NOT the default and its moves should only be revisited after B
lands and proves out. **Both keep every old verb as an alias.**

### Shape B (recommended baseline) — conservative grouping (~13 nouns)
Keep the six existing nouns as-is. Add exactly one new grouping noun, `state`,
for `backup|restore|reset|uninstall` (these all move the stack's on-disk state).
Fold `man` into `help --man`. `secret` and `pack` stay top-level but drop out
of the default help listing into the expert/occasional tiers. This is the
lowest-risk structural change: it touches the test suite's one real tripwire
(the man-page 1:1 invariant) the least, and it avoids the semantic problems of
Shape A. Note `uninstall` removes installed binaries, not just state, so even
under `state` it keeps a clear standalone description.

### Shape A (deferred / not recommended) — aggressive grouping (8 nouns)
Review objections (P1-8): `mcp secret` forces a day-one user through undefined
jargon (`mcp`, `secret`) to reach credentials; `config pack` buries a runtime
context switch (the active pack affects run, status, memory, and knowledge)
under "config"; `state uninstall` is misleading because uninstall removes
binaries, not state. If pursued later, prefer user-facing concepts (e.g.
`integrations credentials`) over protocol names, and keep `pack` and
`uninstall` where a user expects them.

## Redesigned screens (copy)

### Bare `pix` (status, ends with Next)
```
pix 0.0.16    config: ~/.config/pix/config.toml

Host services
  memory      up    :11435
  knowledge   down  :11436   (enabled; start with `pix serve`)

Provider keys (sbx proxy)
  anthropic ok   openai ok   google ok   github ok

Integrations
  gog (Google Workspace)   account set, needs auth  (run `pix gog setup`)
  slack                    not configured

Sandboxes
  (none running)

Next:  pix serve     start the knowledge service
       pix run       launch a sandbox and start working

Everything ok? run `pix doctor`.   Full command list: `pix help`.
```

### `pix help` (named sections, one-line intent, no flag dump)
```
pix — a personal, multi-model pi coding agent in a Docker sandbox.

Usage:  pix <command> [args]

New here?   pix setup      one-time guided setup (a few minutes, resumable)

Workflow
  run [DIR]        launch the sandbox in DIR (default: .). This is the main one.
  serve            start the host services (memory, knowledge); `serve stop|status`
  status           what is up, what is down, what is next   (also the bare command)

Setup & health
  setup            guided setup: keys, memory, knowledge, integrations
  doctor           diagnose problems and print the exact fix commands

Data
  memory           recall | remember | forget | learnings | stats
  knowledge        init | use | ls | query | sync | remote

More
  config, mcp, state, version, man     (see `pix help --all`)

Learn a command:  pix help run     ·     pix <command> -h
Switch context:   pix pack use <path>   run a different pack (work, personal, ...)
```

### First-run prompt (names what pix is before asking)
```
pix: no config file found.

pix runs a coding agent inside an isolated Docker container called a sandbox.
Setup takes about a minute. It checks your API keys and writes a config file.
You can re-run or resume it anytime, and nothing here is destructive.

Run setup now? [Y/n]:
```
Decline:
```
OK. Run `pix setup` whenever you are ready.
```

### `pix setup` (staged plan, each step labeled, stop anytime)
```
pix setup
Configures the stack for first use. Safe to re-run; only changes what is missing.

I will walk through 4 steps. Only the first is required. You can stop after any
step and re-run `pix setup` later.

--------------------------------------------------------------------------------
Step 1 of 4 — Provider API keys        (required)

The agent needs at least one model provider. Keys are stored on this host as
secrets and injected into every sandbox by the network proxy. They never appear
in a config file or inside the sandbox.

  anthropic  ok set
  openai     ok set
  google     missing  ->  sbx secret set -g google -t "AIza..."
  github     ok set

  You have a working provider (anthropic). That is enough to run. Add the
  missing one above anytime; it is optional.

--------------------------------------------------------------------------------
Step 2 of 4 — Memory & knowledge        (recommended)

Two local host services (they run on your machine, not in the sandbox; the
sandbox reaches them across the host bridge, not its own localhost):
  memory     remembers facts across sessions (on by default)      :11435
  knowledge  full-text search over a folder of docs you point at  :11436

  memory enabled.
  Set up a knowledge base now?
    [Enter]  scaffold a fresh one at ~/.config/pix/knowledge
    <path>   use an existing folder or a git URL
    skip     do it later with `pix knowledge init`
  >

--------------------------------------------------------------------------------
Step 3 of 4 — Integrations              (optional; skip if unsure)

Connect the agent to outside tools: Google Workspace (read-only Gmail, Drive,
Docs, Sheets, Calendar), Slack, and others. `pix run` works without any of
this. Skip if you do not need it yet.

  Google Workspace account (email, or Enter to skip):
  >

--------------------------------------------------------------------------------
Step 4 of 4 — Integration credentials   (only if you added one that needs a password)

Some integrations (Slack, some Google setups) need a password or token. If you
use 1Password (an optional password manager), pix can read the secret from
it at startup so the secret never touches disk. This whole step is optional and
not all integrations need it (for example, gog on macOS can use the system
keychain instead).

  If you use 1Password, it works like this:
  1. You keep the secret in 1Password.
  2. You point pix at it with one line in a file:
         SLACK_TOKEN = op://Private/Slack/token
                       (vault)   (item) (field)
  3. When pix starts that integration, it reads the value from 1Password
     and passes it in as an environment variable. The secret never touches disk
     or the sandbox.

  You added no integrations that need a password. Skipping. (No file is created
  until you actually add one.)
  When you do:  `pix secret set <ENV_VAR> op://vault/item/field`, then
  `pix secret check` to verify, then `pix mcp register` so the
  integration picks up the credentials.

--------------------------------------------------------------------------------
Done. Saved ~/.config/pix/config.toml.

  Configured:  memory
  Needs auth:  gog (Google Workspace) - run `pix gog setup`
  Deferred:    knowledge base, slack

You are NOT fully ready yet: no provider key is set. Set one, then run:
  sbx secret set -g anthropic -t "sk-..."

Next:
  pix serve     start the services you set up
  pix run       launch a sandbox and start working
  pix doctor    re-check everything, anytime
```

(When a provider key IS present and sbx is on PATH, the "NOT fully ready" block
is replaced by a plain "You are ready. Run: pix run".)

Key behavior changes vs today:
- A stated plan up front ("4 steps, only the first is required"). This is the
  single biggest fix to "it just did something."
- Each step labeled required / recommended / optional.
- **1Password is deferred by default and only explained when an integration that
  needs it was actually added.** Today setup unconditionally seeds `op-refs.env`
  and prints the raw `op://vault/item/field` mental model even for the ~80% who
  never touch an integration. Stop doing that. When it IS explained, use the
  three-step plain-language version above, not the raw spec paragraph.
- An explicit "Deferred:" line so the user leaves knowing exactly what they
  skipped and the one command to finish it.
- Mechanics unchanged: same idempotent writes, same `--account`/`--knowledge`/
  `--yes` flags, same `save()`.

### Deferral block (when the user skips the optional tiers)
```
You are ready. Run:  pix run

Optional, set up later when you need them:

  Google Workspace / Slack in the agent
      Lets the agent read your mail, calendar, and Slack. May need a credential
      (1Password or system keychain). When you want it:  pix help mcp

  Team knowledge base
      Give the agent a searchable corpus. Off by default.
      When you want it:  pix help knowledge

  Work vs personal packs
      Switch your whole context (skills, MCP, knowledge, memory scope) with
      one command. You do not need this yet.
      When you want it:  pix help pack

Nothing above is required. `pix run` works right now.
```

### Error messages (what happened + what to do)
```
# unknown command
pix: no command named "memoyr".
Did you mean "memory"?
Run `pix help` to see all commands.

# memory service down
Memory service is not running.
Start it with:  pix serve
(runs in the foreground; use a second terminal or `pix serve &`)

# sbx missing
pix run: `sbx` not found on PATH.
sbx is the Docker Sandboxes CLI pix uses to launch sandboxes.
Install it: https://docs.docker.com/sandboxes/

# secret check with no op-refs.env
No op-refs.env at ~/.config/pix/op-refs.env.
This file maps environment variables to 1Password paths, for integrations that
need credentials (Google Workspace, Slack). You only need it if you use one.
Add a ref:  pix secret set <ENV_VAR> op://vault/item/field
```

(All example copy uses Shape B spellings: `secret`, `pack`, `state`. If Shape
A is ever adopted, regenerate every example against that tree in one pass. The
review flagged a copy inconsistency here as P1-9; fixed by standardizing on B.)

## Implementation plan (phased; test suite is a hard gate)

The launcher has a 232-test suite in `services/host/cmd/pix/`. Tests bind
to internal symbol names (`parseRunArgs`, `buildSbxArgs`, `dispatchMemory`,
`runBackup`, `runReset`, `verbUsage`, `knownVerbs`) — do NOT rename them. The one
structural tripwire is `man_test.go`: it enforces a strict 1:1 between
`knownVerbs` (`help.go`) and the `pix <verb>` synopsis lines in
`pix.1` (regex keys on the first word after `pix `). Treat `knownVerbs`
+ `pix.1` as a single edit unit.

**Phase 0 — copy + man page. Ship first (lowest risk).**
No test asserts the body of `helpText`, the setup prose, or the man page text, so
this is low-risk and fixes the two loudest complaints on its own:
- Rewrite `helpText` into named sections with one-line intent (above).
- Rewrite the `setup` narrative into the staged, labeled plan (above).
- **Rewrite the man page too (review P1-6).** The owner called the man page
  useless; hiding it is not fixing it. Add a Quick Start (the golden path),
  a Prerequisites section (sbx + at least one provider key), the setup
  narrative, and the credential model. If the man page will not be maintained,
  drop the feature instead of shipping a stale reference.
- Add `Next:` footers to the interactive human outputs (status, setup, doctor,
  errors). **Do NOT append Next: to `--json`, `serve`, or `version`** (review
  P2-13): it would corrupt machine output and there is nothing meaningful to
  append to a streaming/one-shot command.
- Sharpen the unknown-command error ("did you mean ...").

**Phase 0.5 — the not-actually-copy behavior changes (needs tests).**
The review (P0-2) correctly flags that some "copy" items are behavior changes
with existing tests bound to the old behavior. Split these out, each with tests:
- **Conditional op-refs seeding.** Today setup unconditionally seeds
  `op-refs.env`; `setup_test.go` asserts that. Gating it on "an integration that
  needs a credential was added" changes behavior and the test. Do it deliberately
  with an updated test, not as a silent copy edit.
- **Do not print "ready" without a working provider + sbx** (P0-1). Add a
  readiness check; test both the ready and not-ready summaries.
- **Do not register a credential-required integration until refs resolve** (P0-3):
  today setup registers gog/slack even with an empty `op-refs.env`, so the server
  starts without its token and filling the file later does not re-register it.
  Either defer registration until `secret check` passes, or auto re-register
  after it. The recovery path must include `pix mcp register`.
- **gog is "needs auth", not "configured", until a real auth probe passes** (P0-4):
  setting an email does not complete OAuth. Detect usable auth; otherwise label
  it deferred and print the `pix gog setup` next step, the one guided
  path (**shipped**: see CHANGELOG).
- `help --all` branch (P0-2): **shipped** — tiered help shows Core by default and
  reveals the rest with `--all` (and per-noun `help <verb>`).

**Phase 1 — additive grouping (aliases, no removals).**
- Shape B minimum: add `state.go` with `runState` delegating verbatim to the
  existing `runBackup/runRestore/runReset/runUninstall`; add the `state` switch
  case, `knownVerbs["state"]`, `verbUsage` case, `stateUsage` const, and one
  `.SS state` man block. Keep the four flat verbs as live aliases (and their man
  synopsis lines). Fold `man` into `help --man` (the `--man` global already
  exists). Add `state_test.go`.
- Shape A is deferred / not recommended (see command tree section).
- Verify from the module root: `cd services/host && go test ./...` (the Go module
  starts at `services/host/go.mod`, so a repo-root path is not reliable; P2-11).
  The legacy-spelling regression test must exercise destructive aliases
  (`backup`/`reset`/`uninstall`) through the parser/dispatch seam or `--help`,
  NOT by actually running them (review P2-12), and assert usage/exit-code parity.

**Phase 2 — optional soft deprecation (later).**
- On an aliased flat verb, print a one-line stderr nudge and continue. Drop the
  aliases from the help *listing* only (still dispatchable). No hard removal.

## Open decision for the owner

1. **Shape A vs B** — how aggressively to group. Recommendation: ship Phase 0
   immediately, then do Shape B (add only `state`). Shape A is not recommended
   (review P1-8: `mcp secret` fronts jargon, `config pack` hides a runtime
   switch, `state uninstall` is misleading).
2. **Does `setup` start `serve` on yes, or just print the command?**
   Recommendation (revised by review P0-5): **just print the command.** `serve`
   is a foreground process today, so auto-starting it from `setup` would block
   and never return to `pix run`. Auto-start only becomes viable once there
   is a real background/service-manager mode for `serve`.
3. **Keep `man`?** Recommendation: keep it AND rewrite it in Phase 0 (a stale man
   page is the actual complaint). Fold the invocation into `help --man`; keep
   `man` as a hidden alias.
4. **serve `start` verb?** The examples must not advertise `serve start`: only
   `serve stop` and `serve status` are launcher verbs; bare `serve` starts, and a
   `start` token is currently passed through as a service name and fails
   (review P1-10). Either add a real `start` alias or never print it.

## Success metrics

- Time-to-first-run: median under 5 min, install to agent prompt, unfamiliar user.
- Zero-doc completion: >=80% reach `run` without opening docs or touching a
  deferred verb.
- Default help fits one 80x24 screen.
- Zero undefined jargon (op-refs, MCP, OKF, stdio) in the day-0 setup + run path.
- Every *interactive human* output ends with a Next: line (never `--json`,
  `serve`, or `version`); every error says what + how-to-fix.
- Back-compat: all legacy verb spellings still work (regression test; destructive
  ones tested through the dispatch seam, not by running them).

## Review status

Cross-vendor `review` subagent ran an adversarial pass and returned BLOCK on the
first draft. All P0 findings (false golden path, mislabeled Phase 0 risk, orphaned
MCP registration, premature gog "configured", serve auto-start deadlock) and the
P1/P2 findings (tier naming, Shape A semantics, Shape A/B copy mix, localhost
falsehood, `serve start`, man-page dodge, wrong test path, Next-on-json) are
folded into this revision. Re-review recommended before implementation begins.
