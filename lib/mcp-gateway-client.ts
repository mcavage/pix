// pix — shared sbx MCP Gateway client (transport, not policy).
//
// U9 (docs/design/pix-v2-architecture.md §9.3): the memory extensions used to
// speak a private JSON-RPC dialect directly to the host memory daemon at
// host.docker.internal:11435. That direct path is gone. This module is the
// ONLY thing extensions/memory-recall.ts and extensions/memory-capture.ts use
// to reach memory now: it resolves the Gateway endpoint from the SAME injected
// Pi MCP configuration pi-mcp-adapter reads (~/.pi/agent/mcp.json, or
// $PI_CODING_AGENT_DIR/mcp.json — see pi-mcp-adapter/agent-dir.ts, mirrored
// below so this file has no runtime dependency on that package), and calls
// `tools/call` over standard MCP Streamable HTTP. It never dials the memory
// container or host.docker.internal directly (AC: no direct memory URL).
//
// Why this lives outside extensions/: pi loads EVERY `.ts` under extensions/
// as an extension factory and crashes at startup on a file that isn't one
// (AC-P0-103, see AGENTS.md and scripts/check-recall-transport.sh).
//
// PROTOCOL: one client-side "session" per createMcpGatewayClient() instance —
// initialize, then notifications/initialized, then tools/call, reusing the
// negotiated Mcp-Session-Id (when the server issues one) for subsequent calls.
// A 404 on tools/call is treated as an expired/unknown session per the
// Streamable HTTP spec: the session is dropped and re-established exactly
// once before the call is reported as failed.
//
// HOST UAT STILL NEEDED (see docs/design/pix-v2-architecture.md §9.3): this
// file is written and tested against the *observed* sandbox seam (the literal
// mcp.json shape pi-kit/spec.yaml's setup step writes, and node:http reaching
// mcp-gateway.docker.internal the same way the existing host.docker.internal
// calls do, bypassing pi's global proxy dispatcher). No sandbox in this
// worktree can prove that a SECOND client connection to the injected Gateway
// endpoint can initialize and call tools while the Pi adapter's own connection
// is live, or that a raw node:http POST to mcp-gateway.docker.internal
// actually reaches the gateway without going through the egress proxy. That is
// exactly the acceptance test the architecture doc calls for; it requires a
// live sbx host and is out of reach from this isolated worktree.

import { existsSync, readFileSync } from "node:fs";
import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";
import { homedir } from "node:os";
import { join, resolve } from "node:path";

// ── config discovery (mirrors pi-mcp-adapter/agent-dir.ts exactly) ─────────

/** Same resolution pi-mcp-adapter's getAgentDir() uses: $PI_CODING_AGENT_DIR, else ~/.pi/agent. */
export function resolvePiAgentDir(): string {
	const configured = process.env.PI_CODING_AGENT_DIR?.trim();
	if (!configured) return join(homedir(), ".pi", "agent");
	if (configured === "~") return homedir();
	if (configured.startsWith("~/")) return resolve(homedir(), configured.slice(2));
	return resolve(configured);
}

/** Same file pi-mcp-adapter's getPiGlobalConfigPath() reads by default: <agentDir>/mcp.json. */
export function resolvePiMcpConfigPath(): string {
	return join(resolvePiAgentDir(), "mcp.json");
}

// Literal name pi-kit/spec.yaml's setup step writes into mcp.json's
// mcpServers map ("Register the sbx MCP gateway with pi (pi-mcp-adapter)").
export const GATEWAY_SERVER_NAME = "mcp-gateway";

export interface GatewayServerConfig {
	url: string;
	headers: Record<string, string>;
}

// Same ${VAR}/$env:VAR/{env:VAR} interpolation pi-mcp-adapter applies to
// header values (utils.ts's interpolateEnvVars), so a config that expresses
// its bearer token as a template still resolves. Deliberately does NOT run a
// leading "!" command marker the way the adapter's full secret-expression
// resolver does: this is a lifecycle-hook client, not a config editor, and
// running an arbitrary command from a background hook is not something
// recall/capture should ever do.
export function interpolateEnvVars(value: string): string {
	return value
		.replace(/\$\{(\w+)\}/g, (_, name) => process.env[name] ?? "")
		.replace(/\$env:(\w+)/g, (_, name) => process.env[name] ?? "")
		.replace(/\{env:(\w+)\}/g, (_, name) => process.env[name] ?? "");
}

