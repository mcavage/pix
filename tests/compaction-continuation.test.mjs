import assert from "node:assert/strict";
import { test } from "node:test";

import extension, {
	latestTodoSnapshot,
	shouldContinueAfterCompaction,
} from "../extensions/compaction-continuation.ts";

function todoResult(todos) {
	return {
		type: "message",
		message: {
			role: "toolResult",
			toolName: "manage_todo_list",
			details: { todos },
		},
	};
}

test("latestTodoSnapshot uses the newest durable todo result", () => {
	const entries = [
		todoResult([{ status: "in-progress" }]),
		{ type: "message", message: { role: "assistant" } },
		todoResult([{ status: "completed" }]),
	];
	assert.deepEqual(latestTodoSnapshot(entries), [{ status: "completed" }]);
});

test("durable todo clear supersedes an older active snapshot", () => {
	const entries = [
		todoResult([{ status: "in-progress" }]),
		{ type: "custom", customType: "pi-stack-todo-cleared", data: {} },
	];
	assert.deepEqual(latestTodoSnapshot(entries), []);
});

test("continuation requires threshold compaction and in-progress work", () => {
	const active = [todoResult([{ status: "in-progress" }, { status: "not-started" }])];
	assert.equal(shouldContinueAfterCompaction({ reason: "threshold", willRetry: false }, active), true);
	assert.equal(shouldContinueAfterCompaction({ reason: "manual", willRetry: false }, active), false);
	assert.equal(shouldContinueAfterCompaction({ reason: "overflow", willRetry: true }, active), false);
	assert.equal(
		shouldContinueAfterCompaction(
			{ reason: "threshold", willRetry: false },
			[todoResult([{ status: "completed" }, { status: "not-started" }])],
		),
		false,
	);
});

test("extension queues one private follow-up for a compacted active task", () => {
	const handlers = new Map();
	const sent = [];
	const pi = {
		on(name, handler) {
			handlers.set(name, handler);
		},
		sendMessage(message, options) {
			sent.push({ message, options });
		},
	};
	extension(pi);

	const ctx = {
		isIdle: () => false,
		hasPendingMessages: () => false,
		sessionManager: {
			getBranch: () => [todoResult([{ status: "in-progress" }])],
		},
	};
	const event = {
		reason: "threshold",
		willRetry: false,
		compactionEntry: { id: "compact-1" },
	};
	handlers.get("session_compact")(event, ctx);
	handlers.get("session_compact")(event, ctx);

	assert.equal(sent.length, 1);
	assert.equal(sent[0].message.display, false);
	assert.equal(sent[0].message.customType, "pi-stack-compaction-continuation");
	assert.deepEqual(sent[0].options, { deliverAs: "followUp", triggerTurn: true });
});

test("extension does not race a prompt during idle preflight compaction", () => {
	const handlers = new Map();
	const sent = [];
	const pi = {
		on: (name, handler) => handlers.set(name, handler),
		sendMessage: (...args) => sent.push(args),
	};
	extension(pi);
	const ctx = {
		isIdle: () => true,
		hasPendingMessages: () => false,
		sessionManager: {
			getBranch: () => [todoResult([{ status: "in-progress" }])],
		},
	};
	handlers.get("session_compact")(
		{ reason: "threshold", willRetry: false, compactionEntry: { id: "preflight" } },
		ctx,
	);
	assert.equal(sent.length, 0);
});

test("extension defers to messages already queued by the user", () => {
	const handlers = new Map();
	const sent = [];
	const pi = {
		on: (name, handler) => handlers.set(name, handler),
		sendMessage: (...args) => sent.push(args),
	};
	extension(pi);
	const ctx = {
		isIdle: () => false,
		hasPendingMessages: () => true,
		sessionManager: {
			getBranch: () => [todoResult([{ status: "in-progress" }])],
		},
	};
	handlers.get("session_compact")(
		{ reason: "threshold", willRetry: false, compactionEntry: { id: "queued" } },
		ctx,
	);
	assert.equal(sent.length, 0);
});

test("extension stays quiet for completed work and overflow retry", () => {
	const handlers = new Map();
	const sent = [];
	const pi = {
		on: (name, handler) => handlers.set(name, handler),
		sendMessage: (...args) => sent.push(args),
	};
	extension(pi);
	const ctx = {
		isIdle: () => false,
		hasPendingMessages: () => false,
		sessionManager: {
			getBranch: () => [todoResult([{ status: "completed" }])],
		},
	};
	handlers.get("session_compact")(
		{ reason: "threshold", willRetry: false, compactionEntry: { id: "done" } },
		ctx,
	);
	handlers.get("session_compact")(
		{ reason: "overflow", willRetry: true, compactionEntry: { id: "retry" } },
		ctx,
	);
	assert.equal(sent.length, 0);
});
