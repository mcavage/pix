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
	requestUrl,
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

// ─── requestUrl: reconstruct the HTTP request line from ctx.model ──────────

test("requestUrl: anthropic-messages appends /v1/messages", () => {
	assert.equal(
		requestUrl({ baseUrl: "https://api.anthropic.com", api: "anthropic-messages", id: "claude-opus-4-8" }),
		"https://api.anthropic.com/v1/messages",
	);
});

test("requestUrl: anthropic-messages with an existing /v1 baseUrl does not double it", () => {
	assert.equal(
		requestUrl({ baseUrl: "https://gateway.example.com/v1", api: "anthropic-messages" }),
		"https://gateway.example.com/v1/messages",
	);
});

test("requestUrl: openai-responses appends /v1/responses", () => {
	assert.equal(requestUrl({ baseUrl: "https://api.openai.com", api: "openai-responses" }), "https://api.openai.com/v1/responses");
});

test("requestUrl: openai-responses respects an existing trailing /v1", () => {
	assert.equal(requestUrl({ baseUrl: "https://api.openai.com/v1", api: "openai-responses" }), "https://api.openai.com/v1/responses");
});

test("requestUrl: openai-completions appends /v1/chat/completions", () => {
	assert.equal(
		requestUrl({ baseUrl: "https://api.openai.com", api: "openai-completions" }),
		"https://api.openai.com/v1/chat/completions",
	);
});

test("requestUrl: openai-completions respects an existing trailing /v1", () => {
	assert.equal(
		requestUrl({ baseUrl: "https://api.openai.com/v1", api: "openai-completions" }),
		"https://api.openai.com/v1/chat/completions",
	);
});

test("requestUrl: a google/gemini api builds the streamGenerateContent path with the model id", () => {
	assert.equal(
		requestUrl({ baseUrl: "https://generativelanguage.googleapis.com", api: "google-generative-ai", id: "gemini-2.5-pro" }),
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:streamGenerateContent",
	);
	assert.equal(
		requestUrl({ baseUrl: "https://x.example.com", api: "gemini-messages", id: "gemini-flash" }),
		"https://x.example.com/v1beta/models/gemini-flash:streamGenerateContent",
	);
});

test("requestUrl: google/gemini api does not double an existing /v1beta baseUrl", () => {
	assert.equal(
		requestUrl({ baseUrl: "https://x.example.com/v1beta", api: "generative-language", id: "gemini-flash" }),
		"https://x.example.com/v1beta/models/gemini-flash:streamGenerateContent",
	);
});

test("requestUrl: strips a trailing slash on baseUrl before joining, never a double slash", () => {
	assert.equal(
		requestUrl({ baseUrl: "https://api.anthropic.com/", api: "anthropic-messages" }),
		"https://api.anthropic.com/v1/messages",
	);
});

test("requestUrl: unknown api falls back to baseUrl as-is", () => {
	assert.equal(requestUrl({ baseUrl: "https://custom.example.com/proxy", api: "some-unknown-api" }), "https://custom.example.com/proxy");
	assert.equal(requestUrl({ baseUrl: "https://custom.example.com/proxy" }), "https://custom.example.com/proxy");
});

test("requestUrl: missing baseUrl returns empty string, never throws", () => {
	assert.equal(requestUrl({ api: "anthropic-messages" }), "");
	assert.equal(requestUrl({}), "");
	assert.equal(requestUrl(null), "");
	assert.equal(requestUrl(undefined), "");
});

// ─── hook ordering: provider_request must emit BEFORE its own provider_response ───
// This is the regression under test: pi's documented hook order (docs/extensions.md
// hook-order diagram) is
//   turn_start -> before_provider_headers -> before_provider_request -> after_provider_response -> message_end
// so before_provider_headers fires strictly BEFORE before_provider_request. The
// fix stashes headers at before_provider_headers and EMITS the provider_request
// immediately at before_provider_request (attaching those stashed headers), so a
// turn's request always lands before that same turn's response, and before the
// NEXT turn's request. End-to-end against a stub `pi` + a local HTTP server that
// RECORDS every posted NDJSON line, since the header stash is intentionally
// private to the default export's closure, same as the R2-1 queue/timer state
// below.

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

function assistantMessage(text) {
	return { role: "assistant", content: [{ type: "text", text }], stopReason: "end_turn", usage: {} };
}

test("two turns fired in pi's documented hook order emit provider_request/provider_response in the same order, per turn, with headers on the right turn", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});

	// Turn 1: before_provider_headers -> before_provider_request -> message_end.
	handlers.get("before_provider_headers")?.({
		headers: { authorization: "proxy-managed", "x-request-id": "turn-1" },
	});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "first" }] } }, {});
	handlers.get("message_end")?.({ message: assistantMessage("reply one") }, {});

	// Turn 2: same shape.
	handlers.get("before_provider_headers")?.({
		headers: { authorization: "proxy-managed", "x-request-id": "turn-2" },
	});
	handlers.get("before_provider_request")?.(
		{ payload: { messages: [{ role: "user", content: "first" }, { role: "user", content: "second" }] } },
		{},
	);
	handlers.get("message_end")?.({ message: assistantMessage("reply two") }, {});

	await new Promise((r) => setTimeout(r, 100));

	const kinds = lines.filter((l) => l.kind === "provider_request" || l.kind === "provider_response").map((l) => l.kind);
	assert.deepEqual(
		kinds,
		["provider_request", "provider_response", "provider_request", "provider_response"],
		"each turn's request must precede its own response, and precede the NEXT turn's request",
	);

	const requests = lines.filter((l) => l.kind === "provider_request");
	assert.equal(requests.length, 2);
	assert.equal(requests[0].headers.authorization, "<redacted>");
	assert.equal(requests[0].headers["x-request-id"], "turn-1", "turn 1's request carries the headers stashed at ITS before_provider_headers");
	assert.equal(requests[1].headers["x-request-id"], "turn-2", "turn 2's request carries the headers stashed at ITS before_provider_headers, not turn 1's");
});

