// Regression test for the subagent CHILD-PROCESS LEAK.
// Run: node --test tests/
//
// Children are spawned with detached:true so a watchdog can signal the whole
// process GROUP (child pi + grandchildren). The side effect is that the group
// outlives this process, and both watchdogs are setTimeout()s that die with the
// session — so any subagent still running when the session ends became an
// unkillable orphan that kept burning CPU and provider spend. Sessions
// accumulated them. This pins the reaper that fixes it.
import assert from "node:assert";
import { spawn } from "node:child_process";
import * as fs from "node:fs";
import { register } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "subagents-reaper-"));
fs.mkdirSync(path.join(agentDir, "agents"), { recursive: true });
process.env.PI_TEST_AGENT_DIR = agentDir;

let seq = 0;
// Load the extension and capture the hooks it registers on a fake pi API.
async function loadSubagents() {
	const url = new URL(
		`../extensions/subagents.ts?reaper=${seq++}`,
		import.meta.url,
	);
	const mod = await import(url.href);
	const on = {};
	mod.default({
		on(evt, fn) {
			on[evt] = fn;
		},
		registerTool() {},
		registerCommand() {},
	});
	return { mod, on };
}

// A real detached process group, standing in for a child pi that is still
// working when the session ends.
function spawnOrphanCandidate() {
	const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], {
		detached: process.platform !== "win32",
		stdio: "ignore",
	});
	return child;
}

const alive = (pid) => {
	try {
		process.kill(pid, 0);
		return true;
	} catch {
		return false;
	}
};

const settle = async (pid, ms = 2000) => {
	const deadline = Date.now() + ms;
	while (Date.now() < deadline && alive(pid)) {
		await new Promise((r) => setTimeout(r, 25));
	}
	return alive(pid);
};

test("session shutdown reaps a subagent that is still running", async () => {
	const { mod, on } = await loadSubagents();
	assert.equal(
		typeof on.session_shutdown,
		"function",
		"the extension must hook session_shutdown to reap live children",
	);

	const child = spawnOrphanCandidate();
	child.unref();
	mod.LIVE_CHILD_PGIDS.add(child.pid);
	assert.ok(alive(child.pid), "precondition: the child is running");

	on.session_shutdown();

	assert.equal(
		await settle(child.pid),
		false,
		"a child still running at session_shutdown must be killed, not orphaned",
	);
	assert.equal(
		mod.LIVE_CHILD_PGIDS.size,
		0,
		"the registry must be drained so no stale pid is signalled later",
	);
});

test("the reaper signals the whole process GROUP, not just the direct child", async () => {
	if (process.platform === "win32") return; // no process groups to test
	const { mod } = await loadSubagents();

	// A child that spawns its own grandchild, then reports the grandchild's pid.
	// Killing only the direct pid is what used to leave the real CPU burner —
	// pi's own tool subprocesses — running.
	const child = spawn(
		process.execPath,
		[
			"-e",
			`const { spawn } = require("node:child_process");
			 const g = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { stdio: "ignore" });
			 console.log(g.pid);
			 setInterval(() => {}, 1000);`,
		],
		{ detached: true, stdio: ["ignore", "pipe", "ignore"] },
	);
	const grandPid = await new Promise((resolve) => {
		child.stdout.on("data", (d) => resolve(Number(String(d).trim())));
	});
	assert.ok(alive(grandPid), "precondition: the grandchild is running");

	mod.LIVE_CHILD_PGIDS.add(child.pid);
	mod.reapLiveChildren();

	assert.equal(
		await settle(grandPid),
		false,
		"the grandchild shares the group and must die with it",
	);
	assert.equal(await settle(child.pid), false, "the child must die too");
});

test("reaping is idempotent and survives an already-dead pid", async () => {
	const { mod } = await loadSubagents();
	const child = spawnOrphanCandidate();
	child.unref();
	mod.LIVE_CHILD_PGIDS.add(child.pid);
	mod.reapLiveChildren();
	await settle(child.pid);
	// A second pass (session_shutdown then process 'exit') must not throw.
	assert.equal(mod.reapLiveChildren(), 0);
});
