# Host mode — running pi-stack outside the sandbox

Status: Phase 1 IMPLEMENTED, gated off by default (`pi-stack config set
host.enabled true`). The Go launch path (`pi-stack host [DIR]` + `pi-stack host
setup`, hostrun.go/hostargs.go) is built: default-off gate, host agent dir at
$XDG_STATE_HOME/pi-stack/host-agent, EvalSymlinks workspace refusals
($HOME///etc/secret dirs — secret dirs canonicalized too, and nested entries
like .config/gcloud caught when the workspace sits at $HOME/.config), subagents
disabled (PI_SUBAGENT_DISABLED=1, strict "1" match, enforced centrally in
runSingle so the doctor canary can't spawn either + PI_SUBAGENT_MAX_DEPTH=0,
which now honors an explicit zero), passthrough flags that would displace the
guard (--no-extensions/--extensions/-e/--extension) refused at parse,
host-specific system preamble (launch refuses if it can't be written), op://
just-in-time credentials via hostmode.env (Ollama-only without it), red stderr
banner, and man/help wiring. `host setup` also symlinks capabilities.json
(capability-routing reads $PI_CODING_AGENT_DIR/capabilities.json),
routing.json (subagents intent→model), and keybindings.json into the host
agent dir; mcp.json is deliberately NOT linked — it registers the sbx Cloud
MCP Gateway, which only exists inside a sandbox. MEMORY_URL/KNOWLEDGE_URL
honor MEMORY_PORT/KNOWLEDGE_PORT, and the configured ollama_bridge_model is
exported as OLLAMA_BRIDGE_MODEL. The TS half (host-guard.ts, ollama-bridge OLLAMA_HOSTMODE
bypass, status HOST badge, subagents.ts PI_SUBAGENT_DISABLED guard) tracks
separately — the launch REFUSES until extensions/host-guard.ts exists. Phase 2
hardening remains open.
Owner: TBD
Crew: architect · security-lead · engineer · dx-consultant (+ cross-vendor review)

## The ask

> "It would help a lot to have a way to run pi-stack outside the sandbox, for the
> specific times I need host things. One case is platformio for serial port,
> another is to literally work on pi-stack itself."

Two concrete needs, both fundamentally host operations a network-fenced Docker
sandbox cannot serve:

1. **Self-development (the primary case)** — you can't `make load` / `sbx` /
   rebuild the pi-stack image from inside a pi-stack sandbox (the VM has no host
   Docker or `sbx`). Working on pi-stack itself is inherently a host operation.
2. **Hardware access (secondary, likely a different solution)** — platformio
   flashing/monitoring needs a real `/dev/tty*` USB-serial device the sandbox
   can't see. On reflection this wants a *narrow* capability, not full host
   access, so it is better served by a constrained host-side serial MCP that
   keeps the agent sandboxed (see Alternatives). This doc focuses on self-dev;
   the platformio case is noted but deferred.

## Recommendation

Add a **new sibling verb `pi-stack host [DIR]`** that execs the host-installed
`pi` directly (no `sbx`, no VM), reusing the launcher's config/profile/knowledge
/memory-scope machinery but with its own arg builder and a **fundamentally
different safety posture**: credentials sourced just-in-time and never persisted,
a best-effort `tool_call` guard extension, subagents disabled until they can
carry that guard, and a session that is visually unmistakable as unsandboxed.
Crucially — pi has no built-in prompts or sandbox, so this posture is a set of
guardrails against accidents, **not** a security boundary (see Safety posture).

Frame it as a **narrow, deliberate escape hatch for exactly the two cases above**
— never the default, never a fallback when `sbx` is missing, never claiming
parity with the sandbox. The moment it reads as "a second runtime," people reach
for it by default, which defeats the entire safety model pi-stack is built on.

This was the unanimous conclusion across all four lenses. The disagreements were
narrow (see "Open decisions").

## Why this shape (and not the alternatives)

- **New verb, not `run --host` / `--no-sandbox`.** `run`'s entire identity is
  "boot the disposable, network-fenced, proxy-credentialed VM" — every safety
  invariant is welded to it (`run.go` header, `buildSbxArgs`,
  `modelProviderPreflight`). A `--no-sandbox` flag is a double-negative on the
  safe verb that surfaces in tab-completion right next to muscle-memory
  `pi-stack run`, and inverts credentials + autonomy + boundary all at once with
  no signal. A different safety model deserves a different verb. (Naming caveat:
  `pi-stack host` is the agent-on-host; `pi-stack-host` is the Go daemon binary
  and `pi-stack serve` runs host services — call out the distinction in `man`.)
- **Not a second entrypoint binary.** Duplicates config loading and breaks the
  "one launcher binary" story (`main.go`).
- **Preserves HOST=Go / SANDBOX=TS.** The launcher stays one Go binary (`host` is
  another verb). Extensions/skills/agents stay TS/markdown — they just run under
  the host's `pi` instead of the VM's. The convention's rationale (a *daemon* that
  spawns children from network input is EDR-flagged) is about host *services*;
  host-mode pi is a user-launched interactive process, so it doesn't offend it.

