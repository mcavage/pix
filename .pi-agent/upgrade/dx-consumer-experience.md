# pi-stack: end-to-end consumer developer experience

Design for a repo-less consumer of pi-stack. North stars: Hykes (make the right
thing easy, one composable surface, Unix verbs) and Hejlsberg (guessable names,
progressive disclosure, pit of success). The consumer never clones the repo.

---

## 0. The core problem, stated plainly

Today there are two products wearing one name:

- The **sandbox** is already repo-less: `sbx run pi-stack --kit git+...`.
- The **host side** needs a clone: `bin/pi-stack` is repo-relative (it resolves
  `ROOT` from its own path and points `--kit` at `$ROOT/pi-kit`), and ~9 Make
  targets (`serve`, `run`, `mcp-register`, `pull-models`, `doctor`, `install`,
  `mcp-auth`, `load`, `publish`) plus `config/local.mk` + `config/op-refs.env`
  all live inside the checkout.

A consumer who runs the README's `sbx run` line gets an agent but no `doctor`, no
`serve` (so no memory, no gws), no `setup`. To get those they must clone, which
contradicts "repo-less". The prior review was right that `bin/pi-stack` cannot
just be renamed into a prebuilt binary — it hard-depends on the tree.

**Resolution: one binary, repo-optional.** Ship a single self-contained
`pi-stack` launcher plus the already-compiled `pi-stack-host`. Both install to
`~/.local/bin`, read config from `~/.config/pi-stack/`, and shell out to `sbx`.
Neither needs a checkout. The consumer path (`--kit git+...`) is the default.
`--dev` is a *flag* that layers repo-relative behavior **only when a checkout is
present**, failing loud otherwise. The Makefile stays, but demotes to a
contributor convenience that calls the same binary — it is no longer the user
surface. There is exactly one product and one verb tree; `--dev` is the single
seam between "I use pi-stack" and "I hack on pi-stack".

---

## 1. CLI surface — the full verb tree

The binary is named **`pi-stack`** (the launcher) with a co-installed
**`pi-stack-host`** (the Go services binary, an implementation detail the verbs
drive — the user never types it directly). Names are guessable nouns/verbs; the
bare invocation does the most common thing (launch).

```
pi-stack [DIR]                       Launch a sandbox in DIR (default: cwd). Bare = the 90% path.
pi-stack run [DIR] [flags] [-- ...]  Explicit launch. Flags below; everything after -- goes to pi.
    --dev                            Mode B: mount host skill trees + load them live (checkout only).
    --skills DIR                     Live-mount a skills dir (repeatable). Consumer hot-iterate. §4.
    --kit PATH|git+URL               Add a mixin kit (repeatable). Composes with --skills.
    --mcp NAME                       Attach a registered local stdio MCP server (repeatable).
    --name NAME                      Sandbox name (default derived from DIR).
    --model PROVIDER/ID              Convenience passthrough to pi.

pi-stack setup                       Resumable, idempotent onboarding wizard. §2.
pi-stack doctor                      Health: providers, models, host services, MCP. Copy-paste TODOs.
pi-stack serve [SERVICE...]          Run host services in the foreground (default: from config). Ctrl-C stops.
pi-stack version                     Launcher version + the image tag it will boot.
pi-stack help [VERB]                 Verb help. `--help` works on every verb too.

pi-stack models pull                 Pull the Ollama watcher + embed models the memory loop needs.
pi-stack models ls                   Show required vs pulled models.

pi-stack mcp register [NAME...]      Register local stdio MCP servers (default: from config) with sbx.
pi-stack mcp ls                      List registered servers + which are attached.
pi-stack mcp auth                    (Re)authorize remote OAuth MCP servers (opens a browser per server).

pi-stack config show                 Print the resolved config + its source path.
pi-stack config edit                 Open ~/.config/pi-stack/config.toml in $EDITOR.
pi-stack config path                 Print the config file path (for scripting).

pi-stack secrets                     Show/store provider + github secrets (wraps `sbx secret`).

pi-stack upgrade                     Self-update the binary, checksum-pinned, no sudo. §5.
pi-stack uninstall                   Remove the binaries; keep config unless --purge. §5.
```

Design rationale (Hejlsberg progressive disclosure): the first block is the
daily surface (`run/setup/doctor/serve/version/help`). The grouped `models`,
`mcp`, `config` verbs are the "I'm wiring a data tool" surface — you meet them
only when `doctor` sends you there. `upgrade/uninstall/secrets` are lifecycle,
rarely touched. No flag is required to succeed at the bare `pi-stack`.