/**
 * Read the injected mcp.json and pull out the Gateway server entry. Returns
 * null — never throws — when the file is absent, unparsable, or has no
 * matching server: callers degrade to "memory unavailable" rather than
 * crashing a turn.
 *
 * Primary lookup is the exact key the kit writes (`mcp-gateway`). Defensive
 * fallback: if that key is absent but exactly one configured server carries a
 * `url` (a remote server, never a `command` local-stdio entry), use it — a
 * future kit or hand-edit that renames the single gateway entry must not
 * silently go dark. Two or more unnamed URL entries is treated as "not
 * found": guessing which one is the Gateway would be worse than a visible
 * "memory unavailable".
 */
export function loadGatewayServerConfig(): GatewayServerConfig | null {
	const path = resolvePiMcpConfigPath();
	if (!existsSync(path)) return null;
	let raw: string;
	try {
		raw = readFileSync(path, "utf8");
	} catch {
		return null;
	}
	let parsed: any;
	try {
		parsed = JSON.parse(raw);
	} catch {
		return null;
	}
	const servers = parsed?.mcpServers;
	if (!servers || typeof servers !== "object") return null;

	let entry = servers[GATEWAY_SERVER_NAME];
	if (!entry) {
		const urlEntries = Object.values(servers).filter(
			(s: any) => s && typeof s === "object" && typeof s.url === "string" && s.url.trim() && !s.command,
		);
		if (urlEntries.length === 1) entry = urlEntries[0];
	}
	if (!entry || typeof entry !== "object" || typeof entry.url !== "string" || !entry.url.trim()) return null;

	const headers: Record<string, string> = {};
	if (entry.headers && typeof entry.headers === "object") {
		for (const [k, v] of Object.entries(entry.headers)) {
			if (typeof v === "string") headers[k] = interpolateEnvVars(v);
		}
	}
	return { url: entry.url.trim(), headers };
}

// ── raw Streamable HTTP transport ───────────────────────────────────────────

interface RawResponse {
	status: number;
	headers: Record<string, string>;
	body: string;
}

// node:http/https, not fetch. The same reason extensions/memory-recall.ts and
// extensions/memory-capture.ts already give for host.docker.internal: pi
// installs a global undici proxy dispatcher in the sandbox, and the Gateway
// endpoint (mcp-gateway.docker.internal) is sbx's LOCAL data-plane, not an
// allowlisted egress host (it is deliberately absent from
// permissions.network.allow in pi-kit/spec.yaml) — so it must be reached
// directly, the same way host.docker.internal traffic is. See the HOST UAT
// note at the top of this file: this is the one fact a live sandbox must
// still confirm.
function httpPostRaw(urlStr: string, headers: Record<string, string>, body: string, timeoutMs: number): Promise<RawResponse> {
	return new Promise((resolvePromise, reject) => {
		if (timeoutMs <= 0) return reject(new Error("timeout"));
		let u: URL;
		try {
			u = new URL(urlStr);
		} catch (e) {
			return reject(e);
		}
		const req = (u.protocol === "https:" ? httpsRequest : httpRequest)(
			{
				hostname: u.hostname,
				port: u.port || (u.protocol === "https:" ? 443 : 80),
				path: (u.pathname || "/") + (u.search || ""),
				method: "POST",
				headers: { ...headers, "content-length": Buffer.byteLength(body) },
				timeout: timeoutMs,
			},
			(res) => {
				let chunks = "";
				res.on("data", (c) => (chunks += c));
				res.on("end", () => {
					const outHeaders: Record<string, string> = {};
					for (const [k, v] of Object.entries(res.headers)) {
						if (typeof v === "string") outHeaders[k] = v;
						else if (Array.isArray(v)) outHeaders[k] = v.join(", ");
					}
					resolvePromise({ status: res.statusCode ?? 0, headers: outHeaders, body: chunks });
				});
			},
		);
		req.on("error", reject);
		req.on("timeout", () => req.destroy(new Error("timeout")));
		req.write(body);
		req.end();
	});
}

