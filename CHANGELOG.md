# Changelog

All notable changes to Pix are recorded here. CI publishes versioned images
from `main` and stamps the tag into `pi-kit/spec.yaml`; this file is the
human-readable summary of what changed and whether an upgrade is breaking.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/).

## Unreleased

### Added

- **Self-development UAT for `pix run --dev`.** Fresh dev sandboxes receive a unique, session-scoped host MCP with a closed tool set for committed scenario validation, candidate builds, disposable `pix-uat-*` sandboxes, bounded artifacts, and browser/OAuth checks through a dedicated Chrome profile. The runner owns typed leases, crash cleanup, concurrency limits, and per-step logs without exposing a host shell or changing normal pix configuration. `pix uat status`, `pix uat browser bootstrap`, and `uat/scenarios/smoke.yaml` provide the bootstrap path; the first real host smoke run passed end to end.
- **Optional document-write capability.** `docs-write` defaults to `none`, allowing private packs to attach a separate narrowly scoped writer while the existing `gworkspace` capability remains read-only.
- **Supervision-tree observability, end to end.** `pix-host serve` now publishes
  a typed snapshot of the Suture tree (`~/.local/state/pix/serve.units.json`,
  atomic + 0600, republished every 5 s and on every supervision event), and both
  reader surfaces consume it instead of guessing: `pix serve status [--json]`
  gains a `units[]` array and a per-unit human line, and `pix doctor --json` /
  `pix status --json` gain a `supervisor` object. Each unit reports identity (the
  sha256 admission fingerprint — env grant VALUES enter it only as digests),
  state, pid, health, reattached, restarts, generation, scrubbed last error, and
  last health-probe latency in microseconds. Every published error goes through
  `unitreport.ScrubError`, which redacts `*_TOKEN=`/`Bearer `/`op://` shapes. A
  missing, foreign-pid, schema-mismatched or >20 s stale snapshot renders as
  UNKNOWN, never as healthy. New L0 package `services/host/unitreport` owns the
  shared shape (supervise at L2 writes it; service at L1 and workflow/doctor at
  L3 read it).
- **`scripts/macos/verify-pix-lifecycle.sh`** — the machine-checkable half of
  host UAT, with real assertions and no unconditional PASS: command/flag surface,
  digest-suffixed sandbox naming, lease instance identity, attach fingerprint
  refusal, exit-code propagation, multi-shell teardown, `--keep` vs the orphan
  reaper, serve/doctor supervision JSON (including a no-secrets assertion),
  memory-unit restart across a SIGKILL with `:11435` never dropping, launchd
  respawn + mode-aware `serve stop`, and an opt-in external-OAuth pass. It greps
  itself for blast-radius removal flags, removes only sandboxes it created,
  asserts `$PWD` is unchanged, and reports SKIPs as an INCOMPLETE run (exit 2).
- **`docs/runbooks/host-services.md`** — the on-call runbook: golden-signal SLIs
  with SLOs and an error-budget policy, unit-field reference, alert-to-response
  table, recovery order, a toil ledger, and the postmortem trigger.
- **`TestMemoryListenerSurvivesUnitRestart`** (`services/host`) — a real
  process/socket/SQLite test that SIGKILLs the memory child and proves the three
  properties the design exists for: the listener stays bound, calls fail fast
  while no unit is dispensed (never hang), and the unit recovers with its data
  and a counted restart.

### Changed

- **The Flash tier moves to Gemini 3.7 Flash; 3.6 Flash is retired.** Google
  shipped 3.7 on 2026-08-13, three weeks after 3.6, better on every axis that
  decides a Flash role: Artificial Analysis index 56 (3.6: 52), 340 tok/s output
  (3.6: 225), TTFT 9.8 s (3.6: 19.0 s), Terminal-bench 2.1 85.8%, with coding,
  web dev and document reasoning the headline gains. The three Flash intents —
  `writing`, `verify`, `fast-balanced` — repoint to it and NOTHING else moved:
  `code` stays on Sonnet 5 and `breadth` stays on Flash-Lite. 3.6 is
  `available: false` (kept for audit, hidden from routing) exactly as 3.5 was.

  **The catalog carries Google's INTRODUCTORY price, $0.75/$3.75 per Mtok, which
  expires 2026-12-31 and reverts to 3.6's $1.5/$7.5.** That is deliberate — it
  is what a turn costs today — and it is a dated debt: every cost-objective
  intent is priced off that row, so it must be re-grounded before 2027-01-01 or
  the router will silently believe a Flash turn costs half what it does. The
  expiry is recorded in the model's `notes`, in the scorecard `_note`, and here.

  **Run `pix models add google` after upgrading.** Host availability is
  `catalog.Available && binding.Available` (`routing.RegistryForBindings`), so
  retiring 3.6 in the catalog drops it out of routing on a host that still has a
  probed 3.6 binding — and 3.7 is not in that host's roster until the roster is
  rebuilt. Until then the three Flash intents resolve off their preferred model
  or onto the policy fallback, which names a model the host cannot yet call.

- **Plain `pix` now launches.** It is `pix run` in the current directory:
  attach to that directory's sandbox if one is up, create it otherwise. The old
  behavior (always print status, never launch) charged every interactive session
  a second command to start working, for a guarantee only non-interactive
  callers needed. That half is kept and is now the stated invariant: an
  IMPLICIT launch requires a TTY, so with non-interactive stdin plain `pix`
  still prints status and `pix DIR` still refuses. `pix status` is the explicit
  spelling. Safety invariant 2 in AGENTS.md was rewritten, not deleted.
- **Personal context is editable from inside the sandbox, from a cold start.**
  `~/.local/share/pix/context` is now mounted read-write at its host path
  UNCONDITIONALLY (created if absent), and the mount is the context ROOT rather
  than its `skills/` subdir. Two bugs fell out of the old shape: the dir was
  mounted only when it already had entries, so the FIRST skill could never be
  written from inside a sandbox (nothing was mounted, so there was nowhere to
  write it), and mounting only `skills/` left the standing `AGENTS.md` invisible
  in-session. The mount set (`launch.MountDirs`) is now deliberately distinct
  from the skill set (`launch.LiveSkillDirs`): pi is pointed at
  `<context>/skills`, sbx mounts `<context>`. Net effect: the agent can write
  your skills for you, you can edit them mid-session, and the whole directory
  can be a git repo you commit from either side. `skills/` is live (`/reload`);
  `AGENTS.md` is inlined into a kit at launch, so edits to it apply to the next
  sandbox, matching how Claude Code reads `CLAUDE.md`.
- **Documented that instructions are context, not a fence.** README and
  `docs/reference.md` now state plainly that `AGENTS.md`, skills and pack context
  are guidance a model reads and can edit, and that enforcement is the sandbox
  boundary, the kit's network allowlist, and a `tool_call` extension returning
  `{block: true, reason}`. Pix ships no such extension, and the `guard` skill is
  explicitly a reminder rather than a gate, so neither is claimed as one.
