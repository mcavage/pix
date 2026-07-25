import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

// PR-gate patch test: exercises the real patch scripts against LOCAL fixtures
// vendored from the exact pinned versions (@earendil-works/pi-tui and
// pi-manage-todo-list dist files under tests/fixtures/pi-patches/**), so it
// runs with zero network calls and no `npm install`. The equivalent real
// npm-resolution smoke (install the actual published packages, then patch)
// lives in the release gate (.github/workflows/publish.yml, job
// `patch-smoke`) — see docs there for why a smoke nobody runs is worse than
// none.
//
// Each package also ships a deliberately-broken fixture (pi-tui-broken/,
// pi-manage-todo-list-broken/) whose anchor context no longer matches what
// the patch script expects — simulating upstream drift. This test asserts
// that mismatch is CAUGHT (a loud warning for the non-fatal tui patch, a
// thrown error for the fail-fast todo patch), not silently ignored.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const dockerfile = fs.readFileSync(path.join(repoRoot, "Dockerfile"), "utf8");
const fixturesRoot = path.join(repoRoot, "tests/fixtures/pi-patches");

function run(command, args, env) {
	return execFileSync(command, args, {
		cwd: repoRoot,
		env,
		encoding: "utf8",
		stdio: ["ignore", "pipe", "pipe"],
		timeout: 10_000,
		killSignal: "SIGKILL",
	});
}

// Captures BOTH stdout and stderr (execFileSync's return value only carries
// stdout on success), for asserting on the patch scripts' console.warn/error
// output either way, without caring whether the script exits 0 or nonzero.
function runCapture(command, args, env) {
	return spawnSync(command, args, {
		cwd: repoRoot,
		env,
		encoding: "utf8",
		timeout: 10_000,
		killSignal: "SIGKILL",
	});
}

function markerCount(file, marker) {
	return fs.readFileSync(file, "utf8").split(marker).length - 1;
}

function copyDir(from, to) {
	fs.mkdirSync(to, { recursive: true });
	fs.cpSync(from, to, { recursive: true });
}

// The pi-tui fixture's single runtime dependency (get-east-asian-width, which
// tui.js's utils.js imports as a bare specifier) is vendored under
// tests/fixtures/pi-patches/pi-tui/vendor-modules/ rather than node_modules/,
// so the fixture stays self-contained in git (node_modules/ is gitignored
// repo-wide and would otherwise be silently dropped from any commit/checkout).
// Materializing it as a real node_modules/ here, only in the disposable temp
// fixture, preserves real Node module resolution for the patch + render
// harness without depending on an ignored path.
function copyTuiFixture(from, to) {
	copyDir(from, to);
	const vendorModules = path.join(to, "vendor-modules");
	if (fs.existsSync(vendorModules)) {
		fs.renameSync(vendorModules, path.join(to, "node_modules"));
	}
}

test("Dockerfile and host pin the same pi + curated packages", () => {
	const piPackage = dockerfile.match(/^ARG PI_PACKAGE=(\S+)$/m)?.[1];
	const dockerPackagesBlock = dockerfile.match(
		/RUN set -eux; for p in \\\n([\s\S]*?); do \\\n\s+pi install/,
	)?.[1];
	assert.ok(dockerPackagesBlock, "Dockerfile must contain the curated pi package loop");
	const dockerPackages = dockerPackagesBlock.replaceAll("\\", "").trim().split(/\s+/);
	const todoPackage = dockerPackages.find((entry) => entry.startsWith("pi-manage-todo-list@"));
	assert.equal(piPackage, "@earendil-works/pi-coding-agent@0.82.1");
	assert.ok(todoPackage, "Dockerfile must pin pi-manage-todo-list");

	const hostRun = fs.readFileSync(
		path.join(repoRoot, "services/host/cmd/pi-stack/hostrun.go"),
		"utf8",
	);
	const hostPackagesBlock = hostRun.match(/var hostPiPackages = \[\]string\{([\s\S]*?)\n\}/)?.[1];
	assert.ok(hostPackagesBlock, "hostrun.go must declare hostPiPackages");
	const hostPackages = [...hostPackagesBlock.matchAll(/"([^"]+)"/g)].map((match) => match[1]);
	assert.deepEqual(hostPackages, dockerPackages, "host and Docker curated package pins must match");
});

test("tui bottom-pin patch applies to a fixture pi-tui and passes the render harness", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pi-stack-patches-tui-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const npmPrefix = path.join(temp, "npm-global");
	const tuiRoot = path.join(
		npmPrefix,
		"lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui",
	);
	copyTuiFixture(path.join(fixturesRoot, "pi-tui"), tuiRoot);
	const tuiPath = path.join(tuiRoot, "dist/tui.js");
	const env = { ...process.env, NPM_CONFIG_PREFIX: npmPrefix };

	const tuiPatch = path.join(repoRoot, "scripts/patches/apply-tui-bottom-pin.mjs");
	assert.match(run(process.execPath, [tuiPatch], env), /\[apply-tui-bottom-pin\] patched/);
	assert.match(run(process.execPath, [tuiPatch], env), /already patched/);
	assert.equal(markerCount(tuiPath, "Bottom-block pin"), 1);
	run(process.execPath, ["--check", tuiPath], env);

	for (const [script, result] of [
		["test.mjs", "RESULT: PASS"],
		["edge.mjs", "EDGE RESULT: PASS"],
		["integrity.mjs", "INTEGRITY RESULT: PASS"],
	]) {
		const output = run(
			process.execPath,
			[path.join(repoRoot, "docs/upstream/tui-bottom-pin", script)],
			{ ...env, PI_TUI_INDEX: path.join(tuiRoot, "dist/index.js") },
		);
		assert.match(output, new RegExp(result));
	}
});

