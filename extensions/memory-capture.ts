// pix — capture (client side).
//
// When an exchange finishes, hand it to the host memory service, which runs the
// watcher and decides what's worth remembering. The agent never chooses to
// remember; this just forwards the turn.
//
// Reliability: pi awaits before_agent_start (that's how recall injection lands),
// but does NOT wait on agent_end, so a hand-off fired only at agent_end races
// process teardown in print mode. So we capture the last COMPLETE exchange at
// before_agent_start of the next turn (awaited, reliable), and also try at
// agent_end (best-effort, catches the final exchange of an interactive session).
// A dedup key makes sure each exchange is processed once.
//
//   MEMORY_URL  default http://host.docker.internal:11435

import { basename, join } from "node:path";
import { execSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";

const MEMORY_URL = process.env.MEMORY_URL ?? "http://host.docker.internal:11435";

const safe = async <T>(fn: () => Promise<T>): Promise<T | undefined> => {
	try {
		return await fn();
	} catch {
		return undefined;
	}
};

// node:http, not fetch: in an sbx sandbox pi installs a global undici proxy
// dispatcher and sbx's NO_PROXY omits host.docker.internal, so fetch() to the
// host store would be routed through the proxy and fail. node:http goes direct.
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
					const status = res.statusCode ?? 500;
					if (status < 200 || status >= 300) {
						const detail = chunks.trim().replace(/\s+/g, " ").slice(0, 240);
						return reject(new Error(`memory service HTTP ${status}${detail ? `: ${detail}` : ""}`));
					}
					try {
						resolve(chunks ? JSON.parse(chunks) : null);
					} catch {
						reject(new Error("memory service returned a non-JSON success response"));
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

// Exported only for the transport regression tests; production uses call().
export const testPostJson = postJson;

let rpcId = 0;
async function call(method: string, params: any): Promise<any> {
	const response = await postJson(MEMORY_URL, { jsonrpc: "2.0", id: ++rpcId, method, params }, 3000);
	if (response?.error) {
		throw new Error(typeof response.error.message === "string" ? response.error.message : "memory service RPC error");
	}
	return response;
}

// Surface a disabled watcher ONCE per session instead of dropping captures in
// silence. The daemon returns {result:{accepted:false, reason}} when the watcher
// model isn't pulled/reachable; without this the whole capture half looks dead
// with zero signal (the exact bug this guards).
let warnedCaptureOff = false;
let warnedPostErr = false; // one stderr line per session when the observe POST fails
function warnIfCaptureOff(resp: any): void {
	try {
		const r = resp?.result;
		if (r && r.accepted === false && !warnedCaptureOff) {
			warnedCaptureOff = true;
			const reason = typeof r.reason === "string" ? r.reason : "watcher unavailable";
			process.stderr.write(`[memory] automatic fact capture is OFF: ${reason}\n`);
		}
	} catch {
		/* best-effort; never break a turn over a warning */
	}
}

// The active profile stamps captures (recall then scopes to {profile}∪{default}).
// The launcher writes it to <cwd>/.pix/profile per run, mirroring the
// knowledge scope file; absent => "default" (shared bucket).
//
// Read EXACTLY ONCE at extension load and frozen immutably — the same value
// memory-recall.ts reads — so a second sandbox overwriting the file mid-session
// can't make capture stamp a different profile than recall queries. Never throws
// at load (try/catch).
const ACTIVE_PROFILE: string = (() => {
	try {
		const raw = readFileSync(join(process.cwd(), ".pix", "profile"), "utf8").trim();
		return raw || "default";
	} catch {
		return "default"; // missing file is the normal, un-scoped case
	}
})();

// Inside the sandbox every project mounts at /home/agent/workspace, so the dir
// name is useless. Use the git remote (stable across machines). Cached per
// process; null when we can't tell (treated as global).
let _project: string | null | undefined;
function currentProject(ctx: any): string | null {
	if (_project !== undefined) return _project;
	const cwd = (typeof ctx?.cwd === "string" && ctx.cwd) || process.cwd();
	try {
		const url = execSync(`git -C ${JSON.stringify(cwd)} remote get-url origin`, {
			encoding: "utf8",
			timeout: 1500,
			stdio: ["ignore", "pipe", "ignore"],
		}).trim();
		const name = url.replace(/\.git$/, "").split(/[/:]/).filter(Boolean).pop();
		if (name) return (_project = name);
	} catch {}
	const base = basename(cwd);
	return (_project = base && base !== "workspace" && base !== "/" ? base : null);
}

// pi's session entries are { type: "message", message: { role, content: [blocks] } }
// where each block is { type: "text" | "thinking", text }. getBranch() returns the
// current linear conversation; at before_agent_start it ends at the previous
// completed exchange (the current prompt isn't appended yet).
function entries(ctx: any): any[] {
	const sm = ctx?.sessionManager;
	if (!sm) return [];
	try {
		if (typeof sm.getBranch === "function") return sm.getBranch() ?? [];
	} catch {}
	try {
		if (typeof sm.getEntries === "function") return sm.getEntries() ?? [];
	} catch {}
	return sm.fileEntries ?? [];
}
const roleOf = (e: any): string | undefined => e?.message?.role ?? e?.role;
const textOf = (e: any): string => {
	const c = e?.message?.content ?? e?.content;
	if (typeof c === "string") return c.trim();
	if (Array.isArray(c))
		return c
			.filter((b: any) => (b?.type ?? "text") === "text")
			.map((b: any) => b?.text ?? "")
			.join("")
			.trim();
	return "";
};

// The user message from the most recent complete exchange (the last assistant
// message, and the user message before it). The watcher only ever observes
// the USER'S message (never the agent's reply, see the file header), and the
// assistant text has no other consumer, so only the anchor position (is there
// a completed assistant turn yet?) matters here, not its content.
function lastCompleteExchange(hist: any[]): { user: string } | null {
	let ai = -1;
	for (let i = hist.length - 1; i >= 0; i--)
		if (roleOf(hist[i]) === "assistant") {
			ai = i;
			break;
		}
	if (ai < 1) return null;
	let ui = -1;
	for (let i = ai - 1; i >= 0; i--)
		if (roleOf(hist[i]) === "user") {
			ui = i;
			break;
		}
	if (ui < 0) return null;
	return { user: textOf(hist[ui]) };
}

// Prefix for any user-role message pix itself synthesizes and hands to
// the agent as if the user typed it (e.g. setup.go's onboardingKickoff, see
// services/host/cmd/pix/setup.go's generatedInputMarker). It is NOT the
// user talking — without this filter the watcher model observed the kickoff
// line as a real statement and invented pix facts/events from it. Only a
// message that literally carries the marker is skipped; a real user message
// that merely mentions setup/onboarding is captured normally.
const GENERATED_INPUT_PREFIX = "[pix-generated:";

// Pure predicate: should this user-role text be handed to the watcher at all?
// Exported so tests can exercise the filtering rules without a fake ctx/session.
export function shouldCaptureUserText(text: string): boolean {
	const user = text?.trim() ?? "";
	if (user.startsWith(GENERATED_INPUT_PREFIX)) return false; // machine-generated, not the user
	if (user.startsWith("/")) return false; // slash command
	if (user.length < 12) return false; // too trivial to be worth a watcher call
	return true;
}

let lastSent = ""; // last exchange the daemon positively accepted
let inFlight = ""; // dedup agent_end vs. next-turn capture while one POST is pending

async function capture(ctx: any, awaited: boolean): Promise<void> {
	const ex = lastCompleteExchange(entries(ctx));
	if (!ex) return;
	const user = ex.user?.trim() ?? "";
	if (!shouldCaptureUserText(user)) return;
	// The dedup key is the payload's identity, not the exchange's: it hashes the
	// user text ALONE (assistant text was dropped as an input above, so hashing
	// it here would only rehash something no longer sent). That is a deliberate
	// trade-off, not an oversight: two genuinely identical prompts sent back to
	// back in the same session collide on this key, so the second is skipped as
	// a dup even though it is a distinct turn with a distinct (if repetitive)
	// exchange. Accepted because a real duplicate observe — the
	// before_agent_start retry racing agent_end for the SAME completed exchange,
	// the case this key exists to catch — is far more common than a user
	// deliberately repeating themselves verbatim, and the cost of misfiring on
	// that rare case is one skipped watcher call, not a lost fact.
	const key = createHash("sha256").update(ex.user).digest("hex").slice(0, 16);
	if (key === lastSent || key === inFlight) return;
	inFlight = key;
	const params = { user: ex.user, project: currentProject(ctx), profile: ACTIVE_PROFILE };
	// Observability: a failed observe POST (host daemon unreachable, timeout) used
	// to be swallowed silently, so a broken capture pipe was invisible. Log it once
	// per session to stderr so "memory didn't store" is diagnosable.
	const onErr = (e: any) => {
		if (!warnedPostErr) {
			warnedPostErr = true;
			try {
				process.stderr.write(`[memory] capture POST to ${MEMORY_URL} failed: ${e?.message ?? e}\n`);
			} catch {
				/* best-effort */
			}
		}
	};
	const send = async () => {
		try {
			const response = await call("observe", params);
			warnIfCaptureOff(response);
			lastSent = key; // only a completed RPC earns deduplication
		} catch (e) {
			onErr(e); // leave lastSent untouched so the next awaited hook retries
		} finally {
			if (inFlight === key) inFlight = "";
		}
	};
	if (awaited) await send();
	else void send();
}

export default function (pi: any) {
	// Reliable: capture the previous completed exchange on the awaited hook.
	pi.on("before_agent_start", async (_event: any, ctx: any) => {
		await safe(() => capture(ctx, true));
		return undefined; // don't touch the prompt; recall extension owns that
	});
	// Best-effort: catch the final exchange of an interactive session.
	pi.on("agent_end", async (_event: any, ctx: any) => {
		await safe(() => capture(ctx, false));
	});
	// Reliable end-of-session flush: the previous hooks capture an exchange on the
	// NEXT turn (before_agent_start), so the LAST thing you say never persists
	// unless you send another message. agent_end is best-effort (fire-and-forget)
	// and races process teardown. session_shutdown is the last awaited hook before
	// exit, so AWAIT the capture here (awaited=true) to flush the final exchange.
	// Dedup (lastSent) means this is a no-op if a next turn already captured it.
	pi.on("session_shutdown", async (_event: any, ctx: any) => {
		await safe(() => capture(ctx, true));
	});
}
