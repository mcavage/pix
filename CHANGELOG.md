# Changelog

All notable changes to pi-stack are recorded here. CI publishes versioned images
from `main` and stamps the tag into `pi-kit/spec.yaml`; this file is the
human-readable summary of what changed and whether an upgrade is breaking.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Added

- `pi-stack doctor --json` gains a top-level `schema_version` (currently `3`)
  and a `blocking` flag, and every check now carries a `requirement`
  (`core` | `configured-optional` | `unconfigured-optional`) and an `evidence`
  state (`healthy` | `failed` | `unverifiable` | `not-configured`). All v1
  fields are unchanged; this is additive. v3 (review round 1) made check
  state fully derived from requirement + evidence (never independently
  stored), added an aggregate core `model keys` check under `providers` (at
  least one of anthropic/openai/google confirmed set is enough; zero of
  three verified is the only provider-key core failure), and added
  `sbx_probe_failed` to distinguish a present-but-unhealthy `sbx` from an
  absent one.
- `pi-stack doctor --verbose` shows full per-check group detail; the new
  default is concise (a healthy group collapses to one summary line).
- `pi-stack setup --pull-models` force-pulls any missing watcher/embed/bridge
  Ollama model with no prompt (CI-safe). Without it, an interactive terminal
  offers a default-No prompt per missing model naming which role needs it;
  non-interactive/`--yes` never downloads and prints the exact deferred
  `ollama pull <tag>` command instead. Setup always prints a local-model
  readiness receipt before handoff, regardless of `services` membership.
- `pi-stack gog setup [--account <email>] [--credentials <path>]` is the new
  guided, public path to wiring up Google Workspace: it checks `gog` is
  installed, imports your OAuth client and authorizes the account through
  whichever auth surface the installed `gog` supports, verifies BOTH
  interactive and headless auth (the same path the sbx gateway actually
  spawns), and only then saves the account, enables `gog` in the configured
  MCP set, and registers it. See `docs/gog-setup.md`.

### Fixed

- **`pi-stack doctor` and `pi-stack status` no longer probe/exec a local
  command, or recommend `pi-stack mcp register`, for a REMOTE gateway-catalog
  MCP server (notion/atlassian/granola/etc).** A non-gog configured server
  used to get the same local stdio treatment (`mcpProbeCheck`: read the sbx
  registration, exec it with `--list-tools`) regardless of whether it was
  actually a local pi-stack-host subcommand or a remote server attached
  through the gateway's hosted control plane: a confirmed remote server
  could be exec'd as if it were local, and an unregistered one was always
  told to `pi-stack mcp register` even though that command only knows local
  servers. Both now classify each configured name via `localMCPNames`
  (`pi-stack-host mcp --list`, the same source of truth `pi-stack mcp
  register` already uses) before deciding how to check it: a CONFIRMED local
  server keeps the existing probe + `pi-stack mcp register`; a CONFIRMED
  remote server is never locally probed/exec'd, and instead gets its sbx
  registration checked plus a bounded native `sbx mcp auth status <name>`
  probe: an unregistered remote server recommends `pi-stack mcp bundle`, and
  a registered-but-unauthenticated one recommends `pi-stack mcp auth
  <name>`; when the classification itself can't be established, evidence is
  unverifiable and NO repair command is recommended (never a guess that
  could be wrong for the actual kind of server).
- **`pi-stack status` now folds the active pack's eager MCP integrations
  into its attach-on-run rendering.** A pack's `[[integrations]]` entry
  declared `static = true` is folded into the eager set at launch
  (`applyPackToLaunch`), but `status` previously only consulted cfg's own
  `mcp_static`/`mcp_dynamic`, so a pack-pinned integration rendered
  (incorrectly) as dynamically discoverable. `status` now loads the
  configured active pack READ-ONLY (`loadPack`, no skills mount, no kit
  synth, no credential warning, no cfg mutation) and folds its declared
  static integrations into a shallow copy of `cfg.MCPStatic` before applying
  the same `mcp_dynamic` override precedence `resolveStaticMCP` already
  uses. A broken or unreadable active pack degrades honestly (cfg's own
  static/dynamic pins only), never a false attach-on-run claim.
