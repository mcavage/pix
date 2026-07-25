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
//
// Timing: nothing here sleeps past a real backoff. The retry/backoff timing
// goes through monitor.ts's injectable RetryClock (`setRetryClock`), so the
// shutdown tests install a manually-pumped clock and assert on the SCHEDULE
// (armed? disarmed? what delay?) directly, and everything else waits on an
// observable condition (`waitFor`) instead of a fixed sleep. Production
// defaults are unchanged and asserted below.
import assert from "node:assert";
import * as http from "node:http";
import { test } from "node:test";

const monitor = await import("../extensions/monitor.ts");
const {
	PRODUCTION_RETRY_CLOCK,
	setRetryClock,
	classifyToolSource,
	isMcpSourceInfo,
	mcpServerFromSourceInfo,
	partitionToolNames,
	summarizeRequest,
	extractAssistantOutput,
	isToolResultMessage,
	extractSessionId,
	commandInvokesPi,
	truncatePreview,
} = monitor;

// ─── Timing helpers ────────────────────────────────────────────────────────

/** Poll until `predicate()` holds. Throws (naming what it waited for) on timeout. */
async function waitFor(predicate, what, timeoutMs = 2000) {
	const deadline = Date.now() + timeoutMs;
	for (;;) {
		if (predicate()) return;
		if (Date.now() >= deadline) throw new Error(`timed out after ${timeoutMs}ms waiting for ${what}`);
		await new Promise((r) => setTimeout(r, 1));
	}
}

/**
 * Give a (buggy) extra POST a chance to actually reach the server before
 * asserting an ABSENCE. Absence can't be polled for, but it also doesn't need
 * a backoff-length wait: a loopback round trip is ~1-2ms, so a handful of
 * event-loop turns is an order of magnitude more headroom than required.
 */
function settle(ms = 10) {
	return new Promise((r) => setTimeout(r, ms));
}

/** Drop lingering keep-alive sockets so the run doesn't wait out server.keepAliveTimeout (5s) at exit. */
function closeServer(server) {
	server.closeAllConnections?.();
	server.close();
}

/**
 * A RetryClock whose timers only ever fire when the test says so, and whose
 * `now()` only advances with them — the retry timer, its backoff delay and the
 * `disabledUntil` window all become assertable values instead of wall time.
 */
function makeManualClock(overrides = {}) {
	let nowMs = 1_000_000;
	const timers = [];
	const clock = {
		now: () => nowMs,
		setTimer(fn, delayMs) {
			const handle = { fn, delayMs, state: "armed" };
			timers.push(handle);
			return handle;
		},
		clearTimer(handle) {
			if (handle && handle.state === "armed") handle.state = "disarmed";
		},
		// The quit-flush poll only needs to yield to the event loop, not to wall time.
		sleep: () => new Promise((r) => setImmediate(r)),
		...overrides,
	};
	return {
		clock,
		timers,
		armed: () => timers.filter((h) => h.state === "armed"),
		/**
		 * Fire pending timers exactly as the real clock would: advance `now` past
		 * the delay first (so drain()'s `disabledUntil` window has genuinely
		 * elapsed), then invoke the callback. `includeDisarmed` fires even a
		 * cleared handle — the "the timer already escaped clearTimeout" race that
		 * R2-1's shuttingDown guard, not the clear, has to survive.
		 */
		fireAll({ includeDisarmed = false } = {}) {
			for (const h of [...timers]) {
				if (h.state === "fired") continue;
				if (h.state !== "armed" && !includeDisarmed) continue;
				h.state = "fired";
				nowMs += h.delayMs;
				h.fn();
			}
		},
	};
}

/**
 * Real timers, tiny delays. Used where the code under test genuinely has to
 * poll/await real I/O (the quit-flush drain): the code path is identical to
 * production, only the durations shrink.
 */
const FAST_CLOCK = { baseBackoffMs: 5, maxBackoffMs: 20, quitFlushTimeoutMs: 250, quitFlushPollMs: 1 };

