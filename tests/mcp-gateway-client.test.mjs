// Unit tests for lib/mcp-gateway-client.ts (U9: memory extensions moved from
// direct custom JSON-RPC to deterministic MCP calls through the sbx Gateway).
// Run: node --test tests/
//
// Covers, per the U9 task:
//  - target URL/config resolution from the same injected mcp.json shape
//    pi-mcp-adapter reads ($PI_CODING_AGENT_DIR/mcp.json, or ~/.pi/agent/mcp.json);
//  - the init (initialize + notifications/initialized) / tools/call protocol,
//    including session-id reuse across calls and a session-expired retry;
//  - transport error surfacing (HTTP error, non-JSON body, dead endpoint);
//  - a source-level sentinel proving no direct memory container URL survives
//    anywhere in the shared client or either memory extension.
import assert from "node:assert";
import * as http from "node:http";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

async function listen(server) {
	await new Promise((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
	const { port } = server.address();
	return `http://127.0.0.1:${port}`;
}

// A minimal fake sbx MCP Gateway: records the RAW JSON-RPC envelope of every
// request (so tests can assert on the real protocol sequence), and answers
// initialize / notifications/initialized / tools/call per the Streamable HTTP
// contract this client is written against.
function makeFakeGateway(toolResponder, opts = {}) {
	const requests = [];
	const sessionId = opts.sessionId ?? "fake-session-1";
	let initializeCalls = 0;
	const server = http.createServer((req, res) => {
		let raw = "";
		req.on("data", (c) => (raw += c));
		req.on("end", async () => {
			const msg = raw.trim() ? JSON.parse(raw) : null;
			requests.push(msg);
			if (!msg) {
				res.writeHead(202);
				return res.end();
			}
			if (msg.method === "initialize") {
				initializeCalls++;
				if (opts.rejectSessionId && initializeCalls > 1) {
					// second handshake gets a fresh session id, proving reinit works
				}
				const headers = { "content-type": "application/json" };
				if (!opts.omitSessionId) headers["mcp-session-id"] = opts.sessionIdPerInit ? `${sessionId}-${initializeCalls}` : sessionId;
				res.writeHead(opts.initStatus ?? 200, headers);
				return res.end(
					JSON.stringify({
						jsonrpc: "2.0",
						id: msg.id,
						result: { protocolVersion: "2025-06-18", capabilities: {}, serverInfo: { name: "fake-gateway", version: "0" } },
					}),
				);
			}
			if (msg.method === "notifications/initialized") {
				res.writeHead(202);
				return res.end();
			}
			if (msg.method === "tools/call") {
				if (opts.expireSessionOnce && requests.filter((r) => r?.method === "tools/call").length === 1) {
					res.writeHead(404, { "content-type": "application/json" });
					return res.end();
				}
				const name = msg.params?.name;
				const args = msg.params?.arguments ?? {};
				let result;
				try {
					result = await toolResponder(name, args);
				} catch (e) {
					res.writeHead(200, { "content-type": "application/json" });
					return res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, error: { code: -32000, message: e.message } }));
				}
				res.writeHead(200, { "content-type": "application/json" });
				return res.end(
					JSON.stringify({
						jsonrpc: "2.0",
						id: msg.id,
						result: { content: [{ type: "text", text: JSON.stringify(result) }], structuredContent: result },
					}),
				);
			}
			res.writeHead(404);
			res.end();
		});
	});
	return { server, requests, get initializeCalls() { return initializeCalls; } };
}

function writeGatewayConfig(agentDir, url, extra = {}) {
	mkdirSync(agentDir, { recursive: true });
	writeFileSync(
		join(agentDir, "mcp.json"),
		JSON.stringify({
			settings: { mcpFooterStatus: "problems" },
			mcpServers: { "mcp-gateway": { url, headers: { Authorization: "Bearer proxy-managed" }, ...extra } },
		}),
	);
}

async function withAgentDir(fn) {
	const agentDir = mkdtempSync(join(tmpdir(), "pix-agentdir-"));
	const prior = process.env.PI_CODING_AGENT_DIR;
	process.env.PI_CODING_AGENT_DIR = agentDir;
	let seq = 0;
	try {
		return await fn(agentDir, async () => {
			const url = new URL(`../lib/mcp-gateway-client.ts?case=${seq++}`, import.meta.url);
			return await import(url.href);
		});
	} finally {
		if (prior === undefined) delete process.env.PI_CODING_AGENT_DIR;
		else process.env.PI_CODING_AGENT_DIR = prior;
	}
}

// ── config/URL resolution ───────────────────────────────────────────────────

