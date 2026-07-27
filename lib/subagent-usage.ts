// Aggregate API usage performed behind a tool boundary so pi can include it in
// the parent session's usage totals. Nested subagents appear as toolResult
// messages with their own usage, so walking messages accounts for the full tree
// exactly once.

export interface AggregateUsage {
	input: number;
	output: number;
	cacheRead: number;
	cacheWrite: number;
	cacheWrite1h?: number;
	reasoning?: number;
	totalTokens: number;
	cost: {
		input: number;
		output: number;
		cacheRead: number;
		cacheWrite: number;
		total: number;
	};
}

interface UsageMessage {
	role?: string;
	usage?: Partial<AggregateUsage> & {
		cost?: Partial<AggregateUsage["cost"]>;
	};
}

interface UsageResult {
	messages?: UsageMessage[];
	meteredUsage?: UsageMessage["usage"][];
}

export function hiddenUsageFromChildEvent(
	event: any,
): UsageMessage["usage"] | undefined {
	if (event?.type === "message_end" && event.message?.role === "toolResult")
		return event.message.usage;
	if (event?.type === "compaction_end") return event.result?.usage;
	return undefined;
}

export function aggregateSubagentUsage(
	results: UsageResult[],
): AggregateUsage | undefined {
	const total: AggregateUsage = {
		input: 0,
		output: 0,
		cacheRead: 0,
		cacheWrite: 0,
		totalTokens: 0,
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
	};
	let found = false;
	let hasReasoning = false;
	let hasCacheWrite1h = false;

	const add = (usage: UsageMessage["usage"]): void => {
		if (!usage) return;
		found = true;
		total.input += usage.input ?? 0;
		total.output += usage.output ?? 0;
		total.cacheRead += usage.cacheRead ?? 0;
		total.cacheWrite += usage.cacheWrite ?? 0;
		total.totalTokens += usage.totalTokens ?? 0;
		if (usage.reasoning !== undefined) {
			hasReasoning = true;
			total.reasoning = (total.reasoning ?? 0) + usage.reasoning;
		}
		if (usage.cacheWrite1h !== undefined) {
			hasCacheWrite1h = true;
			total.cacheWrite1h = (total.cacheWrite1h ?? 0) + usage.cacheWrite1h;
		}
		total.cost.input += usage.cost?.input ?? 0;
		total.cost.output += usage.cost?.output ?? 0;
		total.cost.cacheRead += usage.cost?.cacheRead ?? 0;
		total.cost.cacheWrite += usage.cost?.cacheWrite ?? 0;
		total.cost.total += usage.cost?.total ?? 0;
	};

	for (const result of results) {
		for (const message of result.messages ?? []) {
			// Assistant messages are the child's own requests. Tool-result usage is
			// work hidden behind a child tool call, including nested subagents.
			if (message.role === "assistant" || message.role === "toolResult")
				add(message.usage);
		}
		// Compaction is metered but is not represented by a message_end event.
		for (const usage of result.meteredUsage ?? []) add(usage);
	}

	if (!found) return undefined;
	if (!hasReasoning) delete total.reasoning;
	if (!hasCacheWrite1h) delete total.cacheWrite1h;
	return total;
}
