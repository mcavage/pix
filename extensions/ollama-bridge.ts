// Ollama integration for pi inside the sandbox. Two parts, both needed:
//
// 1) REGISTER the provider/model with FULLY-DECLARED metadata via
//    pi.registerProvider(). pi awaits the extension factory before startup, so the
//    model is in the cycle immediately and `--models ollama/...` matches with no
//    warning. Declaring full metadata is what avoids a startup PROBE of the
//    endpoint (the minimal models.json form probes, and that probe can't reach a
//    host service through the sbx proxy at startup -> the "No models match" warning).
//
// 2) BRIDGE the calls. At call time pi hits the provider baseUrl with its undici
//    fetch, which sbx routes through its proxy; that proxy can't reach
//    host.docker.internal (curl/node:http can, because they honor NO_PROXY). So we
//    point the provider at http://localhost:11434 (localhost IS in NO_PROXY, so
//    undici goes direct) and run a tiny reverse proxy here that forwards
//    localhost:11434 -> host.docker.internal:11434 over node:http, the same
//    proxy-dodge extensions/memory-recall.ts uses.
//
// Isolated: never touches pi's global dispatcher, so the cloud providers
// (Claude/GPT/Gemini), whose keys the sbx proxy injects, are unaffected.
//
// Generalizes: change the model list / ports, or copy this for another local server
// (LM Studio, vLLM).

import { createServer, request } from "node:http";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { getAgentDir } from "@earendil-works/pi-coding-agent";
import { parseRoster, type Roster } from "../lib/inference-roster.ts";

// The bridge model is configured on the HOST (`pix config set
// ollama_bridge_model <tag>`); `pix run` writes the resolved value into
// <workspace>/.pix/ollama-bridge.model, and pi runs with the workspace as
// cwd, so we read it here — the same host-writes-file / VM-reads-file seam the
// profile + knowledge-scope files use. This is why you do NOT hand-edit sandbox
// env: set it once with `pix config set` and every `pix run` picks it
// up. A literal OLLAMA_BRIDGE_MODEL env var still wins (power-user override).
function bridgeModelFromWorkspace(): string | undefined {
	try {
		const raw = readFileSync(".pix/ollama-bridge.model", "utf8").trim();
		return raw || undefined;
	} catch {
		return undefined; // absent (e.g. `make run` / consumer `sbx run`) -> use the default
	}
}

const LISTEN_PORT = Number(process.env.OLLAMA_BRIDGE_PORT ?? 11434);
const HOST = process.env.OLLAMA_BRIDGE_HOST ?? "host.docker.internal";
const HOST_PORT = Number(process.env.OLLAMA_BRIDGE_HOST_PORT ?? 11434);

// Which local model to expose in the cycle. It MUST match a tag pulled on the
// HOST (`ollama pull <tag>`), or the call 404s. Default: qwen3.5:9b — the current
// all-rounder that still fits a 16GB box (loads on demand, not resident). This is
// the SAME id the router registers as its local option (models.json), so routing
// to local and the interactive cycle agree. Override via env (e.g. in the
// sandbox's /etc/sandbox-persistent.sh) for a bigger/smaller model — no code edit:
//   OLLAMA_BRIDGE_MODEL, OLLAMA_BRIDGE_CONTEXT (and OLLAMA_BRIDGE_MODEL_NAME to
// override the auto-derived display label). contextWindow is what pi will fill; a
// smaller window means a smaller KV cache on the host, the other half of the DRAM
// story after model size.
const MODEL_ID =
	process.env.OLLAMA_BRIDGE_MODEL ?? bridgeModelFromWorkspace() ?? "qwen3.5:9b";
// The display label is cosmetic; derive it from the tag so you only ever set ONE
// var (OLLAMA_BRIDGE_MODEL). OLLAMA_BRIDGE_MODEL_NAME still overrides if you care.
const MODEL_NAME =
	process.env.OLLAMA_BRIDGE_MODEL_NAME ?? `${MODEL_ID} (local)`;
