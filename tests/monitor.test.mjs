// Unit tests for extensions/monitor.ts (round-2 review fixes: R2-1, R2-3, R2-6).
// Run: node --test "tests/*.test.mjs"
//
// The pure summary/classification helpers are imported and exercised
// directly. The shutdown-race fix (R2-1) has no exported hook (queue/timer
// state lives inside the default export's closure by design, same as
// flushing/disabledUntil), so it's exercised end-to-end: a stub `pi.on`
// captures the real hook handlers and a tiny local HTTP server stands in for
// the host monitor process, letting the test observe request counts across a
// simulated session_shutdown instead of reaching into private state.
import assert from "node:assert";
import * as http from "node:http";
import { test } from "node:test";

const monitor = await import("../extensions/monitor.ts");
const {
	classifyToolSource,
	isMcpSourceInfo,
	mcpServerFromSourceInfo,
	partitionToolNames,
	summarizeRequest,
	extractAssistantOutput,
	normalizeHeaders,
	redactHeaders,
} = monitor;

// ─── R2-3: MCP classification fallback ──────────────────────────────────────

test("R2-3a: unknown provenance defaults to builtin, never skill:<name>", () => {
	// No sourceInfo at all (getAllTools() missing/unavailable) and a name that
	// matches none of the builtin/mcp heuristics.
	assert.equal(classifyToolSource("slack_post", undefined), "builtin");
	assert.equal(classifyToolSource("gmail_search", null), "builtin");
	// sourceInfo present but says nothing positive either way.
	assert.equal(classifyToolSource("some_tool", { path: "index.ts", source: "extension" }), "builtin");
});

test("R2-3a: skill:<name> only with positive sourceInfo evidence", () => {
	assert.equal(classifyToolSource("my_skill_tool", { source: "skill" }), "skill:my_skill_tool");
	// Same name, no positive evidence -> builtin, not skill:.
	assert.equal(classifyToolSource("my_skill_tool", undefined), "builtin");
});

test("R3-1: pi-mcp-adapter package path IS positive MCP provenance, but not a server name", () => {
	// Real gateway tools (slack_post, gmail_search, ...) are registered
	// THROUGH the pi MCP adapter extension, so pi.getAllTools() attributes
	// them to that extension's own package path/source rather than to a
	// per-server identifier. NOTE: the exact runtime sourceInfo shape pi
	// reports here is unverified without a live gateway attachment (no MCP
	// server is attached in this dev sandbox) — this fixture uses the
	// adapter PACKAGE PATH itself as the positive MCP signal, which is the
	// documented, defensible provenance marker (R3-1).
	const info = { path: "pi-mcp-adapter/index.ts", source: "extension" };
	// R3-1: the adapter package is positive MCP provenance...
	assert.equal(isMcpSourceInfo(info), true);
	// ...but the package path is still NOT a server identifier (R2-3b's
	// original fix must be preserved): no name can be anchored out of it.
	assert.equal(mcpServerFromSourceInfo(info), "");
	// So classification is MCP-with-unknown-server, never "builtin" (the
	// R3-1 bug this replaces) and never a mis-parsed "mcp:adapter/index.ts".
	assert.equal(classifyToolSource("slack_post", info), "mcp:unknown");
	assert.equal(classifyToolSource("gmail_search", info), "mcp:unknown");
});

test("R3-1: adapter provenance plus a dedicated server field resolves mcp:<server>", () => {
	const info = { path: "pi-mcp-adapter/index.ts", source: "extension", server: "slack" };
	assert.equal(isMcpSourceInfo(info), true);
	assert.equal(mcpServerFromSourceInfo(info), "slack");
	assert.equal(classifyToolSource("slack_post", info), "mcp:slack");

	const metaInfo = { path: "pi-mcp-adapter/index.ts", metadata: { server: "gmail" } };
	assert.equal(mcpServerFromSourceInfo(metaInfo), "gmail");
	assert.equal(classifyToolSource("gmail_search", metaInfo), "mcp:gmail");
});

test("R3-1: an unrelated extension package that merely contains \"mcp\" is NOT adapter provenance", () => {
	// Guard against re-widening the R2-3b fix: only the whole "pi-mcp-adapter"
	// package name is a positive signal, not any path containing "mcp".
	const info = { path: "some-mcplike-extension/index.ts", source: "extension" };
	assert.equal(isMcpSourceInfo(info), false);
	assert.equal(classifyToolSource("slack_post", info), "builtin");
});

