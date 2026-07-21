// pi-stack — onboarding scaffold (client side).
//
// The PROBLEM this fixes: onboarding is a single skill invocation. Once the
// agent dives into the user's first task, the "teach pi-stack as we go" intent
// falls out of active attention and never comes back — so onboarding degrades to
// a great welcome, one task, then a normal agent forever (observed in practice).
//
// The FIX: a persistent, self-clearing scaffold. The HOST drops a one-shot marker
// <workspace>/.pi-stack/onboarding.state ({"active":true}) when `pi-stack setup`
// hands off. While it's active, this extension appends ONE short reminder line to
// the system prompt on EVERY turn (via before_agent_start, the same awaited hook
// recall uses), so the teaching intent stays present no matter how deep the task
// goes. It biases toward END-OF-TASK moments and "only if relevant" so it never
// becomes contrived mid-task teaching.
//
// It self-clears three ways: the agent deletes/deactivates the file when it
// judges onboarding done (instructed in the reminder); the user opts out; or a
// hard TURN CAP fires as a backstop so it can never nag forever.
//
// Defensive throughout: any failure just means no reminder, never a broken agent.

import { join } from "node:path";
import { existsSync, readFileSync, writeFileSync } from "node:fs";

const safe = <T>(fn: () => T): T | undefined => {
	try {
		return fn();
	} catch {
		return undefined; // best-effort; must NOT break the agent
	}
};

// Hard backstop: after this many turns, stop injecting even if the agent never
// cleared the marker, so onboarding can't nag forever. Tunable via env.
const TURN_CAP = (() => {
	const n = parseInt(process.env.PI_STACK_ONBOARDING_MAX_TURNS ?? "", 10);
	return Number.isFinite(n) && n > 0 ? n : 8;
})();

const REMINDER = [
	"[onboarding — still open]",
	"You are introducing pi-stack to a new user as you work, not running a tour.",
	"At the NEXT natural break in their current task (not mid-flow), if it genuinely",
	"fits, surface ONE capability they haven't seen yet — memory (automatic recall),",
	"the skills/flows you're using, the multi-model crew + cross-vendor review, or",
	"packs (saving reusable context) — in a sentence or two, tied to what just",
	"happened. Never force it, never repeat one, never dump a list. Once you've",
	"given a light tour across a few of these and they're working smoothly, or they",
	"signal they're done, tell them onboarding's wrapped and DELETE the file",
	".pi-stack/onboarding.state so this stops.",
].join(" ");

export default function (pi: any) {
	const statePath = safe(() => join(process.cwd(), ".pi-stack", "onboarding.state"));
	if (!statePath) return;

	// Session-local turn counter (resets per session — that's fine; a session that
	// re-enters onboarding does so only because the marker is still active).
	let turns = 0;

	const isActive = (): boolean =>
		!!safe(() => {
			if (!existsSync(statePath)) return false;
			const raw = readFileSync(statePath, "utf8").trim();
			if (!raw) return true; // present-but-empty = active (lenient)
			const j = JSON.parse(raw);
			return j?.active !== false; // active unless explicitly false
		});

	const deactivate = (): void =>
		void safe(() => {
			if (existsSync(statePath)) writeFileSync(statePath, JSON.stringify({ active: false }) + "\n");
		});

	pi.on("before_agent_start", async (event: any) =>
		safe(() => {
			if (!isActive()) return undefined;
			if (turns >= TURN_CAP) {
				deactivate(); // backstop: never nag past the cap
				return undefined;
			}
			turns++;
			return { systemPrompt: (event?.systemPrompt ?? "") + "\n\n" + REMINDER };
		}),
	);
}
