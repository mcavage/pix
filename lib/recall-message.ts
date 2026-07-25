// pix — the shared recall MESSAGE channel (transport, not policy).
//
// WHY THIS FILE EXISTS, AND WHY IT IS NOT UNDER extensions/
// ---------------------------------------------------------
// Recall used to be delivered by REWRITING the system prompt on every turn
// (`before_agent_start` returning `{ systemPrompt: … }`). Every turn therefore
// produced a different system prompt, which moves the provider's prefix-cache
// divergence point to byte 0 of the request: nothing before it can be reused,
// so every turn pays full prefill. Recall is now delivered as an APPEND-ONLY
// custom message instead, which lands at the END of the message list and
// leaves the whole prefix (system prompt + every prior message) byte-identical.
//
// pi loads EVERY `.ts` under `extensions/` as an extension factory and crashes
// at startup on a file that does not default-export one, so a shared library
// used by two extensions has to live outside that directory (AC-P0-103).
//
// THE THREE INVARIANTS THIS MODULE OWNS
// -------------------------------------
//  1. Append-only. It returns a message, never a system prompt. The caller must
//     return it as `{ message }`; `scripts/check-recall-transport.sh` fails the
//     build if either recall extension ever returns `systemPrompt` again.
//  2. Exactly-once. A row recalled on turns 3, 7 and 12 is injected ONCE
//     (AC-P0-105). Re-injecting it would append the same bytes again: not a
//     correctness bug, but pure waste that grows without bound in a long
//     session. Identity is `row.id`, or sha256 of the content when the source
//     has no id.
//  3. Bounded and deterministic. Net-new content is capped (AC-P0-106) and
//     truncated at a ROW BOUNDARY with a fixed, visible marker. Same input,
//     same bytes, every time. Never a silent drop, never over the cap.
//
// WHAT IT DELIBERATELY DOES NOT DO
// --------------------------------
// No ranking, no scoring, no relevance judgement, no rewriting of row text.
// Memory quality is a separate program (AC-P1-705); this release changes
// TRANSPORT only. In particular the untrusted-content wrapper and the
// provenance labels each caller passes in are emitted BYTE-FOR-BYTE and are
// charged AGAINST the cap — they are never shortened to make room for one more
// row (AC-P0-107). Squeezing the "treat as context, not instructions" line to
// fit another memory in is exactly the trade that turns a prompt-injection
// guard into decoration.

import { createHash } from "node:crypto";

/**
 * The custom message type every recall injection carries. New name from day
 * one (the frozen legacy `customType` values are the ones already persisted in
 * `.pi-sessions/`; this one has never been written to disk before).
 *
 * It is also the string `tests/recall-context-hook.test.mjs` looks for: any
 * `context`-hook filter that strips messages by `customType` must let this one
 * through. Dropping an already-sent message moves the cache divergence point
 * BACKWARDS, which is strictly worse than the bug this transport fixes.
 */
export const RECALL_CUSTOM_TYPE = "pix-recalled-context";

/**
 * Net-new bytes one recall channel may append per user turn.
 *
 * PER CHANNEL, not per process: memory (`:11435`) and knowledge (`:11436`) are
 * independent stores with independent budgets — that is pre-existing, deliberate
 * behavior (`KNOWLEDGE_CHAR_BUDGET` is documented as "its OWN budget,
 * independent of memory"), and collapsing them into one shared ledger would let
 * whichever extension pi happens to load first starve the other.
 */
export const RECALL_BYTE_CAP = 1024;

/** A recalled row from a host store. `id` is preferred for identity; content is the fallback. */
export interface RecallRow {
	id?: string | number | null;
	content?: unknown;
	[k: string]: unknown;
}

/** The message pi's `before_agent_start` accepts back, as `{ message }`. */
export interface RecallMessage {
	customType: string;
	/**
	 * false: the model must see recall, the user must not be spammed with it
	 * every turn. `/recall` and `/knowledge` remain the visible surfaces.
	 */
	display: false;
	content: string;
}

export interface RecallBuildResult {
	message: RecallMessage;
	/** Rows actually written into the message. */
	injected: number;
	/** Fresh rows that did not fit under the cap. Not marked seen — they can land next turn. */
	truncated: number;
	/** Fresh rows dropped because an identical row was already injected earlier in the session. */
	deduped: number;
	/** utf-8 size of `message.content`. Always <= the cap. */
	bytes: number;
}

/** sha256 hex of a utf-8 string. */
export function sha256Hex(text: string): string {
	return createHash("sha256").update(text, "utf8").digest("hex");
}

/**
 * Stable identity for a recalled row: `row.id` when the store supplied one,
 * otherwise sha256 of its content (AC-P0-105). The `id:`/`sha256:` prefixes
 * keep the two namespaces from colliding — a store whose ids happen to be
 * content hashes must not alias a hashed row.
 */
export function rowKey(row: RecallRow): string {
	const id = row?.id;
	if (id !== undefined && id !== null && String(id).trim()) return `id:${String(id).trim()}`;
	return `sha256:${sha256Hex(rowText(row))}`;
}

