// Unit tests for extensions/memory-recall.ts (the /recall command UX fix).
// Run: node --test tests/
//
// MEMORY_URL/TIMEOUT_MS are read once at module load, so each scenario that
// needs a live (fake) memory service sets the env first, then dynamic-imports
// a fresh module instance via a cache-busting query string.
import assert from "node:assert";
import * as http from "node:http";
import { test } from "node:test";

// A minimal fake memory daemon: records every JSON-RPC request it receives and
// replies with a canned result per method. Lets tests assert on query/timeout
// behavior without touching the real host service.
function makeFakeDaemon(responder) {
	const requests = [];
	const server = http.createServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			const parsed = JSON.parse(body);
			requests.push(parsed);
			const result = responder(parsed);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(JSON.stringify({ jsonrpc: "2.0", id: parsed.id, result }));
		});
	});
	return { server, requests };
}

async function listen(server) {
	await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
	const { port } = server.address();
	return `http://127.0.0.1:${port}`;
}

let seq = 0;
// Import a FRESH memory-recall.ts instance with MEMORY_URL/timeouts pinned to
// this test's fake server, restoring the env afterward.
async function loadWithEnv(env) {
	const ENV_KEYS = ["MEMORY_URL", "MEMORY_TIMEOUT_MS", "MEMORY_COMMAND_TIMEOUT_MS"];
	const saved = {};
	for (const k of ENV_KEYS) saved[k] = process.env[k];
	for (const k of ENV_KEYS) delete process.env[k];
	for (const [k, v] of Object.entries(env)) process.env[k] = v;
	try {
		const url = new URL(`../extensions/memory-recall.ts?case=${seq++}`, import.meta.url);
		return await import(url.href);
	} finally {
		for (const k of ENV_KEYS) {
			if (saved[k] === undefined) delete process.env[k];
			else process.env[k] = saved[k];
		}
	}
}

function fakeCtx(notes) {
	return { ui: { notify: (msg, level) => notes.push({ msg, level }) } };
}

function getRecallHandler(mod, pi) {
	let handler = null;
	mod.default({
		on() {},
		registerCommand(name, cfg) {
			if (name === "recall") handler = cfg.handler;
		},
		...pi,
	});
	assert.ok(handler, "/recall command registered");
	return handler;
}

// ── (1) blank args query "*" (show everything), matching the host CLI ───────
test("blank /recall queries '*' instead of an empty string", async (t) => {
	const { server, requests } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const handler = getRecallHandler(mod);

	const notes = [];
	await handler(undefined, fakeCtx(notes));
	await handler("", fakeCtx(notes));
	await handler("   ", fakeCtx(notes));

	assert.equal(requests.length, 3);
	for (const req of requests) {
		assert.equal(req.method, "recall");
		assert.equal(req.params.query, "*");
	}
});

test("an explicit /recall query is passed through unchanged", async (t) => {
	const { server, requests } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const handler = getRecallHandler(mod);

	await handler("  docker sandboxes  ", fakeCtx([]));
	assert.equal(requests[0].params.query, "docker sandboxes");
});

// Rendered hits preserve the RFC3339 offset supplied by the host daemon. The
// sandbox timezone may differ, so converting with Date#getTimezoneOffset is wrong.
const pureMod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });

test("formatMemoryIso preserves the encoded offset and removes fractions", () => {
	assert.equal(pureMod.formatMemoryIso("2026-07-22T10:59:34-07:00"), "2026-07-22T10:59:34-07:00");
	assert.equal(pureMod.formatMemoryIso("2026-07-22T10:59:34.987654-07:00"), "2026-07-22T10:59:34-07:00");
	assert.equal(pureMod.formatMemoryIso("2026-07-22T17:59:34.123Z"), "2026-07-22T17:59:34Z");
});

test("formatMemoryIso omits absent or invalid createdAt cleanly", () => {
	assert.equal(pureMod.formatMemoryIso(undefined), null);
	assert.equal(pureMod.formatMemoryIso(null), null);
	assert.equal(pureMod.formatMemoryIso(""), null);
	assert.equal(pureMod.formatMemoryIso("not-a-date"), null);
});

test("formatHitLine includes the timestamp when createdAt is valid, omits it when not", () => {
	const mod = pureMod;
	const withDate = mod.formatHitLine({
		id: "abcdef1234567890",
		kind: "fact",
		durability: "durable",
		content: "hello",
		createdAt: "2026-07-22T10:59:34-07:00",
	});
	assert.equal(withDate, "• [abcdef12] (fact/durable) 2026-07-22T10:59:34-07:00 hello");

	const noDate = mod.formatHitLine({
		id: "abcdef1234567890",
		kind: "fact",
		durability: "durable",
		content: "hello",
		createdAt: null,
	});
	assert.equal(noDate, "• [abcdef12] (fact/durable) hello");

	const badDate = mod.formatHitLine({
		id: "abcdef1234567890",
		kind: "fact",
		durability: "durable",
		content: "hello",
		createdAt: "garbage",
	});
	assert.equal(badDate, "• [abcdef12] (fact/durable) hello");
});