## What it execs

pi is a plain npm package — `Dockerfile` installs it with a vanilla
`npm install -g @earendil-works/pi-coding-agent@<PI_PACKAGE>` and nothing about
it depends on sbx or Docker. On the host it's the same install. Setup and launch enforce the **same version**
as the Dockerfile `ARG PI_PACKAGE`; a missing or stale copy is rejected before
extensions load, with the exact pinned install command. Launch also requires the
curated-extension lock marker written by `pi-stack host setup`, so upgrading the
core alone cannot load an extension set pinned for an older release.

The clean lever the engineer found: **`PI_CODING_AGENT_DIR`** (pi's config-dir
env, default `~/.pi/agent`). The repo root layout is identical to `~/.pi/agent`
(`agents/ extensions/ skills/ prompts/ themes/` + `settings.json`
`keybindings.json` `mcp.json` `capabilities.json` — exactly the files the
Dockerfile copies into `/home/agent/.pi/agent/`). So the whole harness loads live
from the checkout by pointing one env var at it. This is cleaner than mirroring
the Mode-B `--skill`/`-e` flags in `sbxargs.go`, because **agent presets have no
CLI flag** — `subagents.ts` resolves them via `getAgentDir()`, which reads the
config dir; so live agents require the config-dir lever regardless.

**Caveat (must handle):** pointing `PI_CODING_AGENT_DIR` straight at the checkout
would (a) miss the npm-installed extension packages that live in the *config
dir's* `npm/node_modules` and are listed in its mutated `settings.json` (the
repo's `settings.json` is the minimal 4-key file), and (b) let pi write
`sessions/`, `auth.json` into the tree. So provision a **dedicated host config
dir** that symlinks `skills/extensions/agents/prompts/themes` → repo and holds
its own `npm/node_modules` (the curated packages installed once, mirroring the
Dockerfile RUN loop), with `--session-dir` pointed away from the checkout.

**Where that dir lives:** under the existing pi-stack **state** home, not a new
dotdir. pi-stack already uses three XDG homes — `~/.config/pi-stack` (config),
`$XDG_DATA_HOME/pi-stack` (default `~/.local/share/pi-stack`, precious *data*:
memory + knowledge DBs, what `pi-stack backup` archives), and
`$XDG_STATE_HOME/pi-stack` (default `~/.local/state/pi-stack`,
rebuildable *state*: per-task clones under `tasks/`, `task.go`). The host-agent
dir is exactly state-flavored — fully rebuildable (symlinks + `pi install`), never
precious — so it belongs beside `tasks/`:

```
$XDG_STATE_HOME/pi-stack/            (default ~/.local/state/pi-stack)
├── tasks/                            # existing: per-task clones
└── host-agent/                       # PI_CODING_AGENT_DIR for host mode
    ├── {skills,agents,extensions,prompts,themes} -> <checkout>/...
    ├── settings.json                 # host-specific (guard on, own trust)
    ├── npm/node_modules/             # natively-installed pi extension pkgs
    └── sessions/                     # --session-dir, out of the git tree
```

Putting it here (not the data home) keeps it out of `pi-stack backup` and lets
`pi-stack state reset` nuke it freely — it's disposable, unlike memory. Honor
`XDG_STATE_HOME` the same way `task.go` does.

Resulting invocation (illustrative — **not** copy-paste ready; the ollama line
self-loops without the bypass, see the extensions table):

