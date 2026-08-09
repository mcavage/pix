# pi-tui: bottom jitter and duplicate scrollback lines during streaming

**Repo:** `earendil-works/pi` · **Package:** `packages/tui` (`@earendil-works/pi-tui`)
**Affected:** `0.79.8` through `0.84.1` (`0.84.1` reverified) · **Type:** rendering
bug + fix (tested)

> **0.84.0 moved the code, not the bug.** The renderer was split into
> `dist/tui-main-screen.js` (main screen, terminal-owned scrollback) and
> `dist/tui-alt-screen.js` (the new experimental `--tui-mode fullscreen`).
> `doRender()` moved to the former with the anchor, both preceding
> `fullRender(true)` guards, and the differential loop byte-identical, so the
> patch applies unchanged — `apply-tui-bottom-pin.mjs` just looks for the new
> filename first. The alt-screen renderer is a different implementation and does
> not have this bug; in `fullscreen` mode the patch is inert.

> 0.79.9 fixes two *related but distinct* chat-component things — Markdown streaming
> **code-fence** shrink/flicker (#5846) and clearing stale lines when content shrinks **to zero** —
> neither of which is this renderer-level reanchor. The jitter from general markdown re-wrap, the
> hidden `Thinking...` spacer, and tool-row collapse remains.

## Summary

While the agent streams, the **input editor and any `belowEditor` widgets (e.g. a powerbar) jump
up a row and back down**, repeatedly, whenever content *above* the editor gets shorter: markdown
re-wrapping as tokens arrive, the hidden `Thinking...` spacer toggling, or a tool row collapsing
from "running" to its result.

Root cause is in `TUI.doRender()` (`packages/tui/src/tui.ts`): on a **bottom-anchored buffer
shrink whose change lies *within* the visible window**, the differential render path repaints from
`firstChanged` relative to the *un-re-anchored* `viewportTop`, so the unchanged bottom block is
re-emitted `index − viewportTop` = one row higher. It never re-anchors `viewportTop` to the new
bottom. (This is *not* fixable from an extension: extensions only own the `status`/`aboveEditor`
rows, which are already constant; the churn is in `chatContainer`, rendered entirely by pi.)

The first vendored fix repainted the newly visible window in place. That held the editor still but
caused the intermittent duplicate-line bug: the new top row was copied from terminal scrollback
onto the visible screen, while its original immutable scrollback row remained. The physical buffer
then contained two adjacent copies even though the visible viewport looked correct. Once later
output scrolled that boundary into view, pi appeared to print the same line twice.

## Why only *this* shrink (not the ones that already full-redraw)

`doRender()` already routes other shrinks correctly, verified with `PI_DEBUG_REDRAW=1`:

- **Deleted-tail shrink** (`firstChanged >= newLines.length`) → `deleted lines moved viewport up`
  → `fullRender(true)`. ✅ already pinned (via full redraw).
- **Change above the viewport** (`firstChanged < prevViewportTop`, e.g. a "stuck-high" viewport
  left by a transient overlay) → `firstChanged < viewportTop` → `fullRender(true)`. ✅ already
  cleared.
- **Change *within* the viewport** (`firstChanged >= prevViewportTop`) → falls through to the
  differential loop, which does **not** re-anchor `viewportTop` → **the bottom block drifts up.**
  ← this is the bug, and the only path that needs fixing.

## Fix

Insert one guarded branch **after** the `firstChanged < prevViewportTop` full-redraw guard and
before the differential render loop. On a bottom-anchored shrink, call the existing
`fullRender(true)` path. It clears and rebuilds the physical terminal buffer under synchronized
output, so the bottom block keeps its screen rows and every logical row has exactly one physical
copy. The normal full-render path also handles inline-image deletion and row reservation.

Placement matters: it sits **below** the two existing `fullRender(true)` guards, so their shrink
cases are untouched. The new branch applies only when content was and remains bottom-anchored and
no overlay is active. A buffer that now fits on screen returns naturally to top anchoring.

Full patch (renderer + tests): **`tui-bottom-pin/tui-src.patch`** (`git apply` from the repo root).

## Tests — all green

The vendored patch is also tested against the exact `PI_PACKAGE` pin in CI. The
check installs pi, applies the patch twice to prove idempotence, and runs the
headless regression, edge, and integrity harnesses against the installed
renderer. Pi `0.82.1` passes all three harnesses.

The patched upstream source passes `packages/tui/test/tui-render.test.ts`: **25 / 25**.
`biome check` also passes for the renderer and test file.

The regression harness checks both views of the terminal state:

- The editor, widgets, and footer keep their screen rows across one-line, multi-line, and sustained
  bottom-anchored shrinks.
- The complete physical buffer equals the logical render history after a shrink. This catches the
  old in-place repaint, which left a duplicate viewport-top row in scrollback while the visible
  viewport still looked correct.
- Growth, steady-state, top-anchored, overlay, large-collapse, and IME cursor cases remain covered.

### Supplementary headless harness (`tui-bottom-pin/`)

`emulator.mjs` (ANSI terminal emulator that tracks cursor/scroll/`\x1b[2K`/clear + scrollback),
`test.mjs` / `edge.mjs` / `integrity.mjs` — drive the real `TUI` and read the *physical screen
row* of `EDITOR_ROW`/`POWERBAR_ROW`/`FOOTER_ROW` sentinels.

```
BEFORE (original):  test FAIL(4)   edge FAIL(5)   integrity FAIL(5)
AFTER  (patched):   test PASS      edge PASS      integrity PASS      tui-render 25/25
```

(`edge.mjs` scenario 1 exercises an *in-viewport* multi-line shrink. An *above*-viewport shrink
already uses the upstream full-render path and is not this bug.)

## Vendored locally

This fix is also applied at build time in pi-stack's image (`scripts/patches/`), independent of
upstream, so the sandboxes get it now. Upstreaming lets that vendored patch be dropped.
