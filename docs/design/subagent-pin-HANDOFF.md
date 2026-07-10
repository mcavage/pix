# Handoff: pinned subagent tracker

Status snapshot for picking this back up after a session/sandbox restart.

## TL;DR
The pinned live subagent tracker is **built, tested, reviewed, committed, pushed,
and has an open PR**. The only thing left is a `make load` on the HOST (which the
in-sandbox agent cannot run) and an eyeball pass in a real TUI. Nothing is
half-finished in the working tree.

- Branch: `feat/pinned-subagent-tracker`
- PR: https://github.com/mcavage/pi-stack/pull/1
- Commit: `203c6fd feat(subagents): pinned live tracker for running subagents`
- Working tree: clean except this handoff doc.

## The ask (original)
Subagent progress streamed inline in the transcript and scrolled away. The user
wants running subagents shown in a **pinned, persistent place** in the TUI (like
Claude Code / OpenCode / Docker agent / Codex), with a clear running / done /
failed state that stays put while the conversation scrolls. Hint that mattered:
pi's TODO list already pins via `ctx.ui.setWidget`, so this is the same mechanism.

## What shipped (in the commit / PR)
All in `extensions/subagents.ts` (one file, per project rule):
- A module-level run registry (`tracker`) rendered into a pinned widget via
  `ctx.ui.setWidget("subagent-tracker", lines)` + a `ctx.ui.setStatus` footer
  rollup. One roster line per run: status glyph (spinner running / `✓` done /
  `✗` failed / `⏱` timeout / `⊘` aborted), agent+model, live turns/tokens/cost,
  elapsed. Header shows `N running · M done · K failed`.
- Hooked into the single `runSingle()` choke point (register / update / finalize),
  so single / parallel / chain all funnel into one panel. Parallel and chain
  **pre-register queued rows** so a full fanout shows all rows at once.
- Successes auto-clear after `PI_SUBAGENT_PIN_TTL_MS` (default 6s); failures /
  timeouts / aborts stay pinned until the next batch or `session_shutdown`.
- Stability preserved: one shared **1s** ticker gated on the running count (never
  spins idle, `unref`ed), every tracker call guarded (never throws at load /
  wedges `/reload`), `session_shutdown` teardown clears interval + widget, and a
  headless tree child (no UI) renders nothing and allocates no timer. **No change**
  to the child-process watchdog / kill / drain / process-group logic.

Docs added: `docs/design/subagent-pin-tracker.md` (impl plan), plus a note in the
top-level `AGENTS.md` subagents section. `.gitignore` now ignores `.scratch/`.

## Design provenance (if you need to re-open the reasoning)
Full product pass: research on how the best agents show subagents, Marc Pincus
proven / better / new framing, designer + architect specs, two cross-vendor peer
reviews. The product/design artifacts are in `.scratch/` (gitignored, so they die
with the sandbox — see the PR body + `docs/design/subagent-pin-tracker.md` for the
durable version):
- `.scratch/subagent-pin-brief.md`   (the brief handed to the crew)
- `.scratch/subagent-pin-plan.md`    (PM plan, proven/better/new)
- `.scratch/subagent-pin-design.md`  (designer visual spec)

## Verification already done (all green)
- `npx tsc --noEmit -p tsconfig.json` → clean.
- Load-check: `pi --no-extensions -e ./extensions/subagents.ts --no-session -p "reply OK"`
  → runs a real turn, no factory error.
- Headless harness drove the REAL tracker code through parallel / chain / timeout
  / abort / narrow-width / headless lifecycles: all states render right; TTL
  auto-clear of successes with sticky failures verified on real timers.
- Leak-regression harness for the 3 peer-review MUST-FIX items (unknown-agent
  queued row, chain early-failure abandoned steps, headless TTL timer): all
  confirmed closed. Second review verdict: APPROVE.
  (The harness recreated temp `node_modules` symlinks to the global pi packages +
  a `.scratch/*_under_test.ts` copy with a test-only export, then deleted them.
  If you re-run it: symlink `@earendil-works/{pi-ai,pi-tui,pi-coding-agent}` and
  `typebox` from
  `/usr/local/share/npm-global/lib/node_modules/@earendil-works/pi-coding-agent/node_modules`
  into a repo-local `node_modules`, run with
  `node --experimental-strip-types`, then remove the symlinks.)