// posInt: a positive finite integer or the fallback — so OLLAMA_BRIDGE_CONTEXT="",
// "0", or "32k" can't feed NaN/0 into the provider metadata (which would break
// context accounting + compaction).
function posInt(v: string | undefined, fallback: number): number {
	const n = Number(v);
	return Number.isFinite(n) && n > 0 ? Math.floor(n) : fallback;
}
const MODEL_CTX = posInt(process.env.OLLAMA_BRIDGE_CONTEXT, 32768);

// One entry in the provider's model list.
type BridgeModel = {
	id: string;
	name: string;
	reasoning: boolean;
	input: string[];
	cost: { input: number; output: number; cacheRead: number; cacheWrite: number };
	contextWindow: number;
	maxTokens: number;
};

// The single model this bridge has always exposed: the configured bridge tag.
// It stays the FALLBACK, and on a stack with no generated manifest it is still
// the whole answer.
function bridgeTagModel(): BridgeModel {
	return {
		id: MODEL_ID,
		name: MODEL_NAME,
		reasoning: true,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: MODEL_CTX,
		maxTokens: 8192,
	};
}

// manifestModels reads the ollama models the HOST decided this sandbox can
// call, from the inference.json the launcher generates.
//
// Why this exists: `pix run` passes pi a `--models` cycle built from every
// callable binding in config, which today includes each probed Ollama model —
// local and `:cloud` alike. This extension used to register exactly ONE, the
// bridge tag. Every other ollama id in that cycle therefore matched no
// registered model, and pi opened the session with
//
//     Warning: No models match pattern "ollama/glm-5.2:cloud"
//
// for each one. The models were genuinely reachable — the reverse proxy below
// forwards anything to the host daemon, which serves cloud tags too — so the
// warning was not protecting anyone from a broken route. It was the two halves
// of the stack disagreeing about what "callable" meant, and the fix is to read
// the host's answer instead of hardcoding a second one.
//
// Best-effort by design: a missing, unreadable, or malformed manifest is the
// pre-manifest world, and falling back to the bridge tag is exactly right there.
// modelsFromManifest is the PURE half: parsed manifest in, provider model list
// out, with the configured bridge tag guaranteed present. The tag must survive
// a manifest that omits it — it is what `pix config set ollama_bridge_model`
// promises and what the interactive cycle offers. Exported for tests.
export function modelsFromManifest(
	parsed: any,
	bridgeTag: BridgeModel,
): BridgeModel[] {
	const out: BridgeModel[] = [];
	const models = parsed && Array.isArray(parsed.models) ? parsed.models : [];
	for (const m of models) {
		if (!m || m.backend !== "ollama" || typeof m.id !== "string") continue;
		// The manifest id is backend-qualified ("ollama/glm-5.2:cloud"); pi
		// qualifies with the PROVIDER name it was registered under, so strip the
		// prefix or the model ends up as "ollama/ollama/glm-5.2:cloud".
		const id = m.id.startsWith("ollama/") ? m.id.slice("ollama/".length) : m.id;
		if (!id || out.some((o) => o.id === id)) continue;
		out.push({
			id,
			name: typeof m.name === "string" && m.name ? m.name : `${id} (ollama)`,
			reasoning: m.reasoning !== false,
			input: ["text"],
			// Ollama is unmetered through the local daemon; a cloud tag bills on the
			// ollama.com subscription, not per token here.
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow:
				typeof m.context_window === "number" && m.context_window > 0
					? m.context_window
					: bridgeTag.contextWindow,
			maxTokens:
				typeof m.max_tokens === "number" && m.max_tokens > 0
					? m.max_tokens
					: bridgeTag.maxTokens,
		});
	}
	if (!out.some((m) => m.id === bridgeTag.id)) out.unshift(bridgeTag);
	return out;
}

// bridgeModels is the impure half: read the manifest, hand it to the pure one.
// Best-effort by design — a missing, unreadable, or malformed manifest is the
// pre-manifest world, where the bridge tag alone is exactly right.
function bridgeModels(): BridgeModel[] {
	return modelsFromManifest(readManifest(), bridgeTagModel());
}

