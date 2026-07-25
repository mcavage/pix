// pix — subagents: delegate work to isolated child pi processes.
//
// A first-party replacement for the off-the-shelf subagent extensions, which
// all freeze in this stack. Two root causes, two fixes (see
// docs/design/subagents-extension.md and docs/upstream/pi-subagents-hang-pi-0.80.md):
//
//   1) STABILITY PILLAR #1 — curated child extension set. A child pi that loads
//      the full extension set re-runs ollama-bridge / the memory extensions,
//      which `await server.listen(<port>)`. The parent already holds those
//      ports, so the child's listen throws EADDRINUSE, the error is swallowed,
//      the factory promise never resolves, and the child DEADLOCKS at startup
//      before running a turn. We spawn every child with
//      `--no-extensions -e <this file>`: no auto-discovered extensions (nothing
//      binds a port), but this extension is re-added explicitly so subagent
//      TREES still work.
//
//   2) STABILITY PILLAR #2 — a real watchdog. pi has no client read timeout, so
//      a dead SSE stream spins a child forever. Every child gets an inactivity
//      timeout (no stdout for N seconds → kill) and a hard wall-clock cap. On
//      either, SIGTERM then SIGKILL, and a clear error result flows back to the
//      parent model. A subagent can be slow; it can never hang the parent.
//
// Modes: single {agent, task} · parallel {tasks:[...]} · chain {chain:[...]}.
// Trees: children carry this extension, depth-capped by PI_SUBAGENT_MAX_DEPTH.
// Self-audit: `/subagents doctor` spawns a real canary end-to-end.
//
// Defensive by construction: an extension that throws at load breaks pi
// startup, so every pi-API touch and every parse is guarded.

import { spawn } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { StringEnum } from "@earendil-works/pi-ai";
import {
	CONFIG_DIR_NAME,
	type ExtensionAPI,
	getAgentDir,
	getMarkdownTheme,
	getPackageDir,
	parseFrontmatter,
} from "@earendil-works/pi-coding-agent";
import { Container, Markdown, Spacer, Text } from "@earendil-works/pi-tui";
import { Type } from "typebox";

// ─── Config (all env-tunable) ────────────────────────────────────────────────
const num = (name: string, dflt: number): number => {
	const v = Number(process.env[name]);
	return Number.isFinite(v) && v > 0 ? v : dflt;
};
// Defaults are generous on purpose: frontier models (e.g. Claude Fable 5) can
// THINK for many minutes emitting nothing the parent can see (raw thinking
// tokens are never streamed), and a real engineering task legitimately runs long.
// The old 120s/600s caps killed productive children mid-work. The idle watchdog
// still catches a genuinely dead SSE stream within a few minutes; the wall cap is
// a cost/livelock ceiling, not a productivity killer. Per-agent frontmatter
// (idle_ms / wall_ms) overrides these for a known-slow agent WITHOUT needing the
// env (which is read once at startup and often unwritable).
const IDLE_MS = num("PI_SUBAGENT_IDLE_MS", 300_000); // no output for this long → dead (5m)
const WALL_MS = num("PI_SUBAGENT_TIMEOUT_MS", 3_600_000); // hard per-child cap (1h)
// Node's setTimeout treats a delay above 2^31-1 ms (~24.8 days) as 1 ms — which
// would fire the watchdog INSTANTLY. Clamp every resolved budget to that range
// (floored to an int) so a fat-fingered env/frontmatter value (e.g.
// wall_ms: 3000000000) degrades to a long-but-valid cap, never an instant kill.
const MAX_TIMER_MS = 2_147_483_647;
const clampDelay = (n: number): number =>
	Math.min(Math.max(Math.floor(n), 1), MAX_TIMER_MS);
// After the child emits agent_settled the turn's output is already captured, so a
// process that lingers (e.g. a web_search includeContent background fetch holding
// the event loop open) gets a fast, CLEAN teardown instead of waiting out the
// idle watchdog and being mislabeled a timeout.
const DRAIN_MS = num("PI_SUBAGENT_DRAIN_MS", 3_000);

// Concurrency is the throughput lever for the overlord's parallel waves. It is NOT
// "set it to 50": every child is a full `pi` process holding a live model stream, so
// the real ceilings are (1) provider rate limits — too many concurrent frontier
// streams return 429s that cascade and make the batch SLOWER, not faster; (2) host
// RAM/CPU — each child is hundreds of MB; and (3) fan-in — the parent must read and
// merge every result, so a 50-wide wave just blows the overlord's context. 8 running
// / 16 queued-per-call is the sweet spot for a normal box; raise via the env when a
// wave is genuinely independent and cheap (e.g. a Flash-Lite fanout).
const MAX_CONCURRENCY = num("PI_SUBAGENT_MAX_CONCURRENCY", 8);
const MAX_PARALLEL = num("PI_SUBAGENT_MAX_PARALLEL", 16);
// MAX_DEPTH does NOT use num(): num() rejects 0, but PI_SUBAGENT_MAX_DEPTH=0 is
// the documented host-mode backup guard ("refuse at depth 0/0") — an explicit
// zero must be honored, not silently replaced by the default 3.
const MAX_DEPTH = (() => {
	const v = Number(process.env.PI_SUBAGENT_MAX_DEPTH);
	return Number.isFinite(v) && v >= 0 ? Math.floor(v) : 3;
})();
// Host mode (docs/design/host-mode.md) sets PI_SUBAGENT_DISABLED=1 as a
// first-class, explicit refusal — belt-and-suspenders with PI_SUBAGENT_MAX_DEPTH=0
// (also set there). Children spawn as `pi --no-extensions -e <this file>`, which
// bypasses extensions/host-guard.ts entirely and inherits the full env
// (credentials included) with no sandbox underneath either. Checked FIRST in the
// tool (host-mode-specific message before the generic depth-limit text) AND
// enforced centrally in runSingle(), so no other entry point (e.g. `/subagents
// doctor`'s canary) can ever spawn a child while disabled. Strict `=== "1"`:
// Boolean(env) would treat "0"/"false" as disabled too.
const SUBAGENTS_DISABLED = process.env.PI_SUBAGENT_DISABLED === "1";
const SUBAGENTS_DISABLED_MSG =
	"Subagents are disabled in host mode (PI_SUBAGENT_DISABLED=1). Children spawn as `pi --no-extensions -e <subagents.ts>`, which bypasses the host-guard tool_call extension and inherits the full environment (credentials included) with no sandbox underneath. Do the work directly in this session instead of delegating. See docs/design/host-mode.md.";
const CURRENT_DEPTH = Math.max(
	0,
	Math.floor(Number(process.env.PI_SUBAGENT_DEPTH) || 0),
);
const PER_TASK_OUTPUT_CAP = 50 * 1024;
const COLLAPSED_ITEMS = 10;
const VALID_THINKING = new Set([
	"off",
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
]);

// ─── Model routing (intent → model, compiled by the host) ────────────────────
// The host (`pix-host route compile`) resolves each INTENT to a concrete
// model against the measured cost/latency/accuracy scorecard and writes
// routing.json next to capabilities.json. We read it ONCE, offline — the sandbox
// never calls the host at spawn time (that path can hang a subagent). An agent
// declares `intent:` instead of hard-pinning `model:`; an explicit `model:`
// still wins (back-compat). A missing/unknown intent degrades to "inherit the
// parent model", never an error. See docs/design/routing.md.
interface CompiledRoute {
	model?: string;
	constraints_met?: boolean;
	reason?: string;
}
interface CompiledRouting {
	version?: number;
	routes?: Record<string, CompiledRoute>;
}
// ROUTING_SCHEMA is the routing.json version this build understands. A file
// written by a NEWER host is rejected (we don't guess at a shape we don't know)
// rather than silently mis-routing.
const ROUTING_SCHEMA = 1;
function loadRouting(): CompiledRouting | null {
	try {
		const p = path.join(getAgentDir(), "routing.json");
		if (!fs.existsSync(p)) return null;
		const parsed = JSON.parse(fs.readFileSync(p, "utf-8"));
		if (!parsed || typeof parsed !== "object" || !parsed.routes) return null;
		// Require an EXACT known schema version. A missing, non-numeric, or newer
		// version means we can't trust the shape — ignore it (agents inherit) rather
		// than guess.
		if (parsed.version !== ROUTING_SCHEMA) {
			try {
				process.stderr.write(
					`[subagents] routing.json version ${JSON.stringify(parsed.version)} != ${ROUTING_SCHEMA}; ignoring (agents inherit parent model).\n`,
				);
			} catch {
				/* best-effort */
			}
			return null;
		}
		return parsed as CompiledRouting;
	} catch {
		/* best-effort; a missing/bad file just means "inherit parent model" */
	}
	return null;
}
const ROUTING = loadRouting();
// resolveIntentModel maps an intent name to its compiled model id. Returns ""
// (caller inherits the parent model) when the intent is unknown, routing.json is
// absent, or the compiled model id is not fully qualified (provider/id) — a bare
// id can resolve to a keyless provider and hang, so we never forward one.
function isQualifiedModelId(id: string): boolean {
	const i = id.indexOf("/");
	return i > 0 && i < id.length - 1;
}
function resolveIntentModel(intent: string): string {
	const raw = ROUTING?.routes?.[intent]?.model;
	// Guard the type explicitly: a malformed routing.json could carry a non-string
	// (e.g. {model: 42}); calling .trim() on that would throw.
	if (typeof raw !== "string") return "";
	const m = raw.trim();
	return isQualifiedModelId(m) ? m : "";
}