- **`pi-stack status` no longer renders a confirmed-missing `✗` for provider
  keys it never actually checked.** When `sbx` was absent or `sbx secret ls`
  failed, every provider (`anthropic`/`openai`/`google`/`github`) rendered a
  plain `✗` in both the human output and `--json`, indistinguishable from a
  verified-absent key. `statusReport` gains an authoritative
  `provider_evidence` map (the same `healthy`/`failed`/`unverifiable` axis
  `doctor` already uses); the human render now shows `⚠` for an unverifiable
  key instead of a false `✗`, and `--json` carries the same tri-state. The
  existing `providers` bool map is unchanged (JSON-compatible) and TODOs are
  unchanged (they already never claimed a specific missing key).
- **`pi-stack status` no longer claims every configured MCP server is
  `attach-on-run`.** It used to hardcode `attach_on_run: true` for any server
  in `cfg.MCP`, even though the DEFAULT attach mode is dynamic (discovered on
  demand) unless a server is pinned eager via `mcp_static`. `status` now
  resolves eager/dynamic with the exact same `resolveStaticMCP` semantics
  (including `mcp_dynamic` precedence) `pi-stack run` uses at launch: a
  default-dynamic registered server renders "dynamically discoverable", and
  only an `mcp_static`-pinned one renders `✓ attach-on-run`. The registration
  TODO (`pi-stack mcp register` for an unregistered configured server) is
  unchanged.
- **`pi-stack gog setup -h` now reaches its own detailed usage (Phase-10
  product/DX review, DX-1).** `pi-stack gog setup -h`/`--help` used to be
  intercepted by the `gog` noun dispatcher's blanket help gate (it scanned the
  WHOLE remaining argv for `-h`/`--help`, matching on `setup -h` before
  `setup` was ever dispatched) and printed the short top-level `gog` usage
  instead of `gog setup`'s detailed usage (numbered steps + flags). The gate
  now only fires for `gog -h`/`gog --help` with no subcommand; a subcommand
  is always dispatched first and owns its own `-h`/`--help`.
- **`pi-stack status` and `pi-stack doctor` now agree on what a missing `sbx`
  binary means (Phase-10 product/DX review, DX-2).** `status` used to tell
  you to "install the Docker Sandboxes CLI (sbx)" the moment `sbx` wasn't on
  PATH — presuming this IS the host and sbx is genuinely missing, when
  absence just as often means running inside the sandbox, where `sbx` is
  structurally never there (exactly the case `doctor` already treats as
  "can't verify from here", not a repair item). `status` now shares that
  same perspective: it advises running `pi-stack doctor` on the host rather
  than presuming a host-install fix it cannot actually confirm applies.
- **The aggregate `model keys` repair TODO is a bare, copy-pasteable command
  again (Phase-10 product/DX review, DX-3).** It used to read `sbx secret
  set -g anthropic  (or: sbx secret set -g openai / google — any one
  model-provider key)` — a parenthetical glued onto the command that breaks
  a straight copy-paste. The TODO is now exactly `sbx secret set -g
  anthropic`; the any-one-of-three alternative and caveat moved to the
  check's `detail` line, where they read as context instead of shell noise.
- **`pi-stack doctor` marks the one CORE blocking check distinctly from an
  optional failure (Phase-10 product/DX review, DX-4).** Both a verified
  core failure and a verified optional failure rendered as a plain `✗` with
  no way to tell which one actually blocks the exit code. The blocking core
  check (`model keys`, zero of three set) now renders `✗ model keys
  (required)`; every other `✗` stays plain.
- **`pi-stack doctor`'s `--verbose` hint only prints when concise mode
  actually hid something (Phase-10 product/DX review, DX-5).** The concise
  "(concise output — run `pi-stack doctor --verbose` for full group detail)"
  hint used to print unconditionally in non-verbose mode, even on a cold run
  where nothing was healthy and so nothing was collapsed — pointing at
  detail `--verbose` would not actually add. It now only prints when at
  least one healthy check was hidden by concise mode.
- **`pi-stack doctor` no longer trusts alternate symlink paths for registered
  probe executables (review round 2, R2-01).** The trusted-executable gate
  used to resolve symlinks at check time and then exec the ORIGINAL
  registered path — a check-then-exec race an attacker could win by swapping
  the symlink between the two. Trust is now strict exact (cleaned) path
  equality with the resolver's answer (`lookPath` for gog/op,
  `findHostBinary` for pi-stack-host), and the probe execs ONLY the
  resolver's canonical token, never the registered spelling. A
  versioned-release symlink install (e.g. `/opt/pi-stack/current/…`) that was
  previously blessed now reads as "probe skipped … never executed"
  (unverifiable) — register the canonical binary path instead.
