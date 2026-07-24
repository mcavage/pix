// Headless reproduction / validation for the "bottom block jitter" bug in
// pi-tui's renderer (doRender). Drives the REAL TUI class against a fake
// terminal backed by a small ANSI emulator, then reads back the absolute
// screen row of editor/powerbar/footer sentinels before and after a chat
// shrink. Bottom block must NOT move on shrink.

import { pathToFileURL } from "node:url";
import { TerminalEmulator } from "./emulator.mjs";

const tuiIndex = process.env.PI_TUI_INDEX ?? "/usr/local/share/npm-global/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui/dist/index.js";
const { TUI, CURSOR_MARKER } = await import(pathToFileURL(tuiIndex).href);

const COLUMNS = 80;
const ROWS = 12; // visible height -> buffer (19 lines) is taller -> bottom-anchored

// Fake terminal: satisfies what TUI.doRender uses (columns/rows/write/hide/show).
function makeTerminal() {
	const emu = new TerminalEmulator(COLUMNS, ROWS);
	const term = {
		columns: COLUMNS,
		rows: ROWS,
		write: (s) => emu.write(s),
		hideCursor: () => {},
		showCursor: () => {},
		start: () => {},
		stop: () => {},
	};
	return { term, emu };
}

// A trivial child component returning whatever lines we set.
function makeSource(initial) {
	return {
		lines: initial.slice(),
		render() {
			return this.lines.slice();
		},
	};
}

// Build a buffer: HEAD + chat lines + bottom block (editor/powerbar/footer).
// The editor line carries the cursor marker (IME position), like the real app.
function buildBuffer(chatLabels) {
	const lines = ["HEAD_ROW"];
	for (const l of chatLabels) lines.push(l);
	lines.push("EDITOR_ROW" + CURSOR_MARKER);
	lines.push("POWERBAR_ROW");
	lines.push("FOOTER_ROW");
	return lines;
}

function chat(n, skip = -1) {
	const out = [];
	for (let i = 0; i < n; i++) {
		if (i === skip) continue;
		out.push("CHAT_" + String(i).padStart(2, "0"));
	}
	return out;
}

function measure(emu) {
	return {
		editor: emu.rowOf("EDITOR_ROW"),
		powerbar: emu.rowOf("POWERBAR_ROW"),
		footer: emu.rowOf("FOOTER_ROW"),
	};
}

let failures = 0;
function check(cond, msg) {
	if (cond) {
		console.log("  PASS  " + msg);
	} else {
		console.log("  FAIL  " + msg);
		failures++;
	}
}

// ---------------------------------------------------------------------------
console.log("=== pi-tui bottom-block jitter test (ROWS=%d) ===", ROWS);

const { term, emu } = makeTerminal();
const tui = new TUI(term);
// 15 chat lines (CHAT_00..CHAT_14) -> total 19 lines > 12 => viewport scrolled
const src = makeSource(buildBuffer(chat(15)));
tui.addChild(src);

// Frame A: initial render
tui.doRender();
const a = measure(emu);
console.log("\n[Frame A] initial render");
console.log("  bottom block rows:", a, "| viewportTop=", tui.previousViewportTop);
check(a.footer === ROWS - 1, `footer pinned to bottom row (${a.footer} === ${ROWS - 1})`);
check(a.powerbar === ROWS - 2 && a.editor === ROWS - 3, "editor/powerbar above footer, contiguous");

// Frame B: SHRINK — chat reflows one line shorter (drop CHAT_07 from the middle)
const fullBeforeB = tui.fullRedraws;
src.lines = buildBuffer(chat(15, 7));
tui.doRender();
const b = measure(emu);
console.log("\n[Frame B] chat shrinks by 1 (reflow/collapse)");
console.log("  bottom block rows:", b, "| viewportTop=", tui.previousViewportTop);
check(b.editor === a.editor, `EDITOR_ROW stayed put (before=${a.editor} after=${b.editor})`);
check(b.powerbar === a.powerbar, `POWERBAR_ROW stayed put (before=${a.powerbar} after=${b.powerbar})`);
check(b.footer === a.footer, `FOOTER_ROW stayed put (before=${a.footer} after=${b.footer})`);
check(tui.fullRedraws === fullBeforeB + 1, "shrink rebuilt the terminal buffer once");
check(
	emu.lines.filter((line) => line.includes("CHAT_05")).length === 1,
	"new viewport-top line has one physical copy (no duplicate left in scrollback)",
);

// Frame C: GROW back — chat returns to 15 lines (no regression, no spurious clear)
const fullBefore = tui.fullRedraws;
src.lines = buildBuffer(chat(15));
tui.doRender();
const c = measure(emu);
const grewWithoutFullRedraw = tui.fullRedraws === fullBefore;
console.log("\n[Frame C] chat grows back by 1");
console.log("  bottom block rows:", c, "| fullRedraws delta=", tui.fullRedraws - fullBefore);
check(c.footer === a.footer && c.powerbar === a.powerbar && c.editor === a.editor,
	"bottom block back at original rows after grow");
check(grewWithoutFullRedraw, "grow did not trigger a full screen clear");

// Frame D: STEADY — identical buffer, expect no movement and no full redraw
const fullBeforeD = tui.fullRedraws;
src.lines = buildBuffer(chat(15));
tui.doRender();
const d = measure(emu);
console.log("\n[Frame D] steady state (no change)");
console.log("  bottom block rows:", d, "| fullRedraws delta=", tui.fullRedraws - fullBeforeD);
check(d.footer === c.footer && d.powerbar === c.powerbar && d.editor === c.editor,
	"steady state keeps bottom block fixed");
check(tui.fullRedraws === fullBeforeD, "steady state did not trigger a full clear");

// ---------------------------------------------------------------------------
console.log("\n========================================");
if (failures === 0) {
	console.log("RESULT: PASS — bottom block stays pinned across shrink/grow/steady");
	process.exit(0);
} else {
	console.log(`RESULT: FAIL — ${failures} assertion(s) failed (bug present)`);
	process.exit(1);
}
