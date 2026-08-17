// Unit tests for extensions/memory-recall.ts (the /recall command UX fix).
// Run: node --test tests/
//
// MEMORY_URL/TIMEOUT_MS are read once at module load, so each scenario that
// needs a live (fake) memory service sets the env first, then dynamic-imports
// a fresh module instance via a cache-busting query string.
import assert from "node:assert";
import * as http from "node:http";
import { register } from "node:module";
import { test } from "node:test";

// memory-recall.ts imports `typebox` for the tool schemas; stub it (and the
// other pi runtime packages) so plain node can import the extension.
register("./stub-loader.mjs", import.meta.url);

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

// Captures every registered slash command and tool so tool tests can exercise
// memory_recall/memory_stats directly.
function capturePi(mod) {
	const commands = new Map();
	const tools = new Map();
	mod.default({
		on() {},
		registerCommand(name, cfg) {
			commands.set(name, cfg);
		},
		registerTool(t) {
			tools.set(t.name, t);
		},
	});
	return { commands, tools };
}

const noopCtx = { cwd: process.cwd() };
const toolText = (r) => r.content?.map((c) => c.text ?? "").join("\n") ?? "";

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
		// A blank /recall means "show everything": it must ask for the full
		// 100-row cap and a charBudget large enough that the daemon's 1200-char
		// default doesn't truncate the response long before `limit` kicks in.
		assert.equal(req.params.limit, 100);
		assert.equal(req.params.charBudget, 1_000_000);
	}
});

test("an explicit /recall query is passed through unchanged, keeping the daemon's own defaults", async (t) => {
	const { server, requests } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const handler = getRecallHandler(mod);

	await handler("  docker sandboxes  ", fakeCtx([]));
	assert.equal(requests[0].params.query, "docker sandboxes");
	assert.equal(requests[0].params.limit, undefined, "an explicit query must not override the daemon's own limit default");
	assert.equal(requests[0].params.charBudget, undefined, "an explicit query must not override the daemon's own charBudget default");
});

test("/recall on '*' appends a truncation notice when the daemon returns a full 100-hit page", async (t) => {
	const hits = Array.from({ length: 100 }, (_, i) => ({ id: `id${i}`.padEnd(8, "0"), kind: "fact", content: `fact ${i}` }));
	const { server } = makeFakeDaemon(() => ({ hits }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const handler = getRecallHandler(mod);

	const notes = [];
	await handler(undefined, fakeCtx(notes));
	assert.match(notes[0].msg, /truncated at 100 hits/i);
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
		content: "hello",
		createdAt: "2026-07-22T10:59:34-07:00",
	});
	assert.equal(withDate, "• [abcdef12] (fact) 2026-07-22T10:59:34-07:00 hello");

	const noDate = mod.formatHitLine({
		id: "abcdef1234567890",
		kind: "fact",
		content: "hello",
		createdAt: null,
	});
	assert.equal(noDate, "• [abcdef12] (fact) hello");

	const badDate = mod.formatHitLine({
		id: "abcdef1234567890",
		kind: "fact",
		content: "hello",
		createdAt: "garbage",
	});
	assert.equal(badDate, "• [abcdef12] (fact) hello");
});

// formatHitLine used to also render a `/durability` segment; the write path
// makes every row durable now (see extensions/memory-recall.ts), so the
// per-hit annotation was deleted along with the perishable score filter
// below. A hit that still carries a legacy durability field must not leak it
// back into the line.
test("formatHitLine no longer renders a durability segment, even if a hit still carries one", () => {
	const line = pureMod.formatHitLine({ id: "abcdef1234567890", kind: "fact", durability: "perishable", content: "hello" });
	assert.equal(line, "• [abcdef12] (fact) hello");
});

// A watcher-sourced (experimental-auto) row renders an "/auto" tag so it is
// visibly distinct from an explicit one; anything else (or an absent source)
// renders exactly as before.
test("formatHitLine tags a watcher-sourced hit as auto, leaves an explicit hit untagged", () => {
	const auto = pureMod.formatHitLine({ id: "abcdef1234567890", kind: "fact", content: "hello", source: "watcher" });
	assert.equal(auto, "• [abcdef12] (fact/auto) hello");

	const explicit = pureMod.formatHitLine({ id: "abcdef1234567890", kind: "fact", content: "hello", source: "user" });
	assert.equal(explicit, "• [abcdef12] (fact) hello");
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

// The command-timeout MAGNITUDE (200ms here vs. the real 10s production
// default) is the timeout/clock seam: MEMORY_COMMAND_TIMEOUT_MS already lets a
// test swap in a tiny value instead of waiting out the real default, so this
// stays a genuine client-side timeout without a multi-second real-time wait.
test("a slow daemon past the command timeout surfaces a visible error, not a hang", async (t) => {
	const server = http.createServer((req, res) => {
		// Never respond, forces the client-side timeout to fire.
	});
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL, MEMORY_COMMAND_TIMEOUT_MS: "20" });
	const handler = getRecallHandler(mod);
	const notes = [];
	const start = Date.now();
	await handler("anything", fakeCtx(notes));
	const elapsed = Date.now() - start;
	assert.ok(elapsed < 2000, `expected the 20ms command timeout to fire quickly, took ${elapsed}ms`);
	assert.equal(notes.length, 1);
	assert.equal(notes[0].level, "error");
	assert.match(notes[0].msg, /\/recall failed/i);
});

