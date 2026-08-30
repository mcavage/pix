// Pins the explicit FALLBACK strategy for delegating a local-process child
// through the pix-session Gateway MCP tool (services/host/session,
// cmd/pix's hidden __pix-session-mcp/__pix-session-child): until launch-time
// wiring registers the reserved static-mcp server, sessionGatewayAvailability
// must report unavailable so every existing subagent spawn call site keeps
// using the direct child_process path (LIVE_CHILD_PGIDS) UNCHANGED, and the
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

test("sessionGatewayAvailability reports unavailable with no env wiring", async () => {
	const mod = await loadSubagents();
	const result = mod.sessionGatewayAvailability({});
	assert.strictEqual(result.available, false);
	assert.match(result.reason, /PIX_SESSION_MCP_TOOL/);
});

test("sessionGatewayAvailability reports available once the Gateway tool is wired", async () => {
	const mod = await loadSubagents();
	const result = mod.sessionGatewayAvailability({
		PIX_SESSION_MCP_TOOL: "pix-session/pix_session_delegate",
	});
	assert.strictEqual(result.available, true);
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