/**
 * Parse one Streamable HTTP response body as a JSON-RPC envelope. Handles
 * both a plain `application/json` body (the common case for a stateless
 * single-request/response call) and a `text/event-stream` body (the standard
 * allows either): the first `data:` frame that parses as a `jsonrpc: "2.0"`
 * envelope wins. An empty body (e.g. a 202 Accepted for a notification)
 * parses to null.
 */
export function parseJsonRpcBody(contentType: string | undefined, body: string): any {
	if (!body || !body.trim()) return null;
	const ct = (contentType ?? "").split(";")[0]?.trim().toLowerCase();
	if (ct === "text/event-stream") {
		for (const block of body.split(/\r?\n\r?\n/)) {
			const data = block
				.split(/\r?\n/)
				.filter((l) => l.startsWith("data:"))
				.map((l) => l.slice(5).replace(/^\s/, ""))
				.join("\n");
			if (!data) continue;
			try {
				const parsed = JSON.parse(data);
				if (parsed && parsed.jsonrpc === "2.0") return parsed;
			} catch {
				continue;
			}
		}
		return null;
	}
	try {
		return JSON.parse(body);
	} catch {
		// A stable diagnostic, not a raw SyntaxError: a 2xx response that isn't
		// valid JSON (e.g. an intermediary error page) must read as a Gateway
		// transport problem, not a parser crash.
		throw new Error("memory gateway returned a non-JSON response");
	}
}

const PROTOCOL_VERSION = "2025-06-18";
const CLIENT_INFO = { name: "pix-memory-client", version: "1" };

function baseHeaders(cfg: GatewayServerConfig): Record<string, string> {
	return { accept: "application/json, text/event-stream", "content-type": "application/json", ...cfg.headers };
}

function remaining(deadline: number): number {
	return Math.max(0, deadline - Date.now());
}

// A short, single-line diagnostic suffix from a non-2xx response body
// (truncated, whitespace-collapsed), matching the detail the old direct
// JSON-RPC transport surfaced ("memory service HTTP 502: dial tcp ...").
function responseDetail(body: string): string {
	const detail = body.trim().replace(/\s+/g, " ").slice(0, 240);
	return detail ? `: ${detail}` : "";
}

function jsonRpcErrorMessage(parsed: any, fallback: string): string {
	const message = parsed?.error?.message;
	return typeof message === "string" && message ? message : fallback;
}

/** MCP tool-result unwrap: prefer structuredContent, else parse the text content as JSON, else raw text. */
function unwrapToolResult(result: any): any {
	if (result?.isError) {
		const text = extractText(result.content);
		throw new Error(text || "memory gateway tool call reported an error");
	}
	if (result?.structuredContent && typeof result.structuredContent === "object") return result.structuredContent;
	const text = extractText(result?.content);
	if (text) {
		try {
			return JSON.parse(text);
		} catch {
			return { text };
		}
	}
	return result ?? null;
}

function extractText(content: unknown): string {
	if (!Array.isArray(content)) return "";
	return content
		.filter((b: any) => b && b.type === "text" && typeof b.text === "string")
		.map((b: any) => b.text)
		.join("\n");
}

interface GatewaySession {
	sessionId?: string;
	protocolVersion: string;
}

class SessionExpiredError extends Error {
	constructor() {
		super("memory gateway session expired");
	}
}

export interface McpGatewayClient {
	/** Call one MCP tool by name through the Gateway and return its unwrapped result. Throws on any failure. */
	callTool(name: string, args: unknown, timeoutMs: number): Promise<any>;
}

/**
 * One Gateway client per extension instance (mirrors createRecallChannel's
 * per-instance state, not module-global state): every session id and
 * negotiated protocol version this client has learned lives only in this
 * closure, so two extensions (or two test-loaded module instances) never
 * share or race each other's Gateway session.
 */