// ── (4) /recall uses the higher command timeout, independent of the 2s ─────
// per-turn auto-recall timeout; a slow daemon that /recall tolerates must not
// silently change the before_agent_start hook's budget.
//
// Production always ships with a 2s auto-recall default and a 10s command
// default (asserted directly below, no waiting required); this behavioral
// test only needs the *ratio* between the two timeouts and the daemon delay,
// so it uses the MEMORY_TIMEOUT_MS/MEMORY_COMMAND_TIMEOUT_MS seam to shrink
// all three proportionally instead of sleeping through the real 3s+ it would
// take to prove the same thing against the actual production magnitudes.
test("the real production timeout defaults are unchanged", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	assert.equal(mod.DEFAULT_MEMORY_TIMEOUT_MS, 2000, "auto-recall default must stay 2000ms");
	assert.equal(mod.DEFAULT_MEMORY_COMMAND_TIMEOUT_MS, 10000, "command default must stay 10000ms");
});

test("/recall survives a delay well past the (shrunk) auto-recall timeout", async (t) => {
	const AUTO_RECALL_TIMEOUT_MS = 10; // stands in for production's 2000ms default
	const COMMAND_TIMEOUT_MS = 100; // stands in for production's 10000ms default
	const DAEMON_DELAY_MS = 25; // > AUTO_RECALL_TIMEOUT_MS, well under COMMAND_TIMEOUT_MS
	const server = http.createServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			const parsed = JSON.parse(body);
			setTimeout(() => {
				res.writeHead(200, { "content-type": "application/json" });
				res.end(JSON.stringify({ jsonrpc: "2.0", id: parsed.id, result: { hits: [] } }));
			}, DAEMON_DELAY_MS);
		});
	});
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({
		MEMORY_URL,
		MEMORY_TIMEOUT_MS: String(AUTO_RECALL_TIMEOUT_MS),
		MEMORY_COMMAND_TIMEOUT_MS: String(COMMAND_TIMEOUT_MS),
	});
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
	// wrapped in safe()), must resolve to undefined, never throw, never notify.
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

test("buildRecallBlock returns null on a zero-hit response, never an empty header-only block", async (t) => {
	const { server } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });

	const block = await mod.buildRecallBlock("some prompt");
	assert.equal(block, null, "no hits must short-circuit to null, not the header rendered with zero rows");
});

// ── typed agent-facing tools (memory_recall/memory_stats, read-only) ───────

test("only the two read-only memory tools are registered; write/delete stay human slash commands", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	const { commands, tools } = capturePi(mod);
	assert.deepEqual([...tools.keys()].sort(), ["memory_recall", "memory_stats"]);
	assert.ok(!tools.has("memory_remember"), "memory_remember must not be agent-callable");
	assert.ok(!tools.has("memory_forget"), "memory_forget must not be agent-callable");
	for (const name of ["recall", "remember", "forget"]) {
		assert.ok(commands.has(name), `/${name} command still registered`);
	}
});

test("memory_recall defaults query to '*', requests up to 100 rows with a large charBudget, and returns formatted hit lines", async (t) => {
	const { server, requests } = makeFakeDaemon(() => ({
		hits: [
			{ id: "abcdef1234567890", kind: "fact", content: "hello", createdAt: null },
		],
	}));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const { tools } = capturePi(mod);

	const r = await tools.get("memory_recall").execute("id", {}, undefined, undefined, noopCtx);
	assert.equal(requests[0].method, "recall");
	assert.equal(requests[0].params.query, "*");
	assert.equal(requests[0].params.limit, 100, "'*' defaults to the full 100-row cap, not the search default of 6");
	assert.equal(requests[0].params.charBudget, 1_000_000, "'*' must not be truncated by the daemon's 1200-char default");
	assert.equal(toolText(r), "• [abcdef12] (fact) hello");
});