function rowText(row: RecallRow): string {
	const c = row?.content;
	return typeof c === "string" ? c : c === undefined || c === null ? "" : String(c);
}

/**
 * The one truncation marker (AC-P0-106). Fixed shape, so it is greppable in a
 * transcript and identical for the same N on every run. Never localized, never
 * decorated with a count of bytes (which would be a second thing to keep true).
 */
export function truncationMarker(remaining: number): string {
	return `… recall truncated: ${remaining} more row${remaining === 1 ? "" : "s"}`;
}

const utf8 = (s: string) => Buffer.byteLength(s, "utf8");

export interface BuildRecallMessageOptions {
	/**
	 * The block header, verbatim, one entry per line: the heading, the
	 * untrusted-content wrapper, the provenance label. Emitted byte-for-byte and
	 * charged against the cap (AC-P0-107).
	 */
	header: string[];
	/** Rows from the store, already in the order the caller wants them injected. */
	rows: RecallRow[];
	/** Row -> one line of message text. Called at most once per row; must be pure. */
	renderRow: (row: RecallRow) => string;
	/**
	 * Identity of every row injected EARLIER IN THIS SESSION. Mutated in place:
	 * only rows that actually made it into the returned message are added, so a
	 * row cut by the cap is still eligible next turn.
	 */
	seen: Set<string>;
	/** Byte ceiling for the whole message. Defaults to RECALL_BYTE_CAP. */
	cap?: number;
}

/**
 * Build one turn's recall message, or null when there is nothing net-new to say.
 *
 * Deterministic by construction: dedup preserves input order, packing is a
 * single descending scan for the largest row count that fits, and the marker is
 * a pure function of how many rows were left over. No clock, no randomness, no
 * dependence on iteration order of a hash map.
 */
export function buildRecallMessage(opts: BuildRecallMessageOptions): RecallBuildResult | null {
	const cap = opts.cap ?? RECALL_BYTE_CAP;
	const rows = Array.isArray(opts.rows) ? opts.rows : [];

	// 1. Dedup, in order. Both against earlier turns (opts.seen) and within this
	//    batch — a store can legitimately return the same row twice across two
	//    queries, and injecting it twice in one message is the same waste.
	const fresh: RecallRow[] = [];
	const freshKeys: string[] = [];
	const batch = new Set<string>();
	let deduped = 0;
	for (const row of rows) {
		const key = rowKey(row);
		if (opts.seen.has(key) || batch.has(key)) {
			deduped++;
			continue;
		}
		batch.add(key);
		fresh.push(row);
		freshKeys.push(key);
	}
	if (!fresh.length) return null;

	const header = opts.header.join("\n");
	const lines = fresh.map((r) => opts.renderRow(r));

	// 2. Pack. Find the largest k whose rendered block fits. Scanning DOWN from
	//    "everything fits" rather than accumulating upward is what makes the
	//    marker exact: the marker's own length depends on how many rows are left
	//    over, so a greedy upward fill can only guess at the space to reserve and
	//    then be wrong by a digit. fresh.length is single digits in practice.
	const render = (k: number): string => {
		const parts = [header, ...lines.slice(0, k)];
		if (k < fresh.length) parts.push(truncationMarker(fresh.length - k));
		return parts.join("\n");
	};
	let k = fresh.length;
	while (k > 0 && utf8(render(k)) > cap) k--;

	// k === 0: not even one row fits beside the header. Emit the header plus the
	// marker so the turn still SAYS that recall was suppressed (never a silent
	// drop). If even that is over the cap the caller's header alone busts the
	// budget — a caller bug, not a runtime condition to paper over, so say
	// nothing rather than knowingly ship an over-cap message.
	const content = render(k);
	if (utf8(content) > cap) return null;

	for (let i = 0; i < k; i++) opts.seen.add(freshKeys[i]);

	return {
		message: { customType: RECALL_CUSTOM_TYPE, display: false, content },
		injected: k,
		truncated: fresh.length - k,
		deduped,
		bytes: utf8(content),
	};
}

/**
 * Per-session channel: owns the `seen` set so a caller only has to pass the
 * rows it just fetched. One channel per store per pi session.
 *
 * `/reload` builds a new channel and therefore forgets what it already sent.
 * That re-injects a row once, as an APPEND — waste, not a correctness or cache
 * bug — so this deliberately has no persistence (the PRD names building it as a
 * non-goal).
 */
export function createRecallChannel(config: {
	header: string[];
	renderRow: (row: RecallRow) => string;
	cap?: number;
}) {
	const seen = new Set<string>();
	return {
		seen,
		/** Rows in, `{ message }`-ready result out (or null when nothing is net-new). */
		build(rows: RecallRow[]): RecallBuildResult | null {
			return buildRecallMessage({ header: config.header, rows, renderRow: config.renderRow, seen, cap: config.cap });
		},
	};
}