// ─── Agent discovery (pix convention: filename = name) ──────────────────
type AgentScope = "user" | "project" | "both";
interface AgentConfig {
	name: string;
	description: string;
	tools?: string[];
	model?: string;
	// Declared routing intent (frontmatter `intent:`). Resolved to `model` via
	// routing.json unless an explicit `model:` overrides it. Kept for display.
	intent?: string;
	thinking?: string;
	maxTurns?: number;
	// Per-agent watchdog overrides (frontmatter idle_ms / wall_ms, milliseconds).
	// A slow-by-design agent (a frontier deep worker with high thinking) declares
	// its own budget here so it isn't killed by the global defaults. Unset =
	// inherit IDLE_MS / WALL_MS.
	idleMs?: number;
	wallMs?: number;
	// Web access defaults ON, but a hermetic agent (e.g. a reviewer/auditor that
	// sees sensitive diffs) can set `web: false` to keep repo context off the wire.
	web?: boolean;
	systemPrompt: string;
	source: "user" | "project";
	filePath: string;
	warnings: string[];
}

function loadAgentsFromDir(
	dir: string,
	source: "user" | "project",
): AgentConfig[] {
	const out: AgentConfig[] = [];
	let entries: fs.Dirent[];
	try {
		entries = fs.readdirSync(dir, { withFileTypes: true });
	} catch {
		return out;
	}
	for (const entry of entries) {
		if (!entry.name.endsWith(".md")) continue;
		if (!entry.isFile() && !entry.isSymbolicLink()) continue;
		const filePath = path.join(dir, entry.name);
		let content: string;
		try {
			content = fs.readFileSync(filePath, "utf-8");
		} catch {
			continue;
		}
		let frontmatter: Record<string, string> = {};
		let body = content;
		try {
			const parsed = parseFrontmatter<Record<string, string>>(content);
			frontmatter = parsed.frontmatter ?? {};
			body = parsed.body ?? content;
		} catch {
			/* treat as bodyonly */
		}
		// Name = frontmatter.name if present, else filename (pix + skills style).
		const name = (frontmatter.name || path.basename(entry.name, ".md")).trim();
		if (!name) continue;
		const description = (frontmatter.description || "").trim();
		const tools = frontmatter.tools
			?.split(",")
			.map((t) => t.trim())
			.filter(Boolean);
		const warnings: string[] = [];
		const explicitModel = frontmatter.model?.trim() || undefined;
		if (explicitModel && !explicitModel.includes("/")) {
			warnings.push(
				`model "${explicitModel}" is not fully qualified (provider/id); a bare name can resolve to a keyless provider and hang. Fix the agent file.`,
			);
		}
		const intent = frontmatter.intent?.trim() || undefined;
		// Model resolution order: explicit `model:` wins (back-compat); else
		// `intent:` resolves through routing.json; else undefined = inherit the
		// parent model at spawn.
		let model = explicitModel;
		if (!model && intent) {
			const routed = resolveIntentModel(intent);
			if (routed) {
				model = routed;
			} else {
				warnings.push(
					`intent "${intent}" not found in routing.json — inheriting the parent model. Run \`pix route compile\` on the host (then \`make load\`).`,
				);
			}
		}
		let thinking = frontmatter.thinking?.trim().toLowerCase() || undefined;
		if (thinking && !VALID_THINKING.has(thinking)) {
			warnings.push(`thinking "${thinking}" is not a valid level; ignoring.`);
			thinking = undefined;
		}
		const maxTurns = frontmatter.max_turns
			? Number(frontmatter.max_turns)
			: undefined;
		// Per-agent watchdog budgets (milliseconds). wall_ms accepts timeout_ms as an
		// alias. Only positive finite values are honored; anything else inherits the
		// global default.
		const posNum = (v: unknown): number | undefined => {
			const n = Number(v);
			return Number.isFinite(n) && n > 0 ? n : undefined;
		};
		const idleMs = posNum(frontmatter.idle_ms);
		const wallMs = posNum(frontmatter.wall_ms ?? frontmatter.timeout_ms);
		// `web: false` opts an agent out of subagent web access (default on).
		const webRaw = String(frontmatter.web ?? "")
			.trim()
			.toLowerCase();
		const web = webRaw === "false" || webRaw === "no" ? false : undefined;
		out.push({
			name,
			description,
			tools: tools && tools.length > 0 ? tools : undefined,
			model,
			intent,
			thinking,
			web,
			maxTurns: Number.isFinite(maxTurns as number)
				? (maxTurns as number)
				: undefined,
			idleMs,
			wallMs,
			systemPrompt: body,
			source,
			filePath,
			warnings,
		});
	}
	return out;
}

function findProjectAgentsDir(cwd: string): string | null {
	let dir = cwd;
	while (true) {
		const candidate = path.join(dir, CONFIG_DIR_NAME, "agents");
		try {
			if (fs.statSync(candidate).isDirectory()) return candidate;
		} catch {
			/* keep walking */
		}
		const parent = path.dirname(dir);
		if (parent === dir) return null;
		dir = parent;
	}
}

function discoverAgents(
	cwd: string,
	scope: AgentScope,
): { agents: AgentConfig[]; projectDir: string | null } {
	const userDir = path.join(getAgentDir(), "agents");
	const projectDir = findProjectAgentsDir(cwd);
	const user = scope === "project" ? [] : loadAgentsFromDir(userDir, "user");
	const project =
		scope === "user" || !projectDir
			? []
			: loadAgentsFromDir(projectDir, "project");
	const map = new Map<string, AgentConfig>();
	if (scope !== "project") for (const a of user) map.set(a.name, a);
	if (scope !== "user") for (const a of project) map.set(a.name, a); // project overrides on "both"
	return { agents: Array.from(map.values()), projectDir };
}

// ─── Child pi invocation (robust resolution) ─────────────────────────────────
let SELF_PATH: string | null = null;
try {
	const p = fileURLToPath(import.meta.url);
	if (fs.existsSync(p)) SELF_PATH = p;
} catch {
	/* trees disabled if we can't find ourselves; stability unaffected */
}

// Extensions re-added to the child on top of --no-extensions. The blanket
// --no-extensions exists because a FULLY loaded child re-binds fixed ports
// (ollama-bridge) or the memory service and deadlocks. pi-web-access is
// different: its interactive browser "curator" is the only server, it binds an
// ephemeral (:0) port, and it starts ONLY when a UI is present. A headless `-p`
// child has ctx.hasUI === false, so pi-web-access's resolveWorkflow() collapses
// the workflow to "none" and the curator/browser never starts. (Note: a config
// default of workflow "auto-summary" still runs a summary MODEL call in a child,
// but binds nothing — no port, no browser, no hang.) The one residual is a
// web_search(includeContent:true) background fetch: in a --no-session child
// sessionActive stays false, so its completion callback (incl. the sendMessage
// triggerTurn re-entry) is a no-op, and the wall/idle watchdog backstops any
// slow-exit. So web_search / fetch_content / get_search_content are safe to give
// subagents that want to research. We resolve the entry from the installed
// package's own `pi.extensions` manifest so a version/path change can't silently
// break it, and keep every resolved path inside the package dir.
const WEB_TOOLS = ["web_search", "fetch_content", "get_search_content"];
function resolveWebAccessExtensions(): string[] {
	const roots: string[] = [path.join(getAgentDir(), "npm", "node_modules")];
	try {
		roots.push(path.join(getPackageDir(), ".."));
	} catch {
		/* getPackageDir may throw in odd installs; the agent-dir root covers us */
	}
	for (const root of roots) {
		const pkgDir = path.join(root, "pi-web-access");
		try {
			const manifest = path.join(pkgDir, "package.json");
			if (!fs.existsSync(manifest)) continue;
			const pkg = JSON.parse(fs.readFileSync(manifest, "utf-8"));
			const entries: unknown = pkg?.pi?.extensions;
			const list = Array.isArray(entries) ? (entries as string[]) : [];
			const base = fs.realpathSync(pkgDir);
			const resolved = list
				.filter((e): e is string => typeof e === "string")
				.map((e) => path.resolve(pkgDir, e))
				.filter((p) => fs.existsSync(p))
				// Containment: never load a path the manifest points OUTSIDE its own
				// package dir (defense-in-depth; a malicious package already has RCE).
				.filter((p) => {
					try {
						return fs.realpathSync(p).startsWith(base + path.sep);
					} catch {
						return false;
					}
				});
			if (resolved.length) return resolved;
		} catch {
			/* try the next root; web access is best-effort, never fatal */
		}
	}
	return [];
}
const WEB_ACCESS_EXTENSIONS = resolveWebAccessExtensions();

function resolvePiCommand(): { command: string; baseArgs: string[] } {
	// Explicit override (testing, or a non-standard pi install). Space-separated:
	// first token is the command, the rest are leading args.
	const override = process.env.PI_SUBAGENT_PI_COMMAND;
	if (override?.trim()) {
		const parts = override.trim().split(/\s+/);
		return { command: parts[0], baseArgs: parts.slice(1) };
	}
	// Prefer the installed package's cli.js run under this same node/bun.
	try {
		const cli = path.join(getPackageDir(), "dist", "cli.js");
		if (fs.existsSync(cli))
			return { command: process.execPath, baseArgs: [cli] };
	} catch {
		/* fall through */
	}
	const argv1 = process.argv[1];
	if (argv1 && !argv1.startsWith("/$bunfs/root/") && fs.existsSync(argv1)) {
		return { command: process.execPath, baseArgs: [argv1] };
	}
	const execName = path.basename(process.execPath).toLowerCase();
	if (!/^(node|bun)(\.exe)?$/.test(execName))
		return { command: process.execPath, baseArgs: [] };
	return { command: "pi", baseArgs: [] };
}

// ─── Result types ────────────────────────────────────────────────────────────
interface UsageStats {
	input: number;
	output: number;
	cacheRead: number;
	cacheWrite: number;
	cost: number;
	contextTokens: number;
	turns: number;
}
const zeroUsage = (): UsageStats => ({
	input: 0,
	output: 0,
	cacheRead: 0,
	cacheWrite: 0,
	cost: 0,
	contextTokens: 0,
	turns: 0,
});

