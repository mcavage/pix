import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const skill = readFileSync(new URL("../skills/healthcheck/SKILL.md", import.meta.url), "utf8");
const diagnostic = skill.match(/### 2\. Memory service[\s\S]*?```bash\n([\s\S]*?)\n```/)?.[1];

test("healthcheck distinguishes capture mode from watcher readiness", () => {
	assert.ok(diagnostic, "Memory check must contain an executable bash diagnostic");
	assert.match(skill, /\.pix\/memory-capture/);
	assert.match(skill, /capture_mode/);
	assert.match(skill, /watcher_healthy.*null.*not yet exercised/is);
	assert.match(skill, /effective automatic capture/is);
});

test("healthcheck parses the launch marker exactly like the extension", () => {
	for (const [name, marker, want] of [
		["missing", null, "explicit"],
		["trimmed opt-in", " \nexperimental-auto\t\n", "experimental-auto"],
		["internal newline", "experimental-\nauto\n", "explicit"],
		["garbled", "enabled\n", "explicit"],
	]) {
		const cwd = mkdtempSync(join(tmpdir(), "pix-healthcheck-"));
		try {
			if (marker !== null) {
				mkdirSync(join(cwd, ".pix"));
				writeFileSync(join(cwd, ".pix", "memory-capture"), marker);
			}
			const run = spawnSync("bash", ["-c", diagnostic], {
				cwd,
				env: process.env,
				encoding: "utf8",
			});
			assert.equal(run.status, 0, `${name}: ${run.stderr}`);
			assert.match(run.stdout, new RegExp(`^sandboxLaunchCaptureMode=${want}$`, "m"), name);
		} finally { rmSync(cwd, {recursive: true, force: true}); }
	}
});
