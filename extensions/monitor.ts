// pi-stack — live wiretap mirror for `pi-stack monitor` (client side / Unit D).
//
// Taps pi's turn/tool/model hooks, summarizes each provider request/response
// and tool call (never the raw text — hashes + short previews + byte counts),
// and ships NDJSON events to a host-side `pi-stack monitor` process over
// node:http. Pure debug mirror: when the host process isn't running, sends
// fail fast, back off, and the agent is never blocked or slowed down.
//
// Wire contract lives in `.pi-agent/deliver/monitor/architecture.md` Section 2
// (frozen; Go source of truth is `services/host/monitor/event.go`). This file
// owns nothing else — it knows the JSON shape, never the Go types.
//
//   PI_STACK_MONITOR_URL   default http://host.docker.internal:11437
//   PI_STACK_MONITOR       "0" disables the extension entirely
//
// pi hook payload field names for `before_provider_request` are intentionally
// `unknown` in pi's own types (it's the provider-specific wire body, which
// differs between Anthropic/OpenAI/Google shapes), so every extractor below
// tries each provider's known field names and falls back to empty, exactly
// like `extractPrompt` in memory-recall.ts. Keep summary/hash logic in small
// exported pure functions so it's testable without pi. Field names below are
// grounded against three sources: pi's shipped `.d.ts`
// (`core/extensions/types.d.ts`, `core/source-info.d.ts`), `docs/extensions.md`
// and `docs/session-format.md` in this pi install, and each provider's own
// wire docs (Anthropic Messages API, OpenAI Responses API, Gemini
// generateContent).

import { createHash, randomUUID } from "node:crypto";
import { request as httpRequest, type ClientRequest } from "node:http";
import { request as httpsRequest } from "node:https";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const MONITOR_URL = process.env.PI_STACK_MONITOR_URL ?? "http://host.docker.internal:11437";
const MONITOR_ENABLED = process.env.PI_STACK_MONITOR !== "0";
const SANDBOX_ID = process.env.SANDBOX_VM_ID ?? "";

const MAX_QUEUE = 500; // bounded in-VM queue; backpressure drops OLDEST, never blocks the agent
const POST_TIMEOUT_MS = 2000;
const BASE_BACKOFF_MS = 500;
const MAX_BACKOFF_MS = 30_000; // cap ~30s per architecture.md 3.D

// Module-level: shared across every extension instance in this process. A
// session switch resets sessionId/turnId/etc via session_start, but seq keeps
// climbing per architecture.md 3.D — "seq = module-level counter per event".
let seqCounter = 0;

const safe = <T>(fn: () => T): T | undefined => {
	try {
		return fn();
	} catch {
		return undefined; // best-effort; must not break the agent
	}
};

// ─── Pure summary / hash helpers (exported for testing without pi) ─────────

/** sha256 hex digest of a UTF-8 string. */
export function sha256Hex(text: string): string {
	return createHash("sha256").update(text, "utf8").digest("hex");
}

/** Single-line, length-capped preview for a row summary. */
export function truncatePreview(text: string, max = 120): string {
	const oneLine = text.replace(/\s+/g, " ").trim();
	return oneLine.length > max ? `${oneLine.slice(0, max - 1)}…` : oneLine;
}

/** Rough token estimate: ~4 bytes/token (no tokenizer available client-side). */
export function estimateTokens(bytes: number): number {
	return Math.max(0, Math.round(bytes / 4));
}

/**
 * Flatten a message/tool "content" field to plain text for hashing/preview.
 * Provider payloads vary: a plain string, an array of content blocks
 * ({type,text} / {type,content}), or an arbitrary JSON-able value. Gemini
 * "parts" entries ({text: "..."}) and Anthropic/OpenAI content blocks both
 * satisfy the `block.text` branch below, so one flattener covers all three.
 */
export function stringifyContent(content: unknown): string {
	if (content == null) return "";
	if (typeof content === "string") return content;
	if (Array.isArray(content)) {
		return content
			.map((block: any) => {
				if (typeof block === "string") return block;
				if (typeof block?.text === "string") return block.text;
				if (typeof block?.content === "string") return block.content;
				return safe(() => JSON.stringify(block)) ?? String(block);
			})
			.join("\n");
	}
	return safe(() => JSON.stringify(content)) ?? String(content);
}

/**
 * Flatten a Gemini `systemInstruction`/`system_instruction` value to text.
 * Gemini shapes this as `{ parts: [{ text }], role? }`, but tolerates a plain
 * string too (some client libraries pass it through as-is).
 */
export function geminiInstructionToText(value: any): string {
	if (value == null) return "";
	if (typeof value === "string") return value;
	if (Array.isArray(value?.parts)) return stringifyContent(value.parts);
	return stringifyContent(value);
}

/**
 * Best-effort extraction of the system prompt text from a provider payload.
 * Tries all three provider shapes pi ships and picks whichever is present:
 *   - Anthropic Messages API: top-level `system` (string or content-block array)
 *   - OpenAI Responses API: top-level `instructions` (string)
 *   - Gemini generateContent: `systemInstruction`/`system_instruction`, which
 *     may also be nested under `config` per the Gemini SDK's request shape
 */
export function extractSystemPromptText(payload: any): string {
	const anthropic = payload?.system ?? payload?.systemPrompt ?? payload?.system_prompt;
	if (typeof anthropic === "string" && anthropic) return anthropic;
	if (Array.isArray(anthropic) && anthropic.length) return stringifyContent(anthropic);

	const openai = payload?.instructions;
	if (typeof openai === "string" && openai) return openai;

	const gemini =
		payload?.systemInstruction ??
		payload?.system_instruction ??
		payload?.config?.systemInstruction ??
		payload?.config?.system_instruction;
	if (gemini != null) {
		const text = geminiInstructionToText(gemini);
		if (text) return text;
	}

	return "";
}

/** Normalize one message-ish entry from any provider shape to {role, content}. */
function normalizeMessageEntry(m: any): { role?: string; content?: unknown } {
	// Anthropic/OpenAI use `content`; Gemini's `contents[]` entries use `parts`
	// instead; a bare string entry (rare, but seen in some OpenAI `input`
	// shapes) becomes user content. Fall back to the whole entry so nothing is
	// silently dropped for an unrecognized shape.
	if (typeof m === "string") return { role: "user", content: m };
	return { role: m?.role, content: m?.content ?? m?.parts ?? m?.text ?? m };
}

