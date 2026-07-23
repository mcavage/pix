// Auto-compaction happens after an agent run. Pi resumes automatically only
// when a retry or queued message exists. If the model ended a long turn while
// structured work is still explicitly in progress, queue one private follow-up
// so the compacted session keeps moving without waiting for the user to type
// "continue".

const TODO_TOOL = "manage_todo_list";
const TODO_CLEARED_ENTRY = "pi-stack-todo-cleared";
const CONTINUATION_TYPE = "pi-stack-compaction-continuation";

export type TodoStatus = "not-started" | "in-progress" | "completed";
export type TodoSnapshot = Array<{ status?: TodoStatus }>;

// Walk the full active branch, not only the compacted LLM context. Tool-result
// details are the todo extension's durable state format and survive compaction.
export function latestTodoSnapshot(entries: any[]): TodoSnapshot {
	let latest: TodoSnapshot = [];
	for (const entry of entries ?? []) {
		if (entry?.type === "custom" && entry.customType === TODO_CLEARED_ENTRY) {
			latest = [];
			continue;
		}
		if (entry?.type !== "message") continue;
		const message = entry.message;
		if (message?.role !== "toolResult" || message.toolName !== TODO_TOOL) continue;
		if (Array.isArray(message.details?.todos)) {
			latest = message.details.todos;
		}
	}
	return latest;
}

export function shouldContinueAfterCompaction(event: any, entries: any[]): boolean {
	if (event?.reason !== "threshold" || event?.willRetry) return false;
	return latestTodoSnapshot(entries).some((todo) => todo?.status === "in-progress");
}

export default function (pi: any) {
	let lastCompactionId = "";
	let continuationOutstanding = false;

	pi.on("session_start", () => {
		lastCompactionId = "";
		continuationOutstanding = false;
	});
	pi.on("agent_start", () => {
		// If our continuation started this run, it is no longer queued. A future
		// threshold compaction may schedule another one if work remains active.
		continuationOutstanding = false;
	});

	pi.on("session_compact", (event: any, ctx: any) => {
		const id = String(event?.compactionEntry?.id ?? "");
		if (id && id === lastCompactionId) return;
		// During the normal post-run threshold path Pi is still settling, so idle
		// is false. If compaction instead ran as a preflight for a newly submitted
		// prompt, idle can be true before that prompt reaches Pi's queue. Do not
		// race it by starting our own turn.
		if (ctx.isIdle?.() || continuationOutstanding || ctx.hasPendingMessages?.()) return;
		if (!shouldContinueAfterCompaction(event, ctx.sessionManager.getBranch())) return;
		lastCompactionId = id;
		continuationOutstanding = true;

		// A custom message is not a user assertion, so memory capture ignores it.
		// followUp queues it behind the compaction; triggerTurn makes it run once
		// Pi settles. Pi's own compaction loop sees the queued message and resumes.
		pi.sendMessage(
			{
				customType: CONTINUATION_TYPE,
				content:
					"Context was compacted while the structured todo list still has work in progress. Continue the current task autonomously from the compaction summary. Do not wait for the user to prompt you to resume.",
				display: false,
			},
			{ deliverAs: "followUp", triggerTurn: true },
		);
	});
}
