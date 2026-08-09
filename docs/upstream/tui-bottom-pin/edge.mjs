// Edge-case / regression coverage for the bottom-pin patch.
import { pathToFileURL } from "node:url";
import { TerminalEmulator } from "./emulator.mjs";

const tuiIndex = process.env.PI_TUI_INDEX ?? "/usr/local/share/npm-global/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui/dist/index.js";
// pi-tui 0.84.0 split the renderer and RENAMED the class: the main-screen
// renderer (the one this harness drives, and the one the bottom-pin patch
// targets) is now exported as `TuiMainScreen`, with `TuiAltScreen` alongside it
// for the new fullscreen mode. Accept either name so the harness runs against a
// pre-0.84 and post-0.84 install without a second copy.
const { TuiMainScreen, TUI, CURSOR_MARKER } = await import(pathToFileURL(tuiIndex).href);
const MainScreen = TuiMainScreen ?? TUI;
if (typeof MainScreen !== "function") {
	throw new Error(`pi-tui at ${tuiIndex} exports neither TuiMainScreen nor TUI; the renderer moved again`);
}

let failures = 0;
const check = (cond, msg) => {
	console.log((cond ? "  PASS  " : "  FAIL  ") + msg);
	if (!cond) failures++;
};

function harness(rows, cols = 80) {
	const emu = new TerminalEmulator(cols, rows);
	const term = { columns: cols, rows, write: (s) => emu.write(s), hideCursor() {}, showCursor() {}, start() {}, stop() {} };
	const tui = new MainScreen(term);
	const src = { lines: [], render() { return this.lines.slice(); } };
	tui.addChild(src);
	return { emu, tui, src };
}
const chat = (n, skip = []) => {
	const s = new Set(skip); const out = [];
	for (let i = 0; i < n; i++) if (!s.has(i)) out.push("CHAT_" + String(i).padStart(2, "0"));
	return out;
};
const buf = (chatLabels, editorMarker = true) => [
	"HEAD_ROW", ...chatLabels,
	"EDITOR_ROW" + (editorMarker ? CURSOR_MARKER : ""), "POWERBAR_ROW", "FOOTER_ROW",
];
const rowsOf = (emu) => ({ editor: emu.rowOf("EDITOR_ROW"), powerbar: emu.rowOf("POWERBAR_ROW"), footer: emu.rowOf("FOOTER_ROW") });

// 1) Multi-line shrink (drop 3 lines at once) stays pinned.
console.log("\n[1] multi-line shrink (drop 3)");
{
	const { emu, tui, src } = harness(12);
	src.lines = buf(chat(15)); tui.doRender();
	const a = rowsOf(emu);
	const fr = tui.fullRedraws;
	src.lines = buf(chat(15, [10, 11, 12])); tui.doRender();
	const b = rowsOf(emu);
	check(b.editor === a.editor && b.powerbar === a.powerbar && b.footer === a.footer,
		`bottom pinned across 3-line shrink (${JSON.stringify(a)} -> ${JSON.stringify(b)})`);
	check(tui.fullRedraws === fr + 1, "3-line shrink rebuilt the terminal buffer once");
	check(emu.lines.length === src.lines.length, "physical buffer has one row per logical row after shrink");
	const curScreenRow = emu.cur - emu.screenTop();
	check(curScreenRow === b.editor, `IME hardware cursor landed on editor row (screenRow=${curScreenRow}, editor=${b.editor})`);
}

// 2) Shrink where the FOOTER text also changes (smaller common suffix).
//    Unchanged editor/powerbar must stay pinned; footer repaints at same row.
console.log("\n[2] shrink + footer content changes");
{
	const { emu, tui, src } = harness(12);
	src.lines = buf(chat(15)); tui.doRender();
	const a = rowsOf(emu);
	src.lines = ["HEAD_ROW", ...chat(15, [7]), "EDITOR_ROW" + CURSOR_MARKER, "POWERBAR_ROW", "FOOTER_ROW v2"];
	tui.doRender();
	const editor = emu.rowOf("EDITOR_ROW"), powerbar = emu.rowOf("POWERBAR_ROW"), footer = emu.rowOf("FOOTER_ROW v2");
	check(editor === a.editor && powerbar === a.powerbar, `editor/powerbar pinned (e=${editor} p=${powerbar})`);
	check(footer === a.footer, `changed footer painted at same row (${footer} === ${a.footer})`);
}

// 3) Non-bottom-anchored (buffer fits on screen): content should move UP
//    naturally on shrink (NOT pinned with a blank top row).
console.log("\n[3] buffer fits on screen (top-anchored) shrink");
{
	const ROWS = 24;
	const { emu, tui, src } = harness(ROWS);
	src.lines = buf(chat(5)); // total 9 lines << 24, top-anchored
	tui.doRender();
	const a = rowsOf(emu);
	check(tui.previousViewportTop === 0, "viewportTop is 0 (top-anchored)");
	src.lines = buf(chat(5, [2])); // shrink to 8 lines
	tui.doRender();
	const b = rowsOf(emu);
	check(b.footer === a.footer - 1, `footer moved up by 1 naturally (${a.footer} -> ${b.footer})`);
	check(emu.rowOf("HEAD_ROW") === 0, "no blank row inserted at top (HEAD still row 0)");
}

// 4) Overlay active during shrink -> patch must NOT pad (overlay path owns layout).
console.log("\n[4] overlay active: pinning skipped, no crash");
{
	const { emu, tui, src } = harness(12);
	src.lines = buf(chat(15)); tui.doRender();
	// minimal overlay component
	const overlay = { render() { return ["OVERLAY_LINE_1", "OVERLAY_LINE_2"]; } };
	tui.overlayStack.push({ component: overlay, focusOrder: 1 });
	let threw = false;
	try { src.lines = buf(chat(15, [7])); tui.doRender(); } catch (e) { threw = true; console.log("    err:", e.message); }
	tui.overlayStack.pop();
	check(!threw, "shrink with overlay did not throw");
}

// 5) Grow-from-empty / first render unaffected (no padding on growth path).
console.log("\n[5] pure growth path untouched");
{
	const { emu, tui, src } = harness(12);
	src.lines = buf(chat(5)); tui.doRender();
	const fr = tui.fullRedraws;
	src.lines = buf(chat(6)); tui.doRender();
	check(emu.rowOf("FOOTER_ROW") !== -1, "footer visible after growth");
	check(tui.fullRedraws === fr, "growth did not force a full clear");
}

console.log("\n========================================");
if (failures === 0) { console.log("EDGE RESULT: PASS"); process.exit(0); }
else { console.log(`EDGE RESULT: FAIL (${failures})`); process.exit(1); }