test("memory_recall keeps a search default of 6 (and no charBudget override) for an explicit non-'*' query", async (t) => {
	const { server, requests } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const { tools } = capturePi(mod);

	await tools.get("memory_recall").execute("id", { query: "docker sandboxes" }, undefined, undefined, noopCtx);
	assert.equal(requests[0].params.query, "docker sandboxes");
	assert.equal(requests[0].params.limit, 6, "an explicit search query keeps the default limit of 6");
	assert.equal(requests[0].params.charBudget, undefined, "an explicit search query must not get the '*' charBudget override");
});

test("memory_recall passes an explicit query through and clamps limit to 1..100", async (t) => {
	const { server, requests } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const { tools } = capturePi(mod);
	const recall = tools.get("memory_recall");

	await recall.execute("id", { query: "docker sandboxes", limit: 250 }, undefined, undefined, noopCtx);
	assert.equal(requests[0].params.query, "docker sandboxes");
	assert.equal(requests[0].params.limit, 100, "limit clamps to the 100 max");

	await recall.execute("id", { limit: 0 }, undefined, undefined, noopCtx);
	assert.equal(requests[1].params.limit, 1, "limit clamps to the 1 min");

	const r = await recall.execute("id", {}, undefined, undefined, noopCtx);
	assert.equal(toolText(r), "(nothing)");
});

test("memory_recall appends a clear truncation line when hits.length equals the effective limit", async (t) => {
	const hits = Array.from({ length: 3 }, (_, i) => ({ id: `id${i}`.padEnd(8, "0"), kind: "fact", content: `fact ${i}` }));
	const { server } = makeFakeDaemon(() => ({ hits }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const { tools } = capturePi(mod);

	const r = await tools.get("memory_recall").execute("id", { query: "x", limit: 3 }, undefined, undefined, noopCtx);
	assert.match(toolText(r), /truncated at 3 hits/i, "hits.length === limit must append a truncation line");
});

test("memory_recall does NOT append a truncation line when hits.length is below the limit", async (t) => {
	const hits = [{ id: "id00000", kind: "fact", content: "only one" }];
	const { server } = makeFakeDaemon(() => ({ hits }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const { tools } = capturePi(mod);

	const r = await tools.get("memory_recall").execute("id", { query: "x", limit: 6 }, undefined, undefined, noopCtx);
	assert.doesNotMatch(toolText(r), /truncated/i);
});

test("memory_recall throws (does not swallow) when the daemon is unreachable", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1", MEMORY_COMMAND_TIMEOUT_MS: "500" });
	const { tools } = capturePi(mod);
	await assert.rejects(() => tools.get("memory_recall").execute("id", {}, undefined, undefined, noopCtx));
});

// memory_stats is a raw passthrough of whatever the host returns; it must
// tolerate the durable/perishable fields the host still emits (inert until
// the U5 schema work) without special-casing or stripping them client-side.
test("memory_stats calls the stats RPC with the active profile and returns the raw counts", async (t) => {
	const { server, requests } = makeFakeDaemon(() => ({ active: 3, durable: 2, perishable: 1 }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });
	const { tools } = capturePi(mod);

	const r = await tools.get("memory_stats").execute("id", {}, undefined, undefined, noopCtx);
	assert.equal(requests[0].method, "stats");
	assert.deepEqual(JSON.parse(toolText(r)), { active: 3, durable: 2, perishable: 1 });
});

test("memory_stats throws when the daemon is unreachable", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1", MEMORY_COMMAND_TIMEOUT_MS: "500" });
	const { tools } = capturePi(mod);
	await assert.rejects(() => tools.get("memory_stats").execute("id", {}, undefined, undefined, noopCtx));
});

// ── buildRecallBlock: durability/perishable filtering deleted ─────────────
//
// The write path makes every row durable now (see AGENTS.md/CHANGELOG), so a
// dedicated auto-inject score floor for "perishable" hits has nothing left to
// filter; it was deleted along with the AUTO_INJECT_PERISHABLE_SCORE_FLOOR
// constant. This is the sentinel: a low-score hit that still carries a legacy
// `durability: "perishable"` field (e.g. a row an older binary wrote) must be
// treated exactly like any other hit, never specially dropped.

test("buildRecallBlock no longer filters hits by durability or score (the perishable floor was deleted)", async (t) => {
	const hits = [{ content: "low-score legacy-perishable hit", durability: "perishable", score: 0.01 }];
	const { server } = makeFakeDaemon(() => ({ hits }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });

	const block = await mod.buildRecallBlock("some prompt");
	assert.ok(block?.includes("low-score legacy-perishable hit"), "a hit must never be dropped for a low score plus a durability field");
});

test("the injected block tells the model it's a relevance-filtered subset and to use memory_recall", async (t) => {
	const hits = [{ content: "some fact", score: 0.9 }];
	const { server } = makeFakeDaemon(() => ({ hits }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);
	const mod = await loadWithEnv({ MEMORY_URL });

	const block = await mod.buildRecallBlock("some prompt");
	assert.match(block, /relevance-filtered subset from the host memory daemon, not the full store/);
	assert.match(block, /Use memory_recall to inspect the store/);
});

// ── descriptions encode read-only + capability semantics ───────────────────
//
// This tool surface (memory_recall/memory_stats) is read-only by design, but
// that is a UX/safety posture on the AGENT'S TYPED TOOLS, not a security
// boundary on the daemon: the host memory service is unauthenticated and
// reachable, so arbitrary sandbox code could still POST to it directly. The
// descriptions must say the tool surface is read-only without claiming the
// agent/sandbox is incapable of mutating the store.

test("memory tool descriptions state this tool surface is read-only, without over-claiming the agent/sandbox cannot mutate memory", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	const { tools } = capturePi(mod);
	for (const name of ["memory_recall", "memory_stats"]) {
		const d = tools.get(name).description;
		assert.match(d, /read-only/i, `${name} description`);
		assert.match(d, /cannot store or delete/i, `${name} description`);
		assert.match(d, /human-driven slash commands/i, `${name} description`);
		assert.match(d, /not a security control/i, `${name} description must caveat this is UX posture, not a security boundary`);
		assert.doesNotMatch(
			d,
			/cannot autonomously mutate/i,
			`${name} description must not claim the agent/sandbox cannot mutate memory (the daemon is unauthenticated and reachable)`,
		);
		assert.doesNotMatch(d, /memory_remember/, `${name} description must not reference memory_remember`);
	}
});

test("memory_recall description tells the model when to use it and that it can return up to 100 rows, not the whole store", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	const { tools } = capturePi(mod);
	const d = tools.get("memory_recall").description;
	assert.match(d, /what is remembered/i);
	assert.match(d, /memory semantics/i);
	assert.match(d, /whether the agent can see memory/i);
	assert.match(d, /up to 100 rows/i, "must describe the real cap, not claim the full/unbounded store");
	assert.doesNotMatch(d, /the full store/i, "must not claim memory_recall sees the unbounded full store");
});

test("every memory tool description states direct-daemon access, never shelling out, and no claim that anything expires", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	const { tools } = capturePi(mod);
	for (const name of ["memory_recall", "memory_stats"]) {
		const d = tools.get(name).description;
		assert.match(d, /never shell out to `pix` or `curl`/, `${name} description`);
		assert.match(d, /every memory is durable/i, `${name} description`);
		assert.match(d, /no automatic expiry/i, `${name} description`);
		// The watcher's perishable, 7-day-TTL "events" channel was removed
		// host-side: nothing this tool surface can return expires any more, so
		// the description must never claim otherwise.
		assert.doesNotMatch(d, /expire/i, `${name} description must not claim anything expires`);
		assert.doesNotMatch(d, /7 days/i, `${name} description must not reference the removed 7-day watcher-event TTL`);
	}
});