// readManifest parses <agentDir>/inference.json once so both the model-list
// builder above and the roster reader below work from the identical parsed
// document, never two independent reads that could observe different bytes
// mid-write. Absent/unparseable is the pre-manifest world (null); a present
// but unparseable file is diagnosed loudly, the same guard
// extensions/inference.ts's readManifest applies.
function readManifest(): any {
	const manifestPath = join(getAgentDir(), "inference.json");
	let raw: string | undefined;
	try {
		raw = readFileSync(manifestPath, "utf8");
	} catch {
		/* absent -> bridge tag only, the expected pre-manifest world */
		return null;
	}
	try {
		return JSON.parse(raw);
	} catch (err) {
		process.stderr.write(`[ollama-bridge] ${manifestPath} is present but failed to parse as JSON: ${err}\n`);
		return null;
	}
}

// readRoster resolves the additive roster (docs/design/environments.md §7)
// from the same manifest bridgeModels() just read. Never used to pick THIS
// bridge's own default model: the local-model transport limitation (§4.1)
// means the parent-Ollama inheritance exception stays a subagents.ts
// concern, not a roster-driven one, until native custom-agent local-model
// transport lands. Exported so a test can assert this file resolves the
// identical roster shape extensions/inference.ts and extensions/subagents.ts
// do, from the identical file.
export function readRoster(): Roster | undefined {
	return parseRoster(readManifest());
}

export default async function (pi: any): Promise<void> {
	// 1) Register the provider + models up front (no endpoint probe). In host
	// mode point baseUrl straight at the real ollama (OLLAMA_URL); in the
	// sandbox keep pointing at our own localhost listener (started below).
	try {
		pi.registerProvider("ollama", {
			name: "Ollama (local)",
			baseUrl: `http://localhost:${LISTEN_PORT}/v1`,
			api: "openai-completions",
			apiKey: "ollama", // placeholder; Ollama ignores it, but pi wants auth present
			models: bridgeModels(),
		});
	} catch {
		/* best-effort; must not break the agent */
	}

	// 2) Start the localhost -> host bridge for the actual calls (sandbox only).
	try {
		const server = createServer((req, res) => {
			const upstream = request(
				{
					host: HOST,
					port: HOST_PORT,
					path: req.url,
					method: req.method,
					headers: { ...req.headers, host: `${HOST}:${HOST_PORT}` },
				},
				(up) => {
					res.writeHead(up.statusCode ?? 502, up.headers);
					up.pipe(res);
				},
			);
			upstream.on("error", (e) => {
				if (!res.headersSent)
					res.writeHead(502, { "content-type": "text/plain" });
				res.end(
					`ollama-bridge: cannot reach ${HOST}:${HOST_PORT}: ${String(e)}`,
				);
			});
			req.pipe(upstream);
		});
		server.on("error", (err) => {
			// Suppress EADDRINUSE on listen — handled by the settle promise
			// above. Log everything else so post-startup errors are diagnosable.
			if ((err as any).code !== "EADDRINUSE") {
				process.stderr.write(`[ollama-bridge] server error: ${err}\n`);
			}
		});
		// Settle on BOTH 'listening' and 'error'. Critical for /reload: the factory
		// re-runs while the PRE-reload server may still hold the port, so listen()
		// fires EADDRINUSE. If we only resolved on 'listening', the await would hang
		// forever and wedge the whole reload. On error we just proceed — the existing
		// bridge on that port still serves, so the provider keeps working.
		await new Promise<void>((resolve) => {
			let done = false;
			const settle = () => {
				if (done) return;
				done = true;
				resolve();
			};
			server.once("listening", settle);
			server.once("error", settle);
			server.listen(LISTEN_PORT, "127.0.0.1");
		});
		// Don't let the open listener keep the event loop alive: interactive pi stays
		// up on its own (the bridge serves the whole session), but `pi -p` /
		// `--list-models` must still be able to exit.
		server.unref();
		// Close on shutdown so a /reload frees the port cleanly before the next
		// factory run (belt-and-suspenders with the error-tolerant listen above).
		try {
			pi.on?.("session_shutdown", () => {
				try {
					server.close();
				} catch {
					/* already closed */
				}
			});
		} catch {
			/* best-effort; older pi may not expose on() here */
		}
	} catch {
		/* best-effort */
	}
}
