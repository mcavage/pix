// pi-stack — onboarding scaffold (client side).
//
// The PROBLEM this fixes: onboarding is a single skill invocation. Once the
// agent dives into the user's first task, the "teach pi-stack as we go" intent
// falls out of active attention and never comes back — so onboarding degrades to
// a great welcome, one task, then a normal agent forever (observed in practice).
//
// The FIX: a persistent, self-clearing, PER-CAPABILITY checklist. The HOST drops
// a one-shot marker <workspace>/.pi-stack/onboarding.state
// ({"active":true,"covered":{"memory":false,"skills":false,"crew":false,
// "packs":false,"knowledge":false},"turns":0}) when `pi-stack setup` hands off.
// While it's active and something remains uncovered, this extension appends ONE
// short reminder line to the system prompt on EVERY turn (via before_agent_start,
// the same awaited hook recall uses) naming what's STILL not taught, so the
// teaching intent stays present no matter how deep the task goes — and so it
// never re-teaches something already covered. The agent marks a capability
// taught by calling the `onboarding_progress` tool this extension registers.
//
// It self-clears three ways: all five capabilities are covered (via the tool,
// which flips active:false itself); a hard TURN CAP fires as a backstop so it
// can never nag forever even if the agent forgets to call the tool; or the file
// is deleted/deactivated (by the agent, or the user opting out).
//
// Defensive throughout: any failure just means no reminder / no progress
// tracked, never a broken agent.
//
// ── STATE-LIFECYCLE INVARIANTS (read before touching readState/writeState) ──
//   1. MISSING marker (no file on disk) means "onboarding not active", full
//      stop — nothing we do may ever RECREATE the file. readState() returns
//      null for this case, and every caller of readState() treats null as a
//      silent no-op, never a fallback to a fresh default state.
//   2. MALFORMED marker (file present but unparsable JSON) is treated as
//      ACTIVE with the default checklist. A still-present-but-broken file is
//      evidence onboarding is mid-flight, not evidence it should be silently
//      disabled — so we recover forward instead of going dark.
//   3. The turn cap is PERSISTED in the state file (the `turns` field), not
//      held in memory. An in-memory counter resets every new session, so a
//      user who restarts often would get reminded forever; persisting it means
//      the cap is genuine across the marker's whole lifetime, not per-session.
//   4. Every mutation is written ATOMICALLY (temp file + rename in the same
//      directory) so a process killed mid-write can never leave the marker
//      holding truncated/malformed JSON for the next read to trip over.
//   5. writeState() reports success/failure. A failed persist must never be
//      reported to the model (or the user) as "done" — see the
//      onboarding_progress tool, which throws rather than claiming success.

import { join } from "node:path";
import { existsSync, readFileSync, writeFileSync, renameSync, unlinkSync } from "node:fs";
import { StringEnum } from "@earendil-works/pi-ai";
import { Type } from "typebox";

// The canonical checklist. "onboarded" = active:false OR every value here true.
const CAPABILITIES = ["memory", "skills", "crew", "packs", "knowledge"] as const;
type Capability = (typeof CAPABILITIES)[number];

// Human-readable labels used in the reminder text — plain enough to drop
// straight into a system-prompt sentence.
const LABELS: Record<Capability, string> = {
	memory: "memory (automatic recall across sessions)",
	skills: "the skills/flows (plan, build, debug, code-review, ship, etc.)",
	crew: "the multi-model crew + cross-vendor review",
	packs: "packs (saving reusable context/skills for next time)",
	knowledge: "knowledge (the shared, versioned knowledge bundle)",
};

interface OnboardingState {
	active: boolean;
	covered: Record<Capability, boolean>;
	turns: number; // PERSISTED injection count — see invariant 3 above.
}

const safe = <T>(fn: () => T): T | undefined => {
	try {
		return fn();
	} catch {
		return undefined; // best-effort; must NOT break the agent
	}
};

// Hard backstop: after this many INJECTED turns (persisted, not per-session),
// stop injecting even if the agent never finished the checklist, so onboarding
// can't nag forever. Tunable via env.
const TURN_CAP = (() => {
	const n = parseInt(process.env.PI_STACK_ONBOARDING_MAX_TURNS ?? "", 10);
	return Number.isFinite(n) && n > 0 ? n : 10;
})();

function defaultCovered(): Record<Capability, boolean> {
	return { memory: false, skills: false, crew: false, packs: false, knowledge: false };
}

function isCapability(v: unknown): v is Capability {
	return typeof v === "string" && (CAPABILITIES as readonly string[]).includes(v);
}

function remaining(state: OnboardingState): Capability[] {
	return CAPABILITIES.filter((c) => !state.covered[c]);
}

