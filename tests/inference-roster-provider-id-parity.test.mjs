// E3.2 "exact provider IDs": the roster is only useful if the ids it names
// are BYTE-IDENTICAL to what extensions/inference.ts registers as a provider
// model and what extensions/subagents.ts forwards to a child `pi --model`.
// There is no second, re-derived id anywhere in this path (docs/design/
// environments.md §7: "There is no second generated routing artifact that
// can disagree with provider registration").
import assert from "node:assert/strict";
import * as fs from "node:fs";
import { register } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const { readRoster: inferenceReadRoster } = await import("../extensions/inference.ts");
const { parseRoster } = await import("../lib/inference-roster.ts");

const MANIFEST = {
	version: 1,
	backends: {
		zai: { driver: "openai-compatible", protocol: "openai-completions", base_url: "https://api.z.ai/v1", auth: "none" },
	},
	models: [
		{
			id: "zai/glm-5",
			catalog_model: "zai/glm-5",
			backend: "zai",
			name: "GLM 5",
			context_window: 200000,
			max_tokens: 32000,
		},
	],
	roster: { main: "zai/glm-5", agents: { engineer: "zai/glm-5" } },
};

test("extensions/inference.ts's own readRoster resolves the identical shape lib/inference-roster.ts does", () => {
	assert.deepEqual(inferenceReadRoster(MANIFEST), parseRoster(MANIFEST));
});

test("the roster's model id is the SAME string a registered provider model qualifies to", async () => {
	const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "roster-parity-inference-"));
	fs.writeFileSync(path.join(agentDir, "inference.json"), JSON.stringify(MANIFEST));
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const registered = [];
	const fakePi = {
		registerProvider(name, cfg) {
			registered.push({ name, cfg });
		},
	};
	const inferenceMod = await import("../extensions/inference.ts?parity=1");
	inferenceMod.default(fakePi);
	assert.equal(registered.length, 1);
	const { name, cfg } = registered[0];
	assert.equal(name, "zai");
	assert.equal(cfg.models.length, 1);
	// inference.ts strips the "<backend>/" prefix off the manifest id when it
	// registers the model under its provider; pi re-qualifies with the
	// provider name at call time. The round trip must reproduce the EXACT
	// roster string, or a roster-resolved agent.model would address a model
	// pi never registered.
	const qualified = `${name}/${cfg.models[0].id}`;
	const roster = parseRoster(MANIFEST);
	assert.equal(qualified, roster.main);
	assert.equal(qualified, roster.agents.engineer);
	assert.equal(qualified, MANIFEST.models[0].id);
});

test("extensions/subagents.ts forwards that exact qualified id as the resolved agent.model", async () => {
	const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "roster-parity-"));
	fs.mkdirSync(path.join(agentDir, "agents"), { recursive: true });
	fs.writeFileSync(path.join(agentDir, "inference.json"), JSON.stringify(MANIFEST));
	fs.writeFileSync(
		path.join(agentDir, "agents", "engineer.md"),
		"---\ndescription: eng\n---\nYou are engineer.\n",
	);
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const mod = await import("../extensions/subagents.ts?parity=1");
	const notes = [];
	mod.default({
		on() {},
		registerTool() {},
		registerCommand(_name, cfg) {
			notes.command = cfg;
		},
	});
	const lines = [];
	await notes.command.handler("", { cwd: agentDir, ui: { notify: (m) => lines.push(m) } });
	const registered = [];
	const inferenceMod = await import("../extensions/inference.ts?parity=2");
	inferenceMod.default({
		registerProvider(name, cfg) {
			registered.push(`${name}/${cfg.models[0].id}`);
		},
	});
	assert.match(lines[0], /engineer \(user\) · zai\/glm-5/);
	assert.ok(registered.includes("zai/glm-5"), `provider registration must expose the exact roster id, got: ${registered}`);
});
