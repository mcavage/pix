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

const LISTEN_PORT = Number(process.env.OLLAMA_BRIDGE_PORT ?? 11434);
const HOST = process.env.OLLAMA_BRIDGE_HOST ?? "host.docker.internal";
const HOST_PORT = Number(process.env.OLLAMA_BRIDGE_HOST_PORT ?? 11434);

export default async function (pi: any): Promise<void> {
	// 1) Register the provider + model up front (no endpoint probe).
	try {
		pi.registerProvider("ollama", {
			name: "Ollama (local)",
			baseUrl: `http://localhost:${LISTEN_PORT}/v1`,
			api: "openai-completions",
			apiKey: "ollama", // placeholder; Ollama ignores it, but pi wants auth present
			models: [
				{
					id: "gemma4:latest",
					name: "Gemma 4 (local)",
					reasoning: true,
					input: ["text"],
					cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
					contextWindow: 128000,
					maxTokens: 16384,
				},
			],
		});
	} catch {
		/* best-effort; must not break the agent */
	}

	// 2) Start the localhost -> host bridge for the actual calls.
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
		server.on("error", () => {});
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
