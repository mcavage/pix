// pix — knowledge-base injector (client side).
//
// Sibling of extensions/memory-recall.ts, but a different source of truth. Memory
// is soft, per-user, "context, may be stale". The knowledge base is curated and
// authoritative: before every turn we ask the host knowledge service for the
// concepts most relevant to what you're about to do, and APPEND them under a
// DISTINCT HEADING that says "cite the path". So the model treats memory as
// background and knowledge as ground truth it should reference.
//
// The store lives on the host (:11436, JSON-RPC, same shape as memory :11435);
// this extension only calls it over node:http via host.docker.internal.
// Defensive throughout: knowledge is optional (the capability may be `none`), so
// if the service is down, slow, or empty, recall is skipped SILENTLY and the turn
// proceeds normally — no error spam.
//
//   KNOWLEDGE_URL         default http://host.docker.internal:11436
//   KNOWLEDGE_TIMEOUT_MS  default 2000 (a slow store must never stall a turn)
//   KNOWLEDGE_CHAR_BUDGET default 1000 (its OWN budget, independent of memory)
//
// SCOPE CONTRACT (per-workspace bundle filter). The shared host store indexes
// every bundle it has ever seen (global + every project), so an un-scoped query
// bleeds concepts across projects. The launcher (pix run) resolves the
// bundle set for THIS workspace — {global, this-project} — and communicates it
// to us one of two ways, which we resolve in this order:
//
//   1. env  KNOWLEDGE_SCOPE          comma- or newline-separated bundle ids
//   2. file <cwd>/.pix/knowledge.scope   comma- or newline-separated ids
//
// Whichever is found first wins; entries are trimmed and blanks dropped. The ids
// are daemon-supplied identifiers (the exact strings in the store's `bundle`
// column, normally absolute host paths); we NEVER interpret them, we only echo
// them back verbatim as the `bundles` array on the query RPC (U1 accepts a
// `bundles` array; empty/absent = query all bundles). When neither source is
// present or both are empty we send NO `bundles`, so a raw/un-launched sandbox
// hitting the store directly keeps the backward-compatible "all bundles"
// behavior. Reading the file is best-effort (safe()); a missing file or env is
// the normal case, never an error.

import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { RECALL_BYTE_CAP, createRecallChannel } from "../lib/recall-message.ts";

const KNOWLEDGE_URL = process.env.KNOWLEDGE_URL ?? "http://host.docker.internal:11436";
const TIMEOUT_MS = Number(process.env.KNOWLEDGE_TIMEOUT_MS ?? 2000);
const CHAR_BUDGET = Number(process.env.KNOWLEDGE_CHAR_BUDGET ?? 1000);
const QUERY_LIMIT = 6;

const safe = async <T>(fn: () => Promise<T>): Promise<T | undefined> => {
	try {
		return await fn();
	} catch {
		return undefined; // best-effort; must not break the agent
	}
};

// IMPORTANT: use node:http, not fetch. In an sbx sandbox pi installs a global
// undici proxy dispatcher (HTTP_PROXY -> the sbx proxy), and sbx's NO_PROXY does
// NOT include host.docker.internal, so fetch() to the host store gets routed
// through the proxy and fails. node:http ignores that dispatcher and goes direct.
function postJson(urlStr: string, body: unknown, timeoutMs: number): Promise<any> {
	return new Promise((resolve, reject) => {
		const u = new URL(urlStr);
		const data = JSON.stringify(body);
		const req = (u.protocol === "https:" ? httpsRequest : httpRequest)(
			{
				hostname: u.hostname,
				port: u.port || (u.protocol === "https:" ? 443 : 80),
				path: u.pathname || "/",
				method: "POST",
				headers: { "content-type": "application/json", "content-length": Buffer.byteLength(data) },
				timeout: timeoutMs,
			},
			(res) => {
				let chunks = "";
				res.on("data", (c) => (chunks += c));
				res.on("end", () => {
					try {
						resolve(chunks ? JSON.parse(chunks) : null);
					} catch (e) {
						reject(e);
					}
				});
			},
		);
		req.on("error", reject);
		req.on("timeout", () => req.destroy(new Error("timeout")));
		req.write(data);
		req.end();
	});
}

let rpcId = 0;
async function rpc(method: string, params: any): Promise<any> {
	const j = await postJson(KNOWLEDGE_URL, { jsonrpc: "2.0", id: ++rpcId, method, params }, TIMEOUT_MS);
	return j?.result ?? null;
}

// Split a raw scope string (from env or the scope file) into a clean bundle-id
// list: comma- OR newline-separated, trimmed, blanks dropped.
function parseScope(raw: string): string[] {
	return raw
		.split(/[\n,]/)
		.map((s) => s.trim())
		.filter(Boolean);
}

// Resolve the per-workspace bundle scope (see SCOPE CONTRACT at the top). Env
// KNOWLEDGE_SCOPE first, then <cwd>/.pix/knowledge.scope. Returns [] when
// neither is present/non-empty, in which case the caller sends NO `bundles`.
// File read is defensive: a missing/unreadable file is the normal case.
function resolveScope(): string[] {
	const env = process.env.KNOWLEDGE_SCOPE;
	if (typeof env === "string" && env.trim()) {
		const fromEnv = parseScope(env);
		if (fromEnv.length) return fromEnv;
	}
	try {
		const raw = readFileSync(join(process.cwd(), ".pix", "knowledge.scope"), "utf8");
		return parseScope(raw);
	} catch {
		return []; // missing/unreadable file is normal — no scope, query all
	}
}