test("retry clock seam: production defaults are the shipped constants and setRetryClock() restores them", () => {
	assert.deepEqual(
		{
			postTimeoutMs: PRODUCTION_RETRY_CLOCK.postTimeoutMs,
			baseBackoffMs: PRODUCTION_RETRY_CLOCK.baseBackoffMs,
			maxBackoffMs: PRODUCTION_RETRY_CLOCK.maxBackoffMs,
			quitFlushTimeoutMs: PRODUCTION_RETRY_CLOCK.quitFlushTimeoutMs,
			quitFlushPollMs: PRODUCTION_RETRY_CLOCK.quitFlushPollMs,
		},
		{ postTimeoutMs: 2000, baseBackoffMs: 500, maxBackoffMs: 30_000, quitFlushTimeoutMs: 1500, quitFlushPollMs: 20 },
		"the seam must not change any shipped timing default",
	);

	// A partial override leaves every unnamed field at its production value...
	const overridden = setRetryClock({ baseBackoffMs: 1 });
	assert.equal(overridden.baseBackoffMs, 1);
	assert.equal(overridden.maxBackoffMs, PRODUCTION_RETRY_CLOCK.maxBackoffMs);
	assert.equal(overridden.setTimer, PRODUCTION_RETRY_CLOCK.setTimer);
	// ...and resetting restores the production clock itself, not a copy.
	assert.equal(setRetryClock(), PRODUCTION_RETRY_CLOCK);

	// The production timer pair round-trips (arm then disarm without throwing).
	const handle = PRODUCTION_RETRY_CLOCK.setTimer(() => {
		throw new Error("production retry timer must have been cleared");
	}, 5_000);
	PRODUCTION_RETRY_CLOCK.clearTimer(handle);
});

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

// ─── isToolResultMessage: cross-provider tool-result detection ─────────────

test("isToolResultMessage: Anthropic content array with a tool_result block is true", () => {
	assert.equal(
		isToolResultMessage({ role: "user", content: [{ type: "tool_result", tool_use_id: "call_1", content: "42" }] }),
		true,
	);
});

test("isToolResultMessage: OpenAI role:tool is true", () => {
	assert.equal(isToolResultMessage({ role: "tool", tool_call_id: "call_1", content: "42" }), true);
});

test("isToolResultMessage: OpenAI a bare tool_call_id (no role:tool) is still true", () => {
	assert.equal(isToolResultMessage({ role: "user", tool_call_id: "call_1", content: "42" }), true);
});

test("isToolResultMessage: Gemini a part with functionResponse is true", () => {
	assert.equal(
		isToolResultMessage({ role: "user", parts: [{ functionResponse: { name: "lookup", response: { result: 42 } } }] }),
		true,
	);
});

test("isToolResultMessage: a plain user text message is false", () => {
	assert.equal(isToolResultMessage({ role: "user", content: "what's the weather" }), false);
	assert.equal(isToolResultMessage({ role: "user", content: [{ type: "text", text: "hi" }] }), false);
});

test("isToolResultMessage: null/undefined/non-object is false", () => {
	assert.equal(isToolResultMessage(null), false);
	assert.equal(isToolResultMessage(undefined), false);
	assert.equal(isToolResultMessage("just a string"), false);
});

// ─── hook ordering: provider_request must emit BEFORE its own provider_response ───
// This is the regression under test: each turn's request must land before
// that same turn's response, and before the NEXT turn's request.
// End-to-end against a stub `pi` + a local HTTP server that RECORDS every
// posted NDJSON line, since this ordering is intentionally internal to the
// default export's closure, same as the R2-1 queue/timer state below.

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

test("two turns emit provider_request/provider_response in the same order, per turn", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => closeServer(server));

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});

	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "first" }] } }, {});
	handlers.get("message_end")?.({ message: assistantMessage("reply one") }, {});

	handlers.get("before_provider_request")?.(
		{ payload: { messages: [{ role: "user", content: "first" }, { role: "user", content: "second" }] } },
		{},
	);
	handlers.get("message_end")?.({ message: assistantMessage("reply two") }, {});

	await waitFor(
		() => lines.filter((l) => l.kind === "provider_request" || l.kind === "provider_response").length === 4,
		"both turns' request+response to be posted",
	);

	const kinds = lines.filter((l) => l.kind === "provider_request" || l.kind === "provider_response").map((l) => l.kind);
	assert.deepEqual(
		kinds,
		["provider_request", "provider_response", "provider_request", "provider_response"],
		"each turn's request must precede its own response, and precede the NEXT turn's request",
	);

	const requests = lines.filter((l) => l.kind === "provider_request");
	assert.equal(requests.length, 2);
});

test("a fresh user prompt: turn_start and provider_request both carry trigger=user", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => closeServer(server));

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});
	handlers.get("message_end")?.({ message: assistantMessage("reply") }, {});

	await waitFor(
		() => lines.some((l) => l.kind === "turn_start") && lines.some((l) => l.kind === "provider_request"),
		"turn_start and provider_request to be posted",
	);

	const ts = lines.find((l) => l.kind === "turn_start");
	const pr = lines.find((l) => l.kind === "provider_request");
	assert.ok(ts && pr, "both turn_start and provider_request must be emitted");
	assert.equal(ts.trigger, "user");
	assert.equal(pr.trigger, "user");
});

