import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const themesDir = path.join(repoRoot, "themes");
const requiredColors = [
	"accent", "border", "borderAccent", "borderMuted", "success", "error", "warning", "muted", "dim", "text", "thinkingText",
	"scrollbarTrack", "scrollbarThumb", "selectedBg", "searchMatchBg", "searchMatchText", "userMessageBg", "userMessageText",
	"customMessageBg", "customMessageText", "customMessageLabel", "toolPendingBg", "toolSuccessBg", "toolErrorBg", "toolTitle", "toolOutput",
	"mdHeading", "mdLink", "mdLinkUrl", "mdCode", "mdCodeBlock", "mdCodeBlockBorder", "mdQuote", "mdQuoteBorder", "mdHr", "mdListBullet",
	"toolDiffAdded", "toolDiffRemoved", "toolDiffContext", "syntaxComment", "syntaxKeyword", "syntaxFunction", "syntaxVariable", "syntaxString",
	"syntaxNumber", "syntaxType", "syntaxOperator", "syntaxPunctuation", "thinkingOff", "thinkingMinimal", "thinkingLow", "thinkingMedium",
	"thinkingHigh", "thinkingXhigh", "thinkingMax", "bashMode",
];

function resolveColor(value, vars, seen = new Set()) {
	if (value === "" || Number.isInteger(value) || /^#[0-9a-f]{6}$/.test(value)) return value;
	assert.equal(typeof value, "string");
	assert.ok(Object.hasOwn(vars, value), `unknown color variable ${JSON.stringify(value)}`);
	assert.ok(!seen.has(value), `cyclic color variable ${value}`);
	return resolveColor(vars[value], vars, new Set([...seen, value]));
}

test("Pix ships a substantial, valid theme catalog", () => {
	const files = fs.readdirSync(themesDir).filter((name) => name.endsWith(".json")).sort();
	assert.ok(files.length >= 15, `expected at least 15 shipped themes, found ${files.length}`);
	const names = new Set();
	for (const file of files) {
		const doc = JSON.parse(fs.readFileSync(path.join(themesDir, file), "utf8"));
		assert.equal(doc.name, file.slice(0, -5), `${file}: filename and theme name differ`);
		assert.match(doc.name, /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/);
		assert.ok(!names.has(doc.name), `duplicate theme name ${doc.name}`);
		names.add(doc.name);
		assert.deepEqual(requiredColors.filter((color) => !(color in doc.colors)), [], `${file}: missing theme colors`);
		assert.deepEqual(Object.keys(doc.colors).filter((color) => !requiredColors.includes(color)), [], `${file}: unknown theme colors`);
		for (const [name, value] of Object.entries(doc.vars ?? {})) {
			assert.doesNotThrow(() => resolveColor(value, doc.vars ?? {}), `${file}: invalid variable ${name}`);
		}
		for (const [name, value] of Object.entries(doc.colors)) {
			assert.doesNotThrow(() => resolveColor(value, doc.vars ?? {}), `${file}: invalid color ${name}`);
		}
		assert.ok(doc.export?.pageBg && doc.export?.cardBg && doc.export?.infoBg, `${file}: missing export palette`);
	}
	for (const expected of ["catppuccin-mocha", "catppuccin-latte", "nord", "gruvbox-dark", "gruvbox-light", "solarized-dark", "solarized-light", "rose-pine", "rose-pine-dawn", "tokyo-night"]) {
		assert.ok(names.has(expected), `missing expected built-in theme ${expected}`);
	}
});
