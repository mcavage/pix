// Unit tests for extensions/memory-capture.ts's capture-filtering predicate.
// Run: node --test tests/
//
// Root cause this guards: setup.go's onboardingKickoff is a synthesized
// user-role message (not something the user typed), and the watcher model
// observed it like a real statement and invented pix facts/events from
// it. The fix is a marker contract: setup.go prefixes any generated message
// with "[pix-generated:...] " and shouldCaptureUserText skips it.
import assert from "node:assert";
import http from "node:http";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

const { shouldCaptureUserText, testPostJson } = await import("../extensions/memory-capture.ts");

async function withServer(handler, fn) {
	const server = http.createServer(handler);
	await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
	try {
		const { port } = server.address();
		await fn(`http://127.0.0.1:${port}`);
	} finally {
		await new Promise((resolve) => server.close(resolve));
	}
}

// The workspace + module-load machinery needed to exercise CAPTURE_MODE: a
// temp dir with .pix/memory-capture set to `mode` (omit for the
// un-launched, no-marker case), a fresh module import with process.cwd()
// pointed at it (CAPTURE_MODE is read once at load). Returns the
// before_agent_start hook, callable as many times as a test needs.
let seq = 0;
async function loadCaptureHook(mode, memoryUrl) {
	const dir = mkdtempSync(join(tmpdir(), "pix-capture-"));
	if (mode !== undefined) {
		mkdirSync(join(dir, ".pix"), { recursive: true });
		writeFileSync(join(dir, ".pix", "memory-capture"), mode + "\n");
	}
	const prevCwd = process.cwd();
	const priorUrl = process.env.MEMORY_URL;
	process.chdir(dir);
	process.env.MEMORY_URL = memoryUrl;
	let mod;
	try {
		mod = await import(`../extensions/memory-capture.ts?case=${seq++}`);
	} finally {
		process.chdir(prevCwd);
		if (priorUrl === undefined) delete process.env.MEMORY_URL;
		else process.env.MEMORY_URL = priorUrl;
	}
	let hook;
	mod.default({ on(event, fn) { if (event === "before_agent_start") hook = fn; } });
	return hook;
}

function exchangeCtx(user, assistant) {
	return { sessionManager: { getBranch: () => [
		{ message: { role: "user", content: user } },
		{ message: { role: "assistant", content: assistant } },
	] } };
}

async function runCapture(mode, memoryUrl) {
	const hook = await loadCaptureHook(mode, memoryUrl);
	await hook({}, exchangeCtx("a perfectly normal, long-enough user message", "acknowledged"));
}

test("a pix-generated message (onboarding kickoff shape) is never captured", () => {
	assert.equal(
		shouldCaptureUserText(
			"[pix-generated:onboarding] I just ran pix setup — give me the guided walkthrough and help me get started.",
		),
		false,
	);
});

test("any [pix-generated:...] prefixed message is skipped, not just onboarding", () => {
	assert.equal(shouldCaptureUserText("[pix-generated:some-other-flow] hello there, this is long enough"), false);
});

test("a slash command is not captured", () => {
	assert.equal(shouldCaptureUserText("/recall docker sandboxes"), false);
});

test("text under 12 chars is not captured", () => {
	assert.equal(shouldCaptureUserText("hi there"), false);
});

test("a normal user message is captured", () => {
	assert.equal(shouldCaptureUserText("i like cheese"), true);
});

test("a real message that merely mentions setup/onboarding is still captured (no broad skip)", () => {
	assert.equal(shouldCaptureUserText("I just ran pix setup and it broke on step 3"), true);
});

test("capture transport reports a plain-text gateway failure as HTTP, not a JSON parser error", async () => {
	await withServer((_req, res) => {
		res.writeHead(502, { "content-type": "text/plain" });
		res.end("dial tcp 127.0.0.1:11435: connect: connection refused");
	}, async (url) => {
		await assert.rejects(
			testPostJson(url, { jsonrpc: "2.0", method: "observe" }, 500),
			/memory service HTTP 502: dial tcp .*connection refused/,
		);
	});
});

test("capture transport gives a stable diagnostic for non-JSON 2xx responses", async () => {
	await withServer((_req, res) => {
		res.writeHead(200, { "content-type": "text/plain" });
		res.end("not json");
	}, async (url) => {
		await assert.rejects(
			testPostJson(url, { jsonrpc: "2.0", method: "observe" }, 500),
			/memory service returned a non-JSON success response/,
		);
	});
});

test("a failed observe POST is retried on the next awaited hook instead of being deduplicated as sent", async () => {
	let observeRequests = 0;
	await withServer((req, res) => {
		observeRequests++;
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			if (observeRequests === 1) {
				res.writeHead(502, { "content-type": "text/plain" });
				res.end("dial tcp: connection refused");
				return;
			}
			const parsed = JSON.parse(body);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(JSON.stringify({ jsonrpc: "2.0", id: parsed.id, result: { accepted: true } }));
		});
	}, async (url) => {
		const hook = await loadCaptureHook("experimental-auto", url);
		const ctx = exchangeCtx("this exchange should retry after a temporary outage", "the answer is complete");
		const write = process.stderr.write;
		process.stderr.write = () => true;
		try {
			await hook({}, ctx);
			await hook({}, ctx);
		} finally {
			process.stderr.write = write;
		}
		assert.equal(observeRequests, 2, "the failed first POST must not suppress the retry");
	});
});

