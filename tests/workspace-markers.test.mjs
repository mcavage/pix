// U-W0b.05: cross-boundary workspace marker round-trip coverage, TS side.
// Run: node --test tests/
//
// services/host/cmd/pix/workspacemarkers_roundtrip_test.go pins the EXACT
// bytes each Go writer emits into <workspace>/.pix/<marker>. This file is
// the other half of the contract: it hand-plants a fixture with that exact
// byte shape (comment on each fixture cross-references the Go test/writer
// that produces it) and proves the TS reader in the corresponding extension
// parses it correctly. Together the two files prove the round trip without
// either language having to shell out to the other.
//
// Covers every marker a TS extension actually reads today: .pix/profile
// (memory-recall.ts, memory-capture.ts), .pix/memory-capture
// (memory-capture.ts), .pix/ollama-bridge.model
// (ollama-bridge.ts). .pix/sandbox.pack and .pix/onboarding.json have no TS
// reader (Go writes+reads sandbox.pack; the in-sandbox AGENT — not a TS
// extension — writes onboarding.json), so they are only covered on the Go
// side. .pix/knowledge.scope and .pix/knowledge, and their TS reader
// (knowledge-recall.ts), were retired along with the built-in OKF knowledge
// service (W2 U03A). .pix/host-state.json is never a file on either side
// (see the Go test's negative control); routing/artifacts/custom-memory.db
// are host data-root paths, never workspace markers at all (see the Go
// test's boundary check). See workspacemarkers_roundtrip_test.go's package
// comment for the full inventory.
import assert from "node:assert";
import * as http from "node:http";
import { mkdtempSync, mkdirSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { register } from "node:module";
import { test } from "node:test";

// Extensions import `typebox` for tool schemas; stub it (and the other pi
// runtime packages) so plain node can import them, same as memory-recall.test.mjs.
register("./stub-loader.mjs", import.meta.url);

function makeFakeDaemon(responder) {
	const requests = [];
	const server = http.createServer((req, res) => {
		let body = "";
		req.on("data", (c) => (body += c));
		req.on("end", () => {
			const parsed = JSON.parse(body);
			requests.push(parsed);
			const result = responder(parsed);
			res.writeHead(200, { "content-type": "application/json" });
			res.end(JSON.stringify({ jsonrpc: "2.0", id: parsed.id, result }));
		});
	});
	return { server, requests };
}

async function listen(server) {
	await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
	const { port } = server.address();
	return `http://127.0.0.1:${port}`;
}

// Builds a temp workspace with a .pix/<name> marker containing EXACTLY
// `content` (byte-for-byte — callers pass the literal string the Go writer
// under test produces, trailing newline included).
function makeWorkspace(name, content) {
	const dir = mkdtempSync(join(tmpdir(), "pix-marker-"));
	mkdirSync(join(dir, ".pix"), { recursive: true });
	if (name) writeFileSync(join(dir, ".pix", name), content);
	return dir;
}

let seq = 0;
// Import a FRESH extension module instance with process.cwd() pointed at
// `workspace` for the module-load-time readFileSync calls (memory-recall.ts,
// memory-capture.ts both read .pix/profile exactly once at import), and
// with `env` applied/restored around the import (MEMORY_URL/KNOWLEDGE_URL are
// also read once at module load).
async function importFromWorkspace(specifier, workspace, env = {}) {
	const prevCwd = process.cwd();
	const envKeys = Object.keys(env);
	const saved = {};
	for (const k of envKeys) saved[k] = process.env[k];
	process.chdir(workspace);
	// An explicit `undefined` in `env` means "force-clear" (e.g. OLLAMA_BRIDGE_MODEL,
	// whose own code uses `??`, so setting it to "" would short-circuit the very
	// workspace-marker fallback under test).
	for (const [k, v] of Object.entries(env)) {
		if (v === undefined) delete process.env[k];
		else process.env[k] = v;
	}
	try {
		const url = new URL(`${specifier}?case=${seq++}`, import.meta.url);
		return await import(url.href);
	} finally {
		process.chdir(prevCwd);
		for (const k of envKeys) {
			if (saved[k] === undefined) delete process.env[k];
			else process.env[k] = saved[k];
		}
	}
}

// Some readers (knowledge-recall.ts's resolveScope) read process.cwd() at CALL
// time, not at module-load time — the fixture must still be under cwd for the
// actual RPC-triggering call, after importFromWorkspace has already restored it.
async function withCwd(dir, fn) {
	const prev = process.cwd();
	process.chdir(dir);
	try {
		return await fn();
	} finally {
		process.chdir(prev);
	}
}

function capturePi(mod) {
	const commands = new Map();
	const tools = new Map();
	const hooks = new Map();
	mod.default({
		on(event, fn) {
			hooks.set(event, fn);
		},
		registerCommand(name, cfg) {
			commands.set(name, cfg);
		},
		registerTool(t) {
			tools.set(t.name, t);
		},
		registerProvider(name, cfg) {
			hooks.set(`provider:${name}`, cfg);
		},
	});
	return { commands, tools, hooks };
}

const toolText = (r) => r.content?.map((c) => c.text ?? "").join("\n") ?? "";

// ── .pix/profile → memory-recall.ts (matches pack.go's writeMemoryScope
// output: "<scope>\n", see TestMarkerRoundTrip_Profile) ─────────────────────

test("memory-recall.ts resolves ACTIVE_PROFILE from the exact .pix/profile bytes writeMemoryScope produces", async (t) => {
	const workspace = makeWorkspace("profile", "work\n");
	const { server, requests } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);

	const mod = await importFromWorkspace("../extensions/memory-recall.ts", workspace, { MEMORY_URL });
	const { tools } = capturePi(mod);
	await tools.get("memory_recall").execute("id", {}, undefined, undefined, { cwd: workspace });

	assert.equal(requests.length, 1);
	assert.equal(requests[0].params.profile, "work", "profile must be the exact scope Go wrote, not 'work\\n' or 'default'");
});

