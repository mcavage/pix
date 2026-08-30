// Proves the REAL pix_session_delegate call path and the fallback rule it
// feeds (resolveSessionDelegation): a capability-absent failure falls back
// to the direct spawn, any OTHER failure is reported, never swallowed.
import assert from "node:assert";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { register } from "node:module";
import { test, after } from "node:test";
import { makeFakeGateway, listen, writeGatewayConfig } from "./fake-mcp-gateway.mjs";

register("./stub-loader.mjs", import.meta.url);

let seq = 0;
const ENV_KEYS = ["PI_CODING_AGENT_DIR"];
const ORIGINAL_ENV = Object.fromEntries(ENV_KEYS.map((k) => [k, process.env[k]]));
after(() => {
	for (const k of ENV_KEYS) {
		if (ORIGINAL_ENV[k] === undefined) delete process.env[k];
		else process.env[k] = ORIGINAL_ENV[k];
	}
});

async function loadSubagents() {
	const url = new URL(
		`../extensions/subagents.ts?sessiondelegate=${seq++}-${Date.now()}-${Math.random()}`,
		import.meta.url,
	);
	return import(url.href);
}

function pointAgentDirAt(url) {
	const agentDir = mkdtempSync(join(tmpdir(), "pix-agentdir-"));
	writeGatewayConfig(agentDir, url);
	process.env.PI_CODING_AGENT_DIR = agentDir;
}

test("delegateViaSessionGateway returns the bounded {tree, node} result on success", async () => {
	const mod = await loadSubagents();
	const gateway = makeFakeGateway((name, args) => {
		assert.strictEqual(name, mod.SESSION_DELEGATE_TOOL);
		assert.strictEqual(args.agent, "fanout");
		assert.strictEqual(args.target, "local-process");
		return { tree: "t1", node: "n1" };
	});
	const url = await listen(gateway.server);
	try {
		pointAgentDirAt(url);
		// delegateChildViaGateway is the real, ready-to-call entry point: it uses
		// this module's own shared Gateway client, resolving PI_CODING_AGENT_DIR's
		// mcp.json exactly the way lib/mcp-gateway-client.ts does for memory.
		const req = mod.buildSessionDelegateRequest("fanout", "do the thing", undefined);
		const result = await mod.delegateChildViaGateway(req, 2000);
		assert.deepStrictEqual(result, { ok: true, result: { tree: "t1", node: "n1" } });
	} finally {
		gateway.server.close();
	}
});

test("resolveSessionDelegation falls back when no Gateway is configured at all", async () => {
	const mod = await loadSubagents();
	const req = mod.buildSessionDelegateRequest("fanout", "task", undefined);
	const outcome = await mod.resolveSessionDelegation(
		{ callTool: async () => { throw new Error("must never be called"); } },
		req,
		1000,
		mod.sessionGatewayAvailability(() => null),
	);
	assert.strictEqual(outcome.ok, false);
	assert.strictEqual(outcome.fallback, true);
	assert.match(outcome.reason, /no MCP gateway is registered/i);
});

test("resolveSessionDelegation falls back when the Gateway reports the tool unknown", async () => {
	const mod = await loadSubagents();
	const fakeClient = {
		callTool: async () => {
			throw new Error("Tool not found");
		},
	};
	const req = mod.buildSessionDelegateRequest("fanout", "task", undefined);
	const outcome = await mod.resolveSessionDelegation(
		fakeClient,
		req,
		1000,
		mod.sessionGatewayAvailability(() => ({ url: "http://127.0.0.1:1/mcp", headers: {} })),
	);
	assert.strictEqual(outcome.ok, false);
	assert.strictEqual(outcome.fallback, true);
	assert.match(outcome.reason, /tool not found/i);
});

test("resolveSessionDelegation does NOT fall back on an arbitrary call failure", async () => {
	const mod = await loadSubagents();
	const fakeClient = {
		callTool: async () => {
			throw new Error("memory gateway HTTP 503 calling pix_session_delegate: upstream unavailable");
		},
	};
	const req = mod.buildSessionDelegateRequest("fanout", "task", undefined);
	const outcome = await mod.resolveSessionDelegation(
		fakeClient,
		req,
		1000,
		mod.sessionGatewayAvailability(() => ({ url: "http://127.0.0.1:1/mcp", headers: {} })),
	);
	assert.strictEqual(outcome.ok, false);
	assert.strictEqual(outcome.fallback, false);
	assert.ok(outcome.error instanceof Error);
	assert.match(outcome.error.message, /503/);
});

test("resolveSessionDelegation reports success without ever attempting a fallback", async () => {
	const mod = await loadSubagents();
	const fakeClient = { callTool: async () => ({ tree: "t9", node: "n9" }) };
	const req = mod.buildSessionDelegateRequest("fanout", "task", undefined);
	const outcome = await mod.resolveSessionDelegation(
		fakeClient,
		req,
		1000,
		mod.sessionGatewayAvailability(() => ({ url: "http://127.0.0.1:1/mcp", headers: {} })),
	);
	assert.deepStrictEqual(outcome, { ok: true, result: { tree: "t9", node: "n9" } });
});

test("isSessionCapabilityAbsent classifies absence signatures and nothing else", async () => {
	const mod = await loadSubagents();
	for (const msg of [
		"no MCP gateway registered: \"mcp-gateway\" is missing from /x/mcp.json",
		"Tool not found",
		"unknown tool: pix_session_delegate",
		"Method not found",
	]) {
		assert.strictEqual(mod.isSessionCapabilityAbsent(new Error(msg)), true, msg);
	}
	for (const msg of [
		"memory gateway HTTP 500 calling pix_session_delegate",
		"timeout",
		"pix-session Gateway returned an unexpected pix_session_delegate result: null",
	]) {
		assert.strictEqual(mod.isSessionCapabilityAbsent(new Error(msg)), false, msg);
	}
});

test("delegateViaSessionGateway throws on a malformed (non {tree,node}) result", async () => {
	const mod = await loadSubagents();
	const fakeClient = { callTool: async () => ({ ok: true }) };
	const req = mod.buildSessionDelegateRequest("fanout", "task", undefined);
	await assert.rejects(
		() => mod.delegateViaSessionGateway(fakeClient, req, 1000),
		/unexpected pix_session_delegate result/,
	);
});