interface SingleResult {
	agent: string;
	agentSource: "user" | "project" | "unknown";
	task: string;
	exitCode: number; // -1 = still running
	messages: any[];
	stderr: string;
	usage: UsageStats;
	model?: string;
	stopReason?: string;
	errorMessage?: string;
	timedOut?: "idle" | "wall" | null;
	// Effective watchdog budgets this run used (agent override or global default),
	// so timeout messages report the real numbers, not the module constants.
	idleMs?: number;
	wallMs?: number;
	step?: number;
}

interface SubagentDetails {
	mode: "single" | "parallel" | "chain";
	scope: AgentScope;
	projectDir: string | null;
	results: SingleResult[];
}

function finalText(messages: any[]): string {
	for (let i = messages.length - 1; i >= 0; i--) {
		const m = messages[i];
		if (m?.role === "assistant" && Array.isArray(m.content)) {
			for (const part of m.content)
				if (part?.type === "text") return part.text ?? "";
		}
	}
	return "";
}
function isFailed(r: SingleResult): boolean {
	return (
		r.exitCode !== 0 ||
		r.stopReason === "error" ||
		r.stopReason === "aborted" ||
		Boolean(r.timedOut)
	);
}
function resultOutput(r: SingleResult): string {
	if (isFailed(r)) {
		if (r.timedOut === "idle")
			return `Timed out: no output for ${Math.round((r.idleMs ?? IDLE_MS) / 1000)}s (killed). Partial output:\n${finalText(r.messages) || r.stderr || "(none)"}`;
		if (r.timedOut === "wall")
			return `Timed out: exceeded ${Math.round((r.wallMs ?? WALL_MS) / 1000)}s wall-clock (killed). Partial output:\n${finalText(r.messages) || r.stderr || "(none)"}`;
		return r.errorMessage || r.stderr || finalText(r.messages) || "(no output)";
	}
	return finalText(r.messages) || "(no output)";
}
function capOutput(s: string): string {
	if (Buffer.byteLength(s, "utf8") <= PER_TASK_OUTPUT_CAP) return s;
	let t = s.slice(0, PER_TASK_OUTPUT_CAP);
	while (Buffer.byteLength(t, "utf8") > PER_TASK_OUTPUT_CAP) t = t.slice(0, -1);
	return `${t}\n\n[Output truncated; full result in tool details.]`;
}

// ─── Formatting helpers (TUI) ────────────────────────────────────────────────
function fmtTokens(n: number): string {
	if (n < 1000) return String(n);
	if (n < 1_000_000) return `${Math.round(n / 1000)}k`;
	return `${(n / 1_000_000).toFixed(1)}M`;
}
function fmtUsage(u: UsageStats, model?: string): string {
	const p: string[] = [];
	if (u.turns) p.push(`${u.turns} turn${u.turns > 1 ? "s" : ""}`);
	if (u.input) p.push(`↑${fmtTokens(u.input)}`);
	if (u.output) p.push(`↓${fmtTokens(u.output)}`);
	if (u.cacheRead) p.push(`R${fmtTokens(u.cacheRead)}`);
	if (u.cost) p.push(`$${u.cost.toFixed(4)}`);
	if (u.contextTokens > 0) p.push(`ctx:${fmtTokens(u.contextTokens)}`);
	if (model) p.push(model);
	return p.join(" ");
}
type DisplayItem =
	| { type: "text"; text: string }
	| { type: "toolCall"; name: string; args: any };
function displayItems(messages: any[]): DisplayItem[] {
	const items: DisplayItem[] = [];
	for (const m of messages) {
		if (m?.role === "assistant" && Array.isArray(m.content)) {
			for (const part of m.content) {
				if (part?.type === "text")
					items.push({ type: "text", text: part.text ?? "" });
				else if (part?.type === "toolCall")
					items.push({
						type: "toolCall",
						name: part.name,
						args: part.arguments,
					});
			}
		}
	}
	return items;
}
function fmtToolCall(
	name: string,
	args: Record<string, any>,
	fg: (c: any, t: string) => string,
): string {
	const short = (p: string) => {
		const home = os.homedir();
		return p?.startsWith?.(home) ? `~${p.slice(home.length)}` : p;
	};
	switch (name) {
		case "bash": {
			const c = String(args?.command ?? "...");
			return (
				fg("muted", "$ ") +
				fg("toolOutput", c.length > 60 ? `${c.slice(0, 60)}...` : c)
			);
		}
		case "read":
			return (
				fg("muted", "read ") +
				fg("accent", short(String(args?.path ?? args?.file_path ?? "...")))
			);
		case "write":
			return (
				fg("muted", "write ") +
				fg("accent", short(String(args?.path ?? args?.file_path ?? "...")))
			);
		case "edit":
			return (
				fg("muted", "edit ") +
				fg("accent", short(String(args?.path ?? args?.file_path ?? "...")))
			);
		case "grep":
			return (
				fg("muted", "grep ") + fg("accent", `/${String(args?.pattern ?? "")}/`)
			);
		case "subagent":
			return (
				fg("muted", "subagent ") +
				fg(
					"accent",
					String(
						args?.agent ??
							(args?.tasks ? "parallel" : args?.chain ? "chain" : "..."),
					),
				)
			);
		default: {
			const s = JSON.stringify(args ?? {});
			return (
				fg("accent", name) +
				fg("dim", ` ${s.length > 50 ? `${s.slice(0, 50)}...` : s}`)
			);
		}
	}
}

async function mapWithLimit<I, O>(
	items: I[],
	limit: number,
	fn: (item: I, i: number) => Promise<O>,
): Promise<O[]> {
	if (items.length === 0) return [];
	const n = Math.max(1, Math.min(limit, items.length));
	const out: O[] = Array.from({ length: items.length });
	let next = 0;
	const worker = async (): Promise<void> => {
		while (true) {
			const cur = next++;
			if (cur >= items.length) return;
			out[cur] = await fn(items[cur], cur);
		}
	};
	const workers: Promise<void>[] = [];
	for (let w = 0; w < n; w++) workers.push(worker());
	await Promise.all(workers);
	return out;
}

async function writePrompt(
	name: string,
	prompt: string,
): Promise<{ dir: string; file: string }> {
	const dir = await fs.promises.mkdtemp(path.join(os.tmpdir(), "pi-subagent-"));
	const file = path.join(dir, `prompt-${name.replace(/[^\w.-]+/g, "_")}.md`);
	await fs.promises.writeFile(file, prompt, { encoding: "utf-8", mode: 0o600 });
	return { dir, file };
}

type OnUpdate = (partial: { content: any[]; details: SubagentDetails }) => void;

// ─── Pinned live tracker ─────────────────────────────────────────────────────
// A stable, always-visible roster of every subagent this (interactive) process
// is running, pinned above the editor via ctx.ui.setWidget — the same mechanism
// pi's todo-list / plan-mode pin uses. It answers "what is running right now, how
// far along, did it succeed" without scrolling. The rich inline renderResult
// block stays as the permanent record; this is the live cockpit.
//
// Discipline (peer-reviewed): top-level runs only (a headless tree child has no
// UI, so it renders nothing and never allocates a timer); a SINGLE 1s ticker
// (not an 8Hz spinner — repaint churn, see status.ts); ticker gated on the
// RUNNING count so sticky failures don't spin it forever; successes auto-clear
// after a TTL, failures/timeouts/aborts stay pinned until the next batch or
// shutdown; every pi-API touch guarded so nothing throws at load or wedges
// /reload.
const TRACKER_WIDGET_ID = "subagent-tracker";
const FINISHED_TTL_MS = num("PI_SUBAGENT_PIN_TTL_MS", 6_000);
const MAX_VISIBLE_ROWS = 10; // string[] widgets are capped ~10 lines by pi
const SPINNER_FRAMES = ["⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"];

type RunStatus =
	| "queued"
	| "running"
	| "done"
	| "failed"
	| "timeout"
	| "aborted";

interface RunEntry {
	id: string;
	agent: string;
	model?: string;
	mode: "single" | "parallel" | "chain";
	status: RunStatus;
	task: string;
	step?: number;
	startedAt: number;
	endedAt: number | null;
	turns: number;
	tokensIn: number;
	tokensOut: number;
	cost: number;
	lastTool: string | null;
	error: string | null;
}

const tracker = {
	runs: new Map<string, RunEntry>(), // insertion order = display order
	seq: 0,
	ui: null as any,
	ticker: null as ReturnType<typeof setInterval> | null,
	frame: 0,
	lastRendered: null as string | null,
	timers: new Map<string, ReturnType<typeof setTimeout>>(), // TTL removals
};

function captureUi(ctx: any): void {
	try {
		if (ctx?.hasUI && ctx.ui) {
			if (tracker.ui !== ctx.ui) tracker.lastRendered = null; // new UI → repaint
			tracker.ui = ctx.ui;
		}
	} catch {
		/* best-effort; must not break the agent */
	}
}

function runningCount(): number {
	let n = 0;
	for (const r of tracker.runs.values())
		if (r.status === "queued" || r.status === "running") n++;
	return n;
}

// Before a fresh batch registers, sweep away any lingering finished rows (incl.
// sticky failures) so a new run starts on a clean pin — but only when nothing is
// still running, so we never wipe a live sibling.
function clearFinishedIfIdle(): void {
	try {
		if (runningCount() > 0) return;
		for (const [id, r] of tracker.runs)
			if (r.status !== "running" && r.status !== "queued") {
				tracker.runs.delete(id);
				const t = tracker.timers.get(id);
				if (t) {
					clearTimeout(t);
					tracker.timers.delete(id);
				}
			}
	} catch {
		/* best-effort */
	}
}