**Mapping from today's Make targets (nothing is lost, everything is renamed to a guessable verb):**

| today | becomes | notes |
| --- | --- | --- |
| `bin/pi-stack` / `make run` | `pi-stack` / `pi-stack run [--dev]` | `--dev` == old Mode B; bare == consumer Mode A |
| `make serve` | `pi-stack serve` | reads `SERVICES` from config.toml, not local.mk |
| `make doctor` | `pi-stack doctor` | same copy-paste TODO output, repo-less |
| `make pull-models` | `pi-stack models pull` | |
| `make mcp-register` | `pi-stack mcp register` | |
| `make mcp-auth` | `pi-stack mcp auth` | |
| `make secrets` | `pi-stack secrets` | |
| `make install` | install script + `pi-stack upgrade` | §5 |
| `make load`/`publish` | stay in the Makefile | contributor-only; never a consumer verb |

`make run` etc. become one-line wrappers: `make run: ; pi-stack run --dev .`.
Contributors get muscle memory continuity; consumers never see Make.

**The `--dev` seam (the concrete answer to the split).** `pi-stack --dev`:
1. Locates a checkout: cwd is inside a pi-stack repo, or `PI_STACK_DEV_ROOT` is
   set, or `config.toml [dev].root` is set. If none → hard error:
   `--dev needs a pi-stack checkout; clone it and run from there, or set PI_STACK_DEV_ROOT`.
2. From that root, does exactly what `bin/pi-stack --dev` does now: mounts
   `$ROOT/skills` (+ overlay skills) writable and passes `--no-skills --skill ...`.

So the repo-relative logic lives behind one flag, gated on a checkout actually
existing, and is invisible to the consumer. One binary, two modes, no second
product.

---

## 2. `pi-stack setup` — the resumable onboarding wizard

Goal: from "installed the binary" to "agent that can ship code and recall memory"
in one command you can re-run any time. Every step is a **check → report →
(prompt only if missing) → confirm** loop. Nothing is clobbered; re-running is a
no-op where already done. It reuses `doctor`'s copy-pasteable `TODO: <cmd>`
grammar so the two tools speak the same language.

State model: setup owns no state of its own. It derives everything from the
world (`sbx secret ls`, `ollama list`, `sbx mcp ls`, port probes) and from
`~/.config/pi-stack/config.toml`, so it is idempotent by construction and safe
to Ctrl-C and resume. It writes only two files, both no-clobber: `config.toml`
(created from a template if missing, otherwise merged key-by-key) and, if you opt
into MCP, `~/.config/pi-stack/op-refs.env` (from `.example`, never overwritten).

Flow (each gate prints a header, a state line, and either a green check or a
copy-paste TODO; it advances automatically when the gate is already satisfied):

```
pi-stack setup

pi-stack setup — 6 gates. Ctrl-C any time; re-run to resume. Nothing is overwritten.

[1/6] Provider keys (required)
  These live in sbx and never enter the VM. Checking `sbx secret ls`...
    anthropic  ✓ set
    openai     ✓ set
    google     TODO: sbx secret set -g google      # paste your Gemini key
    github     TODO: gh auth token | sbx secret set -g github
  → 2 set, 2 missing. Set the two above, then press Enter to re-check (or 's' to skip).

[2/6] Local models + memory (optional — recall + fact capture)
  Ollama: ✓ installed, :11434 up
    watcher (gemma4)          TODO: pi-stack models pull
    embed   (nomic-embed-text) TODO: pi-stack models pull
  → Run `pi-stack models pull` (~2 min) to enable capture + semantic recall.
    Skip and recall falls back to keyword-only, capture is off. [p]ull / [s]kip

[3/6] Google Workspace (optional — gworkspace capability)
  gws CLI: ✓ installed
  Auth:    TODO: gws auth login        # opens a browser once
  → [a]uth now / [s]kip

[4/6] 1Password + credential-brokered MCP (optional — chat/Slack + overlay connectors)
  op CLI:  ✓ installed and signed in
  op-refs: TODO: created ~/.config/pi-stack/op-refs.env from template — fill in your op:// refs, then re-run
  → Edit the refs above (each line is `KEY=op://vault/item/field`), then [r]e-check / [s]kip

[5/6] MCP registration (only the servers you enabled in step 4)
  Enabled in config: slack
    slack   TODO: pi-stack mcp register     # registers `op run ... pi-stack-host slack` with the sbx gateway
  → [r]egister now / [s]kip

