#!/usr/bin/env node
// Vendored renderer patch — "bottom-block pin".
//
// pi's TUI renderer (@earendil-works/pi-tui, dist/tui.js `doRender()`) jitters the
// input box + powerbar up/down while streaming: on a bottom-anchored buffer
// SHRINK it re-emits the unchanged bottom block one row higher instead of
// re-anchoring the viewport. This is a renderer bug with no extension/config
// fix, so we vendor the fix into our own image at build time rather than wait on
// an upstream release. See docs/upstream/tui-bottom-pin.md for the full writeup.
//
// This script INSERTS a self-contained branch into doRender(), right before the
// "// Render from first changed line to end" anchor. It is:
//   • idempotent  — no-ops if the marker is already present;
//   • non-fatal   — if pi's layout changed (anchor missing / already-different),
//     it prints a loud warning and exits 0 so the image still builds (you just
//     keep the jitter until the script is refreshed for the new pi version).
//
// Run from the repo (Dockerfile does this after `npm install -g pi`):
//   node scripts/patches/apply-tui-bottom-pin.mjs

import { execSync } from "node:child_process";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const MARKER = "Bottom-block pin";
// Insert the pin block immediately before this line in doRender(). It sits
// AFTER the `firstChanged < prevViewportTop` full-redraw guard, so the deliberate
// full-redraw-on-shrink cases are untouched and only the in-viewport jitter is
// pinned. (Earlier revisions anchored on "// Find first and last changed lines",
// before those guards — that over-reached and changed full-redraw behaviour.)
const ANCHOR = "// Render from first changed line to end";
const BEFORE_ANCHOR = `        if (firstChanged < prevViewportTop) {
            logRedraw(\`firstChanged < viewportTop (\${firstChanged} < \${prevViewportTop})\`);
            fullRender(true);
            return;
        }
`;
const AFTER_ANCHOR = `${ANCHOR}
        // Build buffer with all updates wrapped in synchronized output
        let buffer = "\\x1b[?2026h"; // Begin synchronized output`;
const here = dirname(fileURLToPath(import.meta.url));
const BLOCK_FILE = join(here, "tui-bottom-pin.block.txt");

const warn = (msg) => console.warn(`[apply-tui-bottom-pin] ⚠ ${msg}`);
const info = (msg) => console.log(`[apply-tui-bottom-pin] ${msg}`);

function findTuiJs() {
	// Release smoke installs into an isolated prefix and persists the exact path
	// between workflow steps. Prefer that explicit target over rediscovery.
	if (process.env.TUI_JS) {
		return existsSync(process.env.TUI_JS) ? process.env.TUI_JS : null;
	}
	const prefix =
		process.env.NPM_CONFIG_PREFIX ||
		(() => {
			try {
				return execSync("npm prefix -g", { encoding: "utf8" }).trim();
			} catch {
				return "/usr/local/share/npm-global";
			}
		})();
	// pi-tui 0.84.0 split the renderer in two: the main-screen (terminal-owned
	// scrollback) differential renderer moved out of `tui.js` into
	// `tui-main-screen.js`, and a new alt-screen renderer took the fullscreen
	// path. The jitter this patch fixes lives in the MAIN-screen renderer, whose
	// doRender() carried over byte-identical, so try the new filename FIRST and
	// fall back to the pre-0.84 one.
	const names = ["tui-main-screen.js", "tui.js"];
	for (const name of names) {
		const rel = `node_modules/@earendil-works/pi-tui/dist/${name}`;
		const candidates = [
			join(prefix, "lib/node_modules/@earendil-works/pi-coding-agent", rel),
			join(prefix, `lib/node_modules/@earendil-works/pi-tui/dist/${name}`),
		];
		for (const c of candidates) if (existsSync(c)) return c;
	}
	// Fallback: search under the prefix.
	for (const name of names) {
		try {
			const found = execSync(
				`find ${JSON.stringify(prefix)} -path '*@earendil-works/pi-tui/dist/${name}' 2>/dev/null | head -1`,
				{ encoding: "utf8" },
			).trim();
			if (found && existsSync(found)) return found;
		} catch {
			/* fall through */
		}
	}
	return null;
}

function main() {
	const tuiPath = findTuiJs();
	if (!tuiPath) {
		warn(
			"could not locate @earendil-works/pi-tui dist/tui.js — skipping patch.",
		);
		return; // non-fatal
	}
	const src = readFileSync(tuiPath, "utf8");

	if (src.includes(MARKER)) {
		info(`already patched: ${tuiPath}`);
		return;
	}
	if (!existsSync(BLOCK_FILE)) {
		warn(`block file missing: ${BLOCK_FILE} — skipping patch.`);
		return; // non-fatal
	}
	const block = readFileSync(BLOCK_FILE, "utf8").replace(/\s*$/, "");

	const idx = src.indexOf(ANCHOR);
	const lineStart = idx === -1 ? -1 : src.lastIndexOf("\n", idx) + 1;
	if (
		idx === -1 ||
		!src
			.slice(Math.max(0, lineStart - BEFORE_ANCHOR.length), lineStart)
			.endsWith(BEFORE_ANCHOR) ||
		!src
			.slice(lineStart, lineStart + 8 + AFTER_ANCHOR.length)
			.startsWith(`        ${AFTER_ANCHOR}`)
	) {
		warn(
			`expected renderer context around "${ANCHOR}" not found in ${tuiPath} — ` +
				`pi's renderer changed; refresh scripts/patches/ for this version. Leaving file unpatched.`,
		);
		return; // non-fatal
	}

	// Insert the block (plus a blank separating line) immediately before the line
	// that contains the anchor, preserving that line's leading indentation.
	const patched =
		src.slice(0, lineStart) + block + "\n\n" + src.slice(lineStart);

	writeFileSync(tuiPath, patched);

	// Verify it took and the marker is now present exactly once.
	const check = readFileSync(tuiPath, "utf8");
	const count = check.split(MARKER).length - 1;
	if (count !== 1) {
		warn(`post-write marker count = ${count} (expected 1) in ${tuiPath}`);
		return;
	}
	info(`patched ${tuiPath}`);
}

main();