```
PI_CODING_AGENT_DIR=~/.local/state/pi-stack/host-agent \
MEMORY_URL=http://127.0.0.1:11435 \
KNOWLEDGE_URL=http://127.0.0.1:11436 \
<ollama: real :11434 provider only after the host-mode bypass ships> \
<credentials sourced just-in-time, see below> \
pi --session-dir <dir> --models <cycle> [--model <o.Model>] [-- passthrough]
```

**A host config dir is not a full clone of the sandbox.** Beyond the five
symlinked harness dirs, the image also copies `settings.json`/`keybindings.json`
/`mcp.json`/`capabilities.json` (`Dockerfile`), installs the curated npm
extension packages, and — critically — carries a kit `agentContext`
(`pi-kit/spec.yaml`) that tells the agent it is in a disposable, network-fenced,
full-auto VM. Copying that context onto the host is actively dangerous (it would
assert isolation that no longer exists). Host mode needs its **own** system-prompt
/ AGENTS preamble: real machine, not disposable, destructive commands hit real
files, no network fence, autonomy is the user's responsibility. Also note the
`capability-routing` skill currently hardcodes `~/.pi/agent/capabilities.json`
rather than honoring `PI_CODING_AGENT_DIR`, so it would read stale host-global
config unless fixed — a Phase-1 item, not an afterthought.

## Extensions off-proxy: what works, what needs a look

Nothing hard-breaks — the proxy-specific plumbing was uniformly written
env-overridable and best-effort.

| Extension | Host behavior | Action |
|---|---|---|
| `memory-recall.ts` / `memory-capture.ts` | default `host.docker.internal:11435` (doesn't resolve on host); every call is `safe()`-wrapped → silently skips | set `MEMORY_URL=http://127.0.0.1:11435`; the service already runs natively there under `pi-stack serve`. Works, no code change. |
| `knowledge-recall.ts` | default `host.docker.internal:11436`; reads `KNOWLEDGE_SCOPE` (already written by `wireKnowledgeScope`) | set `KNOWLEDGE_URL=http://127.0.0.1:11436`. Works, no code change. |
| `ollama-bridge.ts` | its job is the proxy-dodge (listen `:11434` → forward to `host.docker.internal:11434`); on host that's redundant | **Needs a real fix, and the bypass MUST ship before the invocation above is documented.** Review confirmed: `OLLAMA_BRIDGE_HOST=127.0.0.1` is a genuine self-loop — the bridge listens on `127.0.0.1:11434` and forwards to the same host:port, so if ollama is *absent* the bridge binds successfully and recursively proxies to itself until exhaustion. EADDRINUSE is harmless only when real ollama already owns the port (not guaranteed). Fix: a host-mode bypass that registers the provider pointed straight at `http://127.0.0.1:11434/v1` and skips the reverse proxy entirely. |
| `subagents.ts` | spawns `pi --no-extensions -e <self>`, env-inherited; `getAgentDir()` respects the config dir | works — **but children MUST inherit the host-mode safety posture** (see security). Set `PI_SUBAGENT_PI_COMMAND` if host `pi` isn't the default resolution. |
| `status.ts` / `timestamps.ts` / `todo-autoclear.ts` / `help.ts` | pure local/UI | unchanged. |
| MCP (`mcp.json` gateway, `slack`/`gog`) | ride the sbx gateway; no sbx → not attached | `capability-routing` degrades cleanly. Fine — the two target cases need shell/serial/docker, not Slack. |

## Safety posture — read this before believing host mode can be "safe"

The security lens is blunt: **host mode deletes all three safety properties at
once** — the disposable VM, the egress allowlist (`network.allowedDomains` in
`spec.yaml`), and proxy-side key injection. Today full-auto no-prompt execution
is safe *only because* of those.

**The hard truth (correcting an earlier draft): pi has no in-process safety
model to fall back on.** Cross-vendor review + pi's own
`docs/security.md` confirm:

- **pi has no built-in permission prompts and no built-in sandbox.** Built-in
  tools read/write/edit files and run shell commands with the launching user's
  full permissions. `defaultProjectTrust` is *only an input-loading guard*
  (whether to load project-local settings/extensions) — it does **not** gate
  command execution, writes, or network. So there is no `trust=prompt` toggle
  that makes host mode prompt-first, and no meaningful `--yolo` inverse. The
  earlier "prompt-first, forced" framing was wrong.
- **The only in-process seam is the `tool_call` extension hook** (an extension
  can return `{block, reason}` to veto a call). That is real but **advisory-
  strength**, and inadequate as the whole boundary because: (a) a command-text
  denylist cannot reason about shell effects — `python -c`, `node -e`, aliases,
  redirection, symlinks, `find -delete`, build scripts, and Docker mounts all
  slip past pattern matching; (b) **subagents bypass it entirely** — children
  spawn as `pi --no-extensions -e <subagents.ts>` (see `extensions/subagents.ts`)
  so any guard extension is deliberately absent, and they inherit the full env
  including keys; (c) in the self-dev case the guard extension is a symlink into
  the very checkout the agent is editing, so it can rewrite its own policy.
- **pi's own guidance:** "Real isolation needs to come from the operating system
  or a virtualization/container boundary" (`docs/security.md`). i.e. the sandbox
  was never a nice-to-have; it *is* the isolation.

**Honest conclusion:** host mode cannot be made "safe" the way the sandbox is.
The mitigations below reduce *accidental* damage and make the loss of the
boundary *visible*; they do **not** contain a prompt-injected or adversarial
agent. Host mode is, unavoidably, "you are trusting this agent with your real
machine and your real credentials for the length of the session." The design job
is to make that trust **deliberate, narrow, and unmistakable** — not to pretend
it's contained. This directly motivates the sandboxed alternative below.

### Credentials — reuse op://, never persist, never read sbx secrets

- **`sbx secret` appears write-only from pi-stack's side** (the tree only ever
  calls `sbx secret set` and `sbx secret ls` — names, not values). Verify against
  the installed sbx CLI before treating "no reveal verb" as an invariant. Either
  way, **do not** add a path that reveals secrets to a host process — it would
  destroy the invariant the sandbox path protects.
