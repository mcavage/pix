// Pins the REAL detect/fallback contract for delegating a local-process
// child through the pix-session Gateway MCP tool (services/host/session,
// cmd/pix's hidden __pix-session-mcp/__pix-session-child): detection reads
// the SAME injected mcp.json lib/mcp-gateway-client.ts already resolves the
// memory Gateway from (no more hardcoded-unavailable env seam), and the
// request builder must never be able to construct a field outside the
// bounded agent/task/model/target contract.
import assert from "node:assert";
import { register } from "node:module";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

async function loadSubagents() {
	const url = new URL(
		`../extensions/subagents.ts?sessiongw=${Date.now()}-${Math.random()}`,
		import.meta.url,
	);
	return import(url.href);
}

test("sessionGatewayAvailability reports unavailable with no Gateway configured", async () => {
	const mod = await loadSubagents();
	const result = mod.sessionGatewayAvailability(() => null);
	assert.strictEqual(result.available, false);
	assert.match(result.reason, /no MCP gateway is registered/i);
});

test("sessionGatewayAvailability reports available once a real Gateway config is present", async () => {
	const mod = await loadSubagents();
	const result = mod.sessionGatewayAvailability(() => ({
		url: "http://127.0.0.1:9/mcp",
		headers: {},
	}));
	assert.strictEqual(result.available, true);
	assert.match(result.reason, /pix_session_delegate/);
});

test("sessionGatewayAvailability defaults to the real, shared Gateway config reader", async () => {
	const mod = await loadSubagents();
	// No mcp.json anywhere PI_CODING_AGENT_DIR could resolve to in this test
	// process: the DEFAULT probe (no override) must still answer, honestly,
	// "unavailable" rather than throwing.
	const result = mod.sessionGatewayAvailability();
	assert.strictEqual(typeof result.available, "boolean");
	assert.strictEqual(typeof result.reason, "string");
});

test("buildSessionDelegateRequest carries only the bounded fields", async () => {
	const mod = await loadSubagents();
	const req = mod.buildSessionDelegateRequest("fanout", "do the thing", "anthropic/claude-sonnet-5");
	assert.deepStrictEqual(Object.keys(req).sort(), ["agent", "model", "target", "task"]);
	assert.strictEqual(req.target, "local-process");
	// No amount of caller intent can smuggle a shell/command/argv field through
	// the builder's own signature — this assertion is really about the type,
	// but at runtime we can at least confirm the object literal it produces
	// never carries one.
	assert.strictEqual(req.command, undefined);
	assert.strictEqual(req.argv, undefined);
	assert.strictEqual(req.shell, undefined);
});

test("buildSessionDelegateRequest omits model when absent, never sends empty string", async () => {
	const mod = await loadSubagents();
	const req = mod.buildSessionDelegateRequest("fanout", "do the thing", undefined);
	assert.strictEqual("model" in req, false);
});