- **`pix mcp` is three verbs: `add`, `ls`, `auth`.** It was six, and the extra
  three taught more than they did. `register` vs a native `sbx mcp add` was a
  distinction only the implementation cared about (both register a server; one
  builds the command for you, including the `op run` credential wrapper), so
  they are now one verb with three shapes: `pix mcp add <name> --url <url>`,
  `pix mcp add <name>` (a name pix knows how to build, or whose endpoint it
  already knows), and bare `pix mcp add` (everything in the config mcp list).
  `bundle` registered three hardcoded SaaS vendors and is DELETED, along with
  the whole sbx `mcp bundle` grammar-compatibility layer it existed to drive
  (~700 lines of code and tests). `load` (live-attach to a running sandbox) is
  DELETED: recreating is `pix rm BOX && pix run` in a stack whose sandboxes are
  disposable, and it wrote no receipt anyway. `mcp.McpCatalog` survives as what
  it always really was, a lookup table of endpoints pix knows, so `pix mcp add
  notion` needs no URL. The safety property that outlives the bundle: `add`
  fetches registration evidence once, classifies every name against it, and
  fails closed rather than overwrite a server registered at a different
  endpoint. Your `notion` is never replaced by ours.
- **`pix models route` is gone from the CLI.** Recompiling the intent map was
  never a user action (every `pix run` recompiles from current bindings), and
  the only real caller is a maintainer baking the image default. That is now
  `make routing`. Keeping a verb whose honest answer to "when do I run this?"
  is "never" taught a step that does nothing.
- **Web search defaults to Parallel.** The image bakes
  `~/.pi/web-search.json` with `provider: "parallel"`, so a host with
  `PARALLEL_API_KEY` set uses it without being asked. Pinning is safe unkeyed:
  pi-web-access falls through to the first available backend when the named one
  has no key, so an unwired host keeps the keyless providers.
- **`pix secret` help is generated from the key registries** instead of naming
  one key in prose. It describes the two CATEGORIES (model keys, tool keys) and
  lists the members from data, so adding a key cannot make the help stale and no
  single key reads as a special case. It also points at `sbx secret set` for
  keys the sandbox runtime holds directly, like GitHub.
- **`pix models add` no longer tells you to run `pix models route`.** It never
  needed to: every `pix run` recompiles the intent-to-model map from the current
  bindings and ships it into the sandbox, so adding a provider is complete when
  the command returns. `models route` writes the repo's baked default map and is
  a maintainer verb.
- **`pix doctor`'s `providers` line distinguishes a key from a callable model.**
  Keys live in the sbx secret store; routing resolves over probed bindings. A
  host with three keys and one binding reported three ready providers while
  every intent preferring another vendor silently fell back, which is how a
  fresh install ended up with a session model nobody chose and a fully green
  doctor. The line now reads `anthropic (openai, google: key set, no model wired
  - pix models add openai)`. Still READY: one callable provider is all a launch
  needs.
- **`pix secret` help names the two kinds of key.** Model keys wire to models
  with `pix models add`; tool keys (`PARALLEL_API_KEY`) buy a capability, route
  nowhere, and never block a launch.
- **Web-search backend keys are first-class.** `PARALLEL_API_KEY` is seeded,
  checked and mirrored into the sbx secret store exactly like a model key
  (`pix secret set PARALLEL_API_KEY op://vault/item/field` then `pix secret
  sync`; the value stays in 1Password). The kit injects it as the `x-api-key`
  header on `api.parallel.ai` and allows that host for egress, so only the proxy
  sentinel enters the VM. A new `secret.ToolKeyRefOrder` keeps capability keys
  OUT of `ProviderKeyRefOrder` and `ModelProviders`, so `pix models add` never
  offers a name it would reject and a missing search key can never refuse a
  launch.
- **README rewritten** around the current surface: no retired-verb section, pack
  detail cut to one example plus a link, and the daily-use commands re-verified
  against `pix help`.
- **pi bumped to 0.84.1**, and the interactive TUI now defaults to pi's
  `fullscreen` mode (`settings.json`: `tuiMode: "fullscreen"`). 0.84 splits the
  renderer into a main-screen and an alt-screen implementation; fullscreen owns
  its own scroll region, so the transcript scrolls with the mouse/`pageUp` while
  the editor and widgets stay docked, instead of the whole terminal snapping to
  the bottom on every repaint whenever a subagent streams. Flip it back per
  session or permanently from `/settings`.
- **`scripts/patches/apply-tui-bottom-pin.mjs` follows the 0.84 renderer split.**
  The main-screen `doRender()` moved from `dist/tui.js` to
  `dist/tui-main-screen.js` with its anchor and guards byte-identical, so the
  script now looks for the new filename first and falls back to the old one.
  Still idempotent, still non-fatal.
- **`jq`, `procps` (`ps`/`top`) and `file` are installed in the image.** `jq` is
  load-bearing, not a convenience: sbx injects its own `/usr/local/bin/xdg-open`
  that builds its JSON body with `jq -nc` before POSTing to the host's
  `_sbx/browser-open` endpoint. On a DHI base with no `jq` the shim died on that
  line and fell back to printing the URL, which is why links were never
  clickable from inside the sandbox. `BROWSER=xdg-open` is set so link-openers
  route through the same shim.
- **`overlord` carries a `max_cost_usd` of 0.30**, matching `strategy`. See
  Fixed.
- **doctor/status JSON is schema_version 5.** v5 adds the required top-level
  `supervisor` object; `RetiredSchemas[4]` records the migration (a v4 consumer
  keeps every field it knows). Corpus contract cases updated.
- `docs/HOST-UAT.md` rewritten around the script: what the machine proves, what
  only a human can judge, and the exit-code contract.

### Fixed

- **UAT's browser-capture timeout was diagnosed two different ways for the same
  hang, and turned `main` red.** When the capture window closes with nothing
  written, `realMCP.Auth` sits in a `select` on two cases that become ready at
  the same instant: the deadline (`captureCtx.Done()`) and the process wait. The
  command context derives from the same `ctx`, so an expired window tears the
  process down itself — whatever lands on `waitErrCh` next (a nil status, or
  `signal: killed` from a real sbx) is that teardown, not sbx giving up on its
  own. Go picks between ready select cases at random, so the identical failure
  reported as "sbx exited without writing capture URL" on one run and the
  browser-capture timeout on the next. It passed on the PR and failed on the
  push to `main`, taking both `test` and `publish` with it (publish gates on the
  same macOS job). The deadline now wins whenever it has passed, and both routes
  out of that state share one `errBrowserCaptureTimeout()` so they cannot drift
  again. The regression test drives the select 60 times per run — one iteration
  proves nothing here, which is exactly why this shipped green.
- **`pix setup`'s required `providers` row could never go green — it asked the
  key store for names the store does not hold.** It passed `ProviderKeyProbe` its
  own `ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY` list, but `sbx secret
  ls` lists a secret under the name it was STORED as, and the writer
  (`secret.setSbxSecret`) stores a provider key by PROVIDER name — `anthropic`,
  `openai`, `google`. Those two spellings never met, so setup reported "none of
  ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY is set" and exited non-zero
  on a host with all three keys wired, synced into sbx, and answering live
  requests. `pix doctor` and `pix run`'s launch gate were already correct: both
  pass `secret.ModelProviders`, which setup now passes too. Setup's own comment
  claimed this was "the same probe `pix doctor` reports from, never a second
  implementation of the same classification" — the second implementation was
  that argument. The unit test agreed with the code (`echo ANTHROPIC_API_KEY` →
  ready) and neither agreed with the store, which is why it shipped; the fixture
  now speaks the store's vocabulary and a new test pins the real `sbx secret ls`
  format.