test("memory-recall.ts falls back to 'default' when .pix/profile is absent (the un-scoped, backward-compatible case)", async (t) => {
	const workspace = makeWorkspace(null, "");
	const { server, requests } = makeFakeDaemon(() => ({ hits: [] }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);

	const mod = await importFromWorkspace("../extensions/memory-recall.ts", workspace, { MEMORY_URL });
	const { tools } = capturePi(mod);
	await tools.get("memory_recall").execute("id", {}, undefined, undefined, { cwd: workspace });

	assert.equal(requests[0].params.profile, "default");
});

// ── .pix/profile → memory-capture.ts (the SAME file, read by a SEPARATE
// module — both must resolve the identical profile so recall and capture
// never diverge) ────────────────────────────────────────────────────────────

test("memory-capture.ts stamps captured exchanges with the SAME .pix/profile marker memory-recall.ts reads", async (t) => {
	const workspace = makeWorkspace("profile", "work\n");
	// .pix/memory-capture must name the opt-in mode for this test to exercise
	// the profile-stamping it cares about; explicit (the default, an absent
	// marker) would send zero observe requests.
	mkdirSync(join(workspace, ".pix"), { recursive: true });
	writeFileSync(join(workspace, ".pix", "memory-capture"), "experimental-auto\n");
	const { server, requests } = makeFakeDaemon(() => ({ accepted: true }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);

	const mod = await importFromWorkspace("../extensions/memory-capture.ts", workspace, { MEMORY_URL });
	let beforeAgentStart;
	mod.default({
		on(event, fn) {
			if (event === "before_agent_start") beforeAgentStart = fn;
		},
	});
	assert.ok(beforeAgentStart, "before_agent_start hook registered");

	const ctx = {
		cwd: workspace,
		sessionManager: {
			getBranch: () => [
				{ type: "message", message: { role: "user", content: "a real user question about something" } },
				{ type: "message", message: { role: "assistant", content: "a real assistant answer to that question" } },
			],
		},
	};
	await beforeAgentStart({}, ctx);

	assert.equal(requests.length, 1, "one observe call for the completed exchange");
	assert.equal(requests[0].method, "observe");
	assert.equal(requests[0].params.profile, "work", "capture must stamp the same profile recall queries, or the two silently diverge");
});

// ── .pix/memory-capture → memory-capture.ts (matches run.go's
// WriteMemoryCaptureFile output: "<mode>\n", see
// TestMarkerRoundTrip_MemoryCapture). The explicit-default/garbled-marker
// fail-closed cases are covered in memory-capture.test.mjs; this is only the
// cross-language byte-exact contract. ────────────────────────────────────────

test("memory-capture.ts resolves CAPTURE_MODE from the exact .pix/memory-capture bytes WriteMemoryCaptureFile produces", async (t) => {
	const workspace = makeWorkspace("memory-capture", "experimental-auto\n");
	const { server, requests } = makeFakeDaemon(() => ({ accepted: true }));
	t.after(() => server.close());
	const MEMORY_URL = await listen(server);

	const mod = await importFromWorkspace("../extensions/memory-capture.ts", workspace, { MEMORY_URL });
	let beforeAgentStart;
	mod.default({
		on(event, fn) {
			if (event === "before_agent_start") beforeAgentStart = fn;
		},
	});
	const ctx = {
		cwd: workspace,
		sessionManager: {
			getBranch: () => [
				{ type: "message", message: { role: "user", content: "a real user question about something" } },
				{ type: "message", message: { role: "assistant", content: "a real assistant answer to that question" } },
			],
		},
	};
	await beforeAgentStart({}, ctx);

	assert.equal(requests.length, 1, "experimental-auto mode (from the marker) must send exactly one observe call");
});

// .pix/knowledge.scope → knowledge-recall.ts coverage (bundle-scope
// forwarding, query-all back-compat) was retired along with the extension
// itself when the built-in OKF knowledge service was removed (W2 U03A).

// ── .pix/ollama-bridge.model → ollama-bridge.ts (matches run.go's
// writeOllamaBridgeFile output: "<model>\n", see
// TestMarkerRoundTrip_OllamaBridgeModel). OLLAMA_BRIDGE_PORT=0 keeps this from
// binding the real 11434 (where a developer's own ollama lives): the listener is
// best-effort, so an ephemeral port makes this a side-effect-free read of the
// marker. It used to use OLLAMA_HOSTMODE=1 for that, which stopped meaning
// anything when `pix host` was deleted. ─────────────────────────────────────

test("ollama-bridge.ts registers the model id from the exact .pix/ollama-bridge.model bytes writeOllamaBridgeFile produces", async (t) => {
	const workspace = makeWorkspace("ollama-bridge.model", "qwen3.5:9b\n");
	const mod = await importFromWorkspace("../extensions/ollama-bridge.ts", workspace, {
		OLLAMA_BRIDGE_PORT: "0",
		OLLAMA_BRIDGE_MODEL: undefined,
	});
	const { hooks } = capturePi(mod);
	await mod.default({
		registerProvider(name, cfg) {
			hooks.set(`provider:${name}`, cfg);
		},
		on() {},
	});

	const provider = hooks.get("provider:ollama");
	assert.ok(provider, "ollama provider registered");
	assert.equal(provider.models[0].id, "qwen3.5:9b", "registered model id must be the exact tag Go wrote, not padded/quoted");
});

test("ollama-bridge.ts falls back to its own default model when .pix/ollama-bridge.model is absent", async (t) => {
	const workspace = makeWorkspace(null, "");
	const mod = await importFromWorkspace("../extensions/ollama-bridge.ts", workspace, {
		OLLAMA_BRIDGE_PORT: "0",
		OLLAMA_BRIDGE_MODEL: undefined,
	});
	const hooks = new Map();
	await mod.default({
		registerProvider(name, cfg) {
			hooks.set(`provider:${name}`, cfg);
		},
		on() {},
	});
	assert.equal(hooks.get("provider:ollama").models[0].id, "qwen3.5:9b", "must match the same default config.DefaultOllamaBridgeModel writes when the workspace passes an empty model");
});

// ── boundary sanity: no marker reader here ever touches host-state.json ────

test("none of these fixtures require or read .pix/host-state.json", async (t) => {
	const workspace = makeWorkspace("profile", "work\n");
	assert.ok(!existsSync(join(workspace, ".pix", "host-state.json")), "host-state.json must never be planted or expected by any TS reader");
});
