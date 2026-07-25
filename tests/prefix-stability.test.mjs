// U-W0b.14 (AC-P0-108, 109, 110) — FF5a/b/c: is the request prefix stable?
//
// A provider only reuses a cached prefill up to the FIRST byte that changed.
// Two things can move that point: rewriting the system prompt (divergence at
// byte 0 — nothing is reusable) and mutating or removing an earlier message
// (divergence at that message — everything after it is re-billed). The recall
// transport change exists to stop the first; this measures both.
//
// MEASUREMENT UNIT IS `before_provider_request`, one record per provider call,
// NOT per user turn. A user turn can drive several provider calls (tool result
// continuations, retries) and every one of them pays its own prefill, so per
// turn would undercount exactly the requests that cost the most.
//
// THE NEGATIVE CONTROL IS THE POINT. `session-systemprompt-mutation.ndjson` is
// the old transport recorded through the same tap. If the gate does not fail on
// it, the gate is measuring nothing — so that is asserted too.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const fixtureDir = path.join(repoRoot, "tests/fixtures/monitor");

/** AC-P0-109: at least 95% of evaluated request pairs keep a strict prefix. */
const PREFIX_STABILITY_GATE = 0.95;

function load(name) {
	return fs
		.readFileSync(path.join(fixtureDir, name), "utf8")
		.split("\n")
		.filter(Boolean)
		.map((l) => JSON.parse(l))
		.filter((e) => e.kind === "provider_request");
}

/**
 * FF5a/b/c over one session's provider_request records.
 *
 * Compaction records are EXCLUDED from the pair measurement, and their indices
 * are returned so the assertion can name them (AC-P0-110). Compaction rewrites
 * the history by design: counting it as instability would make the metric
 * measure "did the user have a long session" instead of "does the transport
 * append". The exclusion is stated where it is asserted, never hidden inside a
 * filter that a reader has to go find.
 */
export function analyzePrefixStability(records) {
	const systemPromptHashes = [...new Set(records.map((r) => r.summary.systemPromptHash))];
	const compactionIndices = records.map((r, i) => (r.trigger === "compaction" ? i : -1)).filter((i) => i >= 0);

	const divergences = [];
	let evaluated = 0;
	let stable = 0;
	for (let i = 1; i < records.length; i++) {
		if (compactionIndices.includes(i)) continue;
		const prev = records[i - 1].summary.messageHashes;
		const cur = records[i].summary.messageHashes;
		assert.ok(Array.isArray(prev) && Array.isArray(cur), "fixtures must carry summary.messageHashes");
		evaluated++;
		if (cur.length < prev.length) {
			divergences.push({ pair: [i - 1, i], at: cur.length, why: "message list shrank without a compaction trigger" });
			continue;
		}
		const at = prev.findIndex((h, j) => cur[j] !== h);
		if (at === -1) stable++;
		else divergences.push({ pair: [i - 1, i], at, why: `msgs[${at}] changed` });
	}

	return {
		requests: records.length,
		systemPromptHashes,
		systemPromptStable: systemPromptHashes.length === 1,
		compactionIndices,
		evaluatedPairs: evaluated,
		stablePairs: stable,
		stability: evaluated ? stable / evaluated : 1,
		divergences,
	};
}

// ── FF5a ─────────────────────────────────────────────────────────────────────

test("systemPromptHash is constant across a >=20-turn append-only session", () => {
	const a = analyzePrefixStability(load("session-append-only.ndjson"));
	assert.ok(a.requests >= 20, `need a >=20-request session, got ${a.requests}`);
	assert.equal(
		a.systemPromptHashes.length,
		1,
		`the system prompt changed ${a.systemPromptHashes.length} times across ${a.requests} requests. ` +
			"Recall must be appended as a message, never written into the system prompt: a per-turn system prompt " +
			"moves the cache divergence point to byte 0, so nothing in the request is reusable.",
	);
});

test("compaction does not disturb the system prompt", () => {
	const a = analyzePrefixStability(load("session-compaction.ndjson"));
	assert.equal(a.systemPromptHashes.length, 1);
});

// ── FF5b ─────────────────────────────────────────────────────────────────────