export default function (pi: any) {
	const statePath = safe(() => join(process.cwd(), ".pi-stack", "onboarding.state"));
	if (!statePath) return;

	// readState distinguishes MISSING (no marker -> null -> inactive, invariant 1)
	// from MALFORMED (marker present but unparsable -> active w/ default
	// checklist, invariant 2). The JSON.parse is wrapped in its OWN try/catch so
	// that distinction survives even though the whole function is also guarded by
	// `safe` (which exists for genuinely unexpected errors, e.g. a permission
	// denied on read — those fall back to null, same as missing).
	const readState = (): OnboardingState | null =>
		safe(() => {
			if (!existsSync(statePath)) return null; // MISSING -> inactive.
			const raw = readFileSync(statePath, "utf8");
			let j: any;
			try {
				j = JSON.parse(raw.trim() || "{}");
			} catch {
				// MALFORMED: present but unparsable — recover forward as ACTIVE with
				// the default checklist rather than silently going dark (invariant 2).
				return { active: true, covered: defaultCovered(), turns: 0 };
			}
			const covered = defaultCovered();
			if (j?.covered && typeof j.covered === "object") {
				for (const c of CAPABILITIES) if (j.covered[c] === true) covered[c] = true;
			}
			const rawTurns = typeof j?.turns === "number" ? j.turns : 0;
			const turns = Number.isFinite(rawTurns) && rawTurns > 0 ? Math.floor(rawTurns) : 0;
			return { active: j?.active !== false, covered, turns }; // active unless explicitly false
		}) ?? null;

	// writeState is ATOMIC (temp file in the same dir, then rename — invariant 4)
	// and reports success/failure (invariant 5) so callers never claim a write
	// succeeded when it didn't.
	const writeState = (state: OnboardingState): boolean => {
		const tmp = `${statePath}.tmp.${process.pid}.${Date.now()}`;
		try {
			writeFileSync(tmp, JSON.stringify(state) + "\n");
			renameSync(tmp, statePath);
			return true;
		} catch {
			try {
				unlinkSync(tmp);
			} catch {
				/* best-effort cleanup; nothing more we can do */
			}
			return false;
		}
	};

	// Track registration success so the reminder hook (which tells the agent to
	// call the tool) is only ever wired up if the tool itself actually exists —
	// never point the agent at a tool that isn't there.
	const toolRegistered = !!safe(() => {
		pi.registerTool({
			name: "onboarding_progress",
			label: "Onboarding Progress",
			description: [
				"Mark ONE pi-stack capability as taught during onboarding.",
				`capability is one of: ${CAPABILITIES.join(", ")}.`,
				"Call this right after you've naturally introduced that capability to the",
				"user (not before). Returns the capabilities still remaining and whether",
				"the onboarding checklist is now fully done.",
			].join(" "),
			parameters: Type.Object({
				capability: StringEnum(CAPABILITIES, {
					description: "The capability you just taught the user.",
				}),
			}) as any,

			// Errors are signaled by THROWING (pi catches it, sets isError:true, and
			// reports it to the LLM) — see docs/extensions.md: "To mark a tool
			// execution as failed... throw an error from execute. Returning a value
			// never sets the error flag regardless of what properties you include."
			async execute(_id: string, params: any, _signal: AbortSignal, _onUpdate: any, _ctx: any) {
				const cap = params?.capability;
				if (!isCapability(cap)) {
					throw new Error(`Unknown capability ${JSON.stringify(cap)}. Valid: ${CAPABILITIES.join(", ")}.`);
				}

				// No marker present — missing OR already cleared. NO-OP: never
				// recreate the file (invariant 1). Recreating it here is exactly the
				// self-clear-doesn't-stick bug this fixes.
				const state = readState();
				if (!state) {
					return {
						content: [
							{ type: "text", text: "Onboarding isn't active (no marker present) — nothing to do." },
						],
						details: { ok: true, active: false },
					};
				}

				state.covered[cap] = true;
				const rem = remaining(state);
				const done = rem.length === 0;
				if (done) state.active = false;

				const ok = writeState(state);
				if (!ok) {
					// Write failed — say so. Never claim success on a persist we can't
					// verify happened (invariant 5).
					throw new Error(
						`onboarding_progress: marked "${cap}" covered but FAILED to persist the state file — the reminder may repeat this capability next turn.`,
					);
				}

				return {
					content: [
						{
							type: "text",
							text: done
								? "Onboarding checklist complete — all capabilities covered."
								: `Marked "${cap}" covered. Remaining: ${rem.join(", ")}.`,
						},
					],
					details: { capability: cap, remaining: rem, done },
				};
			},
		});
		return true;
	});

	// ── the per-turn reminder ──
	function buildReminder(rem: Capability[]): string {
		const list = rem.map((c) => LABELS[c]).join("; ");
		return [
			"[onboarding — still open]",
			`Not yet taught: ${list}.`,
			"You are introducing pi-stack to a new user as you work, not running a",
			"tour. At the NEXT natural break in their current task (not mid-flow), if it",
			"genuinely fits, teach ONE remaining capability in a sentence or two, tied to",
			"what just happened — then call the onboarding_progress tool with",
			`{"capability": "<name>"} for the one you just taught.`,
			"Never force it, never repeat one already covered, never dump the list at",
			"the user. If they opt out or signal they're done, tell them onboarding's",
			"wrapped and delete .pi-stack/onboarding.state so this stops.",
		].join(" ");
	}

	// Guard this registration too (not just registerTool above), and only wire it
	// up if the tool actually registered — otherwise stay silent rather than
	// reminding the agent to call a tool that doesn't exist.
	if (toolRegistered) {
		safe(() =>
			pi.on("before_agent_start", async (event: any) =>
				safe(() => {
					const state = readState();
					if (!state || !state.active) return undefined;
					const rem = remaining(state);
					if (rem.length === 0) {
						// Fully covered but active wasn't flipped (e.g. hand-edited file) —
						// deactivate defensively so we never keep injecting for nothing.
						state.active = false;
						writeState(state);
						return undefined;
					}
					if (state.turns >= TURN_CAP) {
						state.active = false; // backstop: never nag past the cap
						writeState(state);
						return undefined;
					}
					state.turns++; // persisted — survives across sessions (invariant 3)
					// FAIL SAFE: only inject if we could RECORD the increment. If the
					// state file is unwritable (readonly / full disk), persisting fails,
					// the cap can never advance, and we'd nag forever — so suppress the
					// reminder this turn instead. It resumes once writes succeed again.
					if (!writeState(state)) return undefined;
					return { systemPrompt: (event?.systemPrompt ?? "") + "\n\n" + buildReminder(rem) };
				}),
			),
		);
	}
}