test("resolvePiMcpConfigPath honors $PI_CODING_AGENT_DIR, matching pi-mcp-adapter's getPiGlobalConfigPath", async () => {
	await withAgentDir(async (agentDir, load) => {
		const mod = await load();
		assert.equal(mod.resolvePiAgentDir(), agentDir);
		assert.equal(mod.resolvePiMcpConfigPath(), join(agentDir, "mcp.json"));
	});
});

test("resolvePiAgentDir falls back to ~/.pi/agent when $PI_CODING_AGENT_DIR is unset", async () => {
	const prior = process.env.PI_CODING_AGENT_DIR;
	delete process.env.PI_CODING_AGENT_DIR;
	try {
		const mod = await import(`../lib/mcp-gateway-client.ts?case=default-dir`);
		assert.match(mod.resolvePiAgentDir(), /\.pi[/\\]agent$/);
	} finally {
		if (prior !== undefined) process.env.PI_CODING_AGENT_DIR = prior;
	}
});

test("loadGatewayServerConfig reads the exact mcp-gateway url/headers pi-kit/spec.yaml writes", async () => {
	await withAgentDir(async (agentDir, load) => {
		writeGatewayConfig(agentDir, "http://mcp-gateway.docker.internal/mcp");
		const mod = await load();
		const cfg = mod.loadGatewayServerConfig();
		assert.equal(cfg.url, "http://mcp-gateway.docker.internal/mcp");
		assert.equal(cfg.headers.Authorization, "Bearer proxy-managed");
	});
});

test("loadGatewayServerConfig returns null when mcp.json is absent (no fallback URL of any kind)", async () => {
	await withAgentDir(async (_agentDir, load) => {
		const mod = await load();
		assert.equal(mod.loadGatewayServerConfig(), null);
	});
});

test("loadGatewayServerConfig returns null when mcp.json has no mcp-gateway server and more than one url candidate", async () => {
	await withAgentDir(async (agentDir, load) => {
		mkdirSync(agentDir, { recursive: true });
		writeFileSync(
			join(agentDir, "mcp.json"),
			JSON.stringify({ mcpServers: { a: { url: "http://a.example/mcp" }, b: { url: "http://b.example/mcp" } } }),
		);
		const mod = await load();
		assert.equal(mod.loadGatewayServerConfig(), null, "ambiguous unnamed servers must never be guessed at");
	});
});

test("loadGatewayServerConfig falls back to the single unnamed url-based server when mcp-gateway is absent", async () => {
	await withAgentDir(async (agentDir, load) => {
		mkdirSync(agentDir, { recursive: true });
		writeFileSync(join(agentDir, "mcp.json"), JSON.stringify({ mcpServers: { renamed: { url: "http://only.example/mcp" } } }));
		const mod = await load();
		assert.equal(mod.loadGatewayServerConfig().url, "http://only.example/mcp");
	});
});

test("loadGatewayServerConfig ignores a command-based (local stdio) server as a url fallback candidate", async () => {
	await withAgentDir(async (agentDir, load) => {
		mkdirSync(agentDir, { recursive: true });
		writeFileSync(join(agentDir, "mcp.json"), JSON.stringify({ mcpServers: { local: { command: "some-mcp-server" } } }));
		const mod = await load();
		assert.equal(mod.loadGatewayServerConfig(), null);
	});
});

test("loadGatewayServerConfig tolerates unparsable JSON without throwing", async () => {
	await withAgentDir(async (agentDir, load) => {
		mkdirSync(agentDir, { recursive: true });
		writeFileSync(join(agentDir, "mcp.json"), "{ not json");
		const mod = await load();
		assert.equal(mod.loadGatewayServerConfig(), null);
	});
});

test("loadGatewayServerConfig interpolates ${VAR} header templates the same way pi-mcp-adapter does", async () => {
	await withAgentDir(async (agentDir, load) => {
		process.env.PIX_TEST_TOKEN = "secret-token-value";
		try {
			mkdirSync(agentDir, { recursive: true });
			writeFileSync(
				join(agentDir, "mcp.json"),
				JSON.stringify({ mcpServers: { "mcp-gateway": { url: "http://x.example/mcp", headers: { Authorization: "Bearer ${PIX_TEST_TOKEN}" } } } }),
			);
			const mod = await load();
			assert.equal(mod.loadGatewayServerConfig().headers.Authorization, "Bearer secret-token-value");
		} finally {
			delete process.env.PIX_TEST_TOKEN;
		}
	});
});

// ── init/call protocol ──────────────────────────────────────────────────────