test("a missing before_provider_headers still emits the request in order, just without headers", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	// before_provider_headers never fires (missing hook on this pi version).
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});
	handlers.get("message_end")?.({ message: assistantMessage("reply") }, {});
	await new Promise((r) => setTimeout(r, 100));

	const kinds = lines.filter((l) => l.kind === "provider_request" || l.kind === "provider_response").map((l) => l.kind);
	assert.deepEqual(kinds, ["provider_request", "provider_response"]);

	const pr = lines.find((l) => l.kind === "provider_request");
	assert.ok(pr, "provider_request must still be emitted with no headers hook");
	assert.equal("headers" in pr, false, "no headers hook fired, so no headers field");
});

// ─── request_headers merge event: real-transport order (request-first) ────
// Root cause under test: in the REAL transport, before_provider_headers fires
// INSIDE transformHeaders, which runs AFTER before_provider_request (onPayload)
// — the opposite of the hook-order-diagram's documented order this file used
// to assume. So provider_request emits first with no headers, and the real
// assembled headers show up one beat later on before_provider_headers. The fix
// is a small merge event, request_headers, carrying the same turnId.

test("request-first order: before_provider_request then before_provider_headers emits provider_request followed by request_headers carrying the redacted headers with the SAME turnId", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "first" }] } }, {});
	handlers.get("before_provider_headers")?.({
		headers: { authorization: "proxy-managed", "x-request-id": "real-1" },
	});
	handlers.get("message_end")?.({ message: assistantMessage("reply") }, {});

	await new Promise((r) => setTimeout(r, 100));

	const kinds = lines.filter((l) => l.kind === "provider_request" || l.kind === "request_headers" || l.kind === "provider_response").map((l) => l.kind);
	assert.deepEqual(kinds, ["provider_request", "request_headers", "provider_response"], "provider_request emits with no headers, then a request_headers merge event follows");

	const pr = lines.find((l) => l.kind === "provider_request");
	assert.equal("headers" in pr, false, "provider_request itself carries no headers in this order");

	const rh = lines.find((l) => l.kind === "request_headers");
	assert.ok(rh, "a request_headers merge event must be emitted");
	assert.equal(rh.turnId, pr.turnId, "request_headers must carry the SAME turnId as its provider_request");
	assert.equal(rh.headers["x-request-id"], "real-1");
	assert.equal(rh.headers.authorization, "<redacted>", "redaction (R6-2) still applies on the merge event too");
});

test("request-first order across two turns: each request_headers merge event carries its own turn's turnId", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});

	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "first" }] } }, {});
	handlers.get("before_provider_headers")?.({ headers: { "x-request-id": "turn-1" } });
	handlers.get("message_end")?.({ message: assistantMessage("reply one") }, {});

	handlers.get("before_provider_request")?.(
		{ payload: { messages: [{ role: "user", content: "first" }, { role: "user", content: "second" }] } },
		{},
	);
	handlers.get("before_provider_headers")?.({ headers: { "x-request-id": "turn-2" } });
	handlers.get("message_end")?.({ message: assistantMessage("reply two") }, {});

	await new Promise((r) => setTimeout(r, 100));

	const requests = lines.filter((l) => l.kind === "provider_request");
	const headerEvents = lines.filter((l) => l.kind === "request_headers");
	assert.equal(requests.length, 2);
	assert.equal(headerEvents.length, 2);
	assert.equal(headerEvents[0].turnId, requests[0].turnId);
	assert.equal(headerEvents[0].headers["x-request-id"], "turn-1");
	assert.equal(headerEvents[1].turnId, requests[1].turnId);
	assert.equal(headerEvents[1].headers["x-request-id"], "turn-2");
	assert.notEqual(requests[0].turnId, requests[1].turnId, "the two turns must have distinct turnIds");
});

test("headers-first order still attaches inline on provider_request and emits NO separate request_headers event", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("before_provider_headers")?.({
		headers: { authorization: "proxy-managed", "x-request-id": "inline-1" },
	});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});
	handlers.get("message_end")?.({ message: assistantMessage("reply") }, {});

	await new Promise((r) => setTimeout(r, 100));

	const pr = lines.find((l) => l.kind === "provider_request");
	assert.ok(pr, "provider_request must be emitted");
	assert.equal(pr.headers["x-request-id"], "inline-1", "headers attach inline on provider_request itself");
	assert.equal(pr.headers.authorization, "<redacted>", "redaction (R6-2) still applies");

	const rh = lines.find((l) => l.kind === "request_headers");
	assert.equal(rh, undefined, "headers-first order must not also emit a separate request_headers merge event");
});

test("provider_request carries method=POST and the requestUrl derived from ctx.model", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => server.close());

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("before_provider_request")?.(
		{ payload: { messages: [{ role: "user", content: "hi" }] } },
		{ model: { baseUrl: "https://api.anthropic.com", api: "anthropic-messages", id: "claude-opus-4-8", provider: "anthropic" } },
	);
	handlers.get("message_end")?.({ message: assistantMessage("reply") }, {});

	await new Promise((r) => setTimeout(r, 100));

	const pr2 = lines.find((l) => l.kind === "provider_request");
	assert.ok(pr2, "provider_request must be emitted");
	assert.equal(pr2.method, "POST");
	assert.equal(pr2.url, "https://api.anthropic.com/v1/messages");
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