[6/6] Write config
  SERVICES = memory gws         (what `pi-stack serve` runs)
  MCP      = slack              (what `pi-stack run` attaches)
  models   = gemma4, nomic-embed-text
  → Wrote ~/.config/pi-stack/config.toml (merged; your edits preserved).

Setup complete. 4/6 gates green, 2 skipped (gws, mcp — optional).
Next:
  pi-stack serve        # start host services (memory, gws) — leave running in a tab
  pi-stack              # launch the agent in this directory
  In the agent, run /getting-started for a 60-second tour.
```

Rules that make it feel like a pit of success:
- **Required vs optional is explicit at every gate.** Only step 1 blocks
  "complete"; everything else can be skipped and the summary says what you lose.
- **Never prompts for something already done.** A fully-set-up user runs
  `setup` and sees six green checks and the "Next" block — it doubles as a
  re-orientation command.
- **Every TODO is a literal command** you can paste, same as `doctor`. `setup`
  and `doctor` share one renderer for the state lines.
- **No hidden mutation.** It tells you the file it wrote and that your edits were
  preserved.

---

## 3. `/help` + `/getting-started` — in-agent discoverability (extension)

This directly fixes the user's stated pain: forgetting the names of their own
skills and agents. The fix lives **inside the agent**, as `extensions/help.ts`,
registering two commands alongside the existing `/status`, `/subagents`,
`/recall`, `/remember`, `/forget`, `/learnings`, `/timestamps`.

### Why an extension, not a skill or a startup banner

- **A skill** is model-invoked, burns context tokens, and the model *paraphrases*
  it — so it drifts, can hallucinate a skill that was removed, and can't be
  triggered deterministically. Discoverability must be exact and free.
- **A startup banner** is static. pi-stack loads skills *live* (Mode B) and a
  consumer can `--skills DIR` their own in, so a hardcoded banner would lie the
  moment the loaded set changes — the exact staleness the user is complaining
  about.
- **An extension** is a deterministic slash command that reads the **live loaded
  set** at call time via `pi.getCommands()`, costs zero tokens, always matches
  reality, and shows up in the command palette next to `/status`. It matches the
  established pattern (`status.ts`, `subagents.ts`). This is the only option that
  can't go stale.

### `/help` — the live map of everything loaded

Reads live, every invocation, so it is never stale:

- **Skills** — `pi.getCommands().filter(c => c.source === "skill")`, printing
  `name — description`. (Descriptions come straight from the loaded SKILL.md
  frontmatter, so `--skills` mounts and overlay skills appear automatically.)
- **Commands** — `source === "extension"` for the pi-stack commands, plus a
  short hardcoded line for the built-in interactive ones `getCommands()`
  deliberately omits (`/model`, `/reload`, `/settings`, `/resume`) with a note
  that they're pi built-ins.
- **Prompt templates** — `source === "prompt"`.
- **Agents (subagent roles)** — read `getAgentDir()/agents/*.md` (+ project
  `.pi/agents`) and parse frontmatter the same way `subagents.ts` already does
  (`name`, `description`, `model`), printing `name (model) — role`. This is the
  list the user forgets most (`architect`, `review`, `deep`, `fanout`,
  `security-lead`, ...). Reuse the parser; do not duplicate the roster.
- **Keybindings** — the notable ones: `Alt+P` cycle model, `Ctrl+Alt+S` status,
  plus any shortcuts registered by loaded extensions (read from the same source
  those register against). Keep it to the handful that matter, not the full
  editor map.
- **Capability map** — read `~/.pi/agent/capabilities.json` and print
  `capability → provider` (e.g. `chat → mcp:slack`, `docs → none`), so a user
  can see at a glance what data is actually wired vs degraded.

Output is grouped, compact, single-screen where possible, via
`ctx.ui.notify(...)` (no LLM turn). Optional argument filter:
`/help agents`, `/help skills`, `/help capabilities` narrows to one section
(register with `getArgumentCompletions` so the section names autocomplete —
discoverability for the discoverability command).

Sketch (defensive, matching the house style — every pi touch guarded, must never
break startup):

```ts
export default function (pi: any) {
  const safe = <T>(fn: () => T): T | undefined => { try { return fn(); } catch { return undefined; } };

  const render = (ctx: any, section?: string) => {
    const cmds = safe(() => pi.getCommands()) ?? [];
    const skills = cmds.filter((c: any) => c.source === "skill");
    const exts   = cmds.filter((c: any) => c.source === "extension");
    const prompts= cmds.filter((c: any) => c.source === "prompt");
    const agents = readAgents();          // reuse subagents.ts frontmatter parse
    const caps   = readCapabilities();     // parse ~/.pi/agent/capabilities.json
    const L: string[] = [];
    const want = (s: string) => !section || section === s;
    if (want("skills"))   { L.push("Skills:");   for (const s of skills) L.push(`  /${s.name} — ${s.description ?? ""}`); }
    if (want("commands")) { L.push("Commands:"); for (const c of exts) L.push(`  /${c.name} — ${c.description ?? ""}`);
                            L.push("  (built-ins: /model /reload /settings /resume)"); }
    if (want("agents"))   { L.push("Agents (subagent roles):"); for (const a of agents) L.push(`  ${a.name} (${a.model ?? "?"}) — ${a.description}`); }
    if (want("prompts") && prompts.length) { L.push("Prompt templates:"); for (const p of prompts) L.push(`  /${p.name}`); }
    if (want("capabilities")) { L.push("Capabilities:"); for (const [k, v] of caps) L.push(`  ${k} → ${v}`); }
    if (want("keys"))     { L.push("Keys: Alt+P cycle model · Ctrl+Alt+S status · Esc cancel turn"); }
    ctx.ui.notify(L.join("\n"), "info");
  };

  safe(() => pi.registerCommand("help", {
    description: "List everything loaded: skills, agents, commands, keys, capability map",
    getArgumentCompletions: (p: string) =>
      ["skills","commands","agents","prompts","capabilities","keys"]
        .filter(x => x.startsWith(p)).map(x => ({ value: x, label: x })) || null,
    handler: async (args: string, ctx: any) => render(ctx, args?.trim() || undefined),
  }));
}
```

### `/getting-started` — the guided first-run tour

A short, deterministic, interactive-feeling walkthrough (also `ctx.ui.notify`,
no LLM), written as concrete next actions, not prose:

```
Welcome to pi-stack. You're in a throwaway sandbox — it runs full-auto, no prompts.

1. Do a real task. Try:  "fix the failing test in X and open a PR"  → the `ship` skill runs it end to end.
2. Switch models:  Alt+P cycles Claude / GPT / Gemini / local. Reviews use a DIFFERENT vendor on purpose.
3. Delegate:  ask for a "cross-vendor review" and the `review` agent argues against your diff.
4. Memory:  I remember preferences across sessions. `/recall` to see, `/remember` to pin, `/forget` to drop.
5. Find anything:  `/help` lists every skill, agent, command, and what data is wired.

Now pick one: run `ship`, run `investigate` on a bug, or just describe what you're working on.
```

Kept to one screen, all commands real. It ends by handing control back with a
concrete choice — the setup-user skill's "run a first useful task" instinct,
but instant and token-free.

### One-time first-turn nudge

On `session_start` (a pure event, never `sendMessage` — that would trigger an LLM
turn and, on a reasoning model, the assistant-prefill 400 the codebase already
documents), check for a marker file `~/.pi/agent/.pi-stack-welcomed`. If absent,
`ctx.ui.notify("New here? /getting-started for a 60-second tour · /help lists everything", "info")`
and write the marker. One line, once per machine, zero tokens, dismissible by
ignoring it. Never repeats. (Guard the fs writes with `safe()`; a nudge that
can't break the agent.)

---

## 4. Consumer skill bind-mount + hot iterate

The "super common pattern": *point pi-stack at my own skills dir and edit them
live, without cloning pi-stack or learning Mode B.*

**Mechanism: `pi-stack run --skills DIR` (repeatable) + `[skills].mount` in config.**

```
pi-stack --skills ~/work/agent-skills           # one run
# or, persistent, in ~/.config/pi-stack/config.toml:
[skills]
mount = ["~/work/agent-skills", "~/dotfiles/skills"]
```

What it does, concretely:
1. Adds each DIR as a **writable extra workspace** to the `sbx run` (so edits
   inside the agent persist to the host dir — the same trick `make run` uses for
   the overlay's `kit/`).
2. Passes `--skill DIR` to pi for each — **without** `--no-skills`, so the baked
   ~35 skills stay and your dirs load **on top**. (Contrast with `--dev`, which
   uses `--no-skills` because a contributor is replacing the baked set with the
   repo's.)
3. Live edit loop: change a `SKILL.md` in `~/work/agent-skills`, `/reload` in
   pi, and it's live. No image rebuild, no kit repack, no pi-stack checkout.

**Composition with published mixin kits (the precedence a user can predict):**

```
baked image skills   (lowest — the shipped ~35)
  < --kit mixin kits  (published/versioned, ride every run, read-only)
  < --skills DIR      (highest — live, writable, shadows same-named skills)
```

So a consumer *ships* a stable skill set as a mixin kit (`--kit ./my-kit` or
`--kit git+...`, versioned, reproducible) and *iterates* on new ones with
`--skills` (live, throwaway) — then graduates a matured skill from the `--skills`
dir into the kit. That is the Hykes "make the right thing easy" path: the fast
loop and the durable loop are the same shape, one flag apart.

(Assumption to verify against pi: that a later `--skill` root with a
same-named SKILL.md wins over a baked one. If pi errors on duplicate names
instead of shadowing, `--skills` should imply `--no-skills` only for the
overlapping names, or document "don't reuse a baked skill's name". Flagging so
it's tested, not assumed.)

---

## 5. Install / upgrade / uninstall lifecycle

Prebuilt, no sudo, checksum-pinned, no-clobber config.

**Install** (one line, the README's new first step):

```
curl -fsSL https://pi-stack.dev/install.sh | sh
```

The script:
- Detects OS/arch, downloads `pi-stack` + `pi-stack-host` for that platform from
  a versioned GitHub release, **verifies against a published SHA-256 checksum
  file** (fail closed if it doesn't match), and installs both to `~/.local/bin`
  (no sudo). Prints a PATH hint if `~/.local/bin` isn't on `PATH`.
- Creates `~/.config/pi-stack/config.toml` from a template **only if missing**
  (never clobbers an existing one), and `op-refs.env` likewise.
- Ends with: `Installed pi-stack <version>. Next: pi-stack setup`.
- Pins the version it fetched into config, so `upgrade` knows the baseline.

Config lives at `~/.config/pi-stack/` (XDG; honors `XDG_CONFIG_HOME`), **not** in
a checkout. This is the single most important structural change: it's what makes
the binary repo-less and makes config survive reinstalls/upgrades untouched.

**Upgrade** (`pi-stack upgrade`):
- Queries the latest release, compares to the running version, and if newer,
  downloads + checksum-verifies + atomically replaces the two binaries in place
  (download to temp, verify, `rename`). No sudo (same user-owned `~/.local/bin`).
- **Touches nothing else** — config, secrets, op-refs, and the sbx image are all
  independent. Prints the version delta and a one-line changelog link.
- `pi-stack upgrade --check` reports without changing anything (for cron/CI).
- Note: the *image* is versioned separately by the kit's pinned tag; `upgrade`
  bumps the launcher, and `doctor` reports if the launcher expects a newer image
  than the kit pins, with the `sbx run` line to refresh it.

**Uninstall** (`pi-stack uninstall`):
- Removes `~/.local/bin/pi-stack` and `~/.local/bin/pi-stack-host`.
- **Keeps `~/.config/pi-stack/` by default** (your secrets refs and config are
  yours). Prints exactly what it left behind and the `--purge` flag to also
  remove config. Never touches `sbx secret`s or the sbx image (those aren't ours
  to delete).
- Idempotent: running it twice is a clean no-op with a clear message.

---

## Top 5 DX wins, ranked by impact at first contact

1. **One repo-less `pi-stack` binary with a guessable verb tree.** Kills the
   "clone the repo + learn 9 Make targets + edit local.mk" cliff. `--dev` is the
   single, loud-failing seam that keeps the contributor path alive without
   spawning a second product. This is the change everything else hangs off.

2. **`/help` + `/getting-started` in-agent, live from `pi.getCommands()`.**
   Directly fixes the user's stated pain (forgetting their own skills/agents),
   and because it reads the loaded set at call time it can *never* go stale — even
   with `--skills` mounts or overlay skills. Zero tokens, deterministic,
   discoverable next to `/status`.

3. **`pi-stack setup` — one resumable, idempotent wizard.** Collapses scattered
   secret/Ollama/gws/op/MCP setup into a single guided flow that only prompts for
   what's missing, reuses `doctor`'s copy-paste `TODO:` grammar, and doubles as a
   re-orientation command for an already-configured user. Turns "read the README
   five times" into "run one command and paste four lines".

4. **Config moves to XDG `~/.config/pi-stack/config.toml`.** The structural
   enabler for repo-lessness: config survives install/upgrade/uninstall, is never
   clobbered, and is edited via `pi-stack config edit` instead of hunting for a
   file inside a clone. Small change, unlocks 1 and 5.

5. **`--skills DIR` live-mount for consumers.** The "point at my skills and edit
   live" pattern without needing the pi-stack checkout or Mode B. It composes
   predictably with published mixin kits (baked < kit < live mount), so the fast
   iteration loop and the durable ship loop are one flag apart — the natural
   graduation path from prototype skill to versioned kit.