test("R4-4: adapter regex requires a real segment terminator, not a bare word boundary", () => {
	// `\b` also matches before `-` or `.`, so a naive adapter-name regex would
	// treat these unrelated packages as the adapter itself. Only an exact
	// `pi-mcp-adapter` segment (end-of-string or `/`/`@` after it) counts.
	const helperInfo = { path: "pi-mcp-adapter-helper/index.js", source: "extension" };
	assert.equal(isMcpSourceInfo(helperInfo), false);
	assert.equal(classifyToolSource("slack_post", helperInfo), "builtin");

	const backupInfo = { path: "pi-mcp-adapter.backup/index.ts", source: "extension" };
	assert.equal(isMcpSourceInfo(backupInfo), false);
	assert.equal(classifyToolSource("slack_post", backupInfo), "builtin");
});

test("R2-3b: positive MCP evidence (anchored scheme) is still honored", () => {
	assert.equal(mcpServerFromSourceInfo({ path: "<mcp:slack>" }), "slack");
	assert.equal(mcpServerFromSourceInfo({ source: "mcp:gmail" }), "gmail");
	assert.equal(classifyToolSource("slack_post", { path: "<mcp:slack>" }), "mcp:slack");
	assert.equal(classifyToolSource("gmail_search", { source: "mcp:gmail" }), "mcp:gmail");
});

test("R2-3b: MCP provenance certain but server unparseable -> mcp:unknown, not a garbage path", () => {
	// isMcpSourceInfo true (anchored "mcp" scheme) but no capture group value.
	const info = { source: "mcp:" };
	assert.equal(isMcpSourceInfo(info), true);
	assert.equal(mcpServerFromSourceInfo(info), "");
	assert.equal(classifyToolSource("weird_tool", info), "mcp:unknown");
});

test("R2-3: partitionToolNames still routes real gateway tool names correctly with live metadata", () => {
	const sourceByName = new Map([
		["slack_post", { path: "<mcp:slack>" }],
		["gmail_search", { source: "mcp:gmail" }],
		["read", { source: "builtin" }],
	]);
	const { toolNames, mcpToolNames } = partitionToolNames(
		["slack_post", "gmail_search", "read"],
		(n) => classifyToolSource(n, sourceByName.get(n)).startsWith("mcp:"),
	);
	assert.deepEqual(mcpToolNames, ["slack_post", "gmail_search"]);
	assert.deepEqual(toolNames, ["read"]);
});

test("R3-1: partitionToolNames routes adapter-sourced gateway tools into mcpToolNames, not toolNames", () => {
	// Same scenario as above, but with the ADAPTER-STYLE sourceInfo pi
	// actually reports for gateway tools per R3-1 (package path, no bracketed
	// server marker) instead of an idealized "<mcp:slack>" fixture.
	const sourceByName = new Map([
		["slack_post", { path: "pi-mcp-adapter/index.ts", source: "extension" }],
		["gmail_search", { path: "pi-mcp-adapter/index.ts", source: "extension" }],
		["read", { source: "builtin" }],
	]);
	const { toolNames, mcpToolNames } = partitionToolNames(
		["slack_post", "gmail_search", "read"],
		(n) => classifyToolSource(n, sourceByName.get(n)).startsWith("mcp:"),
	);
	assert.deepEqual(mcpToolNames, ["slack_post", "gmail_search"]);
	assert.deepEqual(toolNames, ["read"]);
});

// ─── R2-6: toolSchemaHash on RequestSummary ─────────────────────────────────

test("R2-6: summarizeRequest emits toolSchemaHash matching the enqueued tool-schema blob", () => {
	const payload = {
		messages: [{ role: "user", content: "hi" }],
		tools: [{ name: "read", input_schema: { type: "object" } }],
	};
	const result = summarizeRequest(payload, 0);
	assert.ok(result.toolSchemaHash, "toolSchemaHash must be set when tools are present");
	assert.equal(result.toolSchemaHash.length, 64, "sha256 hex digest");
	const toolBlob = result.blobs.find((b) => b.hash === result.toolSchemaHash);
	assert.ok(toolBlob, "the tool-schema blob must be enqueued under the SAME hash as toolSchemaHash");
	assert.equal(toolBlob.text, JSON.stringify(payload.tools));
});