## What is NOT done / open items
1. ~~**`make load` on the HOST**~~ — **DONE** (run by the user). Image baked and
   sandbox recreated. This session is running on the new image.
2. ~~**Visual QA in a real TUI**~~ — **DONE**. Pin renders correctly in the live
   TUI (parallel / chain / web-search fanouts confirmed this session).
3. **pi bumped to 0.80.6 (commit `6c92fac`, on PR #1).** The vendored renderer
   patch MUST move forward with every pi bump: the build log has to print
   `[apply-tui-bottom-pin] patched`, NOT an `anchor not found` warning. The
   patch anchors on `// Render from first changed line to end` in
   `@earendil-works/pi-tui/dist/tui.js` (guard marker `Bottom-block pin`). On the
   baked 0.80.5 the anchor was present and the patch applied; **re-verify on the
   next `make load` for 0.80.6** — if the warning fires, refresh
   `scripts/patches/tui-bottom-pin.block.txt` + the anchor in
   `apply-tui-bottom-pin.mjs` against the new `tui.js`. (The `apply-subagent-
   timeout.mjs` patch stays disabled — nothing to re-verify there.)
4. **`belowEditor` placement question (open, needs a decision).** Both the TODO
   pin (`todo-list`) and this tracker (`subagent-tracker`) default to ABOVE the
   editor and pi stacks them in registration order. They coexist fine (distinct
   keys), but a long todo list + big fanout could get tall on a short terminal.
   One-line fix if wanted: pass `{ placement: "belowEditor" }` to the tracker's
   `setWidget`/`setStatus` calls in `renderWidget()` so the two pins sit on
   opposite sides of the input. Not done — waiting on the user's preference.
5. **The NEW bet: true background dispatch (deferred, PR #2).** A `background:true`
   tool param so runs return immediately and keep streaming into the pin, results
   collected later. Deliberately isolated out of PR #1. Speced in
   `.scratch/subagent-pin-plan.md` (NEW section) and the "Deferred" section of
   `docs/design/subagent-pin-tracker.md`. Build only if the user asks.

## How to resume (fresh session)
1. Confirm state: `git log --oneline -3` on `feat/pinned-subagent-tracker`; check
   PR #1 status with `gh pr view 1`.
2. If the user has run `make load` + recreated the sandbox, the pin is live: fire
   a `subagent` parallel fanout and watch the roster above the editor; run the
   design-review / qa skills for a visual pass.
3. If addressing review comments or the `belowEditor` toggle: edit
   `extensions/subagents.ts` (`renderWidget()` for placement; the `tracker`
   section for behavior), re-run tsc + load-check, then `ship` (or push to the
   existing branch and comment on the PR). Do NOT merge (ship stops at PR).
4. Key symbols in `extensions/subagents.ts`: `tracker` (registry),
   `registerRun` / `markRunning` / `updateRun` / `setRunStatus` / `finalizeRun` /
   `dropRun`, `renderWidget` / `renderRow` / `renderStatusLine`, `ensureTicker` /
   `maybeStopTicker` / `teardown`, `captureUi`. `runSingle` takes a `track` opts
   arg (`{mode, preRunId, enabled}`); the doctor canary passes `enabled:false`.

## Gotchas carried from this session
- The live extension at `~/.pi/agent/extensions/subagents.ts` is the BAKED copy,
  not the repo edit. Editing the repo file does nothing for the current session
  until `make load` + recreate. For same-session iteration you would sync the file
  into `~/.pi/agent/extensions/` and `/reload` (but you cannot `make load`).
- `string[]` `setWidget` supports per-line `theme.fg(...)` colors (confirmed via
  the plan-mode example) and is capped ~10 lines — the renderer reserves a line
  for the `+N more` summary so it never clips.
- Widgets are keyed; `todo-list` and `subagent-tracker` are distinct, so they
  render together. Same key = replace-in-place.
