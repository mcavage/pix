import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { register } from "node:module";
import test from "node:test";

register("./stub-loader.mjs", import.meta.url);
const { registerThemePicker } = await import("../extensions/theme-picker.ts");

function capture(contextDir) {
	const commands = new Map();
	registerThemePicker({ registerCommand: (name, definition) => commands.set(name, definition) }, { contextDir });
	return commands;
}

function fakeContext(action, calls, notices, previews = []) {
	const themes = new Map(["pix", "dracula", "host"].map((name) => [name, {
		name,
		fg: (_color, text) => text,
		bg: (_color, text) => text,
		bold: (text) => text,
	}]));
	const ctx = {
		mode: "tui",
		ui: {
			theme: { name: "pix", fg: (_color, text) => text, bold: (text) => text },
			beginThemePreview: () => {
				const original = ctx.ui.theme;
				return {
					preview: (name) => { previews.push(name); ctx.ui.theme = themes.get(name); return { success: true }; },
					restore: () => { previews.push("restore"); ctx.ui.theme = original; },
				};
			},
			getAllThemes: () => [...themes.keys()].map((name) => ({ name })),
			getTheme: (name) => themes.get(name),
			setTheme: (value) => { calls.push(value); return { success: true }; },
			notify: (text, level) => notices.push({ text, level }),
			custom: async (factory, options) => { assert.equal(options.overlay, true); return new Promise((resolve) => {
				const tui = { requestRender() {} };
				const component = factory(tui, fakeContextTheme(), {}, resolve);
				component.handleInput("down");
				component.handleInput(action);
			}); },
		},
	};
	return ctx;
}

function fakeContextTheme() {
	return { fg: (_color, text) => text, bold: (text) => text };
}

test("/theme NAME applies and durably records an exact built-in", async (t) => {
	const contextDir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-theme-picker-"));
	t.after(() => fs.rmSync(contextDir, { recursive: true, force: true }));
	const commands = capture(contextDir);
	const calls = [];
	const notices = [];
	await commands.get("theme").handler("DRACULA", fakeContext("enter", calls, notices));
	assert.equal(calls[0], "dracula");
	assert.equal(fs.readFileSync(path.join(contextDir, "themes", "active"), "utf8"), "dracula\n");
	assert.match(notices.at(-1).text, /Dracula/);
});

test("theme browser previews the live interface and Escape restores without persisting", async (t) => {
	const contextDir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-theme-picker-"));
	t.after(() => fs.rmSync(contextDir, { recursive: true, force: true }));
	const commands = capture(contextDir);
	const calls = [];
	const previews = [];
	const ctx = fakeContext("escape", calls, [], previews);
	await commands.get("theme").handler("", ctx);
	assert.deepEqual(previews, ["pix", "dracula", "restore"]);
	assert.equal(ctx.ui.theme.name, "pix");
	assert.deepEqual(calls, []);
	assert.equal(fs.existsSync(path.join(contextDir, "themes", "active")), false);
});

test("theme browser Enter commits the previewed theme and hides the host safety palette", async (t) => {
	const contextDir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-theme-picker-"));
	t.after(() => fs.rmSync(contextDir, { recursive: true, force: true }));
	const commands = capture(contextDir);
	const calls = [];
	await commands.get("theme").handler("", fakeContext("enter", calls, []));
	assert.deepEqual(calls, ["dracula"]);
	assert.equal(fs.readFileSync(path.join(contextDir, "themes", "active"), "utf8"), "dracula\n");
	await commands.get("theme").handler("host", fakeContext("escape", calls, []));
	assert.ok(!calls.includes("host"));
});


test("theme preview is restored when the picker throws", async () => {
	const previews = [];
	const ctx = fakeContext("escape", [], [], previews);
	ctx.ui.custom = async () => { throw new Error("render failed"); };
	await assert.rejects(capture(null).get("theme").handler("", ctx), /render failed/);
	assert.deepEqual(previews, ["restore"]);
});