test("R2-6: toolSchemaHash is empty string when there are no tools", () => {
	const result = summarizeRequest({ messages: [{ role: "user", content: "hi" }] }, 0);
	assert.equal(result.toolSchemaHash, "");
	assert.ok(!result.blobs.some((b) => b.text === "[]"));
});

// ─── redactHeaders: sensitive header VALUES are redacted, keys/shape kept ──

test("redactHeaders redacts known sensitive header names, case-insensitively", () => {
	const headers = {
		Authorization: "proxy-managed",
		"X-Api-Key": "proxy-managed",
		"api-key": "secret-value",
		Cookie: "session=abc",
		"Set-Cookie": "session=abc; Path=/",
		"Proxy-Authorization": "Basic xyz",
		"content-type": "application/json",
		"user-agent": "pi/1.0",
	};
	const redacted = redactHeaders(headers);
	assert.equal(redacted.Authorization, "<redacted>");
	assert.equal(redacted["X-Api-Key"], "<redacted>");
	assert.equal(redacted["api-key"], "<redacted>");
	assert.equal(redacted.Cookie, "<redacted>");
	assert.equal(redacted["Set-Cookie"], "<redacted>");
	assert.equal(redacted["Proxy-Authorization"], "<redacted>");
	// Non-sensitive headers pass through unchanged, and every original key is
	// still present (redaction never drops a header).
	assert.equal(redacted["content-type"], "application/json");
	assert.equal(redacted["user-agent"], "pi/1.0");
	assert.deepEqual(Object.keys(redacted).sort(), Object.keys(headers).sort());
});

test("redactHeaders passes through undefined unchanged", () => {
	assert.equal(redactHeaders(undefined), undefined);
});

test("redactHeaders composes with normalizeHeaders for a before_provider_headers-shaped payload", () => {
	// ProviderHeaders = Record<string, string | null>; a null value is an
	// explicit "suppress this header" per pi's docs and normalizeHeaders drops
	// it (v != null check), so it never reaches redactHeaders at all.
	const raw = { authorization: "proxy-managed", "x-request-id": "abc-123", "x-suppressed": null };
	const redacted = redactHeaders(normalizeHeaders(raw));
	assert.equal(redacted.authorization, "<redacted>");
	assert.equal(redacted["x-request-id"], "abc-123");
	assert.equal("x-suppressed" in redacted, false);
});

// ─── before_provider_headers: stash/flush wiring for request headers ──────
// End-to-end against a stub `pi` + a local HTTP server that RECORDS every
// posted NDJSON line, since the stash (`pendingRequest`) is intentionally
// private to the default export's closure, same as the R2-1 queue/timer
// state above.

function startRecordingServer() {
	const lines = [];
	const server = http.createServer((req, res) => {
		let body = "";
		req.on("data", (chunk) => (body += chunk));
		req.on("end", () => {
			if (req.url === "/ingest") {
				for (const line of body.split("\n")) {
					if (line.trim()) lines.push(JSON.parse(line));
				}
			}
			res.writeHead(200);
			res.end();
		});
	});
	return new Promise((resolve) => {
		server.listen(0, "127.0.0.1", () => resolve({ server, port: server.address().port, lines }));
	});
}

test("before_provider_headers attaches redacted headers to the stashed provider_request", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});
	// Not emitted yet: before_provider_headers hasn't fired.
	await new Promise((r) => setTimeout(r, 100));
	assert.ok(
		!lines.some((l) => l.kind === "provider_request"),
		"provider_request must not be emitted before before_provider_headers fires",
	);

	handlers.get("before_provider_headers")?.({
		headers: { authorization: "proxy-managed", "x-request-id": "abc-123" },
	});
	await new Promise((r) => setTimeout(r, 100));

	const pr = lines.find((l) => l.kind === "provider_request");
	assert.ok(pr, "provider_request must be emitted once before_provider_headers fires");
	assert.equal(pr.headers.authorization, "<redacted>");
	assert.equal(pr.headers["x-request-id"], "abc-123");
});

test("a stash with no before_provider_headers is flushed headers-less by the NEXT before_provider_request", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "first" }] } }, {});
	// before_provider_headers never fires for turn 1 (simulates a missing hook
	// or an out-of-order pi version). A second turn starts instead.
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "first" }, { role: "user", content: "second" }] } }, {});
	await new Promise((r) => setTimeout(r, 100));

	const requests = lines.filter((l) => l.kind === "provider_request");
	assert.equal(requests.length, 1, "the orphaned first stash must be flushed, never silently dropped");
	assert.equal("headers" in requests[0], false, "the flushed orphan carries no headers");
});