- **Every `pi-stack doctor` discovery subprocess is now bounded (review
  round 2, R2-02).** `sbx secret ls`, `sbx mcp ls`, `sbx mcp get <name>`,
  `sbx mcp ls -o json`, `ollama list`, and `op account list` all run through
  the same bounded probe seam as the MCP tool probes (hard 5s timeout +
  capped output), so a hung sbx daemon, wedged ollama, or stuck 1Password CLI
  can never wedge doctor — each hang classifies exactly like the equivalent
  failure (present-but-probe-failed, gateway down, list-unverifiable,
  not-configured). Setup shares the same bounded Ollama probe via
  `probeOllama`.
- **`pi-stack gog setup` now preflights every predictable hard requirement
  BEFORE any OAuth side effect, and the prior gog registration snapshot is
  now tri-state (review round 2, R2-03/R2-04).** Previously, sbx presence
  was only checked right before registration — AFTER the interactive OAuth
  route had already run — and whatever gog registration already existed was
  captured with a single `(argv, bool)` that collapsed three different
  situations (confirmed absent, confirmed present but unparseable, and "the
  sbx probe itself failed") into one "nothing to restore" answer. Now: gog
  CLI + its selected auth route's capability, the credentials regular-file
  check, config loading, the `sbx` binary, and a bounded snapshot of
  whatever gog registration already exists are ALL confirmed before the
  first `runInteractive` call, so a missing `sbx`, an unparseable
  `config.toml`, or an unreadable/unlistable prior registration aborts with
  zero OAuth side effects and zero config mutation. The registration
  snapshot is genuinely tri-state (confirmed absent / confirmed present with
  a restorable argv / unknown): unknown is never treated as absent, so
  rollback can never silently clobber a registration it couldn't actually
  read back.
- **`pi-stack gog setup`'s credentials-path check now requires a TRUE
  regular file (review round 2, R2-05).** It previously only checked
  "exists and isn't a directory", which a FIFO, socket, or device also
  satisfies. It now checks `Mode().IsRegular()`; a symlink POINTING AT a
  regular file is still allowed (the check follows symlinks via `os.Stat`,
  same as before), but a FIFO, socket, device, or a symlink to any of those
  is rejected before ever being handed to `gog`.
- **`pi-stack.1` no longer contradicts itself about where provider keys come
  from (review round 2, R2-06).** The `setup` section previously said keys
  are sourced "PREFERRING 1Password" in one sentence and "1Password ONLY"
  in the very next. There is no preference/fallback: provider keys come
  from 1Password only, matching `setup`'s actual behavior (and the rest of
  the man page).
- **`pi-stack gog setup` now enforces read-only OAuth authorization at grant
  time, not just at MCP runtime.** Every supported auth route always passes
  gog's `--readonly` flag on whichever step actually performs the OAuth
  grant, gated on a capability probe of that SELECTED route's own
  subcommand help (not just the top-level `gog auth --help` names) for
  every flag it needs. If the installed `gog` cannot advertise `--readonly`
  for the selected route, the command fails with upgrade guidance instead of
  authorizing without it or falling back to an older, unguarded route.
  `gog` exposes no stable, parseable scope-inspection surface, so this is
  documented precisely as what it is: a guaranteed read-only REQUEST at
  grant time, backed by gog's own runtime write-blocking flags
  (`--gmail-no-send --wrap-untrusted --readonly --allow-tool read`) — not an
  independently-verified inspection of granted scopes.
- `pi-stack gog setup` no longer skips headless verification when
  1Password/`op-refs.env` aren't set up (previously treated as a fine-on-
  macOS no-op): it now probes the bare hardened gog command directly, the
  same hardened invocation the sbx gateway registers, bounded the same way
  as every other probe. A clean zero-tools result still fails with the
  keyring fix; a timeout or exec error is unverifiable and is never reported
  as success.
- `pi-stack gog setup` now requires `sbx`: a missing `sbx` binary, or a
  failed `sbx mcp add`, is a hard failure — it no longer reports a silent
  "would register" success. Registration is also required to succeed
  BEFORE config is saved (previously config was saved first): a
  registration failure now leaves the persisted config completely
  unchanged, and a config-save failure after a successful registration
  rolls the sbx-side registration back (restoring whatever was registered
  before, or removing the new one) instead of leaving config and the
  gateway to drift apart.