function registerRun(opts: {
	agent: string;
	model?: string;
	mode: "single" | "parallel" | "chain";
	step?: number;
	task: string;
	status?: RunStatus;
}): string {
	const id = `r${tracker.seq++}`;
	try {
		clearFinishedIfIdle();
		tracker.runs.set(id, {
			id,
			agent: opts.agent,
			model: opts.model,
			mode: opts.mode,
			status: opts.status ?? "running",
			task: opts.task,
			step: opts.step,
			startedAt: Date.now(),
			endedAt: null,
			turns: 0,
			tokensIn: 0,
			tokensOut: 0,
			cost: 0,
			lastTool: null,
			error: null,
		});
		ensureTicker();
		renderWidget();
	} catch {
		/* best-effort */
	}
	return id;
}

function markRunning(id: string): void {
	try {
		const r = tracker.runs.get(id);
		if (r && r.status === "queued") {
			r.status = "running";
			r.startedAt = Date.now(); // clock the wait separately from the work
			ensureTicker();
			renderWidget();
		}
	} catch {
		/* best-effort */
	}
}

function lastToolName(messages: any[]): string | null {
	try {
		for (let i = messages.length - 1; i >= 0; i--) {
			const m = messages[i];
			if (m?.role === "assistant" && Array.isArray(m.content)) {
				for (let j = m.content.length - 1; j >= 0; j--) {
					const p = m.content[j];
					if (p?.type === "toolCall" || p?.type === "tool_call")
						return p.name || p.toolName || null;
				}
			}
		}
	} catch {
		/* best-effort */
	}
	return null;
}

function updateRun(id: string, r: SingleResult): void {
	try {
		const e = tracker.runs.get(id);
		if (!e) return;
		e.turns = r.usage.turns;
		e.tokensIn = r.usage.input;
		e.tokensOut = r.usage.output;
		e.cost = r.usage.cost;
		if (r.model) e.model = r.model;
		const lt = lastToolName(r.messages);
		if (lt) e.lastTool = lt;
		renderWidget();
	} catch {
		/* best-effort */
	}
}

// Best-effort status flip the moment a watchdog fires, so the pin shows
// "timeout"/"aborted" without waiting for the child promise to resolve.
function setRunStatus(id: string, status: RunStatus, error?: string): void {
	try {
		const e = tracker.runs.get(id);
		if (e && (e.status === "running" || e.status === "queued")) {
			e.status = status;
			if (error) e.error = error;
			renderWidget();
		}
	} catch {
		/* best-effort */
	}
}

// Remove a run outright — for pre-registered "queued" rows that will never run
// (an unknown agent, or chain steps abandoned after an early failure). Without
// this they keep runningCount() > 0 and leak the ticker.
function dropRun(id: string): void {
	try {
		if (!tracker.runs.has(id)) return;
		tracker.runs.delete(id);
		const t = tracker.timers.get(id);
		if (t) {
			clearTimeout(t);
			tracker.timers.delete(id);
		}
		renderWidget();
		maybeStopTicker();
	} catch {
		/* best-effort */
	}
}

function finalizeRun(id: string, r: SingleResult): void {
	try {
		const e = tracker.runs.get(id);
		if (!e || e.endedAt) return; // idempotent: finalize once
		e.endedAt = Date.now();
		e.turns = r.usage.turns;
		e.tokensIn = r.usage.input;
		e.tokensOut = r.usage.output;
		e.cost = r.usage.cost;
		if (r.model) e.model = r.model;
		if (!isFailed(r)) {
			e.status = "done";
		} else if (r.timedOut) {
			e.status = "timeout";
			e.error = r.timedOut === "idle" ? "idle-timeout" : "wall-timeout";
		} else if (r.stopReason === "aborted") {
			e.status = "aborted";
			e.error = "aborted";
		} else {
			e.status = "failed";
			e.error = (r.errorMessage || r.stderr || "failed").split("\n")[0].slice(0, 40);
		}
		// Successes self-expire; failures stay pinned until the next batch/shutdown.
		// With no UI (headless child) never allocate a timer: just drop the row.
		if (e.status === "done" && !tracker.ui) {
			tracker.runs.delete(id);
		} else if (e.status === "done") {
			const t = setTimeout(() => {
				try {
					tracker.runs.delete(id);
					tracker.timers.delete(id);
					renderWidget();
					maybeStopTicker();
				} catch {
					/* best-effort */
				}
			}, FINISHED_TTL_MS);
			if (typeof t.unref === "function") t.unref();
			tracker.timers.set(id, t);
		}
		renderWidget();
		maybeStopTicker();
	} catch {
		/* best-effort */
	}
}

function ensureTicker(): void {
	try {
		// No UI (headless -p child) → never allocate a timer.
		if (!tracker.ui) return;
		if (tracker.ticker) return;
		if (runningCount() === 0) return;
		tracker.ticker = setInterval(() => {
			try {
				tracker.frame = (tracker.frame + 1) % SPINNER_FRAMES.length;
				renderWidget();
				maybeStopTicker();
			} catch {
				/* best-effort */
			}
		}, 1000);
		if (typeof tracker.ticker.unref === "function") tracker.ticker.unref();
	} catch {
		/* best-effort */
	}
}

function maybeStopTicker(): void {
	try {
		if (runningCount() > 0) return;
		if (tracker.ticker) {
			clearInterval(tracker.ticker);
			tracker.ticker = null;
		}
		// If nothing is left at all, clear the pin entirely.
		if (tracker.runs.size === 0) renderWidget();
	} catch {
		/* best-effort */
	}
}

function teardown(): void {
	try {
		if (tracker.ticker) {
			clearInterval(tracker.ticker);
			tracker.ticker = null;
		}
		for (const t of tracker.timers.values()) clearTimeout(t);
		tracker.timers.clear();
		tracker.runs.clear();
		try {
			tracker.ui?.setWidget?.(TRACKER_WIDGET_ID, undefined);
			tracker.ui?.setStatus?.(TRACKER_WIDGET_ID, undefined);
		} catch {
			/* best-effort */
		}
		tracker.ui = null;
		tracker.lastRendered = null;
	} catch {
		/* best-effort */
	}
}

// ─── Tracker rendering ───────────────────────────────────────────────────────
function fmtElapsed(ms: number): string {
	const s = Math.max(0, Math.floor(ms / 1000));
	if (s < 60) return `${s}s`;
	if (s < 3600) return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
	const h = Math.floor(s / 3600);
	const m = Math.floor((s % 3600) / 60);
	return `${h}h${String(m).padStart(2, "0")}m`;
}

function padTrunc(s: string, w: number): string {
	if (s.length === w) return s;
	if (s.length < w) return s + " ".repeat(w - s.length);
	if (w <= 1) return s.slice(0, w);
	return `${s.slice(0, w - 1)}…`;
}

function statusGlyph(e: RunEntry): { g: string; role: string } {
	switch (e.status) {
		case "queued":
			return { g: "○", role: "dim" };
		case "running":
			return { g: SPINNER_FRAMES[tracker.frame], role: "accent" };
		case "done":
			return { g: "✓", role: "success" };
		case "timeout":
			return { g: "⏱", role: "warning" };
		case "aborted":
			return { g: "⊘", role: "error" };
		default:
			return { g: "✗", role: "error" };
	}
}

function renderWidget(): void {
	try {
		const ui = tracker.ui;
		if (!ui?.setWidget) return;
		const fg = (role: string, s: string): string => {
			try {
				return ui.theme?.fg ? ui.theme.fg(role, s) : s;
			} catch {
				return s;
			}
		};
		const entries = Array.from(tracker.runs.values());
		if (entries.length === 0) {
			if (tracker.lastRendered !== "") {
				ui.setWidget(TRACKER_WIDGET_ID, undefined);
				ui.setStatus?.(TRACKER_WIDGET_ID, undefined);
				tracker.lastRendered = "";
			}
			return;
		}

		const width = Math.max(40, Math.min(process.stdout?.columns || 80, 120));
		const counts: Record<RunStatus, number> = {
			queued: 0,
			running: 0,
			done: 0,
			failed: 0,
			timeout: 0,
			aborted: 0,
		};
		for (const e of entries) counts[e.status]++;
		const totalCost = entries.reduce((a, e) => a + e.cost, 0);

		// Header (omitted for a lone single run — the row is self-describing).
		const lines: string[] = [];
		const gutter = fg("accent", "▎ ");
		const lone = entries.length === 1;
		if (!lone) {
			const parts: string[] = [];
			if (counts.running) parts.push(fg("accent", `${counts.running} running`));
			if (counts.queued) parts.push(fg("dim", `${counts.queued} queued`));
			if (counts.done) parts.push(fg("success", `${counts.done} done`));
			if (counts.failed) parts.push(fg("error", `${counts.failed} failed`));
			if (counts.timeout) parts.push(fg("warning", `${counts.timeout} timed out`));
			if (counts.aborted) parts.push(fg("error", `${counts.aborted} aborted`));
			const title = fg("toolTitle", "subagents");
			const cost = totalCost > 0 ? fg("dim", `  $${totalCost.toFixed(2)}`) : "";
			lines.push(`${gutter}${title}  ${parts.join(fg("muted", " · "))}${cost}`);
		}

		// Rows: running/queued first, then finished; cap and fold the rest.
		const ordered = entries
			.slice()
			.sort((a, b) => rank(a.status) - rank(b.status));
		const budget = MAX_VISIBLE_ROWS - lines.length;
		// Reserve one line for the "+N more" summary when we must truncate, so the
		// total never exceeds MAX_VISIBLE_ROWS (pi clips string widgets ~10 lines).
		const rowCap = ordered.length > budget ? budget - 1 : budget;
		const shown = ordered.slice(0, Math.max(0, rowCap));
		const hidden = ordered.length - shown.length;
		for (const e of shown) lines.push(renderRow(e, width, fg));
		if (hidden > 0)
			lines.push(`${gutter}${fg("muted", `… +${hidden} more`)}`);

		const key = lines.join("\n");
		if (key !== tracker.lastRendered) {
			ui.setWidget(TRACKER_WIDGET_ID, lines);
			ui.setStatus?.(TRACKER_WIDGET_ID, renderStatusLine(counts, fg));
			tracker.lastRendered = key;
		}
	} catch {
		/* best-effort; a render failure must never break the turn */
	}
}