test("tui bottom-pin patch catches upstream drift instead of silently applying", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pi-stack-patches-tui-broken-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const npmPrefix = path.join(temp, "npm-global");
	const tuiRoot = path.join(
		npmPrefix,
		"lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui",
	);
	// Start from the good fixture (for the runtime siblings tui.js imports), then
	// swap in the deliberately-broken tui.js whose anchor context no longer
	// matches what the patch script expects.
	copyTuiFixture(path.join(fixturesRoot, "pi-tui"), tuiRoot);
	fs.copyFileSync(
		path.join(fixturesRoot, "pi-tui-broken/dist/tui.js"),
		path.join(tuiRoot, "dist/tui.js"),
	);
	const tuiPath = path.join(tuiRoot, "dist/tui.js");
	const before = fs.readFileSync(tuiPath, "utf8");
	const env = { ...process.env, NPM_CONFIG_PREFIX: npmPrefix };

	const tuiPatch = path.join(repoRoot, "scripts/patches/apply-tui-bottom-pin.mjs");
	// Non-fatal by design (image build must still succeed), but LOUD: the
	// mismatch is reported, and the file is left untouched rather than
	// half-patched. The warning goes to stderr (console.warn), so capture both
	// streams instead of run()'s stdout-only return.
	const { status, stdout, stderr } = runCapture(process.execPath, [tuiPatch], env);
	assert.equal(status, 0, "non-fatal patch mismatch must still exit 0");
	const output = stdout + stderr;
	assert.match(output, /expected renderer context around .* not found/);
	assert.match(output, /pi's renderer changed; refresh scripts\/patches/);
	assert.equal(fs.readFileSync(tuiPath, "utf8"), before, "broken fixture must be left unpatched");
	assert.equal(markerCount(tuiPath, "Bottom-block pin"), 0);
});

test("todo durable-clear patch applies to a fixture pi-manage-todo-list", async (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pi-stack-patches-todo-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const home = path.join(temp, "home");
	const todoRoot = path.join(home, ".pi/agent/npm/node_modules/pi-manage-todo-list");
	copyDir(path.join(fixturesRoot, "pi-manage-todo-list"), todoRoot);
	const env = { ...process.env, HOME: home };

	const todoPatch = path.join(repoRoot, "scripts/patches/apply-todo-durable-clear.mjs");
	assert.match(run(process.execPath, [todoPatch], env), /patched/);
	assert.match(run(process.execPath, [todoPatch], env), /already patched/);
	const indexPath = path.join(todoRoot, "dist/index.js");
	const stateManagerPath = path.join(todoRoot, "dist/state-manager.js");
	assert.equal(markerCount(indexPath, "pi-stack-todo-cleared"), 1);
	assert.equal(markerCount(stateManagerPath, "pi-stack-todo-cleared"), 1);
	run(process.execPath, ["--check", indexPath], env);
	run(process.execPath, ["--check", stateManagerPath], env);

	const { TodoStateManager } = await import(pathToFileURL(stateManagerPath).href);
	const todo = (title) => ({ id: 1, title, description: title, status: "in-progress" });
	const result = (title) => ({
		type: "message",
		message: {
			role: "toolResult",
			toolName: "manage_todo_list",
			details: { todos: [todo(title)] },
		},
	});
	const clear = { type: "custom", customType: "pi-stack-todo-cleared" };
	const manager = new TodoStateManager();
	manager.loadFromSession({ sessionManager: { getBranch: () => [result("stale"), clear] } });
	assert.deepEqual(manager.read(), []);
	manager.loadFromSession({
		sessionManager: { getBranch: () => [result("stale"), clear, result("new")] },
	});
	assert.equal(manager.read()[0]?.title, "new");
});

test("todo durable-clear patch catches upstream drift instead of silently applying", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pi-stack-patches-todo-broken-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const home = path.join(temp, "home");
	const todoRoot = path.join(home, ".pi/agent/npm/node_modules/pi-manage-todo-list");
	copyDir(path.join(fixturesRoot, "pi-manage-todo-list-broken"), todoRoot);
	const env = { ...process.env, HOME: home };

	const todoPatch = path.join(repoRoot, "scripts/patches/apply-todo-durable-clear.mjs");
	// This patch has no non-fatal fallback — a context mismatch throws, so a
	// deliberately broken fixture must fail the process rather than corrupt
	// the file or silently skip.
	const { status, stderr } = runCapture(process.execPath, [todoPatch], env);
	assert.notEqual(status, 0, "a context mismatch must fail the process, not silently skip");
	assert.match(stderr, /anchor not found in .*state-manager\.js/);
});