test("a stash with no before_provider_headers is flushed headers-less by a session reset (session_start)", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});
	// before_provider_headers never fires; the session resets instead (e.g. a
	// new session_start on the same live extension instance).
	handlers.get("session_start")?.({}, {});
	await new Promise((r) => setTimeout(r, 100));

	const requests = lines.filter((l) => l.kind === "provider_request");
	assert.equal(requests.length, 1, "the orphaned stash must be flushed on session reset, never silently dropped");
	assert.equal("headers" in requests[0], false, "the flushed orphan carries no headers");
});

// ─── R2-1: shutdown must not resurrect the retry timer / requeue ───────────
// End-to-end against a stub `pi` + a local HTTP server, since the queue/timer
// state is intentionally private to the default export's closure.

function startServer() {
	let count = 0;
	const server = http.createServer((req, res) => {
		count += 1;
		res.writeHead(500); // always "fail" so a failed send schedules a retry
		res.end();
	});
	return new Promise((resolve) => {
		server.listen(0, "127.0.0.1", () => resolve({ server, port: server.address().port, count: () => count }));
	});
}

async function loadMonitorAgainst(port) {
	const prevUrl = process.env.PI_STACK_MONITOR_URL;
	const prevEnabled = process.env.PI_STACK_MONITOR;
	process.env.PI_STACK_MONITOR_URL = `http://127.0.0.1:${port}`;
	process.env.PI_STACK_MONITOR = "1";
	try {
		// Bust the module cache with a unique query string so each test gets a
		// fresh closure (module-level seqCounter aside, which is harmless here).
		const mod = await import(`../extensions/monitor.ts?t=${Date.now()}-${Math.random()}`);
		return mod.default;
	} finally {
		if (prevUrl === undefined) delete process.env.PI_STACK_MONITOR_URL;
		else process.env.PI_STACK_MONITOR_URL = prevUrl;
		if (prevEnabled === undefined) delete process.env.PI_STACK_MONITOR;
		else process.env.PI_STACK_MONITOR = prevEnabled;
	}
}

function makeStubPi() {
	const handlers = new Map();
	return {
		pi: {
			on(name, fn) {
				handlers.set(name, fn);
			},
			getAllTools() {
				return [];
			},
		},
		handlers,
	};
}

test("R2-1: session_shutdown stops further retries after a failed in-flight send", async (t) => {
	const { server, port, count } = await startServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	// Trigger one event -> enqueue -> POST -> server returns 500 -> drain()'s
	// failure branch requeues + schedules a retry (BASE_BACKOFF_MS ~500ms).
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});

	// Let the first (failing) send complete and the retry timer get armed.
	await new Promise((r) => setTimeout(r, 100));
	assert.equal(count(), 1, "exactly one send attempted before shutdown");

	// Shut down while the retry timer is pending.
	handlers.get("session_shutdown")?.();

	// Wait past what would have been the retry delay. Without the fix, the
	// armed timer fires kick()->drain() and the server sees a 2nd (and more)
	// request; with the fix, shuttingDown cancels it and nothing more is sent.
	await new Promise((r) => setTimeout(r, 800));
	assert.equal(count(), 1, "no further POSTs after session_shutdown");

	// And new events after shutdown are no-ops too (enqueue guards on shuttingDown).
	handlers.get("model_select")?.({ model: { provider: "x", id: "y" } });
	await new Promise((r) => setTimeout(r, 100));
	assert.equal(count(), 1, "events emitted after shutdown are dropped, not sent");
});