- **Reuse the existing op:// pattern.** The repo already resolves MCP creds via
  1Password `op run --env-file` with op:// refs (`config/op-refs.env.example`,
  `config.go` `OpRefsTemplate`, `doctor.go` op-ref validation). Host mode fits the
  same model: `op run --env-file=~/.config/pi-stack/hostmode.env -- pi …`, refs
  file holds `ANTHROPIC_API_KEY=op://vault/item/field`, never a value.
- **Env only, scoped to the child, short-lived.** Inject via the child `cmd.Env`
  (like `run.go:157`), never `export` into the shell, never write to
  `/etc/sandbox-persistent.sh` or an rc file. Keys vanish when pi exits.
- **Host preflight replaces the sbx one — with a caveat.** `modelProviderPreflight`
  reads `sbx secret ls` and returns `block=false` when sbx is absent, so the
  "no model to talk to" guard silently no-ops in exactly the host case. But if we
  exec `op run --env-file ... -- pi`, the Go parent **cannot inspect the resolved
  child env** — 1Password resolves refs inside the `op` process. So the preflight
  must either probe via `op` itself, or check that the *refs file exists and
  names* the providers, or simply accept pi's own missing-key failure. Ollama-only
  host mode (no cloud key) is valid and must be allowed.
- **GitHub blast radius.** Prefer a fine-grained PAT scoped to the working repo
  over the user's ambient `gh auth token` (full push/delete on every repo).

### Guard extension — the intended coverage (best-effort)

The `tool_call` guard extension should target the irreversible set: `rm -rf`
above the working dir, `sudo`, `curl|sh`/`wget|sh`, writes to shell rc /
`/etc/sandbox-persistent.sh`, `git push --force` / branch delete / history
rewrite, disk/partition tools, global package installs, and writes outside the
resolved working dir. **This list is illustrative, not exhaustive, and
pattern-matching command text cannot catch equivalents run via `python -c`,
`node -e`, aliases, or build scripts** — which is exactly why the section below
calls it guardrails, not a boundary.

### Autonomy controls — what they actually are

Given the above, the controls are a `tool_call`-hook guard extension plus loud
signposting, explicitly labeled best-effort:

- A **host-mode guard extension** using the `tool_call` hook. Confirmed against
  pi's docs: the hook can call `ctx.ui.confirm(...)` and return `{block, reason}`
  — pi's own example gates `rm -rf` this way (`docs/extensions.md`). So
  interactive confirm-before-destructive IS buildable (it just isn't a built-in
  setting; it's an extension). The guard should (a) refuse to *launch* with the
  working dir at `$HOME`/`/`/`/etc` or a path holding SSH/cloud/password-store
  secrets, (b) confirm-or-block the irreversible commands, (c) confine writes to
  the resolved working dir, and auto-allow reads + in-tree edits so you're not
  clicking through everything. Note the current `validateRunWorkspace` (`run.go`)
  only `os.Stat`s the dir — it does not canonicalize, so a `/tmp/link-to-home`
  symlink defeats a lexical path check. Resolve real paths (`filepath.EvalSymlinks`)
  before enforcing.
- **Subagents are a Phase-1 blocker, not a Phase-2 hardening item.** Children
  spawn as `pi --no-extensions -e <subagents.ts>`, so the guard extension is
  deliberately absent and they inherit the full env (keys included). Host mode
  must **disable subagent spawning** until the child argv provably carries the
  guard. Shipping host mode with live subagents is shipping the hole.
- The guard is real for *accidents* but **bypassable in principle** — command
  text can't be reasoned about (`python -c`, `node -e`, build scripts), and in
  self-dev the guard extension is a symlink into the very tree being edited, so
  the agent can rewrite its own policy. It is guardrails, not containment.

State the residual gap plainly in user-facing docs: **these are guardrails
against accidents, not a security boundary.** For anything you wouldn't hand a
shell to, use the sandbox.

### Decision: guardrails, not a separate user account

The *theoretically* strongest boundary is a dedicated macOS user account (its own
`$HOME`, its own git/gh creds, no access to the primary account's SSH keys /
password store / other repos) — the one option that actually *contains* a
compromised session. **We are not doing that for v1**, deliberately, because for
the self-dev case it costs more than it's worth:

- Self-dev needs the whole host toolchain — `make load`/`sbx`/`docker`/`go`/`node`
  /`brew` + the checkout + per-user `sbx` templates/secrets — duplicated in the
  second account and kept in sync. A parallel dev environment.
- **Docker Desktop is per-user on macOS**, so the second account needs its own
  Docker login/VM — and driving Docker is the whole point of self-dev, so this
  friction is unavoidable *and* central.
- User-switching (fast-switch / `su -`, separate Keychain + `gh` auth) is far too
  heavy for a "dip in for ten minutes to fix pi-stack" workflow — the cost lands
  squarely on the ergonomics that make host mode worth having.

And guardrails are *sufficient here*, not just a resigned compromise: **the
self-dev work product is the pi-stack repo, which is in git.** The blast radius
that matters most is version-controlled and revertible, so the `tool_call` guard
(accidents) + op:// creds never persisted (the one thing git doesn't protect) +
refuse-to-launch-in-`$HOME` is the right ceiling. The separate-account option
stays documented as the escalation path for anyone who wants real containment.
Offered but rejected: `sandbox-exec`/Seatbelt — for self-dev you must allow the
Docker socket, which is root-equivalent and reopens everything, so it adds
ceremony without a real boundary (it *would* fit the narrower platformio case).

## UX — leaving the boundary must be unmistakable

Layer the signals; don't rely on one:

1. **One-time config gate** (deliberate opt-in), mirroring how services/mcp are
   gated: `pi-stack config set host.enabled true`. First invocation with the gate
   off prints *why* it's dangerous and the exact enable command — friction is the
   feature here.
2. **Per-launch banner** (stderr, red): `⚠ HOST MODE — commands run on YOUR
   machine. No sandbox, no network fence, real credentials. Ctrl-C to abort.`
   Mirror the existing stderr note style in `run.go`.
3. **Persistent in-session skin** (the one that matters — a banner scrolls away):
   a dedicated red-tinted `host` theme (`themes/`) and a permanent **`HOST` badge
   in the powerbar/status line** via `status.ts` (`ctx.ui.setWidget`/`setStatus`).
   A glance should always tell you you're unsandboxed.

**Help / config / anti-drift wiring:**
- `host` goes in the expert help tier (`helpAllText` + man page) **only** — never
  in Core `helpText`. Keeping it off `pi-stack help` keeps it off the easy path.
- Add `host` to `knownVerbs` (`help.go`) and a man entry, or
  `TestManPageDocumentsEveryKnownVerb` (`man_test.go`) fails — a feature: it
  forces documentation. Also wires `suggestVerb` ("did you mean host?").
- Namespace config: `host.enabled`, reserve `host.autonomy` (`pi-stack config
  set`; never hand-edit `config.toml`).
- **Profile:** keep profile resolution for skill/knowledge/memory scoping; the
  sandbox-name namespacing half (`o.Name += "-" + profile`) is meaningless with no
  sandbox — document that host mode ignores it.

**Self-dev loop ergonomics (case 2):**
- When `pi-stack host` is launched from a pi-stack checkout (`isRepoRoot`, already
  in `run.go`), print a tailored footer: skill/extension/agent edits are live on
  next `/reload`; Dockerfile / pi-kit / baked-file edits need `make load` (host
  only, ~1GB tar) + a fresh sandbox to reach consumers. Surface the Mode-A/B rule
  instead of making the agent remember it.
- Host mode from a checkout should behave like `--dev` for skills (live from the
  tree) so editing a `SKILL.md` + `/reload` is instant, matching Mode-B
  ergonomics. Don't force `make load` for anything that doesn't need it.
- This case is the strongest evidence host mode is a narrow hatch, not a runtime:
  a host pi *can* run `make load`/`sbx`, which the VM structurally cannot.

## Feasibility

**Verdict: SMALL–MEDIUM.** The launch mechanism is small (pi is a plain binary;
extensions already degrade off-proxy; `pi-stack serve` already provides
memory/knowledge on `127.0.0.1`). It's "medium" only because two *design*
decisions gate it — where host keys come from, and how much autonomy with the
sandbox gone — not because of code volume.

Rough sizing (engineer):
- New `host` launch path (`services/host/cmd/pi-stack/hostrun.go` + `hostargs.go`):
  reuse `resolveRepoRoot`, `loadResolvedConfig`, `writeProfileFile`,
  `wireKnowledgeScope`; skip `buildSbxArgs`; exec `pi` not `sbx`. ~120–200 LOC Go.
- Host config-dir provisioning (`pi-stack host setup`): symlink harness dirs +
  `pi install` the pinned extension list. **Review flags the ~30–60 LOC estimate
  as not credible** for release-installed users: `install.sh` ships only two Go
  binaries — no checkout, no Node, no `pi` runtime, no TUI patch, no curated
  extension set. For the platformio case (a normal user, not a pi-stack dev), host
  mode needs a real install/update/uninstall/version-drift story, since the image
  pins Node/DHI + seven extension packages + a TUI patch that a bare npm install
  does not reproduce. Treat this as its own medium-sized workstream, and note it
  clashes with the "checkout-less host use is a non-goal" line — which of the two
  cases v1 actually serves needs an explicit call.
- Host-service URL wiring (`MEMORY_URL`/`KNOWLEDGE_URL`/`OLLAMA_BRIDGE_HOST` →
  `127.0.0.1`): ~5 LOC.
- Host safety settings variant (force `trust`→prompt, auto-load `guard`): tiny
  code, real policy work.
- Credential sourcing (op-refs → child env): ~30–50 LOC once the source is chosen.
- Help / status badge / theme / docs: small.

## Alternatives considered

**A. Keep the agent sandboxed; expose only the narrow host capability.** The two
cases have very different privilege needs: platformio wants a *narrow*
serial/build capability; self-dev wants Docker + `sbx`, which is effectively full
host control. Lumping both into one full-host-user envelope is the maximal grant.
For platformio specifically, a constrained **host-side service/MCP** (expose
`/dev/tty*` flash/monitor as typed tools, the `slack`/`gog` pattern) would let the
agent stay in the sandbox while reaching the one device it needs — far smaller
blast radius. This is worth a real spike before committing to full host mode for
the hardware case. Full host mode may still be the right answer for self-dev
(which genuinely needs Docker/`sbx`), so the two cases may not share a solution.

**B. `run --host` flag / `--no-sandbox`.** Rejected (see "Why this shape").

**C. Do nothing; document the manual escape hatch.** For self-dev, a user can
already `npm i -g` pi and run it by hand on the checkout. Cheapest, but no
safety signposting, no memory/knowledge wiring, and no guard — the worst of the
full-host risk with none of the mitigation. Not recommended, but it is the
status-quo baseline any proposal must beat.

## Phased plan

**Phase 0 — decide (blocks everything):**
1. Confirm pi's flags/hooks: config-dir env behavior, per-extension `-e`, and the
   exact `tool_call`-hook API (block/confirm) the guard extension will use. There
   is no built-in prompt/denylist to fall back on (see Safety posture), so the
   guard extension is the whole autonomy story — confirm its capabilities.
2. Pick the credential source: op:// refs via `op run --env-file` (recommended).
3. Pick the guard-extension coverage bar and the exact "not a boundary" wording.

**Phase 1 — minimal, safe hatch:**
- `pi-stack host [DIR]` verb behind `host.enabled` gate.
- Host config dir provisioning at `$XDG_STATE_HOME/pi-stack/host-agent`.
- Host preflight (env-based), host-service URL wiring, ollama-bridge bypass.
- `tool_call` guard extension (best-effort); write-jail to workspace (via
  `EvalSymlinks`); refuse launching at `$HOME`/`/`/`/etc`; subagents disabled.
- Banner + red `host` theme + `HOST` status badge.
- Help/man/`knownVerbs` wiring + docs framing it as a self-dev escape hatch.

**Phase 2 — harden (fast follow):**
- Broaden the `tool_call` guard beyond the obvious command set (best-effort;
  never claimed complete).
- Fine-grained-PAT guidance for GitHub instead of ambient `gh auth token`.
- Read-scoping consideration for adjacent secrets (documented as unenforceable
  in-process; the real answer is "don't run host mode in a dir near secrets").

Move into **Phase 1 blockers** (review reclassified these — they are not
fast-follows): disable subagents until guarded; host-specific agentContext (not
the sandbox one); the ollama self-loop bypass; the `EvalSymlinks` fix; the
`capability-routing` config-dir path fix; and a plain-language "guardrails, not a
boundary" warning in the launch banner and docs.

**Explicit non-goals (v1):** checkout-less host use (materializing a full baked
config dir), MCP gateway parity, Slack/gog in host mode, any framing as a second
first-class runtime, and any claim that the guard extension is a security
boundary.

## Open decisions to close before building

1. **Enforcement strength of the autonomy posture** — non-bypassable pre-exec
   gate vs. `trust=prompt` + `guard` skill. Security wants the former; be honest
   about which v1 ships and state the residual gap.
2. **ollama-bridge** — harmless EADDRINUSE no-op vs. needs a host-mode bypass.
   Verify empirically; a small bypass is cheap insurance either way.
3. **Credential source** — op:// refs (recommended) vs. plain env vars the user
   exports (simpler, but weaker: persisted plaintext, agent can read).
4. **Verb name** — `pi-stack host` (recommended) and the `pi-stack-host` /
   `pi-stack serve` naming-collision note for `man`.

## Files this touches (map for the build)

- `services/host/cmd/pi-stack/main.go` — dispatch `case "host":`, help text.
- `services/host/cmd/pi-stack/{hostrun.go,hostargs.go}` (new) — the host launch
  path; reuse `run.go` helpers, replace `buildSbxArgs`/`modelProviderPreflight`.
  Note: `host` needs its **own subcommand parser** so `pi-stack host setup` and
  `pi-stack host [DIR]` don't collide (a bare `setup` would otherwise parse as a
  workspace and be rejected as a non-directory).
- `services/host/cmd/pi-stack/{help.go,man.go,pi-stack.1}` — `knownVerbs`, expert
  tier, man entry (`man_test.go` enforces).
- `services/host/cmd/pi-stack/config.go` — `host.*` keys; reuse `OpRefsTemplate`.
- `extensions/ollama-bridge.ts` — optional host-mode bypass.
- `extensions/status.ts` — persistent `HOST` badge.
- `themes/` — red-tinted `host` theme.
- `settings.json` (or a host variant) — trust override for host sessions.
- `pi-kit/spec.yaml` — unchanged (host mode doesn't touch the kit).
- `docs/` + README — the two-case framing.
