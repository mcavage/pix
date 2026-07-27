import assert from "node:assert/strict";
import { test } from "node:test";
import {
	aggregateSubagentUsage,
	hiddenUsageFromChildEvent,
} from "../lib/subagent-usage.ts";

const usage = (overrides = {}) => ({
	input: 10,
	output: 2,
	cacheRead: 3,
	cacheWrite: 4,
	totalTokens: 19,
	cost: {
		input: 0.1,
		output: 0.2,
		cacheRead: 0.03,
		cacheWrite: 0.04,
		total: 0.37,
	},
	...overrides,
});

test("aggregates child and nested subagent usage exactly once", () => {
	const result = aggregateSubagentUsage([
		{
			messages: [
				{ role: "user" },
				{ role: "assistant", usage: usage({ reasoning: 1 }) },
				{
					role: "toolResult",
					usage: usage({
						input: 20,
						output: 5,
						cacheRead: 0,
						cacheWrite: 0,
						reasoning: 2,
						totalTokens: 25,
						cost: {
							input: 0.2,
							output: 0.5,
							cacheRead: 0,
							cacheWrite: 0,
							total: 0.7,
						},
					}),
				},
			],
		},
	]);

	assert.deepEqual(
		{
			...result,
			cost: undefined,
		},
		{
			input: 30,
			output: 7,
			cacheRead: 3,
			cacheWrite: 4,
			reasoning: 3,
			totalTokens: 44,
			cost: undefined,
		},
	);
	assert.ok(Math.abs(result.cost.input - 0.3) < 1e-12);
	assert.ok(Math.abs(result.cost.output - 0.7) < 1e-12);
	assert.ok(Math.abs(result.cost.cacheRead - 0.03) < 1e-12);
	assert.ok(Math.abs(result.cost.cacheWrite - 0.04) < 1e-12);
	assert.ok(Math.abs(result.cost.total - 1.07) < 1e-12);
});

test("recognizes actual nested-tool and compaction event shapes", () => {
	const nested = usage();
	const compacted = usage({ input: 7, totalTokens: 16 });
	assert.equal(
		hiddenUsageFromChildEvent({
			type: "message_end",
			message: { role: "toolResult", usage: nested },
		}),
		nested,
	);
	assert.equal(
		hiddenUsageFromChildEvent({
			type: "compaction_end",
			result: { usage: compacted },
		}),
		compacted,
	);
	assert.equal(
		hiddenUsageFromChildEvent({ type: "tool_result_end", message: {} }),
		undefined,
	);
});

test("includes compaction usage that has no message", () => {
	const result = aggregateSubagentUsage([
		{
			messages: [{ role: "assistant", usage: usage() }],
			meteredUsage: [usage({ input: 7, totalTokens: 16 })],
		},
	]);
	assert.equal(result.input, 17);
	assert.equal(result.totalTokens, 35);
	assert.ok(Math.abs(result.cost.total - 0.74) < 1e-12);
});

test("returns undefined when no metered messages were captured", () => {
	assert.equal(
		aggregateSubagentUsage([{ messages: [{ role: "user" }] }, {}]),
		undefined,
	);
});