- `pi-stack gog setup`'s version-awareness now probes the exact subcommand
  help + flags for the SELECTED route, not only the top-level subcommand
  names — a `gog` release that changed a subcommand's flags under an
  unchanged name is caught with installed-version guidance before any auth
  command runs, instead of failing as an opaque exec error mid-flow.
- Every noninteractive `gog setup` probe (subcommand-help/capability,
  version, auth-doctor, and the new bare headless probe) runs through the
  same bounded timeout + output-cap machinery `doctor` uses; only the
  interactive, browser-opening auth commands keep inherited stdio and stay
  user-cancellable.
- **`pi-stack run --mcp M` now actually attaches eagerly (ship-gate review,
  finding #1).** An explicit per-run `--mcp` flag used to be silently folded
  into the same default-dynamic pool as a config-listed server, so on a
  default config it was filtered OUT of `--static-mcp` and never reached the
  sandbox as eager — the flag was a promise the launcher didn't keep. A
  server named via `--mcp` now attaches eagerly (`--static-mcp`) unless the
  user pins it lazy with the stronger, already-documented `mcp_dynamic`
  override; a server merely listed in config (`mcp` in `config.toml`) keeps
  the existing default-dynamic/`mcp_static` behavior, unchanged.
- **`pi-stack gog setup`'s pre-commit headless verification now probes the
  EXACT argv the sbx gateway will register (ship-gate review, finding #2).**
  It used to probe a lighter reconstruction (missing `--no-masking`,
  `--gmail-no-send`, `--wrap-untrusted`, `--readonly`, `--allow-tool read`)
  when 1Password was available — a healthy verification against flags
  registration never actually uses. Both `gog setup`'s verification and
  `doctor`'s best-effort fallback probe now build their argv through the ONE
  canonical registrar (`gogRegisteredArgv`), so a probe can never silently
  drift from what actually gets registered.
- **`doctor`/`status` failure paths point at `pi-stack gog setup`, never the
  legacy `gog auth login`/`add-client` recipe (ship-gate review, finding
  #3).** A not-authorized account used to carry a raw `gog auth add-client
  <client.json> && gog --account <acct> auth login` repair command in
  `doctor`, and bare `pi-stack status` told you to "run gog auth login" —
  both bypass the guided, hardened, headless-verified setup flow. Both now
  point at `pi-stack gog setup`.
- **`status`/`doctor` no longer report a missing alternative model-provider
  key or a missing GitHub key as outstanding (ship-gate review, finding
  #4).** With any ONE of anthropic/openai/google set, the other two used to
  each add their own `sbx secret set -g <key>` TODO — contradicting the
  documented "any one of three is enough" policy and inflating the
  outstanding-items count. GitHub's key was also always treated as a
  verified failure when unset, even though it has always been
  configured-optional. Both now agree: an individually-missing model key is
  only ever a gap when ALL THREE are missing (a single aggregate TODO, as
  before), and a missing GitHub key never adds a TODO or counts as
  outstanding.
- **`pi-stack gog setup` now resolves op/op-refs/gog into ONE immutable
  snapshot before probing, closing a TOCTOU between probe and registration
  (ship-gate review round 2, finding #1).** It used to probe the headless
  path with one resolution of `gog`/`op`/op-refs.env, then hand off to the
  generic `registerServers`, which independently RE-RESOLVED all three when
  it actually ran `sbx mcp add` — a window in which another process mutating
  PATH or op-refs.env could make the REGISTERED command differ from the one
  that was just proven healthy. `buildGogRegistrar` now resolves
  `gog`/`op`/op-refs exactly once, and the same immutable `mcpRegistrar` is
  reused, unchanged, for both the probe and the registration
  (`registerGogRegistrar`) — there is no second resolution to drift.
- **`pi-stack mcp register`'s bare-gog note no longer prints the raw legacy
  `gog auth login` recipe (ship-gate review round 2, finding #2).** It used
  to say "gog authenticates via OAuth (gog auth login)" when registering gog
  without an op-refs.env wrapper — the last shipping string still pointing
  at the bypassed legacy recipe (see finding #3 above). It now points at the
  guided `pi-stack gog setup` flow instead, and a source-string anti-drift
  test (`TestShippingGoStringsHaveNoRepoOnlyRecoveryCommands`) now rejects
  `"gog auth login"` in any shipping Go string, so this can't regress.

### Changed

- `pi-stack doctor` now exits **1** on a VERIFIED core requirement failure
  (e.g. a provider key confirmed unset) instead of only on a usage error.
  Everything else — optional gaps and any check that couldn't be verified,
  including every provider check when running inside the sandbox with `sbx`
  absent — still exits 0. A usage error still exits 2.
- Rewrote `docs/gog-setup.md` as a current task guide for `pi-stack gog
  setup`: dropped the obsolete `SBX_MCP_URL`/unreleased-gateway framing (the
  sbx local data-plane gateway is generally available), the repo-relative
  `config/op-refs.env` path, and `make mcp-register` as the consumer path.
- Reconciled `services/host/cmd/pi-stack/pi-stack.1` with current behavior:
  removed the contradictory `--use-sbx-keys` claims (that flag was removed;
  1Password is the only provider-key source), the retired `--profile`
  flag/`active_profile`/`profiles.*` config, the stale `%20`-encoding claim
  for op-refs (refs are stored with literal spaces), the stale `gemma3:4b`
  watcher-model default (now `qwen3.5:9b`), and added `doctor --verbose`,
  `setup --pull-models`, and the `gog` verb.
- README's first-run and local-model sections now describe setup's
  default-No model-pull prompt, `--pull-models`, `doctor`'s concise-by-default
  output with `--verbose`, its core-only exit semantics, and `pi-stack gog
  setup` as the Google Workspace entry point.
- `skills/onboarding/SKILL.md`: the gog and other-MCP setup gaps now point at
  `pi-stack gog setup` / dynamic discovery instead of a `config set` +
  `mcp register` + `run --replace` recipe (a registered server no longer
  needs a sandbox recreate to be usable); host-mode guidance no longer tacks
  on a redundant `config set host.enabled true` after `pi-stack host setup`
  (one command already does both); local-model questions now point at
  `pi-stack doctor` instead of inferring readiness from the host-state
  payload's configured model names.

### Fixed

- `pi-stack host` now launches its interactive session under `op run
  --no-masking`. op's default output masking pipes pi's stdout/stderr through a
  filter, which makes them non-TTYs — pi's TUI then saw no terminal and exited
  immediately (banner, then straight back to the shell, exit 0). Non-interactive
  paths were unaffected, which is why it looked like a silent failure. (The
  mcp-gateway op-run path already used `--no-masking`.)
- Provider-key op:// refs are stored with **literal spaces** again. An earlier
  change percent-encoded spaces (`Anthropic%20API%20Key`) on a false premise;
  op 2.35.0's `op read` AND `op run --env-file` both reject `%20`, so any
  1Password item whose name has a space (very common) failed to resolve and
  `pi-stack setup` aborted. Existing encoded refs self-heal: they're decoded on
  read and rewritten literal on the next write.

### Changed

- The sandbox and opt-in host mode now use pi `0.82.0`. Curated pi
  extensions were re-pinned to the newest versions published by the 0.82.0
  release, and CI now checks both vendored runtime patches against the exact pi
  and todo-list package pins.
- Fixed intermittent adjacent duplicate lines in terminal scrollback. The
  bottom-pin patch had repainted a row copied from immutable scrollback, leaving
  both physical copies behind. Bottom-anchored shrinks now rebuild the terminal
  buffer under synchronized output, which also keeps the editor and footer from
  jumping.
- `pi-stack setup` no longer provisions or enables **host mode** (the unsandboxed
  escape hatch). It was noisy (it needs `pi` on PATH, which sandbox-only users
  don't have) and only relevant to some people. Host mode is now opt-in via a
  single command: `pi-stack host setup` now PROVISIONS **and** enables it (when
  provisioning succeeds), so the separate `config set host.enabled true` step is
  gone.
- `pi-stack setup` no longer prints the redundant up-front sbx provider-key
  status block; the 1Password flow reports each provider's ref + sync itself.
- Identity seeded from git config is now **first name only** — no surname, no
  email. It's recalled into every session, so it carries the minimum to greet.
  (`readGitIdentity` no longer reads `user.email`; memory stores one first-name
  fact instead of name + email.)

### Changed (breaking)

- **MCP migrated to sbx's nightly gateway.** The sandbox flag is now
  `--static-mcp` (sbx removed `--mcp`), and MCP runs through sbx's **local
  data-plane gateway** — always available, **no `SBX_MCP_URL`** needed (dropped
  the old SBX_MCP_URL gate + "gateway off" warning; `pi-stack mcp register` no
  longer requires it). pi-stack's own `pi-stack run --mcp M` CLI flag is unchanged.
  **Per-server attach mode:** a configured server is attached eagerly at create
  (`--static-mcp`, tools always in context) or left dynamic (the in-VM agent
  discovers + calls it on demand via mcp-find/mcp-exec/code-mode; the daemon
  spawns local stdio servers host-side, so local and remote behave the same). The
  **default is dynamic for every registered server** — keeps heavy tool schemas
  out of context until needed. Pin a server eager with the new `mcp_static` list
  (`mcp_dynamic` is the explicit opposite, and wins if a server is in both). A
  **pack** can request eager attach for its own integrations with
  `[[integrations]] static = true` (folded into the eager set at launch; a user
  `mcp_dynamic` still overrides). New:
  - `pi-stack mcp load <name> [DIR]` — attach an already-registered server to a
    RUNNING sandbox live, no recreate (`sbx mcp load`).
  - `pi-stack mcp auth [args…]` — hosted-control-plane OAuth for remote servers
    (`sbx mcp auth`; e.g. `auth --all`).
  - `pi-stack mcp bundle` — register the shipped public catalog
    (notion/atlassian/granola, `config/mcp-catalog.bundle.json`) in one step.
  `make mcp-auth` already used native `sbx mcp auth`; its tail now points at
  `pi-stack mcp load` instead of a recreate. `doctor` guidance no longer mentions
  `SBX_MCP_URL` (a failed `sbx mcp ls` now points at the sbx daemon).

- **Kit migrated to kit-spec v2** (`pi-kit/spec.yaml`, `schemaVersion: "2"`).
  Credentials are now a `credentials[]` list of `service` + `apiKey`
  (name/proxyManaged/inject[]); egress is `caps.network.allow`. Replaces the v1
  `network.serviceDomains`/`serviceAuth`/`allowedDomains` + `credentials.sources`
  + `environment.proxyManaged`. Injection is unchanged (proxy-managed sentinels;
  all four providers verified). **Requires a recent `sbx` nightly** (v0.37+); the
  per-credential `service:` is mandatory or `sbx run` panics. Overlay/mixin kits
  should move their network rules to `caps.network.allow` too.
- **1Password is now the only provider-key source; the `op` CLI is required.**
  Removed `pi-stack setup --use-sbx-keys` / `--use-1password` (both now error),
  the one-time "use existing sbx keys?" convenience prompt, and the persisted
  `provider_key_mode` config key (dropped from `config.toml`, `config
  get/set/unset`). `pi-stack setup` fails without `op` installed + signed in.
  `pi-stack run` still launches when a usable key is already in `sbx` (op is
  required at setup, not re-checked every run). `install.sh` now warns when `op`
  or `sbx` is missing.

### Known issues

- On `sbx` v0.37.0-rc1 the cosmetic `credential ... discovered but no domains
  allowed by your bindings; not injecting` line prints once per stored provider
  key even though injection works (verified: cloud providers return HTTP 200
  through the proxy). Hand-written `credentials.yaml` bindings are not honored by
  rc1, so it can't be silenced from our side; left visible and filed upstream
  (see `docs/upstream/sbx-0.37-binding-warning.md`). Do not mask `sbx` output.

### Added

- Guided `pi-stack setup` now establishes a complete host: validated 1Password
  references for Anthropic, OpenAI, and Google, rational `sbx` reconciliation,
  memory, the default pack, host mode, and a one-shot in-session handoff.
- `pi-stack setup` accepts `--use-sbx-keys`: trust a COMPLETE existing `sbx`
  provider key set (anthropic, openai, google) instead of the strict
  1Password flow, skipping every op install/signin/ref/reconciliation step. It
  requires an exact successful sbx probe with all three keys (absent,
  erroring, or incomplete sbx fails with a clear message, naming exactly which
  provider(s) are missing), and never deletes an existing 1Password ref or
  synced record, it just isn't used that run. `--use-1password` is the
  mutually exclusive explicit opposite: it forces the strict flow for this
  run even when sbx already has all three keys or a prior run persisted the
  sbx mode. Both flags are **setup-only**; `pi-stack onboard` rejects either
  one (onboard never provisions provider keys at all).
  Interactively, with no flag and no persisted mode, setup also offers a
  one-time convenience prompt when sbx already has all three keys and no
  provider ref is configured yet (default yes); declining falls through to
  the strict flow with no further retries, and the prompt never reappears
  once a ref exists. `--yes` alone does NOT imply the skip.
  Whichever source succeeds is PERSISTED as `provider_key_mode` (`sbx` or
  `1password`) in `config.toml`, so a repeat `pi-stack setup` with no flags
  reuses that exact choice with no prompt — a persisted `sbx` mode still
  re-runs the exact all-three probe every time (never a cached bypass), an
  explicit flag always overrides the persisted choice for that one run, and a
  mode-save failure fails setup honestly rather than reporting success while
  silently failing to remember the choice. Inspect or clear it with
  `pi-stack config get/unset provider_key_mode`.
  Setup no longer claims every run is always cloud-ready: skipping 1Password
  leaves host mode local/Ollama-only until you configure `hostmode.env` refs,
  and `setupHostMode` reports that as an expected result instead of a
  should-not-happen message. Host-mode/setup copy also no longer overclaims
  cloud keys as "wired": it says keys were "validated this run" only after
  the strict 1Password flow actually resolved them, and "configured (not
  verified this run)" when a run used existing sbx keys instead — real
  validation still happens at every `pi-stack host` launch via `op run`.
  `pi-stack secret set` for a provider key now mirrors the ref into
  `hostmode.env` as well as `op-refs.env` in one step, so three `secret set`
  commands (one per provider) really are enough to wire both the sandbox and
  host mode, no separate step needed.
- Read-only `memory_recall` and `memory_stats` tools let the agent inspect memory
  without exposing memory mutation as a normal tool action.
- Long autonomous tasks resume after threshold compaction when their structured
  todo list still has work in progress. Queued user messages and `/todos clear`
  take precedence.
- `model-refresh` skill: teaches how to re-ground the router (registry +
  scorecard + policy) on LIVE model cards and pricing instead of training data,
  then compile + verify. Baked into the public image.
- `pi-stack agent ls` WHY column is now actionable: it shows the intent, the
  objective, the chosen model's accuracy/per-task-$/latency, and either what it
  beat or that a constraint left a `sole fit` — plus a legend. Previously it said
  only `intent <name>`, which explained nothing.
- `pi-stack agent ls` flags an explicit `model:` pin that is not in the registry
  (`pinned (UNKNOWN ...)`) instead of silently resolving to a model that fails at
  spawn.
- Slack MCP results that carry user-authored text (messages, channel
  topics/purposes, profile names/titles) now stamp an untrusted-content guard,
  matching the `gog` server's `--wrap-untrusted` behavior.
- Bounded MCP frame size (Content-Length) so a hostile peer cannot force an
  unbounded host allocation.
- Community health files: `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`,
  this changelog, and GitHub issue/PR templates.
- `docs/README.md` index for the docs tree.

### Changed

- **The built-in pack is named `default`, not `personal`.** Existing `personal`
  and older `pack` directories migrate with their git history, config, trust,
  wrapper ownership, and activation provenance intact. Bare `personal` remains a
  deprecated compatibility alias.
- **Onboarding is one upfront, grounded handoff.** Trusted host facts travel in
  the launcher-generated initial prompt rather than through a workspace file,
  so a cloned repository cannot forge setup guidance.
- **Memory capture is quieter and easier to inspect.** Questions and routine
  watcher narration no longer become durable facts, perishable watcher entries
  expire after seven days, and literal `*` recall lists stored rows newest first.
- **The memory watcher defaults to `qwen3.5:9b`, the same model the ollama-bridge
  uses.** Two defaults previously disagreed (the daemon fell back to `gemma3:4b`,
  the launcher config to `qwen3.5:4b`), and neither matched the model already
  resident for the bridge/router — so a fresh install ran capture on a model that
  usually was not pulled, and capture silently did nothing. Now capture reuses the
  one local model already loaded, so it works out of the box for anyone running
  the bridge. Override with `pi-stack config set memory_watcher_model <model>` on
  a memory-constrained machine.
- **Local Ollama models are now coherent host config, not scattered env.** The
  stale `gemma3:4b` defaults are gone: the new `ollama_bridge_model` setting (the
  sandbox's local chat model + the router's local option) defaults to `qwen3.5:9b`,
  matching the memory watcher, so Ollama keeps a single model resident for both
  capture and local inference. Set it with `pi-stack config set ollama_bridge_model
  <tag>`; `pi-stack run` writes it into `<workspace>/.pi-stack/ollama-bridge.model`
  and the `ollama-bridge` extension reads it — no more hand-editing
  `/etc/sandbox-persistent.sh`. The bridge display label is now derived from the
  tag, so `OLLAMA_BRIDGE_MODEL_NAME` is optional (you set one value, not two).
  `make pull-models` pulls the local models. NOTE: the host memory service uses the
  watcher + embed models; it does NOT use the bridge/router local model — those
  are separate roles.
- **Routing redesign: a real tiered, multi-vendor crew instead of a monoculture,
  grounded in live model data.** Previously 13 of 18 agents collapsed onto one
  model and the rest onto Opus. The registry/scorecard/policy are now seeded from
  the current (July 2026) lineup and pricing: Claude Fable 5 for `deep`
  (`max-accuracy`); Opus 4.8 for `architect`/`product-manager` (`strategy`);
  Sonnet 5 for `engineer`/`designer` (`code`) and the `advisory` specialist crew;
  GPT-5.6 Sol for `review`; Gemini 3.1 Pro for `security-lead` (`red-team`);
  Gemini 3.1 Flash-Lite for `fanout` (`breadth`); Haiku 4.5 for `qa-lead`
  (`verify`). Three cloud vendors plus a local `qwen3.5:9b` option (a current,
  Apache-2.0 all-rounder that fits a 16GB machine, replacing the year-old
  `gpt-oss:20b` and the 58GB `gemma4:31b` that never fit a laptop), tiered by
  leverage with the adversarial roles pinned cross-vendor via provider
  allowlists. New intents: `strategy`, `advisory`, `red-team`.
- **`agent ls` WHY reasons rewritten.** They said `sole fit` (meaningless) and a
  wall of precise-looking but unmeasured numbers. Now each WHY names the actual
  binding constraints that left one model (e.g. `only model matching anthropic,
  <=$0.18, >=0.80 acc`) or what the winner beat in a contest, with a footer that
  flags the metrics as seed priors.
- **`evals` is no longer a command.** The automated eval harness was removed (see
  Removed); the `evals` verb is gone from the launcher, help, and man page.
- Removed company-specific wiring from the public tree: the `SNOW_CONN` env in
  `make serve` (now a generic overlay-populated `SERVE_ENV`), the `snow` probe in
  the `healthcheck` skill (now a generic `EXTRA_CLIS` hook), and the overlay-only
  `:11442` port from the public kit allowlist. Overlays add these themselves.
- Pruned stale docs: two `HANDOFF` snapshots, a superseded migration guide, a
  review artifact, and an upstream issue draft.
- README reworked to lead with the outcome, fix the launch command
  (`pi-stack run`), correct the pi link, and complete the launcher command list.

### Removed

- **Tore out the automated eval harness** (`evals/`, `pi-stack-host evals`,
  `make evals`, `pi-stack agent reassess --model`'s auto-measure path). The
  router never needed it: it only ever reads `scorecard.json`, regardless of
  how the numbers got there. Scores are now hand-maintained — edit
  `services/host/routing/defaults/scorecard.json` directly (seeded from
  published benchmarks/pricing; see the `model-refresh` skill), then
  `pi-stack route compile`. `pi-stack agent new`'s starter eval-suite
  scaffolding is also gone.

### Fixed

- **Automatic fact capture could be silently dead.** The watcher-availability
  check was one-shot at startup and latched: once it marked the watcher
  unavailable, `observe` short-circuited and never retried, so capture stayed off
  until a full daemon restart even after the model was pulled — and the client
  swallowed the failure reason, so nothing surfaced. Added a throttled live
  re-probe (capture recovers within 30s of the model appearing, no restart), a
  one-time client warning when capture is off, and a `pi-stack doctor` line that
  reads the daemon's live capture flag with the exact fix.
- `deliver` skill failed to load: its frontmatter `description` was an unquoted
  YAML scalar containing `: ` sequences, which YAML parsed as a nested mapping.
- `enterprise-admin` agent resolved to a model (`anthropic/sonnet-5`) that is not
  in the registry; it now uses `intent: reasoning` like its peers.