/**
 * Best-effort extraction of the message array from a provider payload. Tries
 * all three provider shapes pi ships and picks whichever is present:
 *   - Anthropic Messages API: top-level `messages[]`
 *   - Gemini generateContent: top-level `contents[]` ({role, parts[]})
 *   - OpenAI Responses API: top-level `input` (array of items, or a bare
 *     string for a single-turn request); `messages[]` as a fallback for
 *     Chat-Completions-shaped payloads
 */
export function extractMessages(payload: any): Array<{ role?: string; content?: unknown }> {
	const anthropic = payload?.messages;
	if (Array.isArray(anthropic) && anthropic.length) return anthropic.map(normalizeMessageEntry);

	const gemini = payload?.contents;
	if (Array.isArray(gemini) && gemini.length) return gemini.map(normalizeMessageEntry);

	const openaiInput = payload?.input;
	if (Array.isArray(openaiInput) && openaiInput.length) return openaiInput.map(normalizeMessageEntry);
	if (typeof openaiInput === "string" && openaiInput) return [{ role: "user", content: openaiInput }];

	return [];
}

/**
 * Best-effort extraction of tool defs, tolerating Anthropic-, OpenAI-, and
 * Gemini-shaped tool entries:
 *   - Anthropic: top-level `tools[]`, each `{name, input_schema}`
 *   - OpenAI Responses API: top-level `tools[]`, each `{name}` (flat) or
 *     Chat-Completions-shaped `{function:{name}}`
 *   - Gemini: `tools[]` (top-level OR nested under `config.tools`), each
 *     `{functionDeclarations:[{name}]}` (or the snake_case
 *     `function_declarations`) rather than one name per array entry
 */
export function extractToolDefs(payload: any): Array<{ name: string }> {
	const rawTools = payload?.tools ?? payload?.config?.tools ?? [];
	if (!Array.isArray(rawTools)) return [];

	const flat: any[] = [];
	for (const t of rawTools) {
		const decls = t?.functionDeclarations ?? t?.function_declarations;
		if (Array.isArray(decls)) flat.push(...decls);
		else flat.push(t);
	}

	return flat
		.map((t: any) => ({ name: String(t?.name ?? t?.function?.name ?? t?.tool?.name ?? "") }))
		.filter((t: { name: string }) => t.name);
}

// Heuristic MCP naming: pi's own MCP tool naming isn't pinned in the docs at
// the time of writing (no mcp*.md ships with this pi version), so this
// matches the common `mcp_<server>_...` / `mcp:...` prefix conventions and
// the `<server>__<tool>` double-underscore namespacing convention used by
// several MCP bridges. It is deliberately NOT trusted as the sole signal
// (see `classifyToolSource`/`isMcpSourceInfo` below): real sbx-gateway tools
// registered by pi-stack (`slack_post`, `gmail_search`, `drive_get` — see
// AGENTS.md "MCP host servers go through the sbx gateway") use a single
// underscore and don't match this regex at all, so live tool metadata is
// checked first whenever it's available.
export function isMcpToolName(name: string): boolean {
	if (!name) return false;
	return /^mcp[_:./-]/i.test(name) || /^[a-z0-9][a-z0-9_-]*__[a-z0-9][a-z0-9_-]*$/i.test(name);
}

export function extractMcpServerName(name: string): string {
	const prefixed = name.match(/^mcp[_:./-]+([^_:./-]+)/i);
	if (prefixed) return prefixed[1];
	const namespaced = name.match(/^([a-z0-9][a-z0-9_-]*)__/i);
	if (namespaced) return namespaced[1];
	return "";
}

/**
 * Shape of `pi.getAllTools()[i].sourceInfo` (pi's `core/source-info.d.ts`).
 * `server`/`metadata.server` are speculative extras: pi's own type doesn't
 * document a dedicated per-server identifier field today, but if a future
 * pi version (or the MCP adapter extension itself) ever attaches one, this
 * is where `mcpServerFromSourceInfo` looks for it — never derived from a
 * package path (R3-1).
 */
export interface ToolSourceInfo {
	path?: string;
	source?: string;
	scope?: string;
	origin?: string;
	server?: string;
	metadata?: { server?: string; [key: string]: unknown };
}

// pi's own built-in MCP adapter extension package. Gateway tools (Slack,
// Gmail, Drive, ...) registered per AGENTS.md's "MCP host servers go through
// the sbx gateway" are NOT registered by pi's core — they're registered
// THROUGH this extension, so `pi.getAllTools()` attributes them to it (e.g.
// `path: "pi-mcp-adapter/index.ts"`, `source: "extension"` or an npm
// specifier that names the package). Matched as a whole path/package-name
// segment (bounded by the start of string, "/", or "@"), never as a bare
// substring, so it can't false-positive on an unrelated name that merely
// contains "mcp". NOTE: the exact runtime `sourceInfo` shape pi reports for
// adapter-owned gateway tools is unverified without a live gateway
// attachment (no MCP server is attached in this dev sandbox) — this is the
// documented, defensible positive signal (the adapter's own package path),
// not a confirmed observation.
// R4-4: the trailing group must be a real path/package SEGMENT terminator
// (end-of-string or a path/scope separator), not `\b`. A word boundary also
// matches before `-` or `.`, so it falsely matched unrelated packages like
// `pi-mcp-adapter-helper` or `pi-mcp-adapter.backup` as the adapter itself.
const MCP_ADAPTER_PACKAGE_RE = /(^|[/@])pi-mcp-adapter(?:$|[/@])/i;