// The agent has NO control over the watcher's automatic capture: it must never
// assert a specific statement will or won't be remembered/saved/pinned/captured
// unless memory_recall confirms it after the fact, and it must always point a
// user who cares at the explicit `/remember` pin. This lives in promptGuidelines
// (not just description) so it's in the model's face on every relevant turn, and
// the exact wording is pinned here so it can't regress or drift between tools.
const MEMORY_CAPTURE_HONESTY_GUIDELINE =
	"The agent does not control automatic capture. Never claim a statement will or will not be remembered, saved, pinned, or auto-captured unless memory_recall confirms it after capture. " +
	"If it matters, tell the user /remember <fact> is the explicit reliable path.";

test("memory_recall and memory_stats promptGuidelines carry the exact capture-honesty guideline", async () => {
	const mod = await loadWithEnv({ MEMORY_URL: "http://127.0.0.1:1" });
	const { tools } = capturePi(mod);
	for (const name of ["memory_recall", "memory_stats"]) {
		const guidelines = tools.get(name).promptGuidelines;
		assert.ok(Array.isArray(guidelines) && guidelines.length > 0, `${name} must declare promptGuidelines`);
		assert.ok(
			guidelines.includes(MEMORY_CAPTURE_HONESTY_GUIDELINE),
			`${name} promptGuidelines must include the exact capture-honesty guideline`,
		);
	}
});

test("the capture-honesty guideline never claims a statement is off-topic for code so it won't save", () => {
	assert.doesNotMatch(
		MEMORY_CAPTURE_HONESTY_GUIDELINE,
		/won'?t help( with)? code/i,
		"must not claim something 'won't help code so won't save'",
	);
});