// ── (3) command errors notify a concise, actionable message, never vanish ──
test("a transport error from a dead daemon surfaces a visible error notification", async () => {
	// Nothing listening on this port: connection refused.
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1", MEMORY_COMMAND_TIMEOUT_MS: "500" });
	const handler = getRecallHandler(mod);
	const notes = [];
	await handler("anything", fakeCtx(notes));
	assert.equal(notes.length, 1, "an error must be reported, not swallowed");
	assert.equal(notes[0].level, "error");
	assert.match(notes[0].msg, /\/recall failed/i);
});

test("an HTTP error surfaces a visible error instead of '(nothing)'", async (t) => {
	const server = http.createServer((_req, res) => {
		res.writeHead(500, { "content-type": "application/json" });
		res.end(JSON.stringify({ error: "internal" }));
	});
	t.after(() => server.close());
	const mod = await loadWithEnv({ MEMORY_URL: await listen(server) });
	const notes = [];
	await getRecallHandler(mod)("anything", fakeCtx(notes));
	assert.equal(notes.length, 1);
	assert.equal(notes[0].level, "error");
	assert.match(notes[0].msg, /HTTP 500/);
});

test("a JSON-RPC error surfaces a visible error instead of '(nothing)'", async (t) => {
	const server = http.createServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			const parsed = JSON.parse(body);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(JSON.stringify({ jsonrpc: "2.0", id: parsed.id, error: { code: -32000, message: "database unavailable" } }));
		});
	});
	t.after(() => server.close());
	const mod = await loadWithEnv({ MEMORY_URL: await listen(server) });
	const notes = [];
	await getRecallHandler(mod)("anything", fakeCtx(notes));
	assert.equal(notes.length, 1);
	assert.equal(notes[0].level, "error");
	assert.match(notes[0].msg, /database unavailable/);
});

test("a slow daemon past the command timeout surfaces a visible error, not a hang", async (t) => {
	const server = http.createServer((req, res) => {
		// Never respond — forces the client-side timeout to fire.
	});
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL, MEMORY_COMMAND_TIMEOUT_MS: "200" });
	const handler = getRecallHandler(mod);
	const notes = [];
	const start = Date.now();
	await handler("anything", fakeCtx(notes));
	const elapsed = Date.now() - start;
	assert.ok(elapsed < 5000, `expected the 200ms command timeout to fire quickly, took ${elapsed}ms`);
	assert.equal(notes.length, 1);
	assert.equal(notes[0].level, "error");
	assert.match(notes[0].msg, /\/recall failed/i);
});

// ── (4) /recall uses the higher command timeout, independent of the 2s ─────
// per-turn auto-recall timeout; a slow daemon that /recall tolerates must not
// silently change the before_agent_start hook's budget.
test("/recall survives a delay well past the default 2s auto-recall timeout", async (t) => {
	const server = http.createServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			const parsed = JSON.parse(body);
			setTimeout(() => {
				res.writeHead(200, { "content-type": "application/json" });
				res.end(JSON.stringify({ jsonrpc: "2.0", id: parsed.id, result: { hits: [] } }));
			}, 3000); // longer than the 2000ms auto-recall default
		});
	});
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	// MEMORY_TIMEOUT_MS left at its 2000ms default; only the command path
	// should be able to outlast the 3s delay.
	const mod = await loadWithEnv({ MEMORY_URL, MEMORY_COMMAND_TIMEOUT_MS: "8000" });
	const handler = getRecallHandler(mod);
	const notes = [];
	await handler("anything", fakeCtx(notes));
	assert.equal(notes.length, 1);
	assert.equal(notes[0].level, "info");
	assert.equal(notes[0].msg, "(nothing)");
});

test("the auto-recall hook (before_agent_start) stays silent on a dead daemon", async () => {
	// Same dead endpoint as the transport-error /recall test above, but through
	// the registered before_agent_start hook (the auto-injection path, still
	// wrapped in safe()) — must resolve to undefined, never throw, never notify.
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	let hookHandler = null;
	mod.default({
		on(event, fn) {
			if (event === "before_agent_start") hookHandler = fn;
		},
		registerCommand() {},
	});
	assert.ok(hookHandler, "before_agent_start hook registered");
	const result = await hookHandler({ prompt: "some prompt" }, { cwd: process.cwd() });
	assert.equal(result, undefined, "a dead daemon must degrade silently, not throw or block the turn");
});

test("buildRecallBlock itself rejects on a dead daemon (safe() is the hook's job, not buildRecallBlock's)", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	await assert.rejects(() => mod.buildRecallBlock("some prompt"));
});