// Build the query params, adding `bundles` only when a scope was resolved so the
// empty case preserves the store's back-compat "all bundles" semantics.
function queryParams(query: string): any {
	const params: any = { query, limit: QUERY_LIMIT };
	const bundles = resolveScope();
	if (bundles.length) params.bundles = bundles;
	return params;
}

// The user's submitted text. pi's event shape isn't fully pinned, so try the
// likely fields, then fall back to the last user entry in session history.
function extractPrompt(event: any, ctx: any): string {
	const direct = event?.prompt ?? event?.input ?? event?.text ?? event?.message?.content;
	if (typeof direct === "string" && direct.trim()) return direct;
	const hist =
		(typeof ctx?.sessionManager?.history === "function"
			? ctx.sessionManager.history()
			: ctx?.sessionManager?.entries) ?? [];
	const lastUser = [...hist].reverse().find((e: any) => e?.role === "user" || e?.type === "user");
	return lastUser?.content ?? lastUser?.text ?? "";
}

// Render one concept as a cited bullet. Prefer the description, fall back to the
// snippet. Always lead with a linked title -> path so the model has the citation
// target inline, and append any explicit citations the service returned.
function formatConcept(c: any): string {
	const title = String(c?.title ?? c?.id ?? "concept").trim();
	const path = String(c?.path ?? "").trim();
	const body = String(c?.description ?? c?.snippet ?? "").replace(/\s+/g, " ").trim();
	const link = path ? `[${title}](${path})` : title;
	let line = body ? `- ${link} — ${body}` : `- ${link}`;
	const cites = Array.isArray(c?.citations) ? c.citations.filter(Boolean).map(String) : [];
	if (cites.length) line += ` (cite: ${cites.join(", ")})`;
	return line;
}

// The block header, verbatim. The "authoritative, cite the path" framing is the
// provenance label that separates this from memory's "context, may be stale";
// it is emitted byte-for-byte and charged against the cap, never shortened to
// fit one more concept (AC-P0-107).
export const KNOWLEDGE_HEADER = [
	"## From the knowledge base (authoritative, cite the path)",
	"Curated, authoritative concepts for this task, most relevant first. Prefer these over memory when they conflict, and cite the path when you rely on one.",
];

// Knowledge keeps its OWN budget so it never starves memory (or gets starved by
// it) — each channel runs its own window. KNOWLEDGE_CHAR_BUDGET can only ever
// LOWER it: the 1 KB per-turn cap is the ceiling either way (AC-P0-106).
const CAP = Math.min(RECALL_BYTE_CAP, CHAR_BUDGET);

// A concept, as a row the shared recall helper can dedupe and cut at a boundary.
// Identity is the concept id, then its path: a concept has no `content` field,
// so leaving it to the helper's sha256(content) fallback would alias every
// concept onto the same key and inject exactly one of them per session.
function conceptRow(c: any): { id?: string; content: string } {
	const id = String(c?.id ?? c?.path ?? "").trim();
	return id ? { id, content: formatConcept(c) } : { content: formatConcept(c) };
}

// Pure-ish and testable: prompt in, concept rows out. Concepts come from the
// host knowledge service.
export async function fetchKnowledgeRows(prompt: string): Promise<{ id?: string; content: string }[]> {
	if (!prompt || !prompt.trim()) return [];
	const r = await rpc("query", queryParams(prompt));
	return (r?.concepts ?? []).map(conceptRow);
}

// Prompt in, injected block out (or null), with a throwaway dedup set — the
// per-session set lives on the extension's channel below.
export async function buildKnowledgeBlock(prompt: string): Promise<string | null> {
	const rows = await fetchKnowledgeRows(prompt);
	const built = createRecallChannel({ header: KNOWLEDGE_HEADER, renderRow: (r: any) => r.content, cap: CAP }).build(rows);
	return built ? built.message.content : null;
}

export default function (pi: any) {
	// One channel per pi session: the same concept recalled on three turns is
	// appended once (AC-P0-105).
	const channel = createRecallChannel({ header: KNOWLEDGE_HEADER, renderRow: (r: any) => r.content, cap: CAP });

	// APPEND-ONLY. This hook must never return `systemPrompt` — see the same note
	// in memory-recall.ts and scripts/check-recall-transport.sh (AC-P0-102).
	pi.on("before_agent_start", async (event: any, ctx: any) =>
		safe(async () => {
			const prompt = extractPrompt(event, ctx);
			const rows = await fetchKnowledgeRows(prompt);
			const built = channel.build(rows);
			if (!built) return undefined;
			return { message: built.message };
		}),
	);

	pi.registerCommand?.("knowledge", {
		description: "Query the knowledge base and show the cited concepts for a query",
		handler: async (args: any, ctx: any) =>
			safe(async () => {
				const q = String(args ?? "").trim();
				if (!q) return ctx?.ui?.notify?.("usage: /knowledge <query>", "info");
				const r = await rpc("query", queryParams(q));
				const concepts = r?.concepts ?? [];
				const text = concepts.length
					? concepts
							.map((c: any) => {
								const cites = Array.isArray(c?.citations) ? c.citations.filter(Boolean).map(String) : [];
								const suffix = cites.length ? `  [cite: ${cites.join(", ")}]` : "";
								return `• ${c.title ?? c.id} (${c.path ?? "?"})${suffix}`;
							})
							.join("\n")
					: "(nothing in the knowledge base)";
				ctx?.ui?.notify?.(text, "info");
			}),
	});
}
