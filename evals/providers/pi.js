// pi custom provider for promptfoo.
//
// Why this exists: pi-stack's model credentials are injected proxy-side; the VM
// only ever sees a `proxy-managed` sentinel, and per-provider auth is pi's job,
// not ours. So the eval framework must NOT talk to providers directly (that would
// mean storing keys). Instead it invokes a model exactly the way a subagent does
// — a headless, hermetic `pi` process — and reads the NDJSON event stream back.
// Every provider pi can already reach (Claude, GPT, Gemini via the proxy; local
// Ollama) is callable with zero per-provider code here. This is the v1 `piRunner`
// insight (services/host/evals.go), re-homed as a promptfoo provider.
//
// Hermetic + safe: --no-tools (the evaluated model cannot touch the host),
// --no-context-files (no AGENTS.md leakage), --no-session --no-extensions, run in
// a throwaway cwd, with a wall-clock timeout (pi has no client read timeout, so a
// dead stream would otherwise hang the sweep forever).
//
// promptfoo calls callApi(prompt); we return { output, tokenUsage, cost } so
// promptfoo's assertions score accuracy and its matrix renders cost + latency.

const { spawn } = require("node:child_process");
const os = require("node:os");
const fs = require("node:fs");
const path = require("node:path");

const DEFAULT_TIMEOUT_MS = Number(process.env.PI_EVAL_TIMEOUT_MS) || 300000;
// Cap the NDJSON we buffer so a runaway generation (pi streams the accumulating
// message on every update) can't exhaust memory. The child is killed if exceeded.
const MAX_BYTES = Number(process.env.PI_EVAL_MAX_BYTES) || 64 * 1024 * 1024;

class PiProvider {
	constructor(options = {}) {
		this.config = options.config || {};
		this.model = this.config.model || options.id;
		this.bin = process.env.PI_BIN || "pi";
		this.timeoutMs = Number(this.config.timeoutMs) || DEFAULT_TIMEOUT_MS;
		if (!this.model) throw new Error("pi provider: config.model is required");
	}

	// A stable, human-readable id promptfoo uses as the column label + scorecard key.
	id() {
		return `pi:${this.model}`;
	}

	async callApi(prompt) {
		// Fully hermetic + reproducible: no tools (can't touch the host), no
		// session, no extensions, and no discovery of the host's skills / prompt
		// templates / themes / context files (which would change tokens + behavior
		// per machine).
		const args = [
			"--model", this.model,
			"-p", prompt,
			"--mode", "json",
			"--no-session", "--no-extensions",
			"--no-tools", "--no-context-files",
			"--no-skills", "--no-prompt-templates", "--no-themes",
		];
		const cwd = fs.mkdtempSync(path.join(os.tmpdir(), "eval-run-"));
		const started = Date.now();
		let stdout = "";
		let stderr = "";
		try {
			const code = await new Promise((resolve, reject) => {
				// stdin MUST be ignored: with an open stdin pipe, headless `pi -p`
				// waits for interactive input and never exits.
				const child = spawn(this.bin, args, { cwd, stdio: ["ignore", "pipe", "pipe"] });
				child.stdout.setEncoding("utf8");
				child.stderr.setEncoding("utf8");
				const timer = setTimeout(() => {
					child.kill("SIGKILL");
					reject(new Error(`pi run timed out after ${this.timeoutMs}ms`));
				}, this.timeoutMs);
				child.stdout.on("data", (d) => {
					stdout += d;
					if (stdout.length > MAX_BYTES) {
						child.kill("SIGKILL");
						clearTimeout(timer);
						reject(new Error(`pi output exceeded ${MAX_BYTES} bytes`));
					}
				});
				child.stderr.on("data", (d) => {
					if (stderr.length < 64 * 1024) stderr += d;
				});
				child.on("error", (e) => {
					clearTimeout(timer);
					reject(e);
				});
				child.on("close", (c) => {
					clearTimeout(timer);
					resolve(c);
				});
			});
			if (code !== 0) {
				return { error: `pi exited ${code}: ${stderr.slice(0, 500)}` };
			}
		} catch (e) {
			if (e && e.code === "ENOENT") {
				return {
					error:
						`pi CLI not found (spawn ${this.bin}). The eval harness runs each model ` +
						`through a headless pi process (so it never handles API keys), which ` +
						`means pi must be on the host PATH. Install it (npm i -g ` +
						`@earendil-works/pi-coding-agent) or set PI_BIN to its path.`,
				};
			}
			return { error: String((e && e.message) || e) };
		} finally {
			try {
				fs.rmSync(cwd, { recursive: true, force: true });
			} catch {
				/* best-effort cleanup */
			}
		}

		let text = "";
		let inTok = 0;
		let outTok = 0;
		let cost = 0;
		for (const line of stdout.split("\n")) {
			const l = line.trim();
			if (!l) continue;
			let ev;
			try {
				ev = JSON.parse(l);
			} catch {
				continue;
			}
			if (ev.type === "message_end" && ev.message && ev.message.role === "assistant") {
				const u = ev.message.usage || {};
				inTok += (u.input || 0) + (u.cacheRead || 0) + (u.cacheWrite || 0);
				outTok += u.output || 0;
				if (u.cost && u.cost.total) cost += u.cost.total;
				const c = ev.message.content;
				if (typeof c === "string") text += c;
				else if (Array.isArray(c)) for (const b of c) if (b && b.text) text += b.text;
			}
		}

		return {
			output: text,
			tokenUsage: { prompt: inTok, completion: outTok, total: inTok + outTok },
			cost,
			metadata: { latencyMs: Date.now() - started, model: this.model },
		};
	}
}

module.exports = PiProvider;