test("the message prefix is stable across consecutive provider requests", () => {
	const a = analyzePrefixStability(load("session-append-only.ndjson"));
	assert.equal(a.compactionIndices.length, 0, "this fixture has no compaction");
	assert.ok(
		a.stability >= PREFIX_STABILITY_GATE,
		`prefix stability ${(a.stability * 100).toFixed(1)}% over ${a.evaluatedPairs} request pairs, ` +
			`gate ${(PREFIX_STABILITY_GATE * 100).toFixed(0)}%. Divergences: ${JSON.stringify(a.divergences)}`,
	);
	// An append-only transport is not 95% stable, it is 100% stable. Anything
	// less is a real defect wearing the gate's headroom.
	assert.equal(a.stability, 1, `expected a strict prefix on every pair, got ${JSON.stringify(a.divergences)}`);
});

// ── FF5c ─────────────────────────────────────────────────────────────────────

test("compaction turns are excluded, and the assertion names which ones", () => {
	const a = analyzePrefixStability(load("session-compaction.ndjson"));
	assert.deepEqual(a.compactionIndices, [14], "the fixture compacts at request index 14 (turn 15)");
	assert.equal(a.evaluatedPairs, a.requests - 1 - a.compactionIndices.length);

	const message =
		`prefix stability ${(a.stability * 100).toFixed(1)}% over ${a.evaluatedPairs} pairs ` +
		`(gate ${(PREFIX_STABILITY_GATE * 100).toFixed(0)}%); ` +
		`excluded compaction request index/indices: ${a.compactionIndices.join(", ")}`;
	// The exclusion is named in the message a failure would print, not buried.
	assert.match(message, /excluded compaction request index\/indices: 14/);
	assert.ok(a.stability >= PREFIX_STABILITY_GATE, message);
	assert.equal(a.stability, 1, message);
});

test("a compaction that was NOT excluded would have failed the gate", () => {
	// Same fixture, exclusion switched off: the compaction pair diverges, which
	// is why it is excluded — and proof the exclusion is load-bearing rather
	// than decorative.
	const records = load("session-compaction.ndjson").map((r) => ({ ...r, trigger: "user" }));
	const a = analyzePrefixStability(records);
	assert.equal(a.compactionIndices.length, 0);
	assert.ok(a.divergences.length >= 1, "the compaction pair must show up as a divergence when not excluded");
	assert.equal(a.divergences[0].pair[1], 14);
});

// ── the negative control ─────────────────────────────────────────────────────

test("the old system-prompt transport fails FF5a, so the gate measures something", () => {
	const a = analyzePrefixStability(load("session-systemprompt-mutation.ndjson"));
	assert.ok(a.requests >= 20);
	assert.equal(
		a.systemPromptHashes.length,
		a.requests,
		"the mutation fixture must produce a different system prompt on every request",
	);
	assert.equal(a.systemPromptStable, false);
});

test("a mutated earlier message is caught even though it is not a new message", () => {
	// The shape the shipped `newMessages` delta cannot see: index 3 rewritten in
	// place, message count unchanged. This is why the fixtures carry
	// summary.messageHashes rather than relying on the delta.
	const records = load("session-append-only.ndjson").map((r) => ({ ...r, summary: { ...r.summary } }));
	const victim = records[10].summary.messageHashes.slice();
	victim[3] = "0".repeat(64);
	records[10].summary.messageHashes = victim;
	const a = analyzePrefixStability(records);
	assert.ok(a.stability < 1);
	assert.ok(a.divergences.some((d) => d.at === 3));
});

test("the fixtures are the real tap's output, regenerable from the committed script", () => {
	// Guards against a hand-edited fixture: every record must carry the fields
	// summarizeRequest() produces, in the monitor's own shapes.
	for (const name of ["session-append-only.ndjson", "session-systemprompt-mutation.ndjson", "session-compaction.ndjson"]) {
		const records = load(name);
		assert.ok(records.length >= 20, `${name} must record >=20 provider requests`);
		for (const r of records) {
			assert.equal(r.kind, "provider_request");
			assert.match(r.summary.systemPromptHash, /^[0-9a-f]{64}$/);
			assert.equal(r.summary.messageHashes.length, r.summary.messageCount);
			assert.ok(Array.isArray(r.summary.newMessages));
			assert.ok(Number.isInteger(r.summary.estTokens));
			for (const h of r.summary.messageHashes) assert.match(h, /^[0-9a-f]{64}$/);
		}
	}
	assert.ok(fs.existsSync(path.join(repoRoot, "scripts/dev/make-monitor-fixtures.mjs")));
});