/**
 * Extract an MCP server name from pi's live tool `sourceInfo`, ONLY when
 * there's an anchored, positive marker for it, or a dedicated server-name
 * field. (R2-3b, extended by R3-1)
 *
 * pi's own convention for a synthesized `path` is a bracketed
 * `"<scheme:identifier>"` token (extensions.md's own builtin example:
 * `path: "<builtin:read>"`) — a real MCP adapter that follows this
 * convention would set `path` to something like `"<mcp:slack>"`, and that
 * bracketed identifier IS positive evidence of the server name. Likewise a
 * `source` value is only trusted when "mcp" is the SCHEME at the very start
 * of the string (`"mcp:slack"`, `"mcp_slack"`, ...), never merely present
 * somewhere in it. A dedicated `info.server` / `info.metadata.server` field
 * is trusted directly, since it's an explicit identifier rather than
 * something parsed out of a path.
 *
 * What this NEVER does (R3-1): derive a server name from the adapter's own
 * package path (`"pi-mcp-adapter/index.ts"`). That path only proves MCP
 * PROVENANCE (see `isMcpSourceInfo` below) — it names the adapter
 * extension, not the downstream MCP server the tool came from. When
 * provenance is known to be MCP but no server name can be anchored out of
 * `sourceInfo`, callers should use `"mcp:unknown"` rather than a mis-parsed
 * path (the R2-3b bug: the previous regex matched "mcp" ANYWHERE in
 * `path`/`source`, turning `"pi-mcp-adapter/index.ts"` into the garbage
 * label `mcp:adapter/index.ts`).
 */
export function mcpServerFromSourceInfo(info: ToolSourceInfo | null | undefined): string {
	if (!info) return "";
	// A genuine, explicit server identifier always wins — never derived from
	// a package path.
	const direct = typeof info.server === "string" ? info.server.trim() : "";
	if (direct) return direct;
	const metaServer = typeof info.metadata?.server === "string" ? info.metadata.server.trim() : "";
	if (metaServer) return metaServer;

	const path = typeof info.path === "string" ? info.path : "";
	// Bracketed builtin-style token: "<mcp:server>" (anchored to the WHOLE
	// bracketed string, not a substring match anywhere inside it).
	const bracketed = path.match(/^<mcp[:_./-]+([^>]+)>$/i);
	if (bracketed?.[1]) return bracketed[1];
	const source = typeof info.source === "string" ? info.source : "";
	// Anchored scheme prefix on `source` — must START with "mcp", not just
	// contain it (a package/extension name like "pi-mcp-adapter" does).
	const sourceMatch = source.match(/^mcp[:_./-]+(.+)$/i);
	if (sourceMatch?.[1]) return sourceMatch[1];
	return "";
}

/**
 * True when `sourceInfo` positively identifies MCP provenance. Two
 * independent positive signals, both anchored (never a bare substring
 * match):
 *   1. `path`/`source` STARTS WITH the "mcp" scheme (optionally inside pi's
 *      own `"<scheme:id>"` bracket convention) — e.g. `"mcp:slack"`,
 *      `"<mcp:slack>"`.
 *   2. `path`/`source`/`origin` names the pi MCP adapter PACKAGE itself
 *      (R3-1) — e.g. `"pi-mcp-adapter/index.ts"`. Gateway tools like
 *      `slack_post`/`gmail_search` are registered THROUGH this extension,
 *      so pi attributes them to its package path even though the tool name
 *      carries no `mcp*`/`__` marker. This is provenance ONLY: it does not
 *      by itself yield a server name (see `mcpServerFromSourceInfo`).
 *
 * Signal (1) alone is what R2-3b anchored against a false positive: an
 * extension package name that happens to CONTAIN "mcp" as a name fragment
 * (`"pi-mcp-adapter/index.ts"`) must never satisfy it merely because "mcp"
 * appears somewhere in the string. Signal (2) is a distinct, deliberate,
 * whole-package-name check added for exactly that adapter package, not a
 * loosening of (1).
 */
export function isMcpSourceInfo(info: ToolSourceInfo | null | undefined): boolean {
	if (!info) return false;
	const source = typeof info.source === "string" ? info.source.toLowerCase() : "";
	const path = typeof info.path === "string" ? info.path.toLowerCase() : "";
	const origin = typeof info.origin === "string" ? info.origin.toLowerCase() : "";
	if (source === "builtin" || source === "sdk") return false;
	if (/^mcp[:_./-]/.test(source) || /^<mcp[:_./-]/.test(path) || /^mcp[:_./-]/.test(path)) return true;
	return MCP_ADAPTER_PACKAGE_RE.test(path) || MCP_ADAPTER_PACKAGE_RE.test(source) || MCP_ADAPTER_PACKAGE_RE.test(origin);
}

// pi's own built-ins (see extensions.md tool_call examples + host-guard.ts).
const BUILTIN_TOOL_NAMES = new Set([
	"bash",
	"read",
	"write",
	"edit",
	"grep",
	"find",
	"ls",
	"glob",
	"todo",
	"task",
	"subagent",
	"web_search",
	"fetch_content",
	"get_search_content",
]);

/**
 * source for a tool_start event: "mcp:<server>" | "builtin" | "skill:<name>".
 * `sourceInfo`, when passed, is pi's live `pi.getAllTools()` metadata for
 * this exact tool name and takes priority over the name heuristic — it's
 * ground truth when available, the heuristic is a fallback for when it
 * isn't (e.g. classifying tool names parsed out of a raw provider request
 * payload, where there's no live tool registry to cross-reference).
 *
 * R2-3a: the fallback for genuinely UNKNOWN provenance is "builtin", never
 * "skill:<name>". `skill:<name>` is only emitted when there is POSITIVE
 * evidence of skill provenance (`sourceInfo.source === "skill"` — the value
 * `pi.getCommands()` documents for skill-backed commands, extensions.md
 * "pi.getCommands()": `source: "extension" | "prompt" | "skill"`), and
 * `mcp:<server>` is only emitted with positive MCP evidence
 * (`isMcpSourceInfo` / the `mcp*`/`__` name heuristic). Get this wrong and
 * real gateway tools with no live sourceInfo match — e.g. `slack_post`,
 * `gmail_search` — get mislabeled `skill:slack_post` and land in
 * `toolNames` instead of `mcpToolNames`; "builtin" is the least-wrong
 * default for a plain, unrecognized tool name instead.
 */
export function classifyToolSource(name: string, sourceInfo?: ToolSourceInfo | null): string {
	if (!name) return "builtin";

	if (isMcpSourceInfo(sourceInfo)) {
		const server = mcpServerFromSourceInfo(sourceInfo) || extractMcpServerName(name);
		return server ? `mcp:${server}` : "mcp:unknown"; // MCP provenance is certain; the server name just isn't parseable.
	}
	if (sourceInfo?.source === "builtin") return "builtin";
	if (BUILTIN_TOOL_NAMES.has(name)) return "builtin";
	// Positive evidence only (R2-3a) — see the doc comment above.
	if (sourceInfo?.source === "skill") return `skill:${name}`;

	if (isMcpToolName(name)) {
		const server = extractMcpServerName(name);
		return server ? `mcp:${server}` : "mcp:unknown";
	}

	return "builtin";
}

