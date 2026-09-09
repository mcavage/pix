import { randomBytes } from "node:crypto";
import { existsSync, lstatSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";

export const THEMES_DIR = "themes";
export const ACTIVE_THEME_FILE = "active";
export const THEME_PREFERENCE_MAX_BYTES = 66;

const THEME_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

export function validateThemePreference(input: string): string | null {
	if (typeof input !== "string") return null;
	const value = input.trim();
	return THEME_NAME.test(value) ? value : null;
}

export function themePreferencePath(contextDir: string): string {
	return path.join(contextDir, THEMES_DIR, ACTIVE_THEME_FILE);
}

export function readThemePreference(contextDir: string): string | null {
	const file = themePreferencePath(contextDir);
	if (!existsSync(file)) return null;
	const stat = lstatSync(file);
	if (stat.isSymbolicLink() || !stat.isFile() || stat.size > THEME_PREFERENCE_MAX_BYTES) return null;
	return validateThemePreference(readFileSync(file, "utf8"));
}

export function writeThemePreference(contextDir: string, preference: string): void {
	const value = validateThemePreference(preference);
	if (!value) throw new Error("Theme name must use 1-64 letters, numbers, dots, underscores, or hyphens.");
	const dir = path.join(contextDir, THEMES_DIR);
	const dirStat = existsSync(dir) ? lstatSync(dir) : null;
	if (dirStat && (dirStat.isSymbolicLink() || !dirStat.isDirectory())) {
		throw new Error(`Theme preference path is not a real directory: ${dir}`);
	}
	mkdirSync(dir, { recursive: true, mode: 0o700 });
	const file = path.join(dir, ACTIVE_THEME_FILE);
	if (existsSync(file) && lstatSync(file).isSymbolicLink()) {
		throw new Error(`Refusing to replace symlinked theme preference: ${file}`);
	}
	const temp = `${file}.tmp-${process.pid}-${randomBytes(8).toString("hex")}`;
	try {
		writeFileSync(temp, `${value}\n`, { encoding: "utf8", mode: 0o600, flag: "wx" });
		renameSync(temp, file);
	} finally {
		rmSync(temp, { force: true });
	}
}
