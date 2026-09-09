import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import {
	readThemePreference,
	themePreferencePath,
	validateThemePreference,
	writeThemePreference,
} from "../lib/theme-preference.ts";

test("theme preferences accept one fixed global selection", () => {
	assert.equal(validateThemePreference("nord"), "nord");
	for (const invalid of ["", "/nord", "nord/", "solarized-light/solarized-dark", "../nord", "has space", "x".repeat(65)]) {
		assert.equal(validateThemePreference(invalid), null, invalid);
	}
});

test("theme preferences round-trip atomically with private permissions", (t) => {
	const contextDir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-theme-"));
	t.after(() => fs.rmSync(contextDir, { recursive: true, force: true }));
	writeThemePreference(contextDir, "catppuccin-mocha");
	const file = themePreferencePath(contextDir);
	assert.equal(readThemePreference(contextDir), "catppuccin-mocha");
	assert.equal(fs.statSync(file).mode & 0o777, 0o600);
	writeThemePreference(contextDir, "nord");
	assert.equal(readThemePreference(contextDir), "nord");
	assert.deepEqual(fs.readdirSync(path.dirname(file)).sort(), ["active"]);
});

test("theme preferences refuse symlinks and ignore malformed state", (t) => {
	const contextDir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-theme-"));
	t.after(() => fs.rmSync(contextDir, { recursive: true, force: true }));
	const themesDir = path.join(contextDir, "themes");
	fs.mkdirSync(themesDir);
	const target = path.join(contextDir, "target");
	fs.writeFileSync(target, "nord\n");
	fs.symlinkSync(target, path.join(themesDir, "active"));
	assert.equal(readThemePreference(contextDir), null);
	assert.throws(() => writeThemePreference(contextDir, "pix"), /symlinked theme preference/);
	fs.rmSync(path.join(themesDir, "active"));
	fs.writeFileSync(path.join(themesDir, "active"), "not a theme\n");
	assert.equal(readThemePreference(contextDir), null);
});