/**
 * Split a request payload's tool names into non-MCP vs MCP for
 * RequestSummary. `isMcp` defaults to the pure name heuristic but callers
 * with live tool metadata (see `classifyToolSource`) should pass a
 * metadata-aware predicate instead — this stays pure either way.
 */
export function partitionToolNames(
	names: string[],
	isMcp: (name: string) => boolean = isMcpToolName,
): { toolNames: string[]; mcpToolNames: string[] } {
	const toolNames: string[] = [];
	const mcpToolNames: string[] = [];
	for (const n of names) {
		if (isMcp(n)) mcpToolNames.push(n);
		else toolNames.push(n);
	}
	return { toolNames, mcpToolNames };
}

export interface MessageSummary {
	role: string;
	bytes: number;
	hash: string;
	preview: string;
}

export interface BlobCandidate {
	hash: string;
	bytes: number;
	text: string;
}

export interface RequestSummaryResult {
	systemPromptHash: string;
	systemPromptBytes: number;
	messageCount: number;
	newMessages: MessageSummary[];
	toolCount: number;
	toolNames: string[];
	mcpToolNames: string[];
	estTokens: number;
	/** sha256 of the serialized tool-schema blob for this turn, or "" when there are no tools (R2-6). */
	toolSchemaHash: string;
	/** Candidate blob bodies (system prompt + newly-added messages + tool schemas); caller dedupes by hash. */
	blobs: BlobCandidate[];
}

/**
 * Build the RequestSummary (Section 2 wire schema) plus candidate blob bodies
 * from a before_provider_request payload. `prevMessageCount` is the message
 * count observed on the previous turn in this session, used to compute
 * `newMessages` (messages added since then). If the payload shrank (e.g.
 * compaction), everything is treated as new.
 */
export function summarizeRequest(payload: any, prevMessageCount: number): RequestSummaryResult {
	const systemText = extractSystemPromptText(payload);
	const systemPromptBytes = Buffer.byteLength(systemText, "utf8");
	const systemPromptHash = systemText ? sha256Hex(systemText) : "";

	const messages = extractMessages(payload);
	const messageCount = messages.length;
	const messageTexts = messages.map((m) => stringifyContent(m?.content));
	const startIdx = prevMessageCount <= messageCount ? prevMessageCount : 0;
	const newMessages: MessageSummary[] = messages.slice(startIdx).map((m, i) => {
		const text = messageTexts[startIdx + i] ?? "";
		const bytes = Buffer.byteLength(text, "utf8");
		return { role: String(m?.role ?? "unknown"), bytes, hash: text ? sha256Hex(text) : "", preview: truncatePreview(text) };
	});

	const toolDefs = extractToolDefs(payload);
	const { toolNames, mcpToolNames } = partitionToolNames(toolDefs.map((t) => t.name));

	// estTokens must reflect the FULL request the provider actually serializes
	// this turn — every message so far, all tool schemas, and the system
	// prompt — not just what's new since the previous turn. Estimating from
	// only system+newMessages means the number goes DOWN as context grows
	// once a turn adds no new messages relative to the running total, which is
	// backwards (R1-2).
	const rawToolsPayload = payload?.tools ?? payload?.config?.tools ?? [];
	const toolSchemaText = Array.isArray(rawToolsPayload) && rawToolsPayload.length ? safe(() => JSON.stringify(rawToolsPayload)) ?? "" : "";
	const toolSchemaBytes = Buffer.byteLength(toolSchemaText, "utf8");
	const allMessagesBytes = messageTexts.reduce((sum, t) => sum + Buffer.byteLength(t, "utf8"), 0);
	const totalBytes = systemPromptBytes + allMessagesBytes + toolSchemaBytes;
	const estTokens = estimateTokens(totalBytes);

	// Tool schemas as a blob too (R1-3), keyed by the SAME hash carried on
	// `summary.toolSchemaHash` (R2-6) so the host can resolve the schema body
	// from the hash on the event, the same way it already does for
	// systemPromptHash/newMessages[].hash/argsHash/resultHash.
	const toolSchemaHash = toolSchemaText ? sha256Hex(toolSchemaText) : "";

	const blobs: BlobCandidate[] = [];
	if (systemText) blobs.push({ hash: systemPromptHash, bytes: systemPromptBytes, text: systemText });
	newMessages.forEach((m, i) => {
		if (!m.hash) return;
		blobs.push({ hash: m.hash, bytes: m.bytes, text: messageTexts[startIdx + i] ?? "" });
	});
	if (toolSchemaText) blobs.push({ hash: toolSchemaHash, bytes: toolSchemaBytes, text: toolSchemaText });

	return {
		systemPromptHash,
		systemPromptBytes,
		messageCount,
		newMessages,
		toolCount: toolDefs.length,
		toolNames,
		mcpToolNames,
		estTokens,
		toolSchemaHash,
		blobs,
	};
}

export interface UsageSummary {
	inputTokens: number;
	outputTokens: number;
	totalTokens: number;
}

/**
 * Extract token usage from a pi `AgentMessage` (assistant), per
 * `docs/session-format.md`'s `Usage` shape: `{input, output, cacheRead,
 * cacheWrite, totalTokens, cost}`. `after_provider_response` does NOT carry
 * usage (it fires before the response stream is consumed — see
 * `AfterProviderResponseEvent` in pi's `core/extensions/types.d.ts`: only
 * `status`/`headers`); the real numbers land later on the assistant's
 * `message_end` event (`event.message.usage`), which is what this is called
 * with. Also tolerates the older/other-provider field spellings this file
 * used to guess at, in case a future pi surface reuses them.
 */
export function extractUsage(source: any): UsageSummary | null {
	const u = source?.usage ?? source?.message?.usage ?? source?.response?.usage;
	if (!u || typeof u !== "object") return null;
	const inputTokens = Number(u.input ?? u.inputTokens ?? u.input_tokens ?? u.promptTokens ?? u.prompt_tokens ?? 0) || 0;
	const outputTokens = Number(u.output ?? u.outputTokens ?? u.output_tokens ?? u.completionTokens ?? u.completion_tokens ?? 0) || 0;
	const totalTokens = Number(u.totalTokens ?? u.total_tokens ?? inputTokens + outputTokens) || inputTokens + outputTokens;
	return { inputTokens, outputTokens, totalTokens };
}

