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
	let requests = 0;
	await withServer((_req, res) => {
		requests++;
		if (requests === 1) {
			res.writeHead(502, { "content-type": "text/plain" });
			res.end("dial tcp: connection refused");
			return;
		}
		res.writeHead(200, { "content-type": "application/json" });
		res.end(JSON.stringify({ jsonrpc: "2.0", id: 2, result: { accepted: true } }));
	}, async (url) => {
		const prior = process.env.MEMORY_URL;
		process.env.MEMORY_URL = url;
		try {
			const mod = await import(`../extensions/memory-capture.ts?retry=${Date.now()}`);
			let hook;
			mod.default({ on(event, fn) { if (event === "before_agent_start") hook = fn; } });
			const ctx = { sessionManager: { getBranch: () => [
				{ message: { role: "user", content: "this exchange should retry after a temporary outage" } },
				{ message: { role: "assistant", content: "the answer is complete" } },
			] } };
			const write = process.stderr.write;
			process.stderr.write = () => true;
			try {
				await hook({}, ctx);
				await hook({}, ctx);
			} finally {
				process.stderr.write = write;
			}
			assert.equal(requests, 2, "the failed first POST must not suppress the retry");
		} finally {
			if (prior === undefined) delete process.env.MEMORY_URL;
			else process.env.MEMORY_URL = prior;
		}
	});
});