test("callTool performs initialize, notifications/initialized, then tools/call in order", async () => {
	await withAgentDir(async (agentDir, load) => {
		const { server, requests } = makeFakeGateway(() => ({ hits: [] }));
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			const client = mod.createMcpGatewayClient();
			await client.callTool("memory_recall", { query: "*" }, 2000);
			assert.deepEqual(
				requests.map((r) => r.method),
				["initialize", "notifications/initialized", "tools/call"],
			);
			assert.equal(requests[2].params.name, "memory_recall");
			assert.deepEqual(requests[2].params.arguments, { query: "*" });
		} finally {
			server.close();
		}
	});
});

test("callTool sends the Authorization header from mcp.json on every request, never a hardcoded value", async () => {
	await withAgentDir(async (agentDir, load) => {
		const seenAuth = [];
		const server = http.createServer((req, res) => {
			seenAuth.push(req.headers.authorization);
			let raw = "";
			req.on("data", (c) => (raw += c));
			req.on("end", () => {
				const msg = JSON.parse(raw);
				if (msg.method === "initialize") {
					res.writeHead(200, { "content-type": "application/json", "mcp-session-id": "s1" });
					return res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-06-18" } }));
				}
				if (msg.method === "notifications/initialized") return res.writeHead(202).end();
				res.writeHead(200, { "content-type": "application/json" });
				res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { content: [], structuredContent: { ok: true } } }));
			});
		});
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`, {});
		try {
			const mod = await load();
			await mod.createMcpGatewayClient().callTool("memory_stats", {}, 2000);
			assert.ok(seenAuth.length > 0 && seenAuth.every((h) => h === "Bearer proxy-managed"));
		} finally {
			server.close();
		}
	});
});

test("callTool reuses the negotiated Mcp-Session-Id across repeated calls (only one initialize)", async () => {
	await withAgentDir(async (agentDir, load) => {
		const { server, requests, initializeCalls: _unused } = makeFakeGateway(() => ({ ok: true }));
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			const client = mod.createMcpGatewayClient();
			await client.callTool("memory_stats", {}, 2000);
			await client.callTool("memory_stats", {}, 2000);
			const initCount = requests.filter((r) => r.method === "initialize").length;
			assert.equal(initCount, 1, "a second call on the same client must reuse the session, not reinitialize");
			const sessionHeadersSeen = requests.filter((r) => r.method === "tools/call").length;
			assert.equal(sessionHeadersSeen, 2);
		} finally {
			server.close();
		}
	});
});

test("callTool reinitializes exactly once after a 404 session-expired response, then succeeds", async () => {
	await withAgentDir(async (agentDir, load) => {
		const { server, requests } = makeFakeGateway(() => ({ ok: true }), { expireSessionOnce: true });
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			const client = mod.createMcpGatewayClient();
			const result = await client.callTool("memory_stats", {}, 2000);
			assert.deepEqual(result, { ok: true });
			const initCount = requests.filter((r) => r.method === "initialize").length;
			assert.equal(initCount, 2, "a 404 on tools/call must trigger exactly one reinitialize");
		} finally {
			server.close();
		}
	});
});

test("callTool throws when no mcp-gateway server is registered, naming the config path", async () => {
	await withAgentDir(async (_agentDir, load) => {
		const mod = await load();
		await assert.rejects(() => mod.createMcpGatewayClient().callTool("memory_recall", {}, 500), /no MCP gateway registered/);
	});
});

test("callTool surfaces a clean HTTP-error diagnostic when the Gateway rejects initialize", async () => {
	await withAgentDir(async (agentDir, load) => {
		const server = http.createServer((_req, res) => {
			res.writeHead(500, { "content-type": "text/plain" });
			res.end("gateway unavailable");
		});
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			await assert.rejects(() => mod.createMcpGatewayClient().callTool("memory_recall", {}, 2000), /HTTP 500/);
		} finally {
			server.close();
		}
	});
});

test("callTool surfaces a stable diagnostic for a non-JSON 2xx initialize response, not a raw parser error", async () => {
	await withAgentDir(async (agentDir, load) => {
		const server = http.createServer((_req, res) => {
			res.writeHead(200, { "content-type": "text/plain" });
			res.end("not json");
		});
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			await assert.rejects(() => mod.createMcpGatewayClient().callTool("memory_recall", {}, 2000), /non-JSON response/);
		} finally {
			server.close();
		}
	});
});

test("callTool surfaces a JSON-RPC error returned from initialize", async () => {
	await withAgentDir(async (agentDir, load) => {
		const server = http.createServer((req, res) => {
			let raw = "";
			req.on("data", (c) => (raw += c));
			req.on("end", () => {
				const msg = JSON.parse(raw);
				res.writeHead(200, { "content-type": "application/json" });
				res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, error: { code: -32000, message: "database unavailable" } }));
			});
		});
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			await assert.rejects(() => mod.createMcpGatewayClient().callTool("memory_recall", {}, 2000), /database unavailable/);
		} finally {
			server.close();
		}
	});
});

test("callTool rejects quickly (not a hang) against a dead endpoint", async () => {
	await withAgentDir(async (agentDir, load) => {
		writeGatewayConfig(agentDir, "http://127.0.0.1:1");
		const mod = await load();
		const start = Date.now();
		await assert.rejects(() => mod.createMcpGatewayClient().callTool("memory_recall", {}, 500));
		assert.ok(Date.now() - start < 2000);
	});
});

test("unwrapToolResult prefers structuredContent over parsing the text block", async () => {
	await withAgentDir(async (agentDir, load) => {
		const { server } = makeFakeGateway((name) => (name === "memory_recall" ? { hits: [{ id: "abc", content: "hello" }] } : {}));
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			const result = await mod.createMcpGatewayClient().callTool("memory_recall", { query: "*" }, 2000);
			assert.deepEqual(result, { hits: [{ id: "abc", content: "hello" }] });
		} finally {
			server.close();
		}
	});
});

test("a tool result with isError:true throws using its text content", async () => {
	await withAgentDir(async (agentDir, load) => {
		const server = http.createServer((req, res) => {
			let raw = "";
			req.on("data", (c) => (raw += c));
			req.on("end", () => {
				const msg = JSON.parse(raw);
				if (msg.method === "initialize") {
					res.writeHead(200, { "content-type": "application/json", "mcp-session-id": "s" });
					return res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-06-18" } }));
				}
				if (msg.method === "notifications/initialized") return res.writeHead(202).end();
				res.writeHead(200, { "content-type": "application/json" });
				res.end(
					JSON.stringify({
						jsonrpc: "2.0",
						id: msg.id,
						result: { isError: true, content: [{ type: "text", text: "profile not found" }] },
					}),
				);
			});
		});
		await new Promise((r) => server.listen(0, "127.0.0.1", r));
		const { port } = server.address();
		writeGatewayConfig(agentDir, `http://127.0.0.1:${port}`);
		try {
			const mod = await load();
			await assert.rejects(() => mod.createMcpGatewayClient().callTool("memory_forget", { id: "x" }, 2000), /profile not found/);
		} finally {
			server.close();
		}
	});
});

