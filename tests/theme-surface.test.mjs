import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import test from "node:test";

const source = fs.readFileSync(new URL("../scripts/patches/theme-surface.block.txt", import.meta.url), "utf8");
const context = vm.createContext({ visibleWidth: (s) => [...s.replace(/\x1b\[[\d;]*m/g, "")].length });
vm.runInContext(source.replace("export function", "function"), context);
const paint = context.pixThemeSurface;
const surface = { fg: "\x1b[38;2;76;79;105m", bg: "\x1b[48;2;239;241;245m" };

test("theme surface paints plain editor text and blank cells to full width", () => {
 assert.equal(paint("draft", 10, surface), surface.fg + surface.bg + "draft" + surface.fg + surface.bg + "     ");
 assert.equal(paint("", 10, surface), surface.fg + surface.bg + surface.fg + surface.bg + "          ");
});
test("nested resets return to the palette while explicit RGB and selection colors survive", () => {
 const line = "\x1b[48;2;0;49;0mselected\x1b[49m plain\x1b[0;1m bold\x1b[39m prose";
 const actual = paint(line, 0, surface);
 assert.ok(actual.includes("\x1b[48;2;0;49;0mselected"));
 assert.ok(actual.includes(surface.bg + " plain"));
 assert.ok(actual.includes("\x1b[0;38;2;76;79;105;48;2;239;241;245;1m bold"));
 assert.ok(actual.includes(surface.fg + " prose"));
 assert.ok(!actual.includes("\x1b]"), "must not mutate terminal-global colors");
});
