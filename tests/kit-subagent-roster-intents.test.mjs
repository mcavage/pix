// The subagent roster line in pi-kit/spec.yaml's agentInstructions is
// AGENT_INSTRUCTIONS prose: it is baked into every sandbox's AGENTS.md at
// create time (see AGENTS.md's own "Models & subagents" section, the live
// version of this same fact). The roster resolves each preset THROUGH the
// router by INTENT (`pix agent ls` shows the resolved model + why) — never a
// hardcoded vendor/model nickname — because a model swap in policy.json must
// not require hand-editing the kit prose to match. This is the drift
// sentinel: it fails the moment the roster bullets regress to naming a
// model instead of an intent.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const specPath = path.join(repoRoot, "pi-kit", "spec.yaml");
const spec = fs.readFileSync(specPath, "utf8");

// The roster lives between "agent = a preset in ~/.pi/agent/agents" and the
// "specialists:" bullet that follows the three orchestration presets.
function rosterSection() {
	const start = spec.indexOf("agent = a preset in ~/.pi/agent/agents");
	assert.ok(start >= 0, "pi-kit/spec.yaml must still document the subagent roster (agent = a preset in ~/.pi/agent/agents)");
	const end = spec.indexOf("- specialists:", start);
	assert.ok(end > start, "pi-kit/spec.yaml roster must still list the specialists bullet after the orchestration presets");
	return spec.slice(start, end);
}

test("the kit roster names intents for fanout/deep/review, not vendor/model nicknames", () => {
	const roster = rosterSection();

	for (const { preset, intent } of [
		{ preset: "fanout", intent: "breadth" },
		{ preset: "deep", intent: "max-accuracy" },
		{ preset: "review", intent: "review" },
	]) {
		const re = new RegExp(`${preset}\\s*\\(intent ${intent}\\b`);
		assert.ok(re.test(roster), `roster bullet for "${preset}" must say "(intent ${intent}", got:\n${roster}`);
	}

	assert.match(roster, /pix agent ls/, "roster must point readers at `pix agent ls` for the live resolved model + why");

	// Vendor/model nicknames regress this the moment anyone hardcodes what an
	// intent currently resolves to (models move; policy.json owns that).
	const vendorNicknames = ["haiku", "opus", "sonnet", "fable", "claude", "gemini", "gpt", "flash", "sol", "terra", "qwen", "glm", "kimi", "ollama"];
	const lowerRoster = roster.toLowerCase();
	const leaked = vendorNicknames.filter((n) => new RegExp(`\\b${n}\\b`).test(lowerRoster));
	assert.deepEqual(leaked, [], `kit roster must never name a vendor/model nickname (found: ${leaked.join(", ")}) — name the intent instead, see \`pix agent ls\``);
});