- **`pix setup` never asked for a provider key, so a host with no pack could not
  finish it.** `providers` is a REQUIRED capability, and setup's only response to
  a missing one was to print `pix models add anthropic` and exit non-zero — a
  repair it knew exactly how to perform, described rather than offered. It read
  as correct for as long as it did because a pack with managed inference
  satisfies that row KEYLESSLY, so on the hosts being exercised the gap never
  appeared; a vanilla `pix setup` in a pack-less checkout hit it every time. The
  `providers` step now carries an Apply that runs the SAME `pix models add`
  interview (injected through `provision.Composition`, like pack adoption —
  provision still may not import the workflow that performs the step), so the
  ref, the binding rebuild, the live probe and the sbx sync all stay in the one
  command that owns a credential. Setup only asks WHICH provider, and the second
  check is still the only thing that reports readiness. It is skipped entirely
  with no terminal, under `--yes` (which suppresses questions, never answers
  them), and on a keyless host — and a declined prompt is now a SKIP rather than
  a failure, via the new `provision.ErrSkipped`, so "not now" reports as the
  choice it was instead of aborting the run or claiming a repair that never
  happened.
- **`pix setup` sat silent for up to sixteen seconds.** Both check rounds are
  concurrent and each probe gets the full 8 s budget, so a round costs as long as
  its slowest probe and printed nothing until it was over — indistinguishable
  from a hang. `health.RunWithProgress` reports each probe the moment it answers,
  and the loop narrates both rounds to a terminal (name, verdict via the shared
  `health.Glyph`, elapsed). A redirected stream still gets the report alone, so
  nothing that parses setup's output has to learn a new preamble.

- **`TypeError: Math.sumPrecise is not a function`, repeatedly, in any session
  that read a PDF.** pi reads PDFs through `unpdf`, which bundles a pdf.js
  recent enough to call `Math.sumPrecise` when summing glyph and font-table
  sizes; the sandbox runs Node v25.9, whose V8 does not expose it (and does not
  behind `--harmony` either). `unpdf` is installed at RUNTIME into
  `~/.pi/agent/npm`, so the build-time patching used for pi-manage-todo-list
  cannot reach it — the fix is `extensions/math-sum-precise.ts`, which supplies
  the method at pi startup, before any PDF is opened, and never replaces a
  native implementation. Neumaier compensation, so the integer sums pdf.js
  actually does are exact; non-finite inputs are tallied rather than summed,
  because feeding an infinity through the compensation term returns NaN where
  the real method returns Infinity.
  This also fixes the font warnings that FOLLOW it (`Cannot substitute the font
  because of its name: DAAA+Gallix-Bold`), which look unrelated and are not:
  the failing calls are in pdf.js's font writer, so a thrown TypeError there
  makes it fall back to substitution, and a subsetted name (the `DAAA+` prefix)
  matches no standard font, so that fails too. Measured on the real image
  against a real PDF: six warnings without the polyfill, zero with it.

- **`pix ls` says who is holding a sandbox.** A new HELD BY column reads the
  live-reference lock — `session` when a `pix` process on this host is still
  attached, `—` when
  nothing holds it, `?` when the lease state could not be read (never `—`,
  because the free value promises teardown will remove the box). Without it, a
  host with several surviving sandboxes reads as "teardown-on-exit is broken"
  when the truth is usually the opposite: teardown ran, found a live reference,
  and correctly kept the box. That evidence existed only in
  `~/.local/state/pix/teardown.jsonl` and a lock nobody could see. The column
  answers from the LOCK, never from a PID, because a PID is reused the instant
  its owner exits.
- **`pix doctor` no longer prints the MCP host-trust footer.** It appeared on
  every run, and a paragraph a user reads once and then skims forever is not a
  control — it is the thing that teaches people to stop reading the bottom of
  the report. The property is unchanged and still documented in `SECURITY.md`:
  local and container MCP servers run on the host, outside the sandbox, with
  your privileges, and what they return can reach your model provider. The
  constant, its render, and the tests pinning it to that surface are removed;
  the gworkspace skill's own disclosure is untouched.
- **`pix doctor`'s launchd row said `✓ agent loaded` about an agent that could
  never start.** Loaded and able-to-start are different questions and it only
  asked the first, so the one surface that could have named the stale-plist bug
  above reported green throughout three days of chasing it. The row now reads
  `program =` out of the launchctl output it already had and checks that file
  exists, reporting a gap with `pix serve install` and naming the missing path
  when it does not. launchctl output that never mentions a program stays READY:
  silence is not evidence of absence.
- **A `brew upgrade` permanently broke the pix LaunchAgent, and every pack
  change then stalled on it.** `pix serve install` resolved the binary through
  its symlink before writing the plist, so a Homebrew install baked
  `…/Cellar/pix/<version>/bin/pix-host` — a path the NEXT upgrade deletes.
  launchd keeps the job, cannot spawn it (`last exit code = 78: EX_CONFIG`),
  parks in `spawn scheduled`, and `launchctl kickstart -k` — which pix runs
  after every pack change — blocks forever on a job that can never start. On a
  real host this presented as three separate "`pix setup` hangs" over two days,
  with an agent pinned to 0.1.44 while only 0.1.54 was installed. The supervised
  path is now the stable one: absolute, symlink INTACT, because a symlink is
  precisely the indirection a package manager maintains across upgrades. If your
  agent is already broken, `pix serve install` rewrites it. - **`pix pack use` could hang forever after registering, printing nothing.**
  Two unbounded waits, both in the post-commit side effects — the one part of
  that command explicitly allowed to fail with a note rather than take the
  terminal with it. The cross-process flock (`sys.Lock`) was a plain blocking
  `LOCK_EX`, so a lock another pix process held or leaked stopped the wrapper
  refresh dead; it now polls `LOCK_NB` to a 30s deadline and fails with an error
  naming the lock file, while ordinary contention still acquires as before. And
  every service-control child (`launchctl kickstart -k`, which kills the daemon
  and waits for it to die) now runs under a 20s budget with a `WaitDelay`, so a
  `pix-host serve` wedged in shutdown produces the "could not restart the
  managed pix service — restart it manually" warning `PropagateConfig` always
  had ready and could never reach. A wait that lasts more than a second also
  SAYS so now (`waiting for another pix process to finish (lock: …)`), because a
  bounded wait that prints nothing is still indistinguishable from a hang — a
  real `pix setup` sat silent for ten seconds while working correctly. An
  uncontended lock stays quiet.
- **`pix setup` registered a pack's MCP servers before running the setup hooks
  that install them.** A pack's hooks build the very commands its servers are,
  so on a first install those binaries do not exist yet and registration cannot
  work. The first attempt at this failed outright, telling the user to run the
  setup step that its own failure had just made unreachable. The second
  registered anyway, warned that a command was "not on PATH", noted a step the
  user had not reached, and retried twenty seconds later — which worked, and
  made every first install read a warning about a missing binary during the run
  whose whole job was to install it. Neither is a fix; the order was. `pix
  setup` now adopts, runs the hooks, and registers once, at the end, with
  nothing to say about it. `pix pack use` on its own still registers inline —
  it runs no hooks, so a missing command there is a real answer rather than a
  stage in a sequence. Registration still happens when a hook FAILS, so an
  unrelated broken step (an expired grant, a bad OAuth scope) does not cost a
  pack the servers that were ready.