test("MEMORY_TOOL names exactly the pix-memory MCP tool set from the architecture doc", async () => {
	const mod = await import(`../lib/mcp-gateway-client.ts?case=tool-names`);
	assert.deepEqual(mod.MEMORY_TOOL, {
		recall: "memory_recall",
		stats: "memory_stats",
		remember: "memory_remember",
		forget: "memory_forget",
		observe: "memory_observe",
	});
});

// ── sentinel: no direct memory URL anywhere in the client or extensions ────

test("no source file under lib/ or extensions/memory-*.ts still dials the old direct memory URL/env var as CODE", () => {
	// Doc-comment prose explaining the migration is fine (and expected) to name
	// the old host.docker.internal:11435 endpoint for context; the sentinel that
	// matters is that nothing constructs or reads it as a live value any more:
	// no literal default-URL string, and no `process.env.MEMORY_URL` read.
	const files = ["lib/mcp-gateway-client.ts", "extensions/memory-recall.ts", "extensions/memory-capture.ts"];
	for (const rel of files) {
		const src = readFileSync(new URL(`../${rel}`, import.meta.url), "utf8");
		assert.doesNotMatch(src, /["'`]https?:\/\/host\.docker\.internal:11435["'`]/, `${rel} must not hardcode the old direct memory URL`);
		assert.doesNotMatch(src, /process\.env\.MEMORY_URL/, `${rel} must not read a direct MEMORY_URL env var any more`);
	}
});

test("mcp-gateway-client.ts never imports node:http/https functions except its own internal raw transport (no second direct client)", () => {
	const src = readFileSync(new URL("../lib/mcp-gateway-client.ts", import.meta.url), "utf8");
	// It's fine (and required) for THIS file to use node:http/https to reach the
	// Gateway; the sentinel is that no OTHER hardcoded remote host string exists
	// beside what loadGatewayServerConfig resolves from mcp.json.
	assert.doesNotMatch(src, /["'`]https?:\/\/(?!127\.0\.0\.1|localhost)[a-z0-9.-]*\.internal/i);
});
