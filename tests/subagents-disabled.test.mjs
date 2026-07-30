// Unit tests for the extensions/subagents.ts host-mode kill switch (H4).
// Run: node --test tests/
//
// subagents.ts reads its env config at import time, so each scenario dynamic-
// imports a fresh module instance (cache-busting query) with the env set first.
// The pi runtime packages are stubbed via a module.register resolve hook.
import assert from "node:assert";
import * as fs from "node:fs";
import { register } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

// An empty agent dir for the stubbed getAgentDir(): no agents, no routing.json.
const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "subagents-test-"));
fs.mkdirSync(path.join(agentDir, "agents"), { recursive: true });
process.env.PI_TEST_AGENT_DIR = agentDir;

// PI_SUBAGENT_DEPTH is cleared too: when this test itself runs INSIDE a
// subagent, the inherited depth would shift the depth-limit assertions.
const ENV_KEYS = [
	"PI_SUBAGENT_DISABLED",
	"PI_SUBAGENT_MAX_DEPTH",
	"PI_SUBAGENT_DEPTH",
];
let seq = 0;

// Import a FRESH subagents.ts instance with the given env, capture what it
// registers on a fake pi API, and restore the env.
async function loadSubagents(env) {
	const saved = {};
	for (const k of ENV_KEYS) {
		saved[k] = process.env[k];
		delete process.env[k];
	}
	for (const [k, v] of Object.entries(env)) process.env[k] = v;
	try {
		const url = new URL(
			`../extensions/subagents.ts?case=${seq++}`,
			import.meta.url,
		);
		const mod = await import(url.href);
		const reg = { tool: null, command: null };
		mod.default({
			on() {},
			registerTool(t) {
				reg.tool = t;
			},
			registerCommand(_name, cfg) {
				reg.command = cfg;
			},
		});
		assert.ok(reg.tool, "subagent tool registered");
		assert.ok(reg.command, "/subagents command registered");
		reg.mod = mod;
		return reg;
	} finally {
		for (const k of ENV_KEYS) {
			if (saved[k] === undefined) delete process.env[k];
			else process.env[k] = saved[k];
		}
	}
}

test("curated children reload generated inference providers before subagents", async () => {
	const reg = await loadSubagents({});
	const self = path.join(agentDir, "extensions", "subagents.ts");
	const inference = path.join(agentDir, "extensions", "inference.ts");
	fs.mkdirSync(path.dirname(self), { recursive: true });
	fs.writeFileSync(self, "");
	fs.writeFileSync(inference, "");
	assert.deepEqual(reg.mod.coreChildExtensionArgs(self), ["-e", inference, "-e", self]);
});

const ctx = { cwd: process.cwd(), hasUI: false, ui: null };
const exec = (reg, params) =>
	reg.tool.execute("id", params, new AbortController().signal, undefined, ctx);
const text = (r) => r.content?.map((c) => c.text ?? "").join("\n") ?? "";

// ── (a) disabled refuses EVERY spawn path: single, parallel, chain, doctor ──
test("PI_SUBAGENT_DISABLED=1 refuses single, parallel, chain, and doctor", async () => {
	const reg = await loadSubagents({ PI_SUBAGENT_DISABLED: "1" });
	for (const params of [
		{ agent: "fanout", task: "t" },
		{ tasks: [{ agent: "fanout", task: "t" }] },
		{ chain: [{ agent: "fanout", task: "t" }] },
	]) {
		const r = await exec(reg, params);
		assert.equal(r.isError, true, JSON.stringify(params));
		assert.match(text(r), /disabled in host mode/i);
		assert.equal(r.details.results.length, 0, "nothing may have run");
	}
	// The doctor path used to call runSingle() directly, bypassing the check
	// that lived only in execute() — it spawned the canary even when disabled.
	const notes = [];
	await reg.command.handler("doctor", {
		cwd: process.cwd(),
		ui: { notify: (msg, level) => notes.push({ msg, level }) },
	});
	assert.equal(notes.length, 1);
	assert.match(notes[0].msg, /refusing to spawn the canary/i);
	assert.match(notes[0].msg, /disabled in host mode/i);
	assert.equal(notes[0].level, "error");
});

// ── (c) "0"/"false" are NOT disabled (Boolean(env) was truthy for both) ─────
test('PI_SUBAGENT_DISABLED="0" / "false" do not disable', async () => {
	for (const v of ["0", "false", ""]) {
		const reg = await loadSubagents({ PI_SUBAGENT_DISABLED: v });
		const r = await exec(reg, { agent: "no-such-agent", task: "t" });
		assert.equal(r.isError, true);
		// Reaches the normal path (unknown agent), NOT the host-mode refusal.
		assert.match(text(r), /Unknown agent/i, `env value ${JSON.stringify(v)}`);
		assert.doesNotMatch(text(r), /disabled in host mode/i);
	}
});

// ── (b) an explicit MAX_DEPTH=0 is honored (num() used to reject zero) ──────
test("PI_SUBAGENT_MAX_DEPTH=0 refuses at depth 0/0", async () => {
	const reg = await loadSubagents({ PI_SUBAGENT_MAX_DEPTH: "0" });
	const r = await exec(reg, { agent: "no-such-agent", task: "t" });
	assert.equal(r.isError, true);
	assert.match(text(r), /depth limit reached \(0\/0\)/i);
});

test("unset / invalid MAX_DEPTH still defaults to 3", async () => {
	for (const env of [{}, { PI_SUBAGENT_MAX_DEPTH: "banana" }]) {
		const reg = await loadSubagents(env);
		const r = await exec(reg, { agent: "no-such-agent", task: "t" });
		// Not a depth refusal — falls through to the unknown-agent path.
		assert.match(text(r), /Unknown agent/i);
	}
});
