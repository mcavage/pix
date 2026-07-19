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

// The bridge model is configured on the HOST (`pi-stack config set
// ollama_bridge_model <tag>`); `pi-stack run` writes the resolved value into
// <workspace>/.pi-stack/ollama-bridge.model, and pi runs with the workspace as
// cwd, so we read it here — the same host-writes-file / VM-reads-file seam the
// profile + knowledge-scope files use. This is why you do NOT hand-edit sandbox
// env: set it once with `pi-stack config set` and every `pi-stack run` picks it
// up. A literal OLLAMA_BRIDGE_MODEL env var still wins (power-user override).
function bridgeModelFromWorkspace(): string | undefined {
	try {
		const raw = readFileSync(".pi-stack/ollama-bridge.model", "utf8").trim();
		return raw || undefined;
	} catch {
		return undefined; // absent (e.g. `make run` / consumer `sbx run`) -> use the default
	}
}

const LISTEN_PORT = Number(process.env.OLLAMA_BRIDGE_PORT ?? 11434);
const HOST = process.env.OLLAMA_BRIDGE_HOST ?? "host.docker.internal";
const HOST_PORT = Number(process.env.OLLAMA_BRIDGE_HOST_PORT ?? 11434);

// HOST MODE (pi-stack host, docs/design/host-mode.md): the Go launcher sets
// OLLAMA_HOSTMODE=1 + OLLAMA_URL. There the bridge's whole reason to exist —
// dodge the sbx proxy by listening on localhost and forwarding to
// host.docker.internal — is moot (there is no proxy, no VM). Worse, starting
// the reverse proxy here would SELF-LOOP: it would listen on
// 127.0.0.1:11434 and forward to the same host:port, so if real ollama is
// absent the bridge binds successfully and recursively proxies to itself
// until exhaustion. So in host mode we skip the listener entirely and just
// register the provider straight at OLLAMA_URL (default the real local
// ollama). Sandbox behavior (HOSTMODE unset) is completely unchanged below.
const HOST_MODE = process.env.OLLAMA_HOSTMODE === "1";
const HOST_MODE_URL = process.env.OLLAMA_URL ?? "http://127.0.0.1:11434/v1";

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

export default async function (pi: any): Promise<void> {
	// 1) Register the provider + model up front (no endpoint probe). In host
	// mode point baseUrl straight at the real ollama (OLLAMA_URL); in the
	// sandbox keep pointing at our own localhost listener (started below).
	try {
		pi.registerProvider("ollama", {
			name: "Ollama (local)",
			baseUrl: HOST_MODE ? HOST_MODE_URL : `http://localhost:${LISTEN_PORT}/v1`,
			api: "openai-completions",
			apiKey: "ollama", // placeholder; Ollama ignores it, but pi wants auth present
			models: [
				{
					id: MODEL_ID,
					name: MODEL_NAME,
					reasoning: true,
					input: ["text"],
					cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
					contextWindow: MODEL_CTX,
					maxTokens: 8192,
				},
			],
		});
	} catch {
		/* best-effort; must not break the agent */
	}

	// HOST MODE: no reverse proxy to start (see the OLLAMA_HOSTMODE comment
	// above) — the provider above already points straight at OLLAMA_URL, so we
	// return here without binding anything.
	if (HOST_MODE) return;

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
