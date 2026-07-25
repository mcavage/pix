import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const dockerfile = fs.readFileSync(path.join(repoRoot, "Dockerfile"), "utf8");

function run(command, args, env) {
	return execFileSync(command, args, {
		cwd: repoRoot,
		env,
		encoding: "utf8",
		stdio: ["ignore", "pipe", "pipe"],
		timeout: 45_000,
		killSignal: "SIGKILL",
	});
}

function markerCount(file, marker) {
	return fs.readFileSync(file, "utf8").split(marker).length - 1;
}

test("pinned pi accepts the vendored runtime patches", { timeout: 120_000 }, async (t) => {
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

	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pi-stack-patches-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));
	const home = path.join(temp, "home");
	const npmPrefix = path.join(temp, "npm-global");
	fs.mkdirSync(home, { recursive: true });
	const env = {
		...process.env,
		HOME: home,
		NPM_CONFIG_PREFIX: npmPrefix,
		PATH: `${path.join(npmPrefix, "bin")}${path.delimiter}${process.env.PATH ?? ""}`,
	};

	run("npm", ["install", "--global", "--ignore-scripts", piPackage], env);
	const piRoot = path.join(
		npmPrefix,
		"lib/node_modules/@earendil-works/pi-coding-agent",
	);
	const tuiRoot = path.join(piRoot, "node_modules/@earendil-works/pi-tui/dist");
	const tuiPath = path.join(tuiRoot, "tui.js");
	const tuiPatch = path.join(repoRoot, "scripts/patches/apply-tui-bottom-pin.mjs");
	assert.match(run(process.execPath, [tuiPatch], env), /\[apply-tui-bottom-pin\] patched/);
	assert.match(run(process.execPath, [tuiPatch], env), /already patched/);
	assert.equal(markerCount(tuiPath, "Bottom-block pin"), 1);
	run(process.execPath, ["--check", tuiPath], env);

	const harnessEnv = { ...env, PI_TUI_INDEX: path.join(tuiRoot, "index.js") };
	for (const [script, result] of [
		["test.mjs", "RESULT: PASS"],
		["edge.mjs", "EDGE RESULT: PASS"],
		["integrity.mjs", "INTEGRITY RESULT: PASS"],
	]) {
		const output = run(
			process.execPath,
			[path.join(repoRoot, "docs/upstream/tui-bottom-pin", script)],
			harnessEnv,
		);
		assert.match(output, new RegExp(result));
	}

	const todoPrefix = path.join(home, ".pi/agent/npm");
	fs.mkdirSync(todoPrefix, { recursive: true });
	run("npm", ["install", "--prefix", todoPrefix, "--ignore-scripts", todoPackage], env);
	const todoRoot = path.join(todoPrefix, "node_modules/pi-manage-todo-list/dist");
	const todoPatch = path.join(repoRoot, "scripts/patches/apply-todo-durable-clear.mjs");
	assert.match(run(process.execPath, [todoPatch], env), /patched/);
	assert.match(run(process.execPath, [todoPatch], env), /already patched/);
	assert.equal(markerCount(path.join(todoRoot, "index.js"), "pi-stack-todo-cleared"), 1);
	assert.equal(markerCount(path.join(todoRoot, "state-manager.js"), "pi-stack-todo-cleared"), 1);

	const { TodoStateManager } = await import(
		pathToFileURL(path.join(todoRoot, "state-manager.js")).href
	);
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
