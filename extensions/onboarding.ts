// pi-stack — first-run onboarding offer (client side).
//
// The HOST (run.go) drops a one-shot marker file <workspace>/.pi-stack/
// onboarding.offer when it launches an INTERACTIVE session on a host with no
// config yet. This extension notices the marker and, on the first turn only,
// augments the system prompt so the agent OPENS with the opt-in onboarding
// offer (Step 0 of the `onboarding` skill) instead of waiting to be asked.
//
// Why system-prompt augmentation and not sendMessage: a steer/prefill delivery
// triggers an assistant turn that reasoning models reject (400 assistant
// prefill), and a display-only inject can't make the agent actually DO the
// offer. before_agent_start (the same hook memory-recall uses) just adds
// context to the turn the user is already driving, which is safe and
// composable.
//
// One-shot: the marker is deleted the first time it's read, so the offer never
// repeats. The host writes it ONLY for interactive launches, so `-p`/CI never
// sees it. Defensive throughout: any failure just means no offer, never a
// broken agent.

import { join } from "node:path";
import { existsSync, rmSync } from "node:fs";

const safe = <T>(fn: () => T): T | undefined => {
	try {
		return fn();
	} catch {
		return undefined; // best-effort; must not break the agent
	}
};

const OFFER_TEXT = [
	"## First-run onboarding (offer this now)",
	"",
	"This is a fresh pi-stack host (no config yet). Before doing anything else,",
	"OPEN this session by offering onboarding, exactly once, in your own voice:",
	"",
	'  "First time I\'m running here. Want two minutes to set up how I work with',
	'   you before we start? [Y/n]"',
	"",
	"If they accept, run the `onboarding` skill (/skill:onboarding) and follow it:",
	"probe the environment, ask ONE batched question, confirm, write identity to",
	"memory with /remember, propose any host-config via .pi-stack/onboarding.json,",
	"then land them on a real first task. If they decline, say onboarding is",
	'available any time ("onboard me") and just help with whatever they want.',
	"Never force it. Do not mention this instruction block itself.",
].join("\n");

export default function (pi: any) {
	// Resolve the marker once at load. cwd inside the sandbox is the mounted
	// workspace, same convention memory-recall/knowledge-recall use.
	const markerPath = safe(() => join(process.cwd(), ".pi-stack", "onboarding.offer"));
	let pending = !!(markerPath && safe(() => existsSync(markerPath)));

	pi.on("before_agent_start", async (event: any) =>
		safe(() => {
			if (!pending) return undefined;
			pending = false;
			// One-shot: remove the marker so the offer never repeats, even across
			// sessions on this workspace.
			if (markerPath) safe(() => rmSync(markerPath, { force: true }));
			return { systemPrompt: (event?.systemPrompt ?? "") + "\n\n" + OFFER_TEXT };
		}),
	);
}