export interface AssistantOutput {
	text: string;
	toolCalls: string[];
}

/**
 * Extract what the assistant actually GENERATED this turn (R6-1): the
 * concatenated text of every text block, plus the names of every tool/
 * function call it emitted. Called with a `message_end` AssistantMessage
 * (`docs/session-format.md`: `content: (TextContent | ThinkingContent |
 * ToolCall)[]`, a tool call block is `{type:"toolCall", name, ...}`), but
 * kept defensive against raw provider shapes too, in case one ever leaks
 * through un-normalized: Anthropic content blocks (`{type:"tool_use",
 * name}`), OpenAI Responses/Chat-Completions items (`{type:"function_call",
 * name}` or a top-level `tool_calls[].function.name`), and Gemini `parts[]`
 * entries, which have no `type` tag at all (`{text}` or `{functionCall:
 * {name}}`). Thinking blocks are deliberately excluded from `text` — only
 * the assistant's actual reply, not its private reasoning.
 */
export function extractAssistantOutput(message: any): AssistantOutput {
	const rawContent = message?.content ?? message?.parts;
	const blocks: any[] = Array.isArray(rawContent) ? rawContent : rawContent != null ? [rawContent] : [];

	const textBlocks: unknown[] = [];
	const toolCalls: string[] = [];
	const pushToolName = (name: unknown) => {
		if (typeof name === "string" && name) toolCalls.push(name);
	};

	for (const block of blocks) {
		if (block == null) continue;
		if (typeof block === "string") {
			textBlocks.push(block);
			continue;
		}
		const type = (block as any)?.type;
		if (type === "text" || (type == null && typeof block.text === "string")) {
			textBlocks.push(block);
			continue;
		}
		// pi's normalized shape (session-format.md AssistantMessage.content) and
		// Anthropic's raw content-block shape both use `name` directly.
		if (type === "toolCall" || type === "tool_use") {
			pushToolName(block.name);
			continue;
		}
		// OpenAI Responses API item shape / Chat-Completions delta shape.
		if (type === "function_call" || type === "functionCall") {
			pushToolName(block.name ?? block.function_call?.name ?? block.functionCall?.name);
			continue;
		}
		// Gemini `parts[]` entries have no `type` tag: `{functionCall:{name}}`.
		if (block.functionCall && typeof block.functionCall.name === "string") {
			pushToolName(block.functionCall.name);
			continue;
		}
		if (block.function && typeof block.function.name === "string") {
			pushToolName(block.function.name);
			continue;
		}
	}

	// OpenAI Chat-Completions-shaped message: top-level `tool_calls[]`, each
	// `{function:{name}}`.
	for (const list of [message?.tool_calls, message?.toolCalls]) {
		if (!Array.isArray(list)) continue;
		for (const tc of list) pushToolName(tc?.function?.name ?? tc?.name);
	}

	return { text: stringifyContent(textBlocks), toolCalls };
}

/** Coerce after_provider_response's normalized headers into Record<string,string>, or undefined when empty. */
export function normalizeHeaders(headers: any): Record<string, string> | undefined {
	if (!headers || typeof headers !== "object") return undefined;
	const out: Record<string, string> = {};
	for (const [k, v] of Object.entries(headers)) {
		if (typeof v === "string") out[k] = v;
		else if (Array.isArray(v)) out[k] = v.join(", ");
		else if (v != null) out[k] = String(v);
	}
	return Object.keys(out).length ? out : undefined;
}

export type TurnTrigger = "user" | "tool_result" | "compaction" | "unknown";

/**
 * Best-effort classification of why a turn started. pi does not surface an
 * explicit "why" on before_provider_request, so this leans on cheap signals
 * only: the first turn of a session is virtually always kicked off by a user
 * message; a request payload that shrank since the previous turn implies
 * compaction ran between turns; otherwise, if the last event emitted was a
 * tool_end, the agent looped straight from a tool result back into the model
 * without new user input. Anything else falls back to "unknown" rather than
 * guessing.
 */
export function inferTurnTrigger(opts: { isFirstTurn: boolean; compacted: boolean; prevEventKind: string }): TurnTrigger {
	if (opts.isFirstTurn) return "user";
	if (opts.compacted) return "compaction";
	if (opts.prevEventKind === "tool_end") return "tool_result";
	return "unknown";
}

/** "provider/id" label for a pi model descriptor (ctx.model / model_select event.model). */
export function modelLabel(model: any): string {
	if (!model) return "";
	const provider = typeof model.provider === "string" ? model.provider : "";
	const id = typeof model.id === "string" ? model.id : "";
	if (provider && id) return `${provider}/${id}`;
	return id || provider;
}

/**
 * Best-effort session id: prefer the session file path (stable, unique per
 * session), fall back to any id-shaped field, else mint a random one for the
 * lifetime of this session (ephemeral/in-memory sessions have no file).
 */
export function extractSessionId(ctx: any): string {
	const file = safe(() =>
		typeof ctx?.sessionManager?.getSessionFile === "function" ? ctx.sessionManager.getSessionFile() : undefined,
	);
	if (typeof file === "string" && file) return file;
	const direct = ctx?.sessionId ?? ctx?.session?.id;
	if (typeof direct === "string" && direct) return direct;
	return safe(() => randomUUID()) ?? String(Date.now());
}