test("a provider_request whose newest new message is a tool result carries trigger=tool_result on both events", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => closeServer(server));

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	// First turn: a plain user prompt (establishes prevMessageCount).
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "read foo.ts" }] } }, {});
	handlers.get("message_end")?.({ message: assistantMessage("reading now") }, {});

	// Second turn: the newest new message is an OpenAI-shaped tool result fed
	// back to the model — no user input in between, so prevEventKind alone
	// (message_end, not tool_end) would NOT have inferred tool_result.
	handlers.get("before_provider_request")?.(
		{
			payload: {
				messages: [
					{ role: "user", content: "read foo.ts" },
					{ role: "tool", tool_call_id: "call_1", content: "file contents..." },
				],
			},
		},
		{},
	);
	handlers.get("message_end")?.({ message: assistantMessage("done") }, {});

	await waitFor(
		() =>
			lines.filter((l) => l.kind === "turn_start").length === 2 &&
			lines.filter((l) => l.kind === "provider_request").length === 2,
		"both turns' turn_start+provider_request to be posted",
	);

	const turnStarts = lines.filter((l) => l.kind === "turn_start");
	const requests = lines.filter((l) => l.kind === "provider_request");
	assert.equal(turnStarts.length, 2);
	assert.equal(requests.length, 2);
	assert.equal(turnStarts[1].trigger, "tool_result");
	assert.equal(requests[1].trigger, "tool_result");
});

// ─── BUG 1: commandInvokesPi must see the FULL command, not the 200-char
// truncated argsSummary ─────────────────────────────────────────────────────
// The TUI correlates a spawned child pi session to the bash tool that
// launched it partly by checking the tool actually invokes `pi`/`pi-stack`.
// A long multi-line for-loop command (several quoted model specs, then a
// `pi --print ...` invocation) can push that invocation well past char 200,
// past where truncatePreview(argsText, 200) cuts argsSummary off — so
// invokesPi must be computed from the full, untruncated text.

function longForLoopCommand() {
	const specs = Array.from({ length: 12 }, (_, i) => `"model-spec-${i}-${"a".repeat(12)}"`).join(" ");
	return `cd /workspace/repo; for spec in ${specs} "ollama/qwen3.5:9b:some-long-tag"; do\n  m="$spec"\n  pi --print --model "$m" --thing extra-arg\ndone`;
}

test("commandInvokesPi: true for a long for-loop command whose `pi --print` lands past char 200", () => {
	const cmd = longForLoopCommand();
	// Sanity-check the fixture actually reproduces the bug precondition: the
	// `pi --print` invocation must land AFTER truncatePreview's 200-char cut.
	assert.ok(cmd.indexOf("pi --print") > 200, "fixture command must put `pi --print` past char 200");
	assert.equal(commandInvokesPi(cmd), true);
	// And the truncated 200-char preview (what argsSummary carries) does NOT
	// contain the invocation — confirming the OLD approach (deriving invokesPi
	// from argsSummary) would have missed it.
	const truncated = truncatePreview(cmd, 200);
	assert.equal(commandInvokesPi(truncated), false, "the truncated preview alone must NOT detect the pi invocation");
});

test("commandInvokesPi: false for curl/grep commands that merely contain \"pi\"-like substrings", () => {
	assert.equal(commandInvokesPi('curl -s https://example.com/api | jq .'), false);
	assert.equal(commandInvokesPi("grep pip requirements.txt"), false);
	assert.equal(commandInvokesPi("pip install requests"), false);
});

test("commandInvokesPi: true for a bare `pi` or `pi-stack` command token in various positions", () => {
	assert.equal(commandInvokesPi('pi --print --model "x"'), true);
	assert.equal(commandInvokesPi("cd /tmp && pi-stack run"), true);
	assert.equal(commandInvokesPi('echo hi; pi "do a thing"'), true);
	assert.equal(commandInvokesPi(""), false);
});

test("commandInvokesPi: a literal newline separator counts (\\s covers newlines)", () => {
	// A real newline directly preceding "pi" (no other whitespace in between) —
	// multi-line bash via a heredoc/for-loop is exactly this shape.
	assert.equal(commandInvokesPi("echo start\npi --print --model x"), true);
	assert.equal(commandInvokesPi("echo start\npip install x"), false);
});

