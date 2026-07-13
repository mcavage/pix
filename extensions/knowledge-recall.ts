// pi-stack — knowledge-base injector (client side).
//
// Sibling of extensions/memory-recall.ts, but a different source of truth. Memory
// is soft, per-user, "context, may be stale". The knowledge base is curated and
// authoritative: before every turn we ask the host knowledge service for the
// concepts most relevant to what you're about to do, and slip them into the
// system prompt UNDER A DISTINCT HEADING that says "cite the path". So the model
// treats memory as background and knowledge as ground truth it should reference.
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

import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";

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

// Truncate s to at most max characters, marking the cut with an ellipsis. Used
// so a single concept with a huge description/citation list can never blow the
// budget. Defensive on tiny/zero max.
function truncate(s: string, max: number): string {
	if (max <= 0) return "";
	if (s.length <= max) return s;
	if (max === 1) return "…";
	return s.slice(0, max - 1).trimEnd() + "…";
}

// Pack concepts into the block, respecting the OWN char budget so knowledge never
// starves memory (or gets starved by it) — each runs its own ~1000-char window.
// The budget is enforced from the FIRST line onward: every concept line is capped
// to the remaining budget (ellipsized if needed) and the running total never
// exceeds CHAR_BUDGET, so even one concept with a giant description or citation
// list cannot inject an oversized block.
function formatBlock(concepts: any[]): string | null {
	if (!concepts?.length) return null;
	const header = [
		"## From the knowledge base (authoritative, cite the path)",
		"Curated, authoritative concepts for this task, most relevant first. Prefer these over memory when they conflict, and cite the path when you rely on one.",
	];
	const lines: string[] = [];
	let used = 0;
	for (const c of concepts) {
		if (used >= CHAR_BUDGET) break;
		// Reserve one char per line for the joining newline in the accounting.
		const remaining = CHAR_BUDGET - used - 1;
		if (remaining <= 0) break;
		const full = formatConcept(c);
		const line = truncate(full, remaining);
		if (!line) break;
		lines.push(line);
		used += line.length + 1;
		if (line !== full) break; // truncated => budget is spent
	}
	if (!lines.length) return null;
	return [...header, ...lines].join("\n");
}

// Pure-ish and testable: prompt in, injected block out (or null). Concepts come
// from the host knowledge service.
export async function buildKnowledgeBlock(prompt: string): Promise<string | null> {
	if (!prompt || !prompt.trim()) return null;
	const r = await rpc("query", { query: prompt, limit: QUERY_LIMIT });
	return formatBlock(r?.concepts ?? []);
}

export default function (pi: any) {
	pi.on("before_agent_start", async (event: any, ctx: any) =>
		safe(async () => {
			const prompt = extractPrompt(event, ctx);
			const block = await buildKnowledgeBlock(prompt);
			if (!block) return undefined;
			return { systemPrompt: (event?.systemPrompt ?? "") + "\n\n" + block };
		}),
	);

	pi.registerCommand?.("knowledge", {
		description: "Query the knowledge base and show the cited concepts for a query",
		handler: async (args: any, ctx: any) =>
			safe(async () => {
				const q = String(args ?? "").trim();
				if (!q) return ctx?.ui?.notify?.("usage: /knowledge <query>", "info");
				const r = await rpc("query", { query: q, limit: QUERY_LIMIT });
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