// ─── Transport: node:http, NOT fetch ─────────────────────────────────────────
// Same rationale as memory-recall.ts's postJson: in an sbx sandbox pi installs
// a global undici proxy dispatcher (HTTP_PROXY -> the sbx proxy), and sbx's
// NO_PROXY does NOT include host.docker.internal, so fetch() to the host
// monitor would get routed through the proxy and fail. node:http ignores that
// dispatcher and goes direct. Fire-and-forget: we only care whether the send
// succeeded (2xx) or failed, never the response body.
//
// `onRequest`, when given, is invoked synchronously with the in-flight
// `ClientRequest` so the caller can abort it later (used on session_shutdown
// to cancel outstanding sends rather than leaving them to time out).
function httpPostRaw(urlStr: string, bodyText: string, timeoutMs: number, onRequest?: (req: ClientRequest) => void): Promise<void> {
	return new Promise((resolve, reject) => {
		const u = safe(() => new URL(urlStr));
		if (!u) return reject(new Error("invalid monitor url"));
		const req = (u.protocol === "https:" ? httpsRequest : httpRequest)(
			{
				hostname: u.hostname,
				port: u.port || (u.protocol === "https:" ? 443 : 80),
				path: u.pathname || "/",
				method: "POST",
				headers: { "content-type": "application/json", "content-length": Buffer.byteLength(bodyText) },
				timeout: timeoutMs,
			},
			(res) => {
				const status = res.statusCode ?? 0;
				res.on("data", () => {}); // drain, response body is unused
				res.on("error", (err) => reject(err instanceof Error ? err : new Error(String(err))));
				res.on("aborted", () => reject(new Error("monitor response aborted")));
				res.on("end", () => {
					// Only a 2xx is success — a 4xx/5xx from the monitor host still
					// means the event was NOT durably recorded, so it must be retried
					// like any other failure (R1-6), not treated as delivered.
					if (status >= 200 && status < 300) resolve();
					else reject(new Error(`monitor host returned status ${status}`));
				});
			},
		);
		onRequest?.(req);
		req.on("error", reject);
		req.on("timeout", () => req.destroy(new Error("timeout")));
		req.write(bodyText);
		req.end();
	});
}