// Wire-level sentinel: the observe RPC params object must carry EXACTLY
// profile/project/user, no more and no less (no session id, no assistant text).
test("the observe RPC params object has exactly profile/project/user", async () => {
	let received;
	await withServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			received = JSON.parse(body);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(JSON.stringify({ jsonrpc: "2.0", id: received.id, result: { accepted: true } }));
		});
	}, async (url) => {
		await runCapture("experimental-auto", url);
		assert.ok(received, "observe POST must have been sent");
		assert.equal(received.method, "observe");
		assert.deepEqual(Object.keys(received.params).sort(), ["profile", "project", "user"]);
	});
});

// The headline requirement: explicit (an absent marker, or any garbled value)
// sends ZERO observe requests — not even one the host would refuse.
test("explicit mode (absent or garbled marker) sends zero observe requests", async () => {
	for (const mode of [undefined, "explicit", "always-on-please"]) {
		let observeCalls = 0;
		await withServer((_req, res) => {
			observeCalls++;
			res.writeHead(200, { "content-type": "application/json" }).end("{}");
		}, async (url) => {
			await runCapture(mode, url);
		});
		assert.equal(observeCalls, 0, `mode ${mode} must never call observe`);
	}
});

test("experimental-auto mode sends exactly one observe call for a completed exchange", async () => {
	let observeCalls = 0;
	await withServer((_req, res) => {
		observeCalls++;
		res.writeHead(200, { "content-type": "application/json" }).end("{}");
	}, async (url) => {
		await runCapture("experimental-auto", url);
	});
	assert.equal(observeCalls, 1, "experimental-auto mode must call observe");
});

// ── not-accepted capture reasons reach the USER, once ──────────────────────
// The host answers {accepted:false, reason} when it refuses a capture (no
// watcher model pulled, daily budget exhausted, capture saturated). That
// reason used to go to raw stderr, which in the shipped fullscreen TUI is
// where messages go to die — so the one line explaining "capture is on but
// nothing is being stored" was invisible. It goes through ctx.ui.notify now,
// still exactly once per session.
function notifyCtx(user, assistant, notes) {
	return { ...exchangeCtx(user, assistant), ui: { notify: (msg, level) => notes.push({ msg, level }) } };
}

test("a not-accepted observe surfaces its reason through ctx.ui.notify, once per session", async () => {
	await withServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			const parsed = JSON.parse(body);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(
				JSON.stringify({
					jsonrpc: "2.0",
					id: parsed.id,
					result: { accepted: false, reason: "daily watcher capture budget exhausted (max 10 stored rows/day); recall still works" },
				}),
			);
		});
	}, async (url) => {
		const hook = await loadCaptureHook("experimental-auto", url);
		const notes = [];
		const write = process.stderr.write;
		let stderrLines = 0;
		process.stderr.write = () => (stderrLines++, true);
		try {
			await hook({}, notifyCtx("the first exchange the host refuses to capture", "ok", notes));
			await hook({}, notifyCtx("a second, different exchange it also refuses", "ok", notes));
		} finally {
			process.stderr.write = write;
		}
		assert.equal(notes.length, 1, "the refusal reason must be surfaced exactly once per session");
		assert.match(notes[0].msg, /capture not accepted: daily watcher capture budget exhausted/);
		assert.equal(stderrLines, 0, "with a ui present nothing may go to raw stderr");
	});
});

test("a failed observe POST notifies through ctx.ui.notify too", async () => {
	await withServer((_req, res) => {
		res.writeHead(502, { "content-type": "text/plain" });
		res.end("dial tcp: connection refused");
	}, async (url) => {
		const hook = await loadCaptureHook("experimental-auto", url);
		const notes = [];
		await hook({}, notifyCtx("an exchange the daemon never answers properly", "ok", notes));
		assert.equal(notes.length, 1);
		assert.match(notes[0].msg, /capture POST to .* failed/);
	});
});

// Print mode / tests have no ctx.ui: the message must still go somewhere
// rather than being dropped on the floor.
test("without a ctx.ui the not-accepted reason falls back to stderr", async () => {
	await withServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			const parsed = JSON.parse(body);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(JSON.stringify({ jsonrpc: "2.0", id: parsed.id, result: { accepted: false, reason: "watcher model unavailable" } }));
		});
	}, async (url) => {
		const hook = await loadCaptureHook("experimental-auto", url);
		const lines = [];
		const write = process.stderr.write;
		process.stderr.write = (chunk) => (lines.push(String(chunk)), true);
		try {
			await hook({}, exchangeCtx("an exchange sent from a ctx with no ui at all", "ok"));
		} finally {
			process.stderr.write = write;
		}
		assert.equal(lines.length, 1);
		assert.match(lines[0], /capture not accepted: watcher model unavailable/);
	});
});