test("R2-1: aborting an in-flight send on shutdown does not requeue or reschedule", async (t) => {
	let firstRequestSeen = () => {};
	let count = 0;
	const server = http.createServer((req, res) => {
		count += 1;
		if (count === 1) {
			firstRequestSeen(); // signal the test once the request lands server-side
			// Never respond to this one; the client-side abort (session_shutdown
			// destroying the request) is what ends it, not a server response.
			return;
		}
		res.writeHead(500);
		res.end();
	});
	await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
	const port = server.address().port;
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	const seen = new Promise((resolve) => {
		firstRequestSeen = resolve;
	});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});
	await seen; // the send is now genuinely in-flight server-side

	// Shut down mid-flight: this destroys the in-flight request, which
	// rejects httpPostRaw's promise and lands in drain()'s failure branch.
	handlers.get("session_shutdown")?.();

	// Give the abort's rejection time to propagate through drain(). If R2-1's
	// guard were missing, the failure branch would requeue the item and
	// schedule a retry, which would produce a second request against the
	// server well within this window.
	await new Promise((r) => setTimeout(r, 800));
	assert.equal(count, 1, "the aborted send must not be requeued or retried");
});

// ─── R6-1: extractAssistantOutput captures the assistant's actual generated output ───

test("R6-1: Anthropic content-block shape — text plus tool_use", () => {
	const message = {
		role: "assistant",
		content: [
			{ type: "text", text: "Let me check that file." },
			{ type: "tool_use", id: "call_1", name: "read", input: { path: "foo.ts" } },
		],
	};
	const { text, toolCalls } = extractAssistantOutput(message);
	assert.equal(text, "Let me check that file.");
	assert.deepEqual(toolCalls, ["read"]);
});

test("R6-1: pi's own normalized AgentMessage shape — text plus toolCall blocks", () => {
	// docs/session-format.md AssistantMessage.content: (TextContent |
	// ThinkingContent | ToolCall)[] — a tool call block is {type:"toolCall"}.
	// Thinking blocks must be excluded from the extracted text.
	const message = {
		role: "assistant",
		content: [
			{ type: "thinking", thinking: "private reasoning, should not appear" },
			{ type: "text", text: "On it." },
			{ type: "toolCall", id: "call_1", name: "bash", arguments: { command: "ls" } },
		],
	};
	const { text, toolCalls } = extractAssistantOutput(message);
	assert.equal(text, "On it.");
	assert.deepEqual(toolCalls, ["bash"]);
});

test("R6-1: OpenAI shape — text content plus top-level tool_calls[].function.name", () => {
	const message = {
		role: "assistant",
		content: [{ type: "text", text: "Searching now." }],
		tool_calls: [{ id: "call_1", type: "function", function: { name: "web_search", arguments: "{}" } }],
	};
	const { text, toolCalls } = extractAssistantOutput(message);
	assert.equal(text, "Searching now.");
	assert.deepEqual(toolCalls, ["web_search"]);
});

test("R6-1: OpenAI Responses-API function_call item shape", () => {
	const message = {
		role: "assistant",
		content: [
			{ type: "text", text: "One sec." },
			{ type: "function_call", name: "get_weather", arguments: "{}" },
		],
	};
	const { text, toolCalls } = extractAssistantOutput(message);
	assert.equal(text, "One sec.");
	assert.deepEqual(toolCalls, ["get_weather"]);
});

test("R6-1: Gemini parts[] shape — untagged text part plus functionCall part", () => {
	// Gemini's raw parts have no "type" tag at all: {text} or {functionCall:{name}}.
	const message = {
		role: "assistant",
		parts: [{ text: "Looking that up." }, { functionCall: { name: "lookup", args: { q: "pi-stack" } } }],
	};
	const { text, toolCalls } = extractAssistantOutput(message);
	assert.equal(text, "Looking that up.");
	assert.deepEqual(toolCalls, ["lookup"]);
});

test("R6-1: empty/no-content message yields empty text and no tool calls", () => {
	assert.deepEqual(extractAssistantOutput({ role: "assistant" }), { text: "", toolCalls: [] });
	assert.deepEqual(extractAssistantOutput({ role: "assistant", content: [] }), { text: "", toolCalls: [] });
	assert.deepEqual(extractAssistantOutput(null), { text: "", toolCalls: [] });
	assert.deepEqual(extractAssistantOutput(undefined), { text: "", toolCalls: [] });
});

test("R6-1: multiple text blocks concatenate, multiple tool calls all collected", () => {
	const message = {
		role: "assistant",
		content: [
			{ type: "text", text: "First." },
			{ type: "tool_use", name: "read" },
			{ type: "text", text: "Second." },
			{ type: "tool_use", name: "bash" },
		],
	};
	const { text, toolCalls } = extractAssistantOutput(message);
	assert.equal(text, "First.\nSecond.");
	assert.deepEqual(toolCalls, ["read", "bash"]);
});