test("tool_start event carries invokesPi computed from the FULL command", async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => closeServer(server));

	const factory = await loadMonitorAgainst(port);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});

	const longCmd = longForLoopCommand();
	handlers.get("tool_execution_start")?.({ toolCallId: "t-long", toolName: "bash", args: { command: longCmd } });
	handlers.get("tool_execution_start")?.({ toolCallId: "t-curl", toolName: "bash", args: { command: "curl -s https://example.com | grep pip" } });

	await waitFor(() => lines.filter((l) => l.kind === "tool_start").length === 2, "both tool_start events to be posted");

	const starts = lines.filter((l) => l.kind === "tool_start");
	const longStart = starts.find((l) => l.toolId === "t-long");
	const curlStart = starts.find((l) => l.toolId === "t-curl");
	assert.ok(longStart, "tool_start for the long command must be emitted");
	assert.ok(curlStart, "tool_start for the curl command must be emitted");
	assert.equal(longStart.invokesPi, true, "long for-loop command past char 200 must set invokesPi=true");
	assert.equal(curlStart.invokesPi, false, "curl/grep-pip command must set invokesPi=false");
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

async function loadMonitorAgainst(port, clockOverrides) {
	const prevUrl = process.env.PI_STACK_MONITOR_URL;
	const prevEnabled = process.env.PI_STACK_MONITOR;
	process.env.PI_STACK_MONITOR_URL = `http://127.0.0.1:${port}`;
	process.env.PI_STACK_MONITOR = "1";
	try {
		// Bust the module cache with a unique query string so each test gets a
		// fresh closure (module-level seqCounter aside, which is harmless here).
		const mod = await import(`../extensions/monitor.ts?t=${Date.now()}-${Math.random()}`);
		// Fresh module instance per test, so the clock swap is scoped to this
		// instance and needs no teardown. It must happen BEFORE the factory runs:
		// the clock is captured once, at instantiation.
		if (clockOverrides) mod.setRetryClock(clockOverrides);
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
	t.after(() => closeServer(server));

	// Manual clock: the retry schedule is asserted directly, so this test never
	// waits out a real 500ms backoff (and the assertions get sharper for it).
	const manual = makeManualClock();
	const factory = await loadMonitorAgainst(port, manual.clock);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	// Trigger one event -> enqueue -> POST -> server returns 500 -> drain()'s
	// failure branch requeues + schedules a retry (baseBackoffMs).
	handlers.get("before_provider_request")?.({ payload: { messages: [{ role: "user", content: "hi" }] } }, {});

	// The failed send is observable two ways, neither of them a wall-clock
	// guess: the server counted it, and a retry got armed on our clock.
	await waitFor(() => count() === 1 && manual.armed().length === 1, "the first send to fail and arm a retry");
	assert.equal(manual.armed()[0].delayMs, PRODUCTION_RETRY_CLOCK.baseBackoffMs, "first retry waits the base backoff");

	// Shut down while the retry timer is pending.
	handlers.get("session_shutdown")?.();
	assert.equal(manual.armed().length, 0, "session_shutdown must disarm the pending retry timer");

	// Fire it anyway, advancing the clock past disabledUntil exactly as a real
	// timer that escaped clearTimeout would. Without the fix, this kick()s
	// drain() and the server sees a 2nd (and more) request; with the fix,
	// shuttingDown short-circuits it and nothing more is sent.
	manual.fireAll({ includeDisarmed: true });
	await settle();
	assert.equal(count(), 1, "no further POSTs after session_shutdown");

	// And new events after shutdown are no-ops too (enqueue guards on shuttingDown).
	handlers.get("model_select")?.({ model: { provider: "x", id: "y" } });
	await settle();
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
	t.after(() => closeServer(server));

	const manual = makeManualClock();
	const factory = await loadMonitorAgainst(port, manual.clock);
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

	// Let the abort's rejection propagate through drain(). If R2-1's guard were
	// missing, the failure branch would requeue the item and schedule a retry —
	// now directly observable on the clock instead of inferred from a wall-clock
	// wait longer than the backoff.
	await settle();
	assert.equal(manual.timers.length, 0, "the aborted send must not schedule a retry at all");
	manual.fireAll({ includeDisarmed: true }); // no-op unless something latent was armed
	await settle();
	assert.equal(count, 1, "the aborted send must not be requeued or retried");
});

