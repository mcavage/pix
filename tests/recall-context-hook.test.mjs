// FF4b (AC-P0-104) — the recall message must survive the `context` hook.
//
// The transport fix appends recall to the message list instead of rewriting the
// system prompt, which keeps the whole request prefix byte-identical. Removing
// an already-sent message undoes that and then some: it moves the cache
// divergence point BACKWARDS, so the turn pays for everything after the removal
// as well. A stripped recall message is strictly worse than the bug being fixed.
//
// This is the invariant a well-meaning follow-up will break, because AGENTS.md
// documents exactly the opposite pattern for display-only annotations ("use
// deliverAs:'nextTurn' and strip them in the `context` hook by customType").
// So the test does two things:
//
//   1. discovers EVERY extension that registers a `context` hook, loads it,
//      runs it over a message list containing a `pix-recalled-context` message,
//      and asserts the message survives; and
//   2. runs a deliberately hostile hook through the SAME harness and asserts it
//      is caught — otherwise a harness that finds no hooks would pass vacuously
//      forever and nobody would know.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { register } from "node:module";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { RECALL_CUSTOM_TYPE } from "../lib/recall-message.ts";

register("./stub-loader.mjs", import.meta.url);

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const extensionsDir = path.join(repoRoot, "extensions");

/** A message list shaped like what pi hands the `context` hook. */
function messageList() {
	return [
		{ role: "user", content: "do the thing" },
		{ role: "assistant", content: "on it" },
		{ role: "user", customType: RECALL_CUSTOM_TYPE, display: false, content: "## From memory (recalled for this task)\n- a fact" },
		{ role: "user", customType: "pi-stack-todo-cleared", content: "cleared" },
		{ role: "user", content: "next" },
	];
}

/**
 * Run one `context` handler and report whether the recall message survived.
 * Mirrors pi's contract: the handler gets `{ messages }` and may return
 * `{ messages }` to replace the list, or nothing to leave it alone.
 */
async function survivesHook(handler) {
	const before = messageList();
	const result = await handler({ messages: before.map((m) => ({ ...m })) }, fakeCtx());
	const after = Array.isArray(result?.messages) ? result.messages : before;
	return after.some((m) => m?.customType === RECALL_CUSTOM_TYPE);
}

function fakeCtx() {
	return {
		cwd: repoRoot,
		model: { id: "test" },
		isIdle: () => true,
		getContextUsage: () => ({}),
		ui: { notify() {}, setStatus() {}, setWidget() {}, setWorkingMessage() {} },
		sessionManager: { history: () => [], getSessionId: () => "test" },
	};
}

/** Minimal `pi` that records every hook an extension registers. */
function fakePi() {
	const hooks = new Map();
	return {
		hooks,
		on(name, fn) {
			if (!hooks.has(name)) hooks.set(name, []);
			hooks.get(name).push(fn);
		},
		registerCommand() {},
		registerShortcut() {},
		registerTool() {},
		registerFlag() {},
		getAllTools: () => [],
		sendMessage() {},
		events: { emit() {}, on() {} },
	};
}

/**
 * Extensions whose SOURCE registers a `context` hook. Scanning first means the
 * test never has to load (and therefore never has to neutralize the side
 * effects of) extensions that cannot possibly touch the message list — e.g.
 * ollama-bridge.ts binds a port at load.
 */
function extensionsWithContextHook() {
	return fs
		.readdirSync(extensionsDir)
		.filter((f) => f.endsWith(".ts"))
		.filter((f) => /\bpi\s*\.\s*on\s*\(\s*["'`]context["'`]/.test(fs.readFileSync(path.join(extensionsDir, f), "utf8")))
		.sort();
}

test("every extension `context` hook leaves the recall message in place", async () => {
	const files = extensionsWithContextHook();
	const stripped = [];
	for (const file of files) {
		const mod = await import(new URL(`../extensions/${file}`, import.meta.url).href);
		const pi = fakePi();
		await mod.default(pi);
		for (const [i, handler] of (pi.hooks.get("context") ?? []).entries()) {
			if (!(await survivesHook(handler))) stripped.push(`${file} (context handler #${i})`);
		}
	}
	assert.deepEqual(
		stripped,
		[],
		`extension(s) strip ${RECALL_CUSTOM_TYPE} from the message list: ${stripped.join(", ")}.\n` +
			"Recall is append-only. Removing an already-sent message moves the provider's prefix-cache\n" +
			"divergence point BACKWARDS, which costs more than the system-prompt rewrite it replaced.\n" +
			"AGENTS.md's strip-by-customType guidance for display-only annotations does NOT apply here.",
	);
});

test("the harness catches a hook that does strip it (so a clean pass means something)", async () => {
	// The naive application of AGENTS.md's display-only-annotation pattern.
	const hostile = (event) => ({ messages: event.messages.filter((m) => !m.customType) });
	assert.equal(await survivesHook(hostile), false);

	// And the shapes that must still count as surviving.
	assert.equal(await survivesHook(() => undefined), true, "a hook that returns nothing must not be read as stripping");
	assert.equal(await survivesHook((e) => ({ messages: e.messages })), true);
	assert.equal(
		await survivesHook((e) => ({ messages: e.messages.filter((m) => m.customType !== "pi-stack-todo-cleared") })),
		true,
		"filtering a DIFFERENT customType is fine",
	);
});

test("the set of extensions with a context hook is pinned", async () => {
	// Today: none. Adding one puts this test in the author's diff, which is the
	// point — it is the moment they need to read the assertion above.
	assert.deepEqual(extensionsWithContextHook(), []);
});

test("the recall custom type is the new name, and the frozen legacy ones are untouched", () => {
	assert.equal(RECALL_CUSTOM_TYPE, "pix-recalled-context");
	// AC-P0-409: persisted customType values already written into .pi-sessions/
	// are frozen forever. Renaming pi-stack-todo-cleared silently resurrects
	// cleared todos after compaction or resume.
	const src = fs.readFileSync(path.join(extensionsDir, "compaction-continuation.ts"), "utf8");
	assert.match(src, /"pi-stack-todo-cleared"/);
});