function rank(s: RunStatus): number {
	if (s === "running") return 0;
	if (s === "queued") return 1;
	return 2;
}

function renderRow(
	e: RunEntry,
	width: number,
	fg: (role: string, s: string) => string,
): string {
	const { g, role } = statusGlyph(e);
	const gutter = fg("accent", "▎ ");
	const icon = fg(role, g);
	const stepLabel = e.step ? `${e.step} ` : "";
	const agent = fg("accent", padTrunc(`${stepLabel}${e.agent}`, 12));

	// Meta cluster (right side): elapsed · turns · tokens.
	const now = e.endedAt ?? Date.now();
	const elapsed = fmtElapsed(now - e.startedAt);
	const meta: string[] = [];
	if (e.status === "running" || e.endedAt) {
		if (e.turns) meta.push(`${e.turns}t`);
		const tok = e.tokensIn + e.tokensOut;
		if (tok) meta.push(`↑${fmtTokens(e.tokensIn)}↓${fmtTokens(e.tokensOut)}`);
	}
	const metaStr = [elapsed, ...meta].join(" ");

	// Body: error tail for failures, live tool for running, else the task.
	let body: string;
	let bodyRole = "dim";
	if (e.status === "queued") {
		body = "queued";
		bodyRole = "muted";
	} else if (e.error) {
		body = e.error;
		bodyRole = e.status === "timeout" ? "warning" : "error";
	} else if (e.status === "running" && e.lastTool) {
		body = `${e.task}  ·${e.lastTool}`;
	} else {
		body = e.task;
	}

	// Layout: "▎ {icon} {agent} {body…}  {meta}" width-budgeted.
	const fixed = 2 /*gutter*/ + 2 /*icon+sp*/ + 12 /*agent*/ + 1 /*sp*/;
	const metaW = metaStr.length + 2; // leading gap
	let bodyW = width - fixed - metaW;
	let metaOut = ` ${fg("dim", metaStr)}`;
	if (bodyW < 12) {
		// Too narrow for meta: drop tokens/turns, keep elapsed only.
		metaOut = ` ${fg("dim", elapsed)}`;
		bodyW = width - fixed - (elapsed.length + 2);
	}
	if (bodyW < 6) bodyW = 6;
	const bodyOut = fg(bodyRole, padTrunc(body, bodyW));
	return `${gutter}${icon} ${agent} ${bodyOut}${metaOut}`;
}

function renderStatusLine(
	counts: Record<RunStatus, number>,
	fg: (role: string, s: string) => string,
): string {
	const active = counts.running + counts.queued;
	const spin = active > 0 ? `${SPINNER_FRAMES[tracker.frame]} ` : "";
	const bits: string[] = [];
	if (counts.running) bits.push(`${counts.running} run`);
	if (counts.queued) bits.push(`${counts.queued} queued`);
	if (counts.done) bits.push(`${counts.done} done`);
	const fail = counts.failed + counts.timeout + counts.aborted;
	if (fail) bits.push(`${fail} failed`);
	return fg(active > 0 ? "accent" : "muted", `${spin}subagents ${bits.join(" · ")}`);
}