// ─── BUG 2: session_shutdown(reason:"quit") must FLUSH, not drop, the final ───
// provider_response ────────────────────────────────────────────────────────────────────────────────
// A `pi --print` child does exactly one turn and exits: its just-queued
// final provider_response must not be discarded the way the pre-existing
// R2-1 abort-and-drop behavior discards it for reload/new/resume/fork.

test('BUG 2: session_shutdown reason="quit" flushes a queued provider_response before teardown', async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => closeServer(server));

	// Real timers (drainAll genuinely polls real I/O here), just short ones.
	const factory = await loadMonitorAgainst(port, FAST_CLOCK);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	// The provider_response is enqueued right here, then session_shutdown
	// fires with reason "quit" before the drain() this enqueue kicked off has
	// had a chance to actually finish sending it — exactly the `pi --print`
	// one-turn-and-exit race.
	handlers.get("message_end")?.({ message: assistantMessage("final reply") }, {});
	await handlers.get("session_shutdown")?.({ reason: "quit" });

	const responses = lines.filter((l) => l.kind === "provider_response");
	assert.equal(responses.length, 1, "the queued provider_response must be FLUSHED, not dropped, on reason=quit");
	assert.equal(responses[0].textPreview, "final reply");
});

test('BUG 2: session_shutdown reason="reload" still drops a queued provider_response (unchanged R2-1 behavior)', async (t) => {
	const { server, port, lines } = await startRecordingServer();
	t.after(() => closeServer(server));

	const factory = await loadMonitorAgainst(port, FAST_CLOCK);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("message_end")?.({ message: assistantMessage("final reply") }, {});
	await handlers.get("session_shutdown")?.({ reason: "reload" });

	// Give any (incorrectly) in-flight send time to land, if the fix ever regressed.
	await settle();

	const responses = lines.filter((l) => l.kind === "provider_response");
	assert.equal(responses.length, 0, "reason=reload must keep dropping the queue, never posting it");
});

test("BUG 2: session_shutdown with no reason (undefined) keeps the strict R2-1 drop behavior", async (t) => {
	// Existing callers (e.g. a bare `session_shutdown` event with no reason
	// field at all) must not accidentally fall into the new flush path.
	const { server, port, lines } = await startRecordingServer();
	t.after(() => closeServer(server));

	const factory = await loadMonitorAgainst(port, FAST_CLOCK);
	const { pi, handlers } = makeStubPi();
	factory(pi);

	handlers.get("session_start")?.({}, {});
	handlers.get("message_end")?.({ message: assistantMessage("final reply") }, {});
	await handlers.get("session_shutdown")?.();

	await settle();
	const responses = lines.filter((l) => l.kind === "provider_response");
	assert.equal(responses.length, 0, "an undefined/missing reason must not be treated as quit");
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

// ─── FIX 2: extractSessionId returns a bare session id, not a full file path ───
// The raw session FILE path made the TUI's short label an ugly `session
// f4.jsonl` instead of a real session id. extractSessionId now strips the
// directory and the `.jsonl` extension.

test("extractSessionId: strips directory and .jsonl extension from the session file path", () => {
	const ctx = {
		sessionManager: {
			getSessionFile: () => "/home/agent/.pi/sessions/2026-07-21T14-28-47-879Z_019f8514-abcd.jsonl",
		},
	};
	assert.equal(extractSessionId(ctx), "2026-07-21T14-28-47-879Z_019f8514-abcd");
});

test("extractSessionId: handles a Windows-style backslash path too", () => {
	const ctx = { sessionManager: { getSessionFile: () => "C:\\Users\\agent\\.pi\\sessions\\f4.jsonl" } };
	assert.equal(extractSessionId(ctx), "f4");
});

test("extractSessionId: same session (same file path) resolves to the same id every call", () => {
	const ctx = { sessionManager: { getSessionFile: () => "/x/sessions/stable-id.jsonl" } };
	assert.equal(extractSessionId(ctx), extractSessionId(ctx));
	assert.equal(extractSessionId(ctx), "stable-id");
});

test("extractSessionId: falls back to a direct id-shaped field when no session file", () => {
	assert.equal(extractSessionId({ sessionId: "abc-123" }), "abc-123");
	assert.equal(extractSessionId({ session: { id: "def-456" } }), "def-456");
});

test("extractSessionId: mints a random uuid-shaped id for an ephemeral session with no file at all", () => {
	const id = extractSessionId({});
	assert.ok(typeof id === "string" && id.length > 0);
	// A random uuid contains no directory separators or .jsonl residue.
	assert.equal(id.includes("/"), false);
	assert.equal(id.endsWith(".jsonl"), false);
});
