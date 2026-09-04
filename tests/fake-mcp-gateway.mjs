// Shared test fixture: a minimal fake sbx MCP Gateway plus the mcp.json config
// fixture memory-recall.test.mjs, memory-capture.test.mjs, and
// workspace-markers.test.mjs all point extensions/memory-*.ts at (via
// $PI_CODING_AGENT_DIR) instead of the retired direct MEMORY_URL JSON-RPC path.
//
// The fake gateway speaks the real protocol (initialize, notifications/initialized,
// tools/call) — see tests/mcp-gateway-client.test.mjs for dedicated protocol-
// sequence assertions. To keep the many pre-existing "requests[0].params.foo"
// assertions in the tests that use this fixture readable, `requests` here
// records one flattened entry per `tools/call` in the OLD JSON-RPC shape
// (`{ method: toolName, params: arguments }`), not the raw MCP envelope.
import * as http from "node:http";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

/**
 * @param {(name: string, args: any) => any} responder maps a tool name + its
 *   arguments to the plain JS value the tool "returns" (mirrors the old RPC
 *   handler's `result`). Throwing surfaces as a JSON-RPC error.
 * @param {{delayMs?: number, failFirstCallStatus?: number}} [opts]
 */
export function makeFakeGateway(responder, opts = {}) {
	const requests = [];
	const server = http.createServer((req, res) => {
		let raw = "";
		req.on("data", (c) => (raw += c));
		req.on("end", async () => {
			const msg = raw.trim() ? JSON.parse(raw) : null;
			if (!msg) {
				res.writeHead(202);
				return res.end();
			}
			if (msg.method === "initialize") {
				res.writeHead(200, { "content-type": "application/json", "mcp-session-id": "fake-session" });
				return res.end(
					JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: "2025-06-18", capabilities: {} } }),
				);
			}
			if (msg.method === "notifications/initialized") {
				res.writeHead(202);
				return res.end();
			}
			if (msg.method === "tools/call") {
				const name = msg.params?.name;
				const args = msg.params?.arguments ?? {};
				if (opts.delayMs) await new Promise((r) => setTimeout(r, opts.delayMs));
				requests.push({ method: name, params: args });
				let result;
				try {
					result = await responder(name, args);
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
	return { server, requests };
}

/** A gateway whose very first request (initialize) fails, matching the old
 * "dead/erroring daemon" fixtures that used to respond to the single direct call. */
export function makeFailingGateway(status, body = "") {
	return http.createServer((_req, res) => {
		res.writeHead(status, { "content-type": "text/plain" });
		res.end(body);
	});
}

/** A gateway whose initialize succeeds with a JSON-RPC error envelope, matching the
 * old fixtures that returned `{jsonrpc, id, error}` for any method. */
export function makeJsonRpcErrorGateway(message) {
	return http.createServer((req, res) => {
		let raw = "";
		req.on("data", (c) => (raw += c));
		req.on("end", () => {
			const msg = JSON.parse(raw);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, error: { code: -32000, message } }));
		});
	});
}

export async function listen(server) {
	await new Promise((resolvePromise) => server.listen(0, "127.0.0.1", resolvePromise));
	const { port } = server.address();
	return `http://127.0.0.1:${port}`;
}

/** Writes <agentDir>/mcp.json with the exact mcp-gateway shape pi-kit/spec.yaml's setup step writes. */
export function writeGatewayConfig(agentDir, url) {
	mkdirSync(agentDir, { recursive: true });
	writeFileSync(
		join(agentDir, "mcp.json"),
		JSON.stringify({
			settings: { mcpFooterStatus: "problems" },
			mcpServers: { "mcp-gateway": { url, headers: { Authorization: "Bearer proxy-managed" } } },
		}),
	);
}