// ─── The core: run one subagent as a child pi, with a watchdog ───────────────
async function runSingle(
	defaultCwd: string,
	agents: AgentConfig[],
	agentName: string,
	task: string,
	cwd: string | undefined,
	step: number | undefined,
	signal: AbortSignal | undefined,
	onUpdate: OnUpdate | undefined,
	makeDetails: (rs: SingleResult[]) => SubagentDetails,
	track?: {
		mode: "single" | "parallel" | "chain";
		preRunId?: string; // pre-registered "queued" row to adopt (parallel/chain)
		enabled?: boolean; // false = don't pin (e.g. the doctor canary)
	},
): Promise<SingleResult> {
	// CENTRAL kill switch: every spawn path (tool single/parallel/chain, trees,
	// the doctor canary, anything added later) funnels through runSingle, so the
	// host-mode disable is enforced HERE — not only in the tool's execute().
	if (SUBAGENTS_DISABLED) {
		const refused: SingleResult = {
			agent: agentName,
			agentSource: "unknown",
			task,
			exitCode: 1,
			messages: [],
			stderr: SUBAGENTS_DISABLED_MSG,
			errorMessage: SUBAGENTS_DISABLED_MSG,
			usage: zeroUsage(),
			step,
		};
		if (track?.enabled !== false && track?.preRunId)
			finalizeRun(track.preRunId, refused);
		return refused;
	}
	const agent = agents.find((a) => a.name === agentName);
	if (!agent) {
		const available = agents.map((a) => `"${a.name}"`).join(", ") || "none";
		const errResult: SingleResult = {
			agent: agentName,
			agentSource: "unknown",
			task,
			exitCode: 1,
			messages: [],
			stderr: `Unknown agent "${agentName}". Available: ${available}.`,
			usage: zeroUsage(),
			step,
		};
		// A pre-registered queued row (parallel/chain) would otherwise leak as
		// "queued" forever, keeping the ticker alive. Finalize it as failed here,
		// since this early return happens before the normal register/finalize path.
		if (track?.enabled !== false && track?.preRunId)
			finalizeRun(track.preRunId, errResult);
		return errResult;
	}

	const { command, baseArgs } = resolvePiCommand();
	const args: string[] = [
		...baseArgs,
		"--mode",
		"json",
		"-p",
		"--no-session",
		"--no-extensions",
	];
	// STABILITY: re-add ONLY safe extensions on top of --no-extensions so trees +
	// web research work without the port-binding extensions that deadlock a
	// fully-loaded child. SELF_PATH = subagents (trees); WEB_ACCESS = web_search
	// et al (headless-safe, see resolveWebAccessExtensions).
	if (SELF_PATH) args.push("-e", SELF_PATH);
	// Web is on by default; a hermetic agent opts out with `web: false` so its
	// (possibly sensitive) context never reaches a search provider. When off we
	// don't even LOAD the extension — clean for restricted and unrestricted alike.
	const webEnabled = agent.web !== false && WEB_ACCESS_EXTENSIONS.length > 0;
	if (webEnabled) for (const ext of WEB_ACCESS_EXTENSIONS) args.push("-e", ext);
	if (agent.model) args.push("--model", agent.model);
	if (agent.thinking) args.push("--thinking", agent.thinking);
	if (agent.tools && agent.tools.length > 0) {
		// A restricted tool allowlist would filter out the web tools we just loaded,
		// so merge them back in — but ONLY when web is enabled for this agent, so a
		// `web: false` allowlist stays exactly as its author wrote it. Unrestricted
		// agents (no tools:) already get everything.
		const tools = webEnabled
			? Array.from(new Set([...agent.tools, ...WEB_TOOLS]))
			: agent.tools;
		args.push("--tools", tools.join(","));
	}

	// Effective watchdog budgets: per-agent frontmatter override, else the global
	// default. Captured on the result so timeout messages report the real numbers.
	const effIdleMs = clampDelay(agent.idleMs ?? IDLE_MS);
	const effWallMs = clampDelay(agent.wallMs ?? WALL_MS);
	const result: SingleResult = {
		agent: agentName,
		agentSource: agent.source,
		task,
		exitCode: -1,
		messages: [],
		stderr: "",
		usage: zeroUsage(),
		model: agent.model,
		idleMs: effIdleMs,
		wallMs: effWallMs,
		timedOut: null,
		step,
	};

	// Pin this run in the live tracker. A pre-registered queued row (parallel /
	// chain) is adopted and flipped to running; otherwise register fresh.
	let runId = "";
	if (track?.enabled !== false) {
		if (track?.preRunId) {
			runId = track.preRunId;
			markRunning(runId);
		} else {
			runId = registerRun({
				agent: agentName,
				model: agent.model,
				mode: track?.mode ?? "single",
				step,
				task,
			});
		}
	}

	const emit = () => {
		if (runId) updateRun(runId, result);
		onUpdate?.({
			content: [
				{ type: "text", text: finalText(result.messages) || "(running...)" },
			],
			details: makeDetails([result]),
		});
	};

	let tmpDir: string | null = null;
	let tmpFile: string | null = null;
	try {
		let systemPrompt = agent.systemPrompt || "";
		if (agent.maxTurns) {
			// max_turns has no CLI flag in pi 0.80.x; inject it as a budget hint.
			systemPrompt += `\n\nYou have a budget of about ${agent.maxTurns} tool-use turns. Be efficient and return your conclusion before you run out.`;
		}
		// Parent-enforced OUTPUT CONTRACT. Subagents run in parallel; without this
		// two of them pick the same "sensible" path (e.g. docs/design/<topic>.md) and
		// silently clobber each other — observed with pm + architect. Give each run a
		// unique artifact path and forbid shared/guessable ones. The primary channel
		// is still the final message; a file is the exception, not the default.
		const outSlug = `${agent.name}-${Date.now().toString(36)}-${Math.random()
			.toString(36)
			.slice(2, 6)}`;
		systemPrompt += `\n\n## Output contract (parent-enforced, overrides any conflicting instruction above)\nYou may be ONE of several subagents running in PARALLEL. To avoid clobbering a sibling's file:\n- Return your findings in your FINAL MESSAGE. That is the primary channel the parent reads.\n- Write a file ONLY if the task explicitly asks for one, or the output is too large to inline.\n- When you do write, use EXACTLY this unique path unless the task gave you an explicit one:\n    .pi-agent/subagents/${outSlug}.md\n- NEVER write to a shared or guessable path (e.g. docs/design/<topic>.md, README.md, a fixed report name) — a sibling may be writing there this instant.\n- NEVER overwrite or edit a file you did not create during THIS run.\n- Always state the path of anything you wrote in your final message.`;
		if (systemPrompt.trim()) {
			const t = await writePrompt(agent.name, systemPrompt);
			tmpDir = t.dir;
			tmpFile = t.file;
			args.push("--append-system-prompt", tmpFile);
		}
		args.push(`Task: ${task}`);

		let killedReason: "idle" | "wall" | null = null;
		let wasAborted = false;
		let drained = false; // clean post-settle teardown, NOT a failure

		const exitCode = await new Promise<number>((resolve) => {
			// detached:true puts the child in its OWN process group so we can signal the
			// whole tree (child pi + any grandchildren it spawned). Killing only the
			// direct pid can leave a grandchild holding the stdout pipe open, so 'close'
			// never fires and the wait wedges — the exact hang this extension exists to
			// prevent. We do NOT unref(), so the parent still awaits it.
			const child = spawn(command, args, {
				cwd: cwd ?? defaultCwd,
				shell: false,
				detached: process.platform !== "win32",
				stdio: ["ignore", "pipe", "pipe"],
				env: { ...process.env, PI_SUBAGENT_DEPTH: String(CURRENT_DEPTH + 1) },
			});

			let settled = false;
			let postSettled = false; // turn finished; watchdogs no longer apply
			let idleTimer: ReturnType<typeof setTimeout> | null = null;
			let wallTimer: ReturnType<typeof setTimeout> | null = null;
			let killTimer: ReturnType<typeof setTimeout> | null = null;
			let graceTimer: ReturnType<typeof setTimeout> | null = null;
			let postSettleTimer: ReturnType<typeof setTimeout> | null = null;

			const done = (code: number) => {
				if (settled) return;
				settled = true;
				clearTimers();
				if (buffer.trim()) processLine(buffer);
				resolve(code);
			};
			// Signal the whole process group (negative pid); fall back to the single pid.
			const signalTree = (sig: NodeJS.Signals) => {
				try {
					if (child.pid && process.platform !== "win32")
						process.kill(-child.pid, sig);
					else child.kill(sig);
				} catch {
					try {
						child.kill(sig);
					} catch {
						/* already gone */
					}
				}
			};
			const kill = (reason: "idle" | "wall" | "abort" | "drain") => {
				if (settled) return;
				// "drain" is a clean teardown after the turn already settled: don't mark
				// it aborted or timed-out, and coerce the exit code to success below.
				if (reason === "abort") wasAborted = true;
				else if (reason === "drain") drained = true;
				else killedReason = reason;
				signalTree("SIGTERM");
				// Reflect the watchdog fire in the pin (best-effort, AFTER the kill
				// signal so a slow UI can never delay SIGTERM), so a killed run does not
				// read as "running" during the SIGTERM/SIGKILL grace.
				if (runId && reason !== "drain")
					setRunStatus(
						runId,
						reason === "abort" ? "aborted" : "timeout",
						reason === "abort"
							? "aborted"
							: reason === "idle"
								? "idle-timeout"
								: "wall-timeout",
					);
				// Escalate to SIGKILL if SIGTERM is ignored, then force-resolve so a
				// wedged grandchild can never keep the parent waiting.
				killTimer = setTimeout(() => {
					signalTree("SIGKILL");
					graceTimer = setTimeout(() => done(137), 2000);
				}, 5000);
			};
			const bumpIdle = () => {
				// Once the turn has settled the idle watchdog is retired — trailing stdout
				// must not re-arm it and relabel a finished run as a timeout.
				if (postSettled) return;
				if (idleTimer) clearTimeout(idleTimer);
				idleTimer = setTimeout(() => kill("idle"), effIdleMs);
			};
			const clearTimers = () => {
				for (const t of [
					idleTimer,
					wallTimer,
					killTimer,
					graceTimer,
					postSettleTimer,
				])
					if (t) clearTimeout(t);
			};

			bumpIdle();
			wallTimer = setTimeout(() => kill("wall"), effWallMs);

			let buffer = "";
			const processLine = (line: string) => {
				if (!line.trim()) return;
				let event: any;
				try {
					event = JSON.parse(line);
				} catch {
					return;
				}
				if (event.type === "message_end" && event.message) {
					const msg = event.message;
					result.messages.push(msg);
					if (msg.role === "assistant") {
						result.usage.turns++;
						const u = msg.usage;
						if (u) {
							result.usage.input += u.input || 0;
							result.usage.output += u.output || 0;
							result.usage.cacheRead += u.cacheRead || 0;
							result.usage.cacheWrite += u.cacheWrite || 0;
							result.usage.cost += u.cost?.total || 0;
							result.usage.contextTokens =
								u.totalTokens || result.usage.contextTokens;
						}
						if (!result.model && msg.model) result.model = msg.model;
						if (msg.stopReason) result.stopReason = msg.stopReason;
						if (msg.errorMessage) result.errorMessage = msg.errorMessage;
					}
					emit();
				} else if (event.type === "tool_result_end" && event.message) {
					result.messages.push(event.message);
					emit();
				} else if (event.type === "agent_settled") {
					// Turn is done and its output is captured. Retire the idle + wall
					// watchdogs (they exist to catch a child stuck DURING work, not one
					// finishing) so neither can relabel a completed turn as a timeout.
					// Then give stdio a brief window to flush + the process to exit on its
					// own; if it lingers (a leaked handle), tear it down cleanly.
					postSettled = true;
					if (idleTimer) clearTimeout(idleTimer);
					if (wallTimer) clearTimeout(wallTimer);
					if (!postSettleTimer && !settled)
						postSettleTimer = setTimeout(() => kill("drain"), DRAIN_MS);
				}
			};

			child.stdout.on("data", (data) => {
				bumpIdle(); // ANY output resets the inactivity watchdog
				buffer += data.toString();
				const lines = buffer.split("\n");
				buffer = lines.pop() || "";
				for (const line of lines) processLine(line);
			});
			child.stderr.on("data", (data) => {
				bumpIdle();
				result.stderr += data.toString();
			});
			// Resolve on 'close' (stdio fully flushed) for the clean case. Also watch
			// 'exit': if the process has ended but 'close' is delayed by a lingering
			// inherited pipe, force completion after a short grace window.
			child.on("close", (code) => done(code ?? 0));
			child.on("exit", (code) => {
				if (settled) return;
				graceTimer = setTimeout(() => done(code ?? 0), 1500);
			});
			child.on("error", (err) => {
				result.stderr += `\nspawn error: ${String(err)}`;
				done(127);
			});

			if (signal) {
				const onAbort = () => kill("abort");
				if (signal.aborted) onAbort();
				else signal.addEventListener("abort", onAbort, { once: true });
			}
		});

		// A drain teardown happens only after a clean agent_settled, so OUR kill
		// signal codes (143=SIGTERM, 137=SIGKILL) are not a failure — normalize just
		// those so isFailed() doesn't trip, while a genuine post-settle nonzero exit
		// is still surfaced.
		result.exitCode =
			drained && (exitCode === 143 || exitCode === 137) ? 0 : exitCode;
		result.timedOut = killedReason;
		if (killedReason && !result.errorMessage) {
			result.errorMessage =
				killedReason === "idle"
					? `Killed after ${Math.round(effIdleMs / 1000)}s of no output (dead stream / stuck).`
					: `Killed after exceeding the ${Math.round(effWallMs / 1000)}s wall-clock cap.`;
			result.stopReason = result.stopReason || "error";
		}
		if (wasAborted) {
			result.stopReason = "aborted";
			result.errorMessage = result.errorMessage || "Subagent aborted.";
		}
		if (runId) finalizeRun(runId, result);
		return result;
	} finally {
		// Safety net: if runSingle threw before its normal finalize, don't leave the
		// pin stuck on "running" (finalizeRun is idempotent, so a normal path no-ops).
		if (runId) finalizeRun(runId, result);
		if (tmpFile)
			try {
				fs.unlinkSync(tmpFile);
			} catch {
				/* ignore */
			}
		if (tmpDir)
			try {
				fs.rmdirSync(tmpDir);
			} catch {
				/* ignore */
			}
	}
}

// ─── Tool schema ─────────────────────────────────────────────────────────────
const TaskItem = Type.Object({
	agent: Type.String({ description: "Name of the agent to invoke" }),
	task: Type.String({ description: "Task to delegate to the agent" }),
	cwd: Type.Optional(
		Type.String({ description: "Working directory for the agent process" }),
	),
});
const ScopeSchema = StringEnum(["user", "project", "both"] as const, {
	description:
		'Which agent dirs to use. Default "user"; "both" adds project-local .pi/agents.',
	default: "user",
});
const SubagentParams = Type.Object({
	agent: Type.Optional(
		Type.String({ description: "Agent name (single mode)" }),
	),
	task: Type.Optional(Type.String({ description: "Task (single mode)" })),
	tasks: Type.Optional(
		Type.Array(TaskItem, { description: "[{agent, task}] run in parallel" }),
	),
	chain: Type.Optional(
		Type.Array(TaskItem, {
			description:
				"[{agent, task}] run sequentially; use {previous} for prior output",
		}),
	),
	agentScope: Type.Optional(ScopeSchema),
	cwd: Type.Optional(
		Type.String({ description: "Working directory (single mode)" }),
	),
});

