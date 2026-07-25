// pix — reply timestamp as a powerbar segment.
//
// Shows the time of the agent's most recent reply as a compact segment in the
// LEFT group of the powerbar, right after `tokens` (the token/cost readout),
// e.g. "14:02". It lives on the left so it's protected from the powerbar's
// right-edge truncation when the bar overflows the terminal width. This
// replaces earlier attempts that injected timestamp lines into the transcript
// or the footer:
//
//   • Injecting a stamp after an ASSISTANT reply is impossible safely — every
//     sendMessage delivery mode triggers a fresh LLM turn that ends on an
//     assistant message, which reasoning models (opus) reject with
//     `400 ... does not support assistant message prefill`.
//   • A footer status (setStatus) is invisible: the powerbar replaces the
//     built-in footer via setFooter().
//
// The powerbar is an event-driven bar: producers emit "powerbar:update" with a
// segment payload, and a segment renders only if its id is listed in the
// powerbar "left"/"right" settings. So we (1) register + emit the segment and
// (2) make sure "reply-ts" is slotted into the powerbar "left" segment list
// (after `tokens`) in ~/.pi/agent/settings-extensions.json, and removed from
// "right" if an older version put it there. Emitting on the idle agent_end hook is
// a pure event — it never touches the conversation and can't break the agent.
//
// Toggle at runtime with `/timestamps`. Disable entirely with PI_TIMESTAMPS=0.
// Defensive by design: every side effect goes through `safe()`.

const SEGMENT_ID = "reply-ts";
const POWERBAR_EXT = "powerbar";
const DEFAULT_LEFT = "git-branch,tokens,context-usage";
// Intentionally omits sub-hourly/sub-weekly: in a proxy-managed pix the API
// keys are injected by the proxy, so those subscription-usage windows are empty
// and only waste bar width. Dropping them here keeps them gone on fresh sandboxes
// too (this extension is what seeds the powerbar segment lists on first run).
const DEFAULT_RIGHT = "provider,model";
// Slot reply-ts immediately after this segment in the left group.
const ANCHOR = "tokens";

const safe = <T>(fn: () => T): T | undefined => {
	try {
		return fn();
	} catch (err) {
		return undefined; /* best-effort; must not break the agent */
	}
};

export default function (pi: any) {
	const proc = (globalThis as any).process;
	const env = proc?.env ?? {};
	let enabled = env.PI_TIMESTAMPS !== "0";

	// Mirror pi's getAgentDir(): $PI_CODING_AGENT_DIR or ~/.pi/agent.
	const agentDir = (): string => {
		const os = require("node:os");
		const path = require("node:path");
		const override = env.PI_CODING_AGENT_DIR;
		if (override) return override.replace(/^~(?=$|\/)/, os.homedir());
		return path.join(os.homedir(), ".pi", "agent");
	};
	const settingsPath = () =>
		require("node:path").join(agentDir(), "settings-extensions.json");

	// Slot "reply-ts" into the powerbar LEFT list (just after `tokens`) and make
	// sure it's not lingering in the RIGHT list from an older version. Idempotent:
	// only writes when something actually changes. Done at load (factory body) so
	// it lands on disk before the powerbar reads settings in its session_start.
	const ensureSegmentPlacement = () =>
		safe(() => {
			const fs = require("node:fs");
			const p = settingsPath();
			let all: any = {};
			if (fs.existsSync(p)) {
				all = JSON.parse(fs.readFileSync(p, "utf-8")) || {};
			}
			const pb = (all[POWERBAR_EXT] ??= {});
			const parse = (s: string) =>
				s
					.split(",")
					.map((x) => x.trim())
					.filter(Boolean);
			const left = parse(pb.left ?? DEFAULT_LEFT);
			const right = parse(pb.right ?? DEFAULT_RIGHT);

			let changed = false;
			// Remove any stale copy from the right group.
			const ri = right.indexOf(SEGMENT_ID);
			if (ri !== -1) {
				right.splice(ri, 1);
				changed = true;
			}
			// Insert into the left group right after the anchor (or append).
			if (!left.includes(SEGMENT_ID)) {
				const ai = left.indexOf(ANCHOR);
				if (ai === -1) left.push(SEGMENT_ID);
				else left.splice(ai + 1, 0, SEGMENT_ID);
				changed = true;
			}
			if (!changed) return;
			pb.left = left.join(",");
			pb.right = right.join(",");
			fs.mkdirSync(require("node:path").dirname(p), { recursive: true });
			fs.writeFileSync(p, JSON.stringify(all, null, "\t"));
		});

	// Compact 24h HH:MM, e.g. "14:02". Kept deliberately short: the powerbar
	// truncates from the right edge when it overflows the terminal width, and
	// reply-ts is the rightmost segment — a long "2:02:05 PM" would be the first
	// thing cut, so we trim to the minute with no seconds/AM-PM.
	const now = () =>
		safe(() =>
			new Date().toLocaleTimeString(undefined, {
				hour: "2-digit",
				minute: "2-digit",
				hour12: false,
			}),
		) ?? new Date().toISOString().slice(11, 16);

	const emit = (text: string | undefined) =>
		safe(() =>
			pi.events.emit("powerbar:update", { id: SEGMENT_ID, text, color: "dim" }),
		);

	const on = (name: string, fn: (e: any, ctx: any) => any) =>
		safe(() => pi.on(name, async (e: any, ctx: any) => safe(() => fn(e, ctx))));

	// Make the segment selectable/orderable in powerbar settings, and slot it
	// into the right list.
	safe(() =>
		pi.events.emit("powerbar:register-segment", {
			id: SEGMENT_ID,
			label: "Reply Time",
		}),
	);
	if (enabled) ensureSegmentPlacement();

	// Agent finished the whole prompt — emit the reply time. Pure event, no LLM.
	on("agent_end", () => {
		if (!enabled) return;
		emit(now());
	});

	safe(() =>
		pi.registerCommand("timestamps", {
			description: "Toggle the powerbar reply-timestamp segment on/off",
			handler: async (_args: any, ctx: any) => {
				enabled = !enabled;
				if (enabled) ensureSegmentPlacement();
				else emit(undefined); // remove the segment
				safe(() =>
					ctx.ui.notify(`Reply timestamp ${enabled ? "ON" : "OFF"}`, "info"),
				);
			},
		}),
	);
}
