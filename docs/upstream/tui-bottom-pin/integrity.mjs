// Deep integrity checks: the re-anchor repaint must show EXACTLY the correct
// bottom-anchored window (real history pulled down, no blanks, no corruption),
// across large and sustained shrinks.
import { pathToFileURL } from "node:url";
import { TerminalEmulator } from "./emulator.mjs";

const tuiIndex = process.env.PI_TUI_INDEX ?? "/usr/local/share/npm-global/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui/dist/index.js";
const { TUI, CURSOR_MARKER } = await import(pathToFileURL(tuiIndex).href);

let failures = 0;
const check = (cond, msg) => { console.log((cond ? "  PASS  " : "  FAIL  ") + msg); if (!cond) failures++; };

const ROWS = 12, COLS = 80;
function harness() {
	const emu = new TerminalEmulator(COLS, ROWS);
	const term = { columns: COLS, rows: ROWS, write: (s) => emu.write(s), hideCursor() {}, showCursor() {}, start() {}, stop() {} };
	const tui = new TUI(term);
	const src = { lines: [], render() { return this.lines.slice(); } };
	tui.addChild(src);
	return { emu, tui, src };
}
const chat = (n, skip = []) => { const s = new Set(skip); const o = []; for (let i = 0; i < n; i++) if (!s.has(i)) o.push("CHAT_" + String(i).padStart(2, "0")); return o; };
const buf = (c) => ["HEAD_ROW", ...c, "EDITOR_ROW" + CURSOR_MARKER, "POWERBAR_ROW", "FOOTER_ROW"];

// Strip the cursor marker the way the renderer does, so we can compare the
// emulator's visible plain text to the expected buffer slice.
const plain = (s) => s.split(CURSOR_MARKER).join("");

function expectedVisible(lines) {
	const top = Math.max(0, lines.length - ROWS);
	return lines.slice(top, top + ROWS).map(plain);
}

// 1) Content integrity after a normal shrink: visible window == bottom-anchored slice
console.log("\n[1] visible window matches bottom-anchored slice after shrink");
{
	const { emu, tui, src } = harness();
	src.lines = buf(chat(15)); tui.doRender();
	src.lines = buf(chat(15, [7])); tui.doRender();
	const exp = expectedVisible(src.lines);
	const got = emu.visibleLines();
	let ok = true;
	for (let r = 0; r < ROWS; r++) if (got[r] !== exp[r]) { ok = false; console.log(`    row ${r}: got=${JSON.stringify(got[r])} exp=${JSON.stringify(exp[r])}`); }
	check(ok, "every visible row equals expected bottom-anchored content (history pulled down, no blanks)");
	check(got.every((l) => l !== "" ) || exp.some((l) => l === ""), "no spurious blank rows introduced");
	check(
		JSON.stringify(emu.lines) === JSON.stringify(src.lines.map(plain)),
		"physical buffer matches logical history exactly (no duplicate viewport-top row)",
	);
}

// 2) Large collapse: 20-line tool output -> 1-line result (shrink by 19)
console.log("\n[2] large collapse (shrink by 19) pins bottom, no blank fill");
{
	const { emu, tui, src } = harness();
	const big = []; for (let i = 0; i < 20; i++) big.push("TOOL_OUT_" + String(i).padStart(2, "0"));
	src.lines = buf([...chat(4), ...big]); tui.doRender(); // total 1+4+20+3 = 28
	const a = { e: emu.rowOf("EDITOR_ROW"), p: emu.rowOf("POWERBAR_ROW"), f: emu.rowOf("FOOTER_ROW") };
	src.lines = buf([...chat(4), "TOOL_RESULT_OK"]); tui.doRender(); // total 1+4+1+3 = 9 (< ROWS now!)
	const b = { e: emu.rowOf("EDITOR_ROW"), p: emu.rowOf("POWERBAR_ROW"), f: emu.rowOf("FOOTER_ROW") };
	// 9 < ROWS(12): after collapse it FITS on screen -> top-anchored, legitimately moves.
	// What we assert: no crash, footer visible, content correct.
	const exp = expectedVisible(src.lines), got = emu.visibleLines();
	check(b.f !== -1 && b.e !== -1, `bottom block visible after large collapse (a=${JSON.stringify(a)} b=${JSON.stringify(b)})`);
	let ok = true; for (let r = 0; r < src.lines.length; r++) if (got[r] !== exp[r]) ok = false;
	check(ok, "content correct after large collapse to fits-on-screen");
}

// 2b) Large collapse that STAYS bottom-anchored (28 -> 14, both > ROWS=12)
console.log("\n[2b] large collapse staying bottom-anchored pins exactly, no blanks");
{
	const { emu, tui, src } = harness();
	const big = []; for (let i = 0; i < 20; i++) big.push("TOOL_OUT_" + String(i).padStart(2, "0"));
	src.lines = buf([...chat(4), ...big]); tui.doRender(); // 28
	const a = { e: emu.rowOf("EDITOR_ROW"), p: emu.rowOf("POWERBAR_ROW"), f: emu.rowOf("FOOTER_ROW") };
	src.lines = buf([...chat(4), ...big.slice(0, 6)]); tui.doRender(); // 1+4+6+3 = 14 (> 12)
	const b = { e: emu.rowOf("EDITOR_ROW"), p: emu.rowOf("POWERBAR_ROW"), f: emu.rowOf("FOOTER_ROW") };
	check(b.e === a.e && b.p === a.p && b.f === a.f, `bottom pinned across big bottom-anchored shrink (${JSON.stringify(a)} -> ${JSON.stringify(b)})`);
	const got = emu.visibleLines();
	check(got.every((l) => l !== ""), "no blank rows after big shrink (history pulled down)");
	const exp = expectedVisible(src.lines);
	let ok = true; for (let r = 0; r < ROWS; r++) if (got[r] !== exp[r]) ok = false;
	check(ok, "visible content matches expected bottom-anchored slice");
}

// 3) Sustained multi-step shrink: pin holds each step, never accumulates blanks
console.log("\n[3] sustained step-by-step shrink (no blank accumulation)");
{
	const { emu, tui, src } = harness();
	src.lines = buf(chat(20)); tui.doRender(); // 24
	const f0 = emu.rowOf("FOOTER_ROW");
	let ok = true, blanksOk = true;
	for (let n = 19; n >= 9; n--) { // shrink one line at a time down to 13 total (still >12)
		src.lines = buf(chat(n));
		tui.doRender();
		if (emu.rowOf("FOOTER_ROW") !== f0) { ok = false; }
		const total = src.lines.length;
		if (total >= ROWS) { // while still bottom-anchored, no blank rows
			if (emu.visibleLines().some((l) => l === "")) blanksOk = false;
		}
	}
	check(ok, `footer stayed at row ${f0} through 11 successive shrinks`);
	check(blanksOk, "no blank rows accumulated at top across sustained shrink");
}

console.log("\n========================================");
if (failures === 0) { console.log("INTEGRITY RESULT: PASS"); process.exit(0); }
else { console.log(`INTEGRITY RESULT: FAIL (${failures})`); process.exit(1); }
