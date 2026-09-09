// Stage-level wall-clock timing for every subagent run. Result details must
// carry startedAt/endedAt/durationMs (epoch ms) for each child in single,
// parallel, and chain mode, and for the early returns (unknown agent, host-mode
// refusal) that never spawn anything. Everything here goes through the tool the
// model actually calls, plus one pure timing helper for the backwards-clock
// edge. This is timing only: nothing here is a second usage or rate system, and
// `usage` stays the accounting channel.
import assert from "node:assert/strict";
import fs from "node:fs";
import { register } from "node:module";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const AGENT = "---\ndescription: reviewer\ntools: read\nmodel: expected/model\n---\nReview.\n";
const OK_CHILD = `console.log(JSON.stringify({type:'message_end',message:{role:'assistant',provider:'actual',model:'response-model',content:[{type:'text',text:'done'}],stopReason:'endTurn'}}));`;
const FAIL_CHILD = `process.stderr.write('child exploded');process.exit(1);`;

let seq = 0;

// Fresh module instance per scenario: subagents.ts reads its env config once at
// import time.
async function withTool(childSource, fn, extraEnv = {}) {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-subagent-timing-"));
	const saved = { ...process.env };
	try {
		fs.mkdirSync(path.join(dir, "agents"));
		fs.writeFileSync(path.join(dir, "agents/review.md"), AGENT);
		fs.writeFileSync(path.join(dir, "agents/second.md"), AGENT);
		const child = path.join(dir, "child.mjs");
		fs.writeFileSync(child, childSource);
		process.env.PI_TEST_AGENT_DIR = dir;
		process.env.PI_SUBAGENT_PI_COMMAND = `${process.execPath} ${child}`;
		delete process.env.PI_SUBAGENT_DISABLED;
		delete process.env.PI_SUBAGENT_DEPTH;
		Object.assign(process.env, extraEnv);
		const mod = await import(`../extensions/subagents.ts?timing=${seq++}`);
		let tool;
		mod.default({ on() {}, registerCommand() {}, registerTool(t) { tool = t; } });
		const run = (params) =>
			tool.execute("test", params, new AbortController().signal, () => {}, {
				cwd: dir,
			});
		await fn({ run, mod, dir });
	} finally {
		for (const key of Object.keys(process.env))
			if (!(key in saved)) delete process.env[key];
		Object.assign(process.env, saved);
		fs.rmSync(dir, { recursive: true, force: true });
	}
}

// Coherence, not a stopwatch: epoch-ms integers inside the observed window, a
// nonnegative duration, and duration exactly matching the two timestamps.
function assertCoherentTiming(r, window, label) {
	assert.equal(typeof r.startedAt, "number", `${label}: startedAt is a number`);
	assert.equal(typeof r.endedAt, "number", `${label}: endedAt is a number`);
	assert.equal(
		typeof r.durationMs,
		"number",
		`${label}: durationMs is a number`,
	);
	assert.ok(Number.isInteger(r.startedAt), `${label}: startedAt is epoch ms`);
	assert.ok(Number.isInteger(r.endedAt), `${label}: endedAt is epoch ms`);
	assert.ok(r.durationMs >= 0, `${label}: duration is nonnegative`);
	assert.equal(
		r.durationMs,
		r.endedAt - r.startedAt,
		`${label}: duration matches the timestamps`,
	);
	assert.ok(
		r.startedAt >= window.before && r.endedAt <= window.after,
		`${label}: timestamps fall inside the observed window (${r.startedAt}..${r.endedAt} vs ${window.before}..${window.after})`,
	);
}

test("single, parallel, and chain result details expose per-child timing", async () => {
	await withTool(OK_CHILD, async ({ run }) => {
		const cases = [
			["single", { agent: "review", task: "Review." }, 1],
			[
				"parallel",
				{
					tasks: [
						{ agent: "review", task: "Review." },
						{ agent: "second", task: "Review again." },
					],
				},
				2,
			],
			[
				"chain",
				{
					chain: [
						{ agent: "review", task: "Review." },
						{ agent: "second", task: "Refine {previous}." },
					],
				},
				2,
			],
		];
		for (const [mode, params, expected] of cases) {
			const before = Date.now();
			const result = await run(params);
			const after = Date.now();
			assert.ok(!result.isError, JSON.stringify(result));
			assert.equal(result.details.mode, mode);
			assert.equal(result.details.results.length, expected);
			for (const [i, r] of result.details.results.entries())
				assertCoherentTiming(r, { before, after }, `${mode}[${i}]`);
			// Timing is per child, so it must not be a shared constant.
			assert.ok(
				result.details.results.every((r) => r.endedAt <= after),
				`${mode}: completion stamped before the tool result returned`,
			);
			// Existing usage accounting is untouched by the timing fields.
			assert.equal(typeof result.details.results[0].usage.turns, "number");
		}
	});
});