export function createMcpGatewayClient(): McpGatewayClient {
	let sessionPromise: Promise<GatewaySession> | null = null;
	let callId = 1;

	async function initSession(cfg: GatewayServerConfig, deadline: number): Promise<GatewaySession> {
		const initBody = JSON.stringify({
			jsonrpc: "2.0",
			id: 0,
			method: "initialize",
			params: { protocolVersion: PROTOCOL_VERSION, capabilities: {}, clientInfo: CLIENT_INFO },
		});
		const res = await httpPostRaw(cfg.url, baseHeaders(cfg), initBody, remaining(deadline));
		if (res.status < 200 || res.status >= 300) {
			throw new Error(`memory gateway HTTP ${res.status} during initialize${responseDetail(res.body)}`);
		}
		const parsed = parseJsonRpcBody(res.headers["content-type"], res.body);
		if (parsed?.error) throw new Error(jsonRpcErrorMessage(parsed, "memory gateway initialize error"));
		const sessionId = res.headers["mcp-session-id"];
		const negotiated = typeof parsed?.result?.protocolVersion === "string" ? parsed.result.protocolVersion : PROTOCOL_VERSION;

		const notifyHeaders: Record<string, string> = { ...baseHeaders(cfg), "mcp-protocol-version": negotiated };
		if (sessionId) notifyHeaders["mcp-session-id"] = sessionId;
		// notifications/initialized carries no id and expects no JSON-RPC reply
		// (typically 202/204); best-effort only, a rejection here must not fail
		// the whole call — the server already accepted initialize.
		await httpPostRaw(
			cfg.url,
			notifyHeaders,
			JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized", params: {} }),
			remaining(deadline),
		).catch(() => {});

		return { sessionId, protocolVersion: negotiated };
	}

	async function ensureSession(cfg: GatewayServerConfig, deadline: number): Promise<GatewaySession> {
		if (!sessionPromise) {
			sessionPromise = initSession(cfg, deadline).catch((err) => {
				sessionPromise = null;
				throw err;
			});
		}
		return sessionPromise;
	}

	async function toolsCall(cfg: GatewayServerConfig, session: GatewaySession, name: string, args: unknown, deadline: number) {
		const headers: Record<string, string> = { ...baseHeaders(cfg), "mcp-protocol-version": session.protocolVersion };
		if (session.sessionId) headers["mcp-session-id"] = session.sessionId;
		const body = JSON.stringify({ jsonrpc: "2.0", id: ++callId, method: "tools/call", params: { name, arguments: args ?? {} } });
		const res = await httpPostRaw(cfg.url, headers, body, remaining(deadline));
		if (res.status === 404 && session.sessionId) throw new SessionExpiredError();
		if (res.status < 200 || res.status >= 300) throw new Error(`memory gateway HTTP ${res.status} calling ${name}${responseDetail(res.body)}`);
		const parsed = parseJsonRpcBody(res.headers["content-type"], res.body);
		if (parsed?.error) throw new Error(jsonRpcErrorMessage(parsed, `memory gateway error calling ${name}`));
		return unwrapToolResult(parsed?.result);
	}

	return {
		async callTool(name: string, args: unknown, timeoutMs: number): Promise<any> {
			const cfg = loadGatewayServerConfig();
			if (!cfg) {
				throw new Error(
					`no MCP gateway registered: "${GATEWAY_SERVER_NAME}" is missing from ${resolvePiMcpConfigPath()} (memory tools are unavailable)`,
				);
			}
			const deadline = Date.now() + timeoutMs;
			let session = await ensureSession(cfg, deadline);
			try {
				return await toolsCall(cfg, session, name, args, deadline);
			} catch (err) {
				if (err instanceof SessionExpiredError) {
					sessionPromise = null;
					session = await ensureSession(cfg, deadline);
					return await toolsCall(cfg, session, name, args, deadline);
				}
				throw err;
			}
		},
	};
}

// MCP tool names the pix-memory server exposes (docs/design/pix-v2-architecture.md
// §9.2). Extensions call these through McpGatewayClient#callTool instead of the
// old bare JSON-RPC method names ("recall", "stats", "remember", "forget",
// "observe").
export const MEMORY_TOOL = {
	recall: "memory_recall",
	stats: "memory_stats",
	remember: "memory_remember",
	forget: "memory_forget",
	observe: "memory_observe",
} as const;