export default function (pi: ExtensionAPI) {
	safe(() => {
		if (!MONITOR_ENABLED) return; // PI_STACK_MONITOR=0: cheap no-op, register nothing

		let sessionId = "";
		let turnSeqCounter = 0;
		let currentTurnId = "";
		let currentModelLabel = "";
		let prevMessageCount = 0;
		let lastEventKind = ""; // last emitted event's "kind", used to infer turn_start.trigger
		// status/headers observed on the most recent after_provider_response,
		// held until message_end supplies the real usage/stopReason for the
		// same round-trip (see the after_provider_response/message_end handlers).
		let pendingResponseMeta: { status: number; headers?: Record<string, string> } | null = null;
		const seenBlobHashes = new Set<string>();
		const toolStartedAt = new Map<string, number>();
		const emittedToolStart = new Set<string>();

		// Bounded queue: {path, text} where text is the exact pre-serialized wire
		// body (NDJSON line for /ingest, plain JSON for /blob). Drop-oldest on
		// overflow, exponential backoff (capped) on connect/timeout/non-2xx
		// error, and a cheap disabledUntil skip so a down host never gets
		// hammered. `onSent` (blob sends only) fires after a CONFIRMED 2xx.
		interface QueueItem {
			path: string;
			text: string;
			onSent?: () => void;
		}
		const queue: QueueItem[] = [];
		let flushing = false;
		let disabledUntil = 0;
		let backoffMs = BASE_BACKOFF_MS;
		let retryTimer: ReturnType<typeof setTimeout> | null = null;
		let inFlightReq: ClientRequest | null = null;
		// R2-1: set by session_shutdown, BEFORE the timer is cleared / the
		// in-flight request is aborted. Every entry point that could keep this
		// extension instance posting or scheduling more work (enqueue, kick,
		// scheduleRetry, and drain()'s post-failure retry branch below) checks
		// this first and no-ops once it's true. Without it, the aborted
		// in-flight request's rejection lands in drain()'s failure branch, which
		// REQUEUES the item and calls scheduleRetry() again — so on quit the
		// retry timer keeps Node alive and retries forever, and on /reload or a
		// session switch the stale (shut-down) extension instance keeps POSTing
		// old-session events alongside the freshly-created one. Scoped to this
		// closure (same as `flushing`/`queue`/`retryTimer` above), not truly
		// module-level: a real module-level flag would stay `true` forever after
		// the FIRST session ever ends, silently disabling every later session's
		// monitor for the rest of the process.
		let shuttingDown = false;

		function clearRetryTimer() {
			if (retryTimer) {
				clearTimeout(retryTimer);
				retryTimer = null;
			}
		}

		function scheduleRetry(delayMs: number) {
			if (shuttingDown) return; // R2-1: no more retries once torn down.
			clearRetryTimer();
			retryTimer = setTimeout(() => {
				retryTimer = null;
				kick();
			}, Math.max(0, delayMs));
		}

		function enqueue(path: string, text: string, onSent?: () => void) {
			if (shuttingDown) return; // R2-1: don't accept new work post-shutdown.
			queue.push({ path, text, onSent });
			if (queue.length > MAX_QUEUE) queue.shift(); // backpressure: drop OLDEST, never block the agent
			kick();
		}

		function kick() {
			if (shuttingDown || flushing) return; // R2-1: no new drains once torn down.
			flushing = true;
			void drain();
		}

		async function drain() {
			try {
				while (true) {
					if (Date.now() < disabledUntil) return; // host TUI likely not running; skip cheaply, no I/O
					const item = queue[0];
					if (!item) return;
					// Reserve the item for the duration of the send by removing it from
					// the queue array now, BEFORE awaiting: enqueue()'s overflow
					// eviction (`queue.shift()`) only ever touches what's still in the
					// array, so the in-flight item can no longer be silently dropped by
					// a concurrent enqueue while this await is outstanding (R1-6).
					queue.shift();
					let ok = false;
					try {
						await httpPostRaw(`${MONITOR_URL}${item.path}`, item.text, POST_TIMEOUT_MS, (req) => {
							inFlightReq = req;
						});
						ok = true;
					} catch {
						ok = false;
					} finally {
						inFlightReq = null;
					}
					if (ok) {
						backoffMs = BASE_BACKOFF_MS; // reset backoff on success
						safe(() => item.onSent?.());
					} else {
						// R2-1: a failure observed AFTER session_shutdown (the abort below
						// rejects this exact in-flight send) must NOT requeue the item or
						// schedule another retry — the queue is already being cleared by
						// session_shutdown and there is no later kick() that will ever see
						// the result. Let drain() end quietly instead.
						if (shuttingDown) return;
						// Put it back at the head so ordering is preserved and this exact
						// event is retried (never silently dropped on failure). If that
						// pushes the queue back over MAX_QUEUE, trim the newest tail entry
						// instead of the retried head — the item we just failed to send is
						// the one retry logic is trying to protect.
						queue.unshift(item);
						if (queue.length > MAX_QUEUE) queue.pop();
						disabledUntil = Date.now() + backoffMs;
						backoffMs = Math.min(backoffMs * 2, MAX_BACKOFF_MS);
						// A quiet agent (no new events) would otherwise never call kick()
						// again, so backoff needs its own timer to actually retry later
						// instead of just sitting disabled forever (R1-6).
						scheduleRetry(disabledUntil - Date.now());
						return;
					}
				}
			} finally {
				flushing = false;
			}
		}

		function nextSeq(): number {
			seqCounter += 1;
			return seqCounter;
		}

		function baseEnvelope(kind: string) {
			return {
				kind,
				sandboxId: SANDBOX_ID,
				sessionId,
				turnId: currentTurnId,
				seq: nextSeq(),
				ts: Date.now(),
			};
		}

		function emitEvent(obj: Record<string, unknown>) {
			lastEventKind = String(obj.kind ?? "");
			enqueue("/ingest", `${JSON.stringify(obj)}\n`);
		}

		/**
		 * Enqueue a content-addressed blob the FIRST time a hash is seen.
		 * `seenBlobHashes` is only updated once the send actually SUCCEEDS
		 * (2xx) — marking it seen at enqueue time (the old behavior) meant a
		 * blob that failed to send, or was rejected, was never retried because
		 * later callers would see it as "already sent" and skip it forever
		 * (R1-6). Returns true when a send was queued (not necessarily
		 * delivered yet), for building `changedBlobs`.
		 */
		function sendBlobIfNew(hash: string, bytes: number, text: string): boolean {
			if (!hash || seenBlobHashes.has(hash)) return false;
			enqueue("/blob", JSON.stringify({ hash, bytes, text }), () => seenBlobHashes.add(hash));
			return true;
		}

		function resetSessionState(ctx: any) {
			sessionId = extractSessionId(ctx);
			turnSeqCounter = 0;
			currentTurnId = "";
			prevMessageCount = 0;
			lastEventKind = "";
			pendingResponseMeta = null;
			seenBlobHashes.clear();
			toolStartedAt.clear();
			emittedToolStart.clear();
			currentModelLabel = modelLabel(ctx?.model);
		}

		/**
		 * Live tool provenance via `pi.getAllTools()` (extensions.md
		 * "pi.getAllTools()"; `ToolInfo.sourceInfo` per
		 * `core/extensions/types.d.ts` + `core/source-info.d.ts`). Called fresh
		 * each time rather than cached: extensions.md notes newly registered
		 * tools "appear in `pi.getAllTools()` ... immediately", so a cache could
		 * go stale mid-session. This is an in-process call with no I/O, so the
		 * repeated lookups are cheap.
		 */
		function liveToolSourceInfo(name: string): ToolSourceInfo | undefined {
			return safe(() => {
				const all = typeof pi.getAllTools === "function" ? pi.getAllTools() : undefined;
				if (!Array.isArray(all)) return undefined;
				return (all.find((t: any) => t?.name === name) as any)?.sourceInfo as ToolSourceInfo | undefined;
			});
		}

		function liveToolSourceIndex(): Map<string, ToolSourceInfo> {
			const map = new Map<string, ToolSourceInfo>();
			safe(() => {
				const all = typeof pi.getAllTools === "function" ? pi.getAllTools() : undefined;
				if (Array.isArray(all)) {
					for (const t of all as any[]) {
						if (t && typeof t.name === "string") map.set(t.name, t.sourceInfo);
					}
				}
			});
			return map;
		}

		function handleToolStart(e: any) {
			const toolId = String(e?.toolCallId ?? e?.toolId ?? "");
			if (!toolId || emittedToolStart.has(toolId)) return; // dedupe: tool_execution_start AND tool_call both fire
			emittedToolStart.add(toolId);
			toolStartedAt.set(toolId, Date.now());
			const name = String(e?.toolName ?? e?.name ?? "");
			const argsText = stringifyContent(e?.args ?? e?.input ?? {});
			const argsHash = sha256Hex(argsText);
			// Enqueue the full args body as a blob keyed by the SAME hash carried
			// on the event, so the host can resolve it later (R1-3) — previously
			// argsHash was computed and shipped on the event but the body itself
			// was never sent, so the TUI could never show it.
			sendBlobIfNew(argsHash, Buffer.byteLength(argsText, "utf8"), argsText);
			emitEvent({
				...baseEnvelope("tool_start"),
				toolId,
				source: classifyToolSource(name, liveToolSourceInfo(name)),
				name,
				argsSummary: truncatePreview(argsText, 200),
				argsHash,
			});
		}

		const on = (name: string, fn: (e: any, ctx: any) => void) =>
			safe(() => pi.on(name as any, (e: any, ctx: any) => safe(() => fn(e, ctx))));

		on("session_start", (_e: any, ctx: any) => resetSessionState(ctx));

		on("before_provider_request", (e: any, ctx: any) => {
			const isFirstTurn = turnSeqCounter === 0;
			const prevEventKind = lastEventKind;
			currentTurnId = String((turnSeqCounter += 1));
			const payload = e?.payload ?? {};
			const result = summarizeRequest(payload, prevMessageCount);
			const compacted = result.messageCount < prevMessageCount;
			prevMessageCount = result.messageCount;

			// Re-partition tool names with live tool metadata when it's available
			// (R1-4): summarizeRequest() only has the raw payload, so it falls
			// back to the pure name heuristic; here we have `pi` in scope, so
			// prefer ground truth per-name provenance from pi.getAllTools() and
			// only fall back to the heuristic per-name when metadata doesn't
			// cover a given tool (e.g. it isn't currently active).
			const toolIndex = liveToolSourceIndex();
			if (toolIndex.size) {
				const allNames = [...result.toolNames, ...result.mcpToolNames];
				const repartitioned = partitionToolNames(allNames, (n) =>
					classifyToolSource(n, toolIndex.get(n)).startsWith("mcp:"),
				);
				result.toolNames = repartitioned.toolNames;
				result.mcpToolNames = repartitioned.mcpToolNames;
			}

			const changedBlobs: string[] = [];
			for (const b of result.blobs) {
				if (sendBlobIfNew(b.hash, b.bytes, b.text)) changedBlobs.push(b.hash);
			}

			const model = (typeof payload?.model === "string" && payload.model) || modelLabel(ctx?.model) || currentModelLabel;
			currentModelLabel = model;

			// Emitted right before provider_request so the TUI's header (which reads
			// turn_start.Model) reflects this turn before the request row lands.
			emitEvent({
				...baseEnvelope("turn_start"),
				model,
				trigger: inferTurnTrigger({ isFirstTurn, compacted, prevEventKind }),
			});

			emitEvent({
				...baseEnvelope("provider_request"),
				model,
				summary: {
					systemPromptHash: result.systemPromptHash,
					systemPromptBytes: result.systemPromptBytes,
					messageCount: result.messageCount,
					newMessages: result.newMessages,
					toolCount: result.toolCount,
					toolNames: result.toolNames,
					mcpToolNames: result.mcpToolNames,
					estTokens: result.estTokens,
					toolSchemaHash: result.toolSchemaHash, // R2-6: same hash the tool-schema blob (below) is enqueued under.
				},
				changedBlobs,
			});
		});

		on("after_provider_response", (e: any) => {
			// after_provider_response (extensions.md / AfterProviderResponseEvent
			// in core/extensions/types.d.ts) carries ONLY status + headers — it
			// fires before the response stream is consumed, so there is no usage
			// or stopReason here yet. The old code emitted a provider_response
			// with a fake all-zero usage block right at this hook; that's gone
			// (R1-5). Stash status/headers and let the message_end handler below
			// emit the actual provider_response once the assistant message (with
			// real usage + stopReason) has finalized.
			pendingResponseMeta = {
				status: Number(e?.status ?? 0) || 0,
				headers: normalizeHeaders(e?.headers),
			};
		});

		on("message_end", (e: any) => {
			// MessageEndEvent.message is an AgentMessage; only the assistant
			// variant carries `usage`/`stopReason` (docs/session-format.md
			// AssistantMessage). before_provider_request -> after_provider_response
			// -> message_end fire in that order for the same round-trip
			// (docs/extensions.md hook-order diagram), so pendingResponseMeta set
			// above is still the right status/headers pairing for this message.
			const message = e?.message;
			if (!message || message.role !== "assistant") return;
			// R6-1: capture what the assistant actually GENERATED this turn — until
			// now provider_response only recorded status/usage/headers, so the
			// model's own reply was lost and only reappeared a turn later as a
			// message in the NEXT provider_request.
			const { text: assistantText, toolCalls } = extractAssistantOutput(message);
			const textBytes = Buffer.byteLength(assistantText, "utf8");
			const textHash = sha256Hex(assistantText);
			// Enqueue the full reply as a first-seen blob keyed by textHash (same
			// pattern as tool argsHash/resultHash) so the TUI can resolve it later.
			sendBlobIfNew(textHash, textBytes, assistantText);
			emitEvent({
				...baseEnvelope("provider_response"),
				status: pendingResponseMeta?.status ?? 0,
				stopReason: String(message?.stopReason ?? ""),
				usage: extractUsage(message),
				headers: pendingResponseMeta?.headers,
				textBytes,
				textPreview: truncatePreview(assistantText, 200),
				textHash,
				toolCalls,
			});
			pendingResponseMeta = null;
		});

		on("tool_execution_start", (e: any) => handleToolStart(e));
		on("tool_call", (e: any) => handleToolStart(e));

		on("tool_execution_end", (e: any) => {
			const toolId = String(e?.toolCallId ?? e?.toolId ?? "");
			const startedAt = toolStartedAt.get(toolId);
			const durationMs = typeof startedAt === "number" ? Math.max(0, Date.now() - startedAt) : 0;
			toolStartedAt.delete(toolId);
			emittedToolStart.delete(toolId);
			const resultText = stringifyContent(e?.result ?? e?.output ?? null);
			const resultHash = sha256Hex(resultText);
			// Same fix as tool_start: ship the full result body as a blob keyed by
			// the hash already on the event (R1-3).
			sendBlobIfNew(resultHash, Buffer.byteLength(resultText, "utf8"), resultText);
			emitEvent({
				...baseEnvelope("tool_end"),
				toolId,
				ok: !e?.isError,
				resultBytes: Buffer.byteLength(resultText, "utf8"),
				resultSummary: truncatePreview(resultText, 200),
				resultHash,
				durationMs,
			});
		});

		on("model_select", (e: any) => {
			const prev = modelLabel(e?.previousModel);
			const next = modelLabel(e?.model);
			currentModelLabel = next || currentModelLabel;
			emitEvent({
				...baseEnvelope("context_event"),
				ctxKind: "model_change",
				detail: `${prev || "none"} -> ${next || "unknown"} (${String(e?.source ?? "unknown")})`,
			});
		});

		// session_compact / thinking_level_select: documented control-plane
		// hooks (docs/extensions.md "session_before_compact / session_compact",
		// "thinking_level_select") that weren't wired before (R1-5).
		on("session_compact", (e: any) => {
			emitEvent({
				...baseEnvelope("context_event"),
				ctxKind: "compaction",
				detail: `reason=${String(e?.reason ?? "unknown")} willRetry=${Boolean(e?.willRetry)}`,
			});
		});

		on("thinking_level_select", (e: any) => {
			emitEvent({
				...baseEnvelope("context_event"),
				ctxKind: "thinking_level",
				detail: `${String(e?.previousLevel ?? "unknown")} -> ${String(e?.level ?? "unknown")}`,
			});
		});

		// R2-4: streaming token deltas are intentionally NOT emitted in the MVP
		// (no stream/token event kind exists on the wire schema).


		on("session_shutdown", () => {
			// R2-1: set shuttingDown FIRST, before clearing the timer or aborting
			// the in-flight request — enqueue/kick/scheduleRetry and drain()'s
			// failure branch all check it and no-op, so the abort below can't
			// resurrect the retry timer or requeue the aborted item. Then clear the
			// pending backoff retry timer, abort any in-flight POST rather than
			// attempting one more flush, and drop anything still queued — the
			// extension runtime is being torn down, so there's no later kick()
			// that would ever see the result (R1-6), and nothing queued now will
			// ever be sent by this instance.
			shuttingDown = true;
			clearRetryTimer();
			if (inFlightReq) safe(() => inFlightReq?.destroy(new Error("session_shutdown")));
			queue.length = 0;
		});
	});
}