test("chain steps are stamped in execution order", async () => {
	await withTool(OK_CHILD, async ({ run }) => {
		const before = Date.now();
		const result = await run({
			chain: [
				{ agent: "review", task: "Review." },
				{ agent: "second", task: "Refine {previous}." },
			],
		});
		const after = Date.now();
		const [first, second] = result.details.results;
		assertCoherentTiming(first, { before, after }, "chain step 1");
		assertCoherentTiming(second, { before, after }, "chain step 2");
		assert.ok(
			second.startedAt >= first.endedAt,
			`step 2 starts no earlier than step 1 ends (${second.startedAt} vs ${first.endedAt})`,
		);
	});
});

test("a failed child still reports timing", async () => {
	await withTool(FAIL_CHILD, async ({ run }) => {
		const before = Date.now();
		const result = await run({ agent: "review", task: "Review." });
		const after = Date.now();
		assert.equal(result.isError, true);
		assert.equal(result.details.results.length, 1);
		assertCoherentTiming(result.details.results[0], { before, after }, "failed");
	});
});

test("an unknown agent reports timing without spawning anything", async () => {
	await withTool(OK_CHILD, async ({ run }) => {
		const before = Date.now();
		const result = await run({ agent: "no-such-agent", task: "Review." });
		const after = Date.now();
		assert.equal(result.isError, true);
		const [r] = result.details.results;
		assert.match(r.stderr, /Unknown agent/);
		assertCoherentTiming(r, { before, after }, "unknown agent");
	});
});

// The refusal is what the model sees, so assert it on the registered tool, not
// on an internal function: an empty details.results would force every consumer
// to special-case "refused" as "no rows" and would drop the refusal's timing.
test("the host-mode refusal returns one timed result through the tool", async () => {
	await withTool(
		OK_CHILD,
		async ({ run }) => {
			const cases = [
				["single", { agent: "review", task: "Review." }],
				["parallel", { tasks: [{ agent: "review", task: "Review." }] }],
				["chain", { chain: [{ agent: "review", task: "Review." }] }],
			];
			for (const [label, params] of cases) {
				const before = Date.now();
				const result = await run(params);
				const after = Date.now();
				assert.equal(result.isError, true, label);
				assert.match(
					result.content.map((c) => c.text).join("\n"),
					/disabled in host mode/i,
					label,
				);
				assert.equal(
					result.details.results.length,
					1,
					`${label}: exactly one refusal row, not an empty array`,
				);
				const [r] = result.details.results;
				assert.equal(r.agent, "review", `${label}: names the requested agent`);
				assert.equal(r.exitCode, 1, label);
				assert.match(r.errorMessage, /disabled in host mode/i);
				// Refusal, not a run: no child output and no usage was recorded.
				assert.equal(r.messages.length, 0, `${label}: nothing may have run`);
				assert.equal(r.usage.turns, 0, `${label}: nothing may have run`);
				assertCoherentTiming(r, { before, after }, `disabled ${label}`);
			}
		},
		{ PI_SUBAGENT_DISABLED: "1" },
	);
});

// A clock that steps backward mid-run (NTP correction) must not produce an
// endedAt before startedAt, nor a durationMs that disagrees with the pair.
// Deterministic because the helper is pure: no clock stub, no child.
test("completionTiming stays coherent when the clock steps backward", async () => {
	await withTool(OK_CHILD, async ({ mod }) => {
		const cases = [
			["forward", 1_000, 1_500, { endedAt: 1_500, durationMs: 500 }],
			["same instant", 1_000, 1_000, { endedAt: 1_000, durationMs: 0 }],
			["backward", 1_000, 400, { endedAt: 1_000, durationMs: 0 }],
		];
		for (const [label, startedAt, now, expected] of cases) {
			const t = mod.completionTiming(startedAt, now);
			assert.deepEqual(t, expected, label);
			assert.ok(t.endedAt >= startedAt, `${label}: endedAt >= startedAt`);
			assert.equal(
				t.durationMs,
				t.endedAt - startedAt,
				`${label}: duration matches the timestamps`,
			);
		}
	});
});