- **`/todos` claimed to toggle the task widget but only refreshed it.** The
  build-time `pi-manage-todo-list@0.4.0` patch now makes bare `/todos` toggle,
  adds explicit `/todos hide` and `/todos show` controls, and binds `Alt+T` as a
  safe shortcut (plain `t` would steal normal typing). Hiding never changes the
  task list, hidden state survives reloads and session-tree navigation, later
  writes stay hidden, and `/todos clear` remains durable across resume. New
  clears use the canonical `pi-stack-todo-cleared` marker; both task restore and
  compaction still honor the earlier `pix-todo-cleared` marker.
- **The default session model silently became Fable 5 on any host where OpenAI
  is not wired.** `overlord` is the shipped `run_intent`, so whatever it
  resolves to is the interactive model. Its `prefer_providers: [openai]` is a
  PREFERENCE, not an allowlist, so an Anthropic-only host correctly falls back
  to the best model overall — and with no cost ceiling that was Fable 5
  (reasoning accuracy 0.94 vs Opus 5's 0.93), the frontier model deliberately
  reserved for `red-team`, installed as the default at roughly twice Opus's
  per-task price without anyone choosing it. `overlord` now carries the same
  0.30 per-task cap `strategy` uses, which excludes Fable (~$0.50) and lands the
  fallback on Opus 5 (~$0.25). The preferred OpenAI route on a fully wired host
  is unchanged. Pinned by `routing/overlord_fallback_test.go` in both
  directions.
- **Subagent children could outlive the session and keep burning CPU.** Every
  child is spawned `detached` so a watchdog can signal the whole process group,
  but that group also survives its parent, and both watchdogs are `setTimeout`s
  that die with the session. A subagent still running at exit therefore became
  an orphan with no wall-clock cap left to stop it, and sessions accumulated
  them — the "CPU climbs after a long session" symptom. `extensions/subagents.ts`
  now tracks every live process group and reaps it from both `session_shutdown`
  and a synchronous `process.on("exit")` hook. Covered by
  `tests/subagent-child-reaper.test.mjs`, including the grandchild case.
- **sbx v0.38 CLI-grammar compatibility.** `pix doctor`/`pix status`'s sbx probe
  now falls back from `sbx --version` to `sbx version` when a newer sbx build
  rejects the root `--version` flag with a recognized usage error (evidence
  says so explicitly); a denied, timed-out, or otherwise-failed probe never
  retries. `pix mcp bundle`'s default `add` now tries the current `NAME --url
  URL` grammar and falls back to the positional `NAME URL` form on the same
  narrow signal. `pix mcp register`'s manifest/remote-URL container
  registrations instead decide their `--url`-flag-vs-positional grammar up
  front from a read-only `sbx mcp add --help` probe, never from a failed
  attempt, because a remote registration can open an interactive OAuth grant
  a retry must not repeat. New `sys.IsUsageMismatch` is the one shared
  classifier every one of these call sites gates its retry on: an
  unknown-flag/unknown-command/wrong-arity signature from the invoked CLI's
  own parser, never an auth/policy/operational failure.
- **`pix monitor` no longer blocks forever on an empty or absent store.**
  `pix monitor --json | head -5` used to hang indefinitely, because `Run`
  always ran the polling `Follow` loop and `NewStore` silently created the
  (empty) store root just by being asked to read it. Fixed: a new read-only
  `monitor.OpenStore` never creates the root, and a non-interactive run (a
  pipe, a script) now defaults to one-shot (`monitor.Once`: print whatever
  is already stored and exit) instead of live-follow. Nothing stored AND no
  `pix-host serve` running now exits 3 with an actionable message instead of
  hanging or printing nothing; nothing stored while an ingest listener IS up
  is still quiet, honest success. An interactive terminal keeps the old
  live-follow default but now prints an honest banner first. New
  `--follow`/`-f` flag forces streaming explicitly either way. See
  `docs/design/monitor.md`.
- `lib/recall-message.ts` no longer documents the deleted knowledge store
  (`:11436`, `KNOWLEDGE_CHAR_BUDGET`, `/knowledge`) as live behaviour; the
  per-channel budget rationale is restated for the store that actually exists.
- **`scripts/macos/verify-pix-lifecycle.sh` false failures.** The OAuth pass no
  longer reads its confirmation prompt from the script's own stdin (a
  background/CI run with stdin closed hung or reported a false FAIL); it now
  asserts `pix mcp auth --all`'s own exit code, certifies completion against a
  machine-readable probe (`pix doctor --json`'s per-server
  registered/authenticated evidence), and treats an optional human
  confirmation — bounded, `/dev/tty`-only — as SKIP, never FAIL, when no TTY
  answers. The `mcp ls` "registration, not attachment" check no longer greps
  for the bare substring `attached`, which always matched `mcp ls`'s own
  honest disclaimer ("not what's attached to...") and so could never pass; it
  now asserts the disclaimer positively and checks for a precise present-tense
  attachment claim instead. The script tracks and restores any MCP catalog
  bundle it registers, redirects every backgrounded `pix run`'s stdin from
  `/dev/null`, bounds every `wait` on one (`bounded_wait`), and preflights
  WHICH `pix-host` binary an already-running `serve` is before trusting it —
  refusing with the exact repair command on a stale/unmanaged mismatch instead
  of silently certifying it.

### Legal / release

- **Phase10 legal blockers closed (B1–B4).** MPL-2.0 disclosure is now real,
  not asserted: `licenses/MPL-2.0.txt` ships the full verbatim text in the
  image and the Homebrew tarball, each MPL component records a Source Code
  Form URL pinned to the exact linked version (go-plugin v1.8.0, yamux
  v0.1.2), and the notices no longer both claim and deny that license texts
  are reproduced. `LICENSE` now names **Docker, Inc. and the pix
  contributors**, with the DHI-redistribution and employer-IP basis recorded
  durably in `docs/legal/AUTHORIZATIONS.md` (A-1/A-2, each listing what it
  explicitly does NOT cover) and `CONTRIBUTING.md`/`NOTICE.md` stating
  inbound = outbound MIT. `LICENSE` + `licenses/` now ship in the image and
  the tarball (MIT s2, MPL-2.0 s3.1). `publish.yml` exports the published
  manifest digest, runs `verify-provenance.sh` against it in a **blocking**
  `provenance` job, and generates the SBOM against that published digest;
  `continue-on-error` is gone from `legal.yml` (SBOM *diffing* remains
  explicitly ungated in `docs/legal/FINDINGS.md` #7). New
  `docs/legal/PRIVACY.md`. All of it gated by
  `scripts/check-third-party-notices.sh` +
  `tests/legal-authorizations-and-privacy.test.mjs`.

### Docs

- **U12: final public docs/release sync against delivered code.** `AGENTS.md`'s
  repo-layout table, go-plugin host architecture section, and the agent/model
  bullets described a stale shape (knowledge (:11436) + a credential broker as
  live `serve` slots, `pix agent new/edit/rm/reassess` as a live CLI, host mode
  gated by `host.enabled` rather than deleted) that no longer matches the code
  — corrected in place, with safety invariant #9 rewritten from "off by
  default" to "retired outright" (`tests/agents-md-invariants.test.mjs`
  updated to match, same stable id). Added `docs/getting-started.md` (first
  session, end to end), `docs/design/lifecycle-trust.md` (sandbox lifecycle +
  pack trust in one place), `docs/MIGRATION.md` (retired-verb table +
  upgrade/host-mode/knowledge migration notes), and `docs/HOST-UAT.md` (the
  exact host-side verification script + in-sandbox prompt, since an agent
  cannot run `make load`/`make run` from inside a sandbox). Marked
  `docs/design/rearchitecture.md` and `docs/design/host-mode.md` superseded
  in place (banner only — the historical prose is unedited). `NOTICE.md` now
  states explicitly that a DHI entitlement is the operator's own, never
  requested or recorded by pix. No application logic changed.

### Changed

- **U11m: `pix agent` cut to `ls` only — `new`/`edit`/`rm`/`reassess` retired.**
  Interactive authoring (`launchInteractiveAuthoring`, a `pi` re-exec), YAML
  frontmatter mutation (`agentNew`/`agentEdit`/`agentRm`, nine flags across two
  handlers, a yaml.Node round-trip), and the reassess host-exec wrapper
  (`repoRoutingTarget`/`readCompiledRoutes`/`resolveRoster`, a `route compile`
  passthrough duplicating what `pix models route` already does directly) are
  gone. `pix agent ls` (table + `--json`) is the entire surviving surface —
  same resolved-model-and-WHY roster read `subagents.ts` depends on
  independently (it reads `agents/*.md` itself and never shelled out to this
  command). Authoring/editing/removing an agent is now a hand-edit of its
  `agents/<name>.md` frontmatter, same as always for its prompt body; a new
  intent's scores go in `scorecard.json` by hand, then `pix models route`
  recompiles `routing.json` and a sandbox relaunch picks it up. Typing
  `new`/`edit`/`rm`/`remove`/`reassess` answers with the standard
  `PIX_RETIRED` notice (exit 2, no side effect) naming that path — five new
  entries in `retired.go` + `corpus/retirement.jsonl`, proved end to end by
  the existing real-binary retirement harness
  (`corpus/retired_dispatch_test.go`), no shard changes needed. Net: `agent.go`
  580 -> 225 prod LOC, `agent_cmd.go` 74 -> 33; two now-obsolete redrive
  regression tests (`TestAgentNew_NextStepsPointAtLiveScorecardPath`,
  `TestAgentReassessModel_PointsAtLiveScorecardPath`) removed with the
  surfaces they pinned. Shrink-only: no budget ceiling raised.

- **U11k: `cmd/pix/corpus` reclassified as test-only support (588 -> 0
  production LOC).** The golden CLI corpus + retirement-manifest harness had
  no runtime caller — it exists solely to be driven by `go test
  ./cmd/pix/corpus`, exercising the real compiled `pix` binary as a
  subprocess — so its four implementation files (`loader.go`, `regen.go`,
  `retirement.go`, `runner.go`, plus `types.go`) are folded into their
  matching `*_test.go` files (`types.go` simply renamed to `types_test.go`,
  no collision). Every `.go` file under the package is now a `_test.go` file:
  `go test ./cmd/pix/corpus` and the full golden-corpus run (CI's `metrics`
  job) are unchanged and still green, including the sharded corpus, the
  append-only retirement-manifest checks, and the real-binary behavioral
  tests. Folding in the duplicate `buildPixBinary`/`BuildPixBinary` wrapper
  pair into one `sync.Once`-cached test helper avoided growing total test
  LOC while merging: the package's combined line count actually shrank
  588+863=1,451 -> 1,421. `arch_test.go`'s `pkgLayer` map and
  `scanPackages` now skip packages with zero non-test `.go` files (a
  package that is only tests has no layer to place), and
  `scripts/arch-metrics/main.go`'s `scan()` mirrors that rule for the same
  reason, so `cmd/pix/corpus` is gone from both the architecture layer map
  and `scripts/arch-metrics/budgets.json` — not zeroed, removed. Both
  changes are shrink-only: `pix/host` and every other package's recorded
  budget ceiling is unchanged.

- **U07b: `pix-host backup`/`restore` collapse into `pix-host memory
  snapshot`/`memory restore`.** The multi-component hot archive (a versioned
  `tar.gz` carrying `memory.db` + `config.toml` + `op-refs.env` + a
  `manifest.json`, with retention/pruning, tar-bomb guards, a plain-file
  install/rollback stack and its own atomic-write helper — 1,610 lines across
  `memory_backup.go`/`memory_restore.go`) is gone. What replaces it is ONE
  artifact: a snapshot is a plain sqlite file written with `VACUUM INTO`
  through a read-only handle (`pix-host memory snapshot PATH`, hot, verified,
  `0600`, never clobbering), and the restore primitive
  (`pix-host memory restore PATH [--force]`) installs one with the service
  STOPPED — enforced by the same advisory store flock the daemon holds, taken
  first and held across the commit, with the previous db and its sidecars kept
  in a reversible `.bak-<ts>-<rand>` set. `config.toml` is reproducible with
  `pix config set` and `op-refs.env` holds `op://` pointers, not values, so
  neither needed an archive format. The DB path, schema and `0600` mode are
  unchanged; the retired top-level `backup`/`restore` verbs answer with
  `PIX_RETIRED` naming the new commands. Documented in `docs/memory.md`.

- **U05b: monitor ingest ownership moved under `pix-host serve`; `pix
  monitor` is now a pure offline reader.** The loopback ingest listener that
  receives NDJSON events from the in-VM monitor tap (`services/host/monitor`,
  `:11437`) used to be started only by `pix monitor` itself. It now composes
  directly inside `runServe` (`services/host/serve.go`), alongside memory,
  gated by the same `services` config/CLI mechanism (`serveServiceAliases`
  gained a `monitor` entry) — `pix config set services monitor`, or the
  existing "empty config means all" default, enables it. `--bind`/`--port`
  moved down with it: they are `pix serve`/`pix-host serve` flags now, not
  `pix monitor` flags. `pix monitor` (`services/host/cmd/pix/monitor.go`)
  lost its listener entirely — `[name] [--path DIR] [--json]` only — and
  tails the new `config.MonitorStoreRoot()` (`<state-dir>/monitor`), the same
  root `serve` writes to, so a reader works whether or not serve is running
  right now. `pix status`/`doctor` still see ingest via the existing
  `monitor.DefaultPort` dial, unchanged by who started it. The wire schema,
  the `:11437` loopback-only bind default, and the kit's `PIX_MONITOR`/
  `PIX_MONITOR_URL` env contract and network allowlist entries are untouched.
  See `docs/design/monitor.md`.

### Removed

- **pix's host is macOS-only now.** Deleted the `systemd --user` managed-
  service implementation (`serve_install_linux.go`, the `pix-serve.service`
  unit generator + template + tests), the Windows lock/process/service/
  credential shims (`lock_windows.go`, `service/ctl_windows.go`,
  `service/start_windows.go`, `slackoauth/*_windows.go`, `sys/lock_windows.go`,
  `workflow/gworkspace/gog_credentials_snapshot_windows.go`), and every
  install/upgrade/release path that shipped a Linux `pix`/`pix-host` binary
  (`install.sh`, the `publish.yml` release-binaries matrix). launchd-managed
  `pix serve install`/`stop`/PID ownership/spawn-lock/plist validation are
  unchanged. `services/host` still `go build`/`go test`s under `GOOS=linux` —
  the pix sandbox IMAGE stays a Linux container and devs hack on this repo
  from inside one — via a single non-darwin compile stub
  (`service.ErrUnsupportedHost`) with no lifecycle behavior. See
  `docs/design/serve-lifecycle.md`.

### Fixed

- **`pix reset` asked a PORT whether the daemon was running, and got it wrong
  in both directions.** The pre-stop "was it up" answer and the post-stop "is it
  down" proof were both a `MEMORY_PORT` health dial, so a daemon whose memory
  service was disabled (monitor-only) or had crashed read as DOWN: reset stopped
  it and never restarted it, and — worse — it moved `~/.local/share/pix` out from
  under that still-live process and deleted the pidfile that was `pix serve
  stop`'s only handle on it. A stop that FAILED or refused an unverifiable pid
  was equally invisible: its error was printed and the destructive steps ran
  anyway. Both questions are now asked of the daemon's IDENTITY
  (`service.ServeIdentityUp`: a loaded managed unit, or a pidfile naming a live
  process that is not provably a stranger's), the same ownership answer
  `serve stop`/`serve status` already share. A daemon that cannot be PROVEN dead
  blocks the data move, keeps its pid/lock files, and is not "restarted" behind
  its own back; `--force` still overrides the data move, but never the runtime
  files. Reproduced by real-process/real-pidfile tests
  (`workflow/reset/reset_process_test.go`) that fail against the old probe.

- **The model router described the shipped catalog, not your host.** `pix
  models show|ls|pick|route` (i.e. the whole `pix-host route` tree) loaded
  `models.json` and nothing else, so its `AVAIL` column reported "Pix ships
  support for this" under a name every user reads as "you can call this" — and
  every intent resolved against it. On a host with no OpenAI key, `pix models
  show` reported the default intent routing to `openai/gpt-5.6-sol`, and `pix
  models route` WROTE that into `routing.json`, which host-mode subagents read.
  The binding-aware resolve already existed but lived in the launcher, out of
  the host binary's reach. It is now `services/host/inference` and both
  binaries share it. `AVAIL` becomes `STATUS` (`wired` / `unwired` /
  `retired`), an intent with no callable model is DROPPED from the compiled map
  and named rather than pointed at an unreachable provider, and `--catalog`
  restores the host-independent view for baking the image default.
- **A declined model download made `pix setup` exit non-zero.** Choosing Ollama
  local and answering "no" to the multi-gigabyte pull bound a candidate,
  probed it, and — since the weights were not on disk — hard-failed with
  "ollama models are bound, but none answered a request", contradicting the
  documented contract that declining is a decision, not a failure. The consent
  check now precedes that error. It was invisible because the covering test
  wired no probe at all (see below).
- **Verification could be silently switched off.** `verifyDirectInference` /
  `verifyOllamaInference` returned `0 attempted, 0 verified, no failures` when
  handed a `shellEnv` with no probe function — a value indistinguishable from a
  clean pass, so callers printed "0 model(s) answered a live request" and
  exited 0. They now return `(probeOutcome, error)` with an explicit
  `errNoProbeSeam`, which is what surfaced the setup bug above.
- **Three `pix doctor` tests failed on a clean checkout** because they read the
  developer's real `~/.config/pix/op-refs.env`. A `TestMain` guard now points
  the launcher package's config resolution at a temp dir.

### Added

- **Trusted pack `[[services]]` now wire into the supervisor (U07d).** The
  pack side exports exactly one seam, `pack.AcceptedGoPluginServices`: the
  minimal normalized view (name, activation, absolute path, sha pin, argv,
  env reference names, loopback front door) of a pack's go-plugin services —
  and ONLY after the pack's current host-exec surface matches the fingerprint
  accepted at the Tier-1 gate, so consent strictly precedes any staging or
  start. The supervisor side (`pack_units.go`) is the integrator hook:
  `packUnitSpec` (view → `supervise.NewExternalUnit`, dispense kinds limited
  to the closed `plugin.PluginMap` set) and `supervisor.reconcilePackUnits`
  (add-only, collision-safe; never replaces a running unit). Root `serve`
  composition is untouched — nothing calls the hook yet. Reserved-port,
  reserved-name, loopback-only, and env-names-only rules are re-validated at
  export, and the staged binary is re-hashed against the consented pin on
  every start, so a binary swapped after acceptance is refused at launch.

- **`pix models add ollama`** — the keyless half of `models add`. `models add`
  derived its provider list from `providerKeyRefOrder`
  (anthropic/openai/google), so the one backend that needs no credential had no
  post-setup path at all: pulling a new local model or gaining a cloud
  entitlement meant re-running `pix setup`. It reads what the daemon lists,
  proves each with a real generate, and widens the roster. `--local` /
  `--cloud` narrow it; the default is both. Downloads nothing — it names a tag
  worth pulling and leaves the decision to you. An explicit `models add
  <provider>` now also widens the roster for a provider already recorded in
  `roster_providers`, without which the SECOND add of any provider bound and
  probed models that then sat outside the roster while the command reported
  success.
- **Sandbox sessions no longer warn `No models match pattern "ollama/…"`.**
  `pix run` passes pi a `--models` cycle built from every callable binding,
  but `extensions/ollama-bridge.ts` registered exactly one hardcoded model. It
  now registers what the host's generated `inference.json` declares, with the
  configured bridge tag guaranteed present.

### Retired

- **The direct `[plugins.*]` config declaration is retired and inert (U07d).**
  A config.toml can no longer name an executable for `pix-host` to launch:
  every declared `plugins.<slot>` is swept at load into the same
  `RetiredKeys` notice surface as other stale keys (shown by `doctor`/
  `config show`), `Config.Plugin()` always answers builtin, and the
  `[plugins.mcp]` MCP-bridge override path is gone. External service units
  are pack-trust-admitted `[[services]]` declarations only (AC-SUP-05).

### Changed

- **`shellEnv` is gone; OS seams live in `services/host/sys`.** It was 22
  nullable function pointers threaded through 254 functions and guarded by 125
  hand-written nil checks that disagreed with each other — for `env.run == nil`
  alone the package held fourteen distinct behaviours, and three shipped bugs
  came out of the gap. `sys` splits the seams into four interfaces by what they
  touch (`Exec`, `FS`, `Env`, `Net`), so a signature says what a function can
  reach; `sys.Real` holds no nullable state, which is what let the guards be
  deleted rather than rewritten (**125 -> 11**, all 11 on domain probes that
  leave in a later phase). Nullability survives only in `sys/systest.Fake`,
  where an unwired method fails loudly instead of returning a zero value that
  reads like an answer. Net **-623 lines**. No user-visible behaviour change,
  but several fixtures were found to have been testing paths no user took —
  see docs/design/rearchitecture.md.

- **An intent's `providers` list is a PREFERENCE, not an allowlist**, and is
  spelled `prefer_providers` in `policy.json` (the old key still loads, with
  the new semantics). The resolver ranks by objective and then floats preferred
  vendors to the front, so a preference can reorder the feasible set but never
  exclude the last usable model, and an unreachable vendor is reported via
  `Decision.PreferenceMet` instead of `ConstraintsMet: false`. The relaxation
  ladder loses its provider rung — the code implementing it already said
  "vendor diversity is a PREFERENCE encoded as a constraint". This mattered
  because the shipped policy pins `overlord` (the interactive orchestrator and
  the default `run_intent`) to OpenAI while `pix setup` wires Anthropic: every
  default install resolved through the ladder and reported `FALLBACK` on its
  most important route while working perfectly. A genuinely hard vendor rule
  belongs in `inference.exclusive_backend` / `exclusive_source`, which enforce
  it at the binding layer where an excluded vendor is uncallable rather than
  merely outranked.

- **Renamed `pix route` to `pix models`** (docs/design/models-cli.md): the
  noun a user actually wants ("what models can pix use, and which are wired
  up") replaces the mechanism it was filed under. `pix models ls|show|pick`
  and the mutating `pix models route` (`--out PATH`; `models compile` stays as
  an undocumented muscle-memory alias) are thin passthroughs to the unchanged
  `pix-host route` subcommand tree — nothing on the host side moved. Bare
  `pix models` is a new read-only status screen: runtime, bound providers,
  the roster, and the resolved session model, ending with a `Next:` line.
  `pix route` keeps working for one release as a hidden alias, printing a
  one-line deprecation to stderr only (stdout/`--json` unaffected);
  `retiredVerbs["route"] = "models"` (help.go) is permanent and is what a
  typed `pix route` resolves to after the alias is removed. `pix models add
  <provider>` and `pix models setup` — the fix for "I can't find how to add a
  second provider key later" — land in a follow-up change; this rename only
  builds the verb tree and leaves the extension point.

### Removed

- **The pre-public pack-directory migration is gone** (`~/.local/share/pix/pack`
  / `.../personal` -> `.../default`, its manifest `name` rewrite, its
  trust-path migration, and the stale-state repair pass that cleaned up after
  an older non-transactional version of itself). Those two directory names were
  only ever written by pre-0.1.0 builds under the OLD product name, and 0.1.0
  was a clean pre-launch cutover with no legacy-path discovery — so on every
  released build the migration probed for directories no pix build creates,
  and `DefaultPackRoot()` did a stat, a flock, a config load and a trust-store
  load to decide "nothing to do". It now returns the path. What is kept, and
  tested: the bare `pix pack use personal` token remains a deprecated alias for
  the default pack (a CLI spelling, not a path probe), and a directory that
  happens to be named `personal` or `pack` is now left strictly alone —
  never renamed, never rewritten, never repointed. -525 lines of production
  code, and the pack package is back under its pre-`[[services]]` budget.
- The `[[services]] UnitSpec` vocabulary (runtime/activation constants, the
  reserved name+port sets, and the value-shape patterns) is one internal value
  instead of ten package-level names, and `packService`/`packServiceResources`
  are unexported: no supervisor consumes a service declaration yet, so the
  exported surface it will need is the supervisor story's to earn. Manifest
  syntax, every rejection, the consent screen, and the fingerprint are
  unchanged.

## 0.1.0 - 2026-07-25

### Breaking

- Renamed the product, CLI, host binary, image, Go module, sandbox namespace,
  runtime paths, workspace markers, environment variables, services, and public
  documentation to Pix. This is a clean pre-launch cutover with no legacy-path
  discovery or compatibility aliases.

### Added

- Added one evidence-backed readiness model shared by setup, doctor, status,
  run, onboarding, Ollama checks, MCP checks, and JSON output.
- Added transactional, resumable setup and optional Google Workspace commands:
  `pix gworkspace setup|status|disable`.
- Moved dynamic memory and knowledge recall to append-only messages, preserving
  provider input-prefix caching with deterministic deduplication and byte caps.
- Added a prescriptive installation path, installer collision checks, absolute
  test timing budgets, workspace marker round trips, and rename guards.

### Changed

- Reduced the warm non-race gate from roughly 42 seconds to under 10 seconds.
- Made isolated worktree parallelism the default orchestration policy for
  independent delivery units.

### Fixed

- **`pi-stack gog setup` now reads current sbx registration tables.** Newer sbx
  builds expose local MCP commands in the plain `sbx mcp ls` table while
  omitting the older `mcp get` and JSON detail forms. Gog setup now parses that
  complete local command as a final bounded fallback, so an existing readable
  gog registration no longer blocks OAuth preflight as "unverifiable."
- **Custom sandbox names survive `mcp load` and `doctor`.** The create receipt
  now records the canonical workspace it was created for (additive schema
  field), and a hardened workspace→sandbox resolver lets `pi-stack mcp load
  NAME [DIR]` and doctor's workspace context find a `run --name pi-stack-demo`
  box again instead of deriving `pi-stack-<basename>` and missing it. A
  positively-clean "no mapping" scan still falls back to the derived default
  (old sandboxes), while an ambiguous or corrupt/tampered mapping refuses
  (`mcp load`) or renders unverifiable (doctor) — never targets an arbitrary
  box. `pi-stack reset --sbx` now clears each positively-removed sandbox's
  receipt through the same hardened helper (a failed removal retains it).
- **gog attachment is no longer claimed from config membership.** Doctor's gog
  "attached" check now reads the same receipt-backed join row as every other
  MCP server: a sandbox created before gog was configured reads
  registered-not-attached (with the exact `pi-stack mcp load gog <workspace>`
  command), not ready. Without a sandbox context, config membership renders as
  intent, never attachment.
- **`status` headline honesty:** an unverifiable per-sandbox MCP row (corrupt
  or absent receipt, failed listing) no longer reads "all systems go" — status
  says some checks are unverifiable without inventing a false TODO. Doctor's
  verified registered-not-attached gap is now an optional TODO with the exact
  load command, consistent with status.

- `pi-stack host` now launches its interactive session under `op run
  --no-masking`. op's default output masking pipes pi's stdout/stderr through a
  filter, which makes them non-TTYs, so pi's TUI then saw no terminal and exited
  immediately (banner, then straight back to the shell, exit 0). Non-interactive
  paths were unaffected, which is why it looked like a silent failure. (The
  mcp-gateway op-run path already used `--no-masking`.)
- Provider-key op:// refs are stored with **literal spaces** again. An earlier
  change percent-encoded spaces (`Anthropic%20API%20Key`) on a false premise;
  op 2.35.0's `op read` AND `op run --env-file` both reject `%20`, so any
  1Password item whose name has a space (very common) failed to resolve and
  `pi-stack setup` aborted. Existing encoded refs self-heal: they're decoded on
  read and rewritten literal on the next write.

### Added

- **`pi-stack doctor` now reports four verdicts, not two**: `ready`, `todo` (a
  verified, fixable gap with the exact command), `unverifiable` (a probe
  timed out or the tool needed to check isn't available; never counted as
  broken), and `denied` (an explicit policy/permission refusal). Exit codes
  follow: `2` on a usage error, `1` only for a positively verified core
  failure (a resolved key for any one of Anthropic/OpenAI/Google, or the
  config itself failing to load), `0` for everything else, including every
  optional or unverifiable gap. `doctor --json` gained `schema_version` (now
  `2`) so a script can tell the shape apart from an older run.
- **`pi-stack gog setup`**: guided, version-aware Google Workspace onboarding.
  It detects which auth subcommands your installed `gog` supports, imports
  your OAuth client, authorizes your account with explicit read-only scopes,
  verifies the exact headless command the sbx gateway will spawn (not just
  interactive auth), and only then registers gog with the gateway and saves
  config, rolling the registration back if the config write fails. Replaces
  hand-running the raw auth + manual `config set`/`mcp register` steps as the
  documented path; `doctor` and `status` now point here for any gog gap.
- **Per-sandbox MCP status, backed by launcher receipts.** `pi-stack status`
  and `pi-stack doctor` now report one of five states per configured server
  per running sandbox: `preloaded` (shipped at create), `loaded` (attached
  live via `pi-stack mcp load`), `registered-not-attached` (known to the
  gateway, but no receipt for this sandbox), `not-registered`, or
  `unverifiable` (an old or externally created sandbox pi-stack has no
  receipt for). Both read from the same shared join of registration evidence
  and receipts, never a live gateway poll, so the two commands can't tell
  different stories from the same facts.
- **`pi-stack setup` never downloads local models without consent.**
  Interactive setup asks once, defaulting to No, before pulling any
  confirmed-missing configured Ollama model; non-interactive setup pulls
  nothing unless you pass `--pull-models`, the only consent it honors (a
  broad `--yes` never downloads). A model setup couldn't positively verify as
  missing is never a pull candidate either way, and setup never installs
  Ollama itself.

### Changed

- The sandbox and opt-in host mode now use pi `0.82.1`. Curated pi
  extensions were re-pinned to the newest versions published by the 0.82.1
  release, and CI now checks both vendored runtime patches against the exact pi
  and todo-list package pins. Host setup and launch now reject a missing or
  stale pi core before loading extensions, with the exact pinned install command;
  readiness and launch also require the matching curated-extension lock marker.
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
- Identity seeded from git config is now **first name only**: no surname, no
  email. It's recalled into every session, so it carries the minimum to greet.
  (`readGitIdentity` no longer reads `user.email`; memory stores one first-name
  fact instead of name + email.)

### Changed (breaking)

- **MCP migrated to sbx's nightly gateway.** The sandbox flag is now
  `--static-mcp` (sbx removed `--mcp`), and MCP runs through sbx's **local
  data-plane gateway**: always available, **no `SBX_MCP_URL`** needed (dropped
  the old SBX_MCP_URL gate + "gateway off" warning; `pi-stack mcp register` no
  longer requires it). pi-stack's own `pi-stack run --mcp M` CLI flag is unchanged.
  **Every configured server preloads at create.** Registering a server
  (`sbx mcp add`, `pi-stack mcp register`) only makes it known to the gateway;
  it does not attach it to any session. Every server in the resolved `mcp`
  list, and every integration an active or transient pack carries, is passed
  to sbx as `--static-mcp <name>` when the sandbox is created, so its tools
  are in context from the start. There is no dynamic discovery and no
  attach-on-run. New:
  - `pi-stack mcp load <name> [DIR]`: attach an already-registered server to a
    RUNNING sandbox live, no recreate (`sbx mcp load`), recording a receipt so
    `status`/`doctor` can report it as `loaded`.
  - `pi-stack mcp auth [args...]`: hosted-control-plane OAuth for remote servers
    (`sbx mcp auth`; e.g. `auth --all`).
  - `pi-stack mcp bundle`: register the shipped public catalog
    (notion/atlassian/granola, `config/mcp-catalog.bundle.json`) in one step.
  `make mcp-auth` already used native `sbx mcp auth`; its tail now points at
  `pi-stack mcp load` instead of a recreate. `doctor` guidance no longer mentions
  `SBX_MCP_URL` (a failed `sbx mcp ls` now points at the sbx daemon). An
  earlier draft of this change introduced `mcp_static`/`mcp_dynamic` config
  keys for an eager-vs-lazy attach split; those keys never shipped in a
  release and are gone before this reaches one, superseded by the
  always-preload-at-create model above (`doctor` flags either key as a
  retired config key if it's still in an existing `config.toml`).

- **Kit migrated to kit-spec v2** (`pi-kit/spec.yaml`, `schemaVersion: "2"`).
  Credentials are now a `credentials[]` list of `service` + `apiKey`
  (name/proxyManaged/inject[]); egress is `caps.network.allow`. Replaces the v1
  `network.serviceDomains`/`serviceAuth`/`allowedDomains` + `credentials.sources`
  + `environment.proxyManaged`. Injection is unchanged (proxy-managed sentinels;
  all four providers verified). **Requires a recent `sbx` nightly** (v0.37+); the
  per-credential `service:` is mandatory or `sbx run` panics. Mixin kits
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
  reuses that exact choice with no prompt; a persisted `sbx` mode still
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
  verified this run)" when a run used existing sbx keys instead. Real
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
  beat or that a constraint left a `sole fit`, plus a legend. Previously it said
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
  resident for the bridge/router, so a fresh install ran capture on a model that
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
  and the `ollama-bridge` extension reads it, no more hand-editing
  `/etc/sandbox-persistent.sh`. The bridge display label is now derived from the
  tag, so `OLLAMA_BRIDGE_MODEL_NAME` is optional (you set one value, not two).
  `make pull-models` pulls the local models. NOTE: the host memory service uses the
  watcher + embed models; it does NOT use the bridge/router local model, those
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
  `make serve` (now a generic pack-populated `SERVE_ENV`), the `snow` probe in
  the `healthcheck` skill (now a generic `EXTRA_CLIS` hook), and the
  company-only `:11442` port from the public kit allowlist. A pack adds these
  itself.
- Pruned stale docs: two `HANDOFF` snapshots, a superseded migration guide, a
  review artifact, and an upstream issue draft.
- README reworked to lead with the outcome, fix the launch command
  (`pi-stack run`), correct the pi link, and complete the launcher command list.

### Removed

- **Tore out the automated eval harness** (`evals/`, `pi-stack-host evals`,
  `make evals`, `pi-stack agent reassess --model`'s auto-measure path). The
  router never needed it: it only ever reads `scorecard.json`, regardless of
  how the numbers got there. Scores are now hand-maintained: edit
  `services/host/routing/defaults/scorecard.json` directly (seeded from
  published benchmarks/pricing; see the `model-refresh` skill), then
  `pi-stack route compile`. `pi-stack agent new`'s starter eval-suite
  scaffolding is also gone.

### Fixed

- **Automatic fact capture could be silently dead.** The watcher-availability
  check was one-shot at startup and latched: once it marked the watcher
  unavailable, `observe` short-circuited and never retried, so capture stayed off
  until a full daemon restart even after the model was pulled, and the client
  swallowed the failure reason, so nothing surfaced. Added a throttled live
  re-probe (capture recovers within 30s of the model appearing, no restart), a
  one-time client warning when capture is off, and a `pi-stack doctor` line that
  reads the daemon's live capture flag with the exact fix.
- `deliver` skill failed to load: its frontmatter `description` was an unquoted
  YAML scalar containing `: ` sequences, which YAML parsed as a nested mapping.
- `enterprise-admin` agent resolved to a model (`anthropic/sonnet-5`) that is not
  in the registry; it now uses `intent: reasoning` like its peers.