// ─── Extension entry point ───────────────────────────────────────────────────
export default function (pi: ExtensionAPI) {
	// Hard guarantee against a leaked ticker/widget outliving the session.
	try {
		pi.on("session_shutdown", () => {
			try {
				teardown();
			} catch {
				/* best-effort; must not break shutdown */
			}
		});
	} catch {
		/* best-effort; must not break the agent at load */
	}

	const makeDetails =
		(
			mode: "single" | "parallel" | "chain",
			scope: AgentScope,
			projectDir: string | null,
		) =>
		(results: SingleResult[]): SubagentDetails => ({
			mode,
			scope,
			projectDir,
			results,
		});

	// ── the tool the model calls ──
	try {
		pi.registerTool({
			name: "subagent",
			label: "Subagent",
			description: [
				"Delegate tasks to specialized subagents, each in an isolated child pi with its own context window.",
				"Modes: single (agent + task), parallel (tasks[]), chain (sequential, {previous} placeholder).",
				`Agents are markdown files (filename = name) in ${path.join(getAgentDir(), "agents")}; set agentScope:"both" to also use project-local ${CONFIG_DIR_NAME}/agents.`,
				"Every child has an inactivity + wall-clock watchdog, so a stuck subagent is killed and reported, never left to hang.",
			].join(" "),
			parameters: SubagentParams as any,

			async execute(
				_id: string,
				params: any,
				signal: AbortSignal,
				onUpdate: any,
				ctx: any,
			) {
				captureUi(ctx); // stable UI ref for the live pin
				const scope: AgentScope = params.agentScope ?? "user";
				const { agents, projectDir } = discoverAgents(ctx.cwd, scope);

				// Host-mode guard: refuse to spawn ANY child (single/parallel/chain
				// alike), explicitly and early. See PI_SUBAGENT_DISABLED comment above.
				if (SUBAGENTS_DISABLED) {
					return {
						content: [{ type: "text", text: SUBAGENTS_DISABLED_MSG }],
						details: makeDetails("single", scope, projectDir)([]),
						isError: true,
					};
				}

				// Depth guard: fork-bomb protection for trees.
				if (CURRENT_DEPTH >= MAX_DEPTH) {
					return {
						content: [
							{
								type: "text",
								text: `Subagent depth limit reached (${CURRENT_DEPTH}/${MAX_DEPTH}). This subagent must do the work itself instead of delegating further. (Raise PI_SUBAGENT_MAX_DEPTH to allow deeper trees.)`,
							},
						],
						details: makeDetails("single", scope, projectDir)([]),
						isError: true,
					};
				}

				const hasChain = (params.chain?.length ?? 0) > 0;
				const hasTasks = (params.tasks?.length ?? 0) > 0;
				const hasSingle = Boolean(params.agent && params.task);
				const modeCount =
					Number(hasChain) + Number(hasTasks) + Number(hasSingle);
				const available =
					agents.map((a) => `${a.name} (${a.source})`).join(", ") || "none";
				if (modeCount !== 1) {
					return {
						content: [
							{
								type: "text",
								text: `Provide exactly one mode (agent+task, tasks[], or chain[]).\nAvailable agents: ${available}`,
							},
						],
						details: makeDetails("single", scope, projectDir)([]),
						isError: true,
					};
				}

				// ── chain ──
				if (hasChain) {
					const md = makeDetails("chain", scope, projectDir);
					const results: SingleResult[] = [];
					let prev = "";
					// Pre-register every step as "queued" so the pin shows the whole
					// chain up front; each step flips to running as it starts.
					const chainIds: string[] = params.chain.map((s: any, i: number) =>
						registerRun({
							agent: s.agent,
							mode: "chain",
							step: i + 1,
							task: String(s.task).replace(/\{previous\}/g, "").trim(),
							status: "queued",
						}),
					);
					for (let i = 0; i < params.chain.length; i++) {
						const s = params.chain[i];
						const task = String(s.task).replace(/\{previous\}/g, prev);
						const chainUpdate: OnUpdate | undefined = onUpdate
							? (partial) => {
									const cur = partial.details?.results[0];
									if (cur)
										onUpdate({
											content: partial.content,
											details: md([...results, cur]),
										});
								}
							: undefined;
						const r = await runSingle(
							ctx.cwd,
							agents,
							s.agent,
							task,
							s.cwd,
							i + 1,
							signal,
							chainUpdate,
							md,
							{ mode: "chain", preRunId: chainIds[i] },
						);
						results.push(r);
						if (isFailed(r)) {
							// Later steps were pre-registered as "queued" but will never run;
							// drop them so they don't leak the ticker as perpetually active.
							for (let j = i + 1; j < chainIds.length; j++)
								dropRun(chainIds[j]);
							return {
								content: [
									{
										type: "text",
										text: `Chain stopped at step ${i + 1} (${s.agent}): ${resultOutput(r)}`,
									},
								],
								details: md(results),
								isError: true,
							};
						}
						prev = finalText(r.messages);
					}
					return {
						content: [
							{
								type: "text",
								text:
									finalText(results.at(-1)?.messages ?? []) || "(no output)",
							},
						],
						details: md(results),
					};
				}

				// ── parallel ──
				if (hasTasks) {
					if (params.tasks.length > MAX_PARALLEL) {
						return {
							content: [
								{
									type: "text",
									text: `Too many parallel tasks (${params.tasks.length}); max is ${MAX_PARALLEL}.`,
								},
							],
							details: makeDetails("parallel", scope, projectDir)([]),
							isError: true,
						};
					}
					const md = makeDetails("parallel", scope, projectDir);
					const all: SingleResult[] = params.tasks.map((t: any) => ({
						agent: t.agent,
						agentSource: "unknown" as const,
						task: t.task,
						exitCode: -1,
						messages: [],
						stderr: "",
						usage: zeroUsage(),
						timedOut: null,
					}));
					// Pre-register all tasks as "queued" so an N-way fanout shows every
					// row at once, even the ones beyond MAX_CONCURRENCY waiting for a slot.
					const parIds: string[] = params.tasks.map((t: any) =>
						registerRun({
							agent: t.agent,
							mode: "parallel",
							task: t.task,
							status: "queued",
						}),
					);
					const emitAll = () => {
						if (!onUpdate) return;
						const running = all.filter((r) => r.exitCode === -1).length;
						const done = all.length - running;
						onUpdate({
							content: [
								{
									type: "text",
									text: `Parallel: ${done}/${all.length} done, ${running} running...`,
								},
							],
							details: md([...all]),
						});
					};
					const results = await mapWithLimit(
						params.tasks,
						MAX_CONCURRENCY,
						async (t: any, i: number) => {
							const r = await runSingle(
								ctx.cwd,
								agents,
								t.agent,
								t.task,
								t.cwd,
								undefined,
								signal,
								(partial) => {
									if (partial.details?.results[0]) {
										all[i] = partial.details.results[0];
										emitAll();
									}
								},
								md,
								{ mode: "parallel", preRunId: parIds[i] },
							);
							all[i] = r;
							emitAll();
							return r;
						},
					);
					const ok = results.filter((r) => !isFailed(r)).length;
					const summaries = results.map((r) => {
						const status = isFailed(r)
							? `failed${r.timedOut ? ` (timeout:${r.timedOut})` : r.stopReason ? ` (${r.stopReason})` : ""}`
							: "completed";
						return `### [${r.agent}] ${status}\n\n${capOutput(resultOutput(r))}`;
					});
					return {
						content: [
							{
								type: "text",
								text: `Parallel: ${ok}/${results.length} succeeded\n\n${summaries.join("\n\n---\n\n")}`,
							},
						],
						details: md(results),
						isError: ok === 0,
					};
				}

				// ── single ──
				const md = makeDetails("single", scope, projectDir);
				const r = await runSingle(
					ctx.cwd,
					agents,
					params.agent,
					params.task,
					params.cwd,
					undefined,
					signal,
					onUpdate,
					md,
					{ mode: "single" },
				);
				if (isFailed(r)) {
					return {
						content: [
							{
								type: "text",
								text: `Agent ${r.timedOut ? `timed out (${r.timedOut})` : r.stopReason || "failed"}: ${resultOutput(r)}`,
							},
						],
						details: md([r]),
						isError: true,
					};
				}
				return {
					content: [
						{ type: "text", text: finalText(r.messages) || "(no output)" },
					],
					details: md([r]),
				};
			},

			renderCall(args: any, theme: any) {
				const scope = args.agentScope ?? "user";
				const title = theme.fg("toolTitle", theme.bold("subagent "));
				if (args.chain?.length) {
					let t = `${title}${theme.fg("accent", `chain (${args.chain.length})`)}${theme.fg("muted", ` [${scope}]`)}`;
					for (const s of args.chain.slice(0, 3))
						t += `\n  ${theme.fg("accent", s.agent)}${theme.fg(
							"dim",
							` ${String(s.task)
								.replace(/\{previous\}/g, "")
								.slice(0, 40)}`,
						)}`;
					return new Text(t, 0, 0);
				}
				if (args.tasks?.length) {
					let t = `${title}${theme.fg("accent", `parallel (${args.tasks.length})`)}${theme.fg("muted", ` [${scope}]`)}`;
					for (const s of args.tasks.slice(0, 3))
						t += `\n  ${theme.fg("accent", s.agent)}${theme.fg("dim", ` ${String(s.task).slice(0, 40)}`)}`;
					return new Text(t, 0, 0);
				}
				const preview = args.task ? String(args.task).slice(0, 60) : "...";
				return new Text(
					`${title}${theme.fg("accent", args.agent || "...")}${theme.fg("muted", ` [${scope}]`)}\n  ${theme.fg("dim", preview)}`,
					0,
					0,
				);
			},

			renderResult(result: any, opts: any, theme: any) {
				const details = result.details as SubagentDetails | undefined;
				const expanded = opts?.expanded;
				if (!details || details.results.length === 0) {
					const c = result.content?.[0];
					return new Text(c?.type === "text" ? c.text : "(no output)", 0, 0);
				}
				const mdTheme = getMarkdownTheme();
				const icon = (r: SingleResult) =>
					r.exitCode === -1
						? theme.fg("warning", "⏳")
						: isFailed(r)
							? theme.fg("error", "✗")
							: theme.fg("success", "✓");
				const renderItems = (items: DisplayItem[], limit: number) => {
					const shown = items.slice(-limit);
					const skipped = items.length - shown.length;
					let t =
						skipped > 0 ? theme.fg("muted", `... ${skipped} earlier\n`) : "";
					for (const it of shown) {
						if (it.type === "text") {
							const prev = expanded
								? it.text
								: it.text.split("\n").slice(0, 3).join("\n");
							t += `${theme.fg("toolOutput", prev)}\n`;
						} else
							t += `${theme.fg("muted", "→ ") + fmtToolCall(it.name, it.args, theme.fg.bind(theme))}\n`;
					}
					return t.trimEnd();
				};
				const agg = (rs: SingleResult[]) => {
					const u = zeroUsage();
					for (const r of rs) {
						u.input += r.usage.input;
						u.output += r.usage.output;
						u.cacheRead += r.usage.cacheRead;
						u.cost += r.usage.cost;
						u.turns += r.usage.turns;
					}
					return u;
				};

				const container = new Container();
				const total = details.results.length;
				const okCount = details.results.filter(
					(r) => r.exitCode !== -1 && !isFailed(r),
				).length;
				const running = details.results.filter((r) => r.exitCode === -1).length;
				let header: string;
				if (details.mode === "single") {
					const r0 = details.results[0];
					header = `${icon(r0)} ${theme.fg("toolTitle", theme.bold(r0.agent))}${theme.fg("muted", ` (${r0.agentSource})`)}`;
				} else {
					const summary =
						running > 0
							? `${total - running}/${total} done, ${running} running`
							: `${okCount}/${total}`;
					header = `${theme.fg("toolTitle", theme.bold(`${details.mode} `))}${theme.fg("accent", summary)}`;
				}
				container.addChild(new Text(header, 0, 0));

				for (const r of details.results) {
					if (details.mode !== "single") {
						container.addChild(new Spacer(1));
						const label = r.step ? `Step ${r.step}: ${r.agent}` : r.agent;
						container.addChild(
							new Text(`${icon(r)} ${theme.fg("accent", label)}`, 0, 0),
						);
					}
					if (r.errorMessage)
						container.addChild(
							new Text(theme.fg("error", `  ${r.errorMessage}`), 0, 0),
						);
					const items = displayItems(r.messages);
					if (expanded) {
						for (const it of items)
							if (it.type === "toolCall")
								container.addChild(
									new Text(
										theme.fg("muted", "→ ") +
											fmtToolCall(it.name, it.args, theme.fg.bind(theme)),
										0,
										0,
									),
								);
						const out = finalText(r.messages);
						if (out) {
							container.addChild(new Spacer(1));
							container.addChild(new Markdown(out.trim(), 0, 0, mdTheme));
						}
					} else if (items.length > 0) {
						container.addChild(
							new Text(renderItems(items, COLLAPSED_ITEMS), 0, 0),
						);
					} else if (r.exitCode === -1) {
						container.addChild(
							new Text(theme.fg("muted", "(running...)"), 0, 0),
						);
					}
					const us = fmtUsage(r.usage, r.model);
					if (us) container.addChild(new Text(theme.fg("dim", us), 0, 0));
				}
				if (details.results.length > 1) {
					container.addChild(new Spacer(1));
					container.addChild(
						new Text(
							theme.fg("dim", `Total: ${fmtUsage(agg(details.results))}`),
							0,
							0,
						),
					);
				}
				if (!expanded)
					container.addChild(
						new Text(theme.fg("muted", "(Ctrl+O to expand)"), 0, 0),
					);
				return container;
			},
		});
	} catch (err) {
		// If tool registration fails, do not break pi — just log to stderr.
		try {
			process.stderr.write(
				`[subagents] failed to register tool: ${String(err)}\n`,
			);
		} catch {
			/* best-effort */
		}
	}

	// ── /subagents command: list agents, config, and a live doctor self-audit ──
	try {
		pi.registerCommand("subagents", {
			description:
				"List subagents and config; `/subagents doctor` runs a live self-audit; `/subagents clear` dismisses finished rows from the tracker.",
			handler: async (rawArgs: any, ctx: any) => {
				const arg = String(rawArgs ?? "")
					.trim()
					.toLowerCase();

				if (arg.startsWith("clear")) {
					// Dismiss finished/failed/timed-out rows (keep anything still running).
					// Failures pin until the next batch or shutdown; this is the manual escape.
					try {
						for (const [id, r] of tracker.runs) {
							if (r.status === "running") continue;
							tracker.runs.delete(id);
							const t = tracker.timers.get(id);
							if (t) {
								clearTimeout(t);
								tracker.timers.delete(id);
							}
						}
						renderWidget();
						maybeStopTicker();
					} catch {
						/* best-effort */
					}
					ctx.ui?.notify?.("Subagent tracker cleared.", "info");
					return;
				}

				const scope: AgentScope = arg.includes("both") ? "both" : "user";
				const { agents, projectDir } = discoverAgents(ctx.cwd, scope);

				if (arg.startsWith("doctor")) {
					// Host mode: the doctor canary is a real child spawn, and it must
					// refuse exactly like the tool does (runSingle enforces this too —
					// belt-and-suspenders — but refusing here gives a clear message
					// instead of a failed canary).
					if (SUBAGENTS_DISABLED) {
						ctx.ui?.notify?.(
							`⛔ subagents doctor: refusing to spawn the canary — ${SUBAGENTS_DISABLED_MSG}`,
							"error",
						);
						return;
					}
					// Live end-to-end self-audit: spawn a real canary subagent with a
					// short timeout and confirm it returns. This is the check that never
					// passed with the old extension.
					try {
						ctx.ui.setWorkingMessage?.("subagents doctor: spawning canary...");
					} catch {
						/* ignore */
					}
					const canary: AgentConfig = {
						name: "__doctor_canary",
						description: "self-audit canary",
						model:
							process.env.PI_SUBAGENT_DOCTOR_MODEL ||
							"anthropic/claude-haiku-4-5",
						thinking: "off",
						tools: ["read"],
						// Tight budgets for the audit regardless of global config, so a broken
						// canary fails fast instead of hanging out the generous defaults.
						// runSingle reads these off the AgentConfig — no env swap needed.
						idleMs: 20_000,
						wallMs: 45_000,
						systemPrompt:
							"You are a health-check canary. Reply with exactly the word PONG and nothing else.",
						source: "user",
						filePath: "(built-in)",
						warnings: [],
					};
					const started = Date.now();
					const r = await runSingle(
						ctx.cwd,
						[canary],
						canary.name,
						"Reply with exactly: PONG",
						undefined,
						undefined,
						undefined,
						undefined,
						makeDetails("single", "user", null),
						{ mode: "single", enabled: false }, // don't pin the canary
					);
					const ms = Date.now() - started;
					const text = finalText(r.messages).trim();
					const pass = !isFailed(r) && /PONG/i.test(text);
					const lines = [
						pass ? "✅ subagents doctor: PASS" : "❌ subagents doctor: FAIL",
						`  spawn resolution: ${resolvePiCommand().command}`,
						`  self extension (-e): ${SELF_PATH ?? "NOT FOUND (trees disabled)"}`,
						`  web access (-e): ${WEB_ACCESS_EXTENSIONS.length ? `${WEB_TOOLS.join(", ")}` : "NOT FOUND (subagents can't web search)"}`,
						`  canary model: ${canary.model}`,
						`  round-trip: ${ms}ms · turns=${r.usage.turns} · exit=${r.exitCode}${r.timedOut ? ` · TIMEOUT:${r.timedOut}` : ""}`,
						`  canary said: ${JSON.stringify(text.slice(0, 60)) || "(nothing)"}`,
						`  depth=${CURRENT_DEPTH}/${MAX_DEPTH} · idle=${Math.round(IDLE_MS / 1000)}s · wall=${Math.round(WALL_MS / 1000)}s · concurrency=${MAX_CONCURRENCY}`,
					];
					if (!pass && r.stderr.trim())
						lines.push(`  stderr: ${r.stderr.trim().slice(0, 300)}`);
					try {
						ctx.ui.setWorkingMessage?.("");
					} catch {
						/* ignore */
					}
					ctx.ui.notify(lines.join("\n"), pass ? "info" : "error");
					return;
				}

				// Default: list agents + config.
				const lines = [`Subagents (scope: ${scope})`];
				if (agents.length === 0) lines.push("  (no agents found)");
				for (const a of agents.sort((x, y) => x.name.localeCompare(y.name))) {
					lines.push(
						`  ${a.name} (${a.source})${a.intent ? ` · intent:${a.intent}` : ""}${a.model ? ` · ${a.model}` : " · model:inherit"}${a.thinking ? ` · think:${a.thinking}` : ""}${a.tools ? ` · tools:${a.tools.length}` : " · tools:all"}`,
					);
					if (a.description)
						lines.push(`    ${a.description.split("\n")[0].slice(0, 100)}`);
					for (const w of a.warnings) lines.push(`    ⚠ ${w}`);
				}
				lines.push("");
				lines.push(
					`config: depth=${CURRENT_DEPTH}/${MAX_DEPTH} · idle=${Math.round(IDLE_MS / 1000)}s · wall=${Math.round(WALL_MS / 1000)}s · concurrency=${MAX_CONCURRENCY} · maxParallel=${MAX_PARALLEL}`,
				);
				lines.push(
					`web access: ${WEB_ACCESS_EXTENSIONS.length ? `on (${WEB_TOOLS.join(", ")})` : "off (pi-web-access not found)"}`,
				);
				lines.push(
					`routing: ${ROUTING ? `on (${Object.keys(ROUTING.routes ?? {}).length} intents, routing.json)` : "off (no routing.json; agents use model:/inherit)"}`,
				);
				if (projectDir) lines.push(`project agents dir: ${projectDir}`);
				lines.push("run `/subagents doctor` for a live self-audit.");
				ctx.ui.notify(lines.join("\n"), "info");
			},
		});
	} catch (err) {
		try {
			process.stderr.write(
				`[subagents] failed to register command: ${String(err)}\n`,
			);
		} catch {
			/* best-effort */
		}
	}
}
