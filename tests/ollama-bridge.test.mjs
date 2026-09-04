import assert from "node:assert/strict";
import * as fs from "node:fs";
import { register } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const { modelsFromManifest, readRoster } = await import("../extensions/ollama-bridge.ts");

const TAG = {
	id: "qwen3.5:9b",
	name: "qwen3.5:9b (local)",
	reasoning: true,
	input: ["text"],
	cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
	contextWindow: 32768,
	maxTokens: 8192,
};

// The reported symptom: a box whose config wires two Ollama cloud models opened
// every session with
//   Warning: No models match pattern "ollama/deepseek-v4-flash:cloud"
//   Warning: No models match pattern "ollama/glm-5.2:cloud"
// because `pix run` passes pi a --models cycle built from EVERY callable
// binding while this extension registered exactly one hardcoded model.
test("every ollama model in the manifest is registered, so the --models cycle matches", () => {
	const models = modelsFromManifest(
		{
			models: [
				{ id: "ollama/glm-5.2:cloud", backend: "ollama", name: "GLM 5.2", context_window: 200000, max_tokens: 16384 },
				{ id: "ollama/deepseek-v4-flash:cloud", backend: "ollama", name: "DeepSeek V4 Flash" },
				{ id: "anthropic/claude-opus-5", backend: "anthropic", name: "Opus 5" },
			],
		},
		TAG,
	);
	const ids = models.map((m) => m.id);
	assert.ok(ids.includes("glm-5.2:cloud"), `missing cloud model: ${ids}`);
	assert.ok(ids.includes("deepseek-v4-flash:cloud"), `missing cloud model: ${ids}`);
	assert.ok(!ids.some((id) => id.startsWith("anthropic")), `other backends must not be registered on the ollama provider: ${ids}`);
});

// pi qualifies a model with the provider it was registered under, so leaving
// the manifest's backend prefix on would produce "ollama/ollama/glm-5.2:cloud"
// — which matches no pattern either, just differently.
test("the manifest's backend prefix is stripped, never double-qualified", () => {
	const [m] = modelsFromManifest(
		{ models: [{ id: "ollama/glm-5.2:cloud", backend: "ollama" }] },
		TAG,
	).filter((x) => x.id !== TAG.id);
	assert.equal(m.id, "glm-5.2:cloud");
});

test("catalog limits are carried through, with the bridge tag's as the floor", () => {
	const models = modelsFromManifest(
		{
			models: [
				{ id: "ollama/glm-5.2:cloud", backend: "ollama", context_window: 200000, max_tokens: 16384 },
				{ id: "ollama/bare:cloud", backend: "ollama" },
			],
		},
		TAG,
	);
	const glm = models.find((m) => m.id === "glm-5.2:cloud");
	assert.equal(glm.contextWindow, 200000);
	assert.equal(glm.maxTokens, 16384);
	// No declared limits -> fall back to the bridge tag's, never to 0/undefined
	// (pi uses these for context accounting and compaction).
	const bare = models.find((m) => m.id === "bare:cloud");
	assert.equal(bare.contextWindow, TAG.contextWindow);
	assert.equal(bare.maxTokens, TAG.maxTokens);
});

// The bridge tag is what `pix config set ollama_bridge_model` promises. It must
// survive a manifest that does not mention it, and must not be duplicated by
// one that does.
test("the configured bridge tag is always present exactly once", () => {
	const without = modelsFromManifest({ models: [{ id: "ollama/glm-5.2:cloud", backend: "ollama" }] }, TAG);
	assert.equal(without.filter((m) => m.id === TAG.id).length, 1);

	const with_ = modelsFromManifest({ models: [{ id: "ollama/qwen3.5:9b", backend: "ollama", name: "Qwen" }] }, TAG);
	assert.equal(with_.filter((m) => m.id === TAG.id).length, 1);
	assert.equal(with_.length, 1);
});

// A missing or malformed manifest is the pre-manifest world (host mode, an
// older launcher, a pack-built sandbox). Degrading to the single bridge tag is
// the previous behavior, which was correct there.
test("a missing or junk manifest degrades to the bridge tag alone", () => {
	for (const junk of [null, undefined, {}, { models: null }, { models: "nope" }, { models: [null, 7, { backend: "ollama" }] }]) {
		const models = modelsFromManifest(junk, TAG);
		assert.deepEqual(models.map((m) => m.id), [TAG.id], `junk input ${JSON.stringify(junk)}`);
	}
});

// E3.2: the additive `roster` field is compatible with everything above —
// the bridge's own model-list building is UNCHANGED by its presence (the
// parent-Ollama inheritance exception stays a subagents.ts concern, per
// docs/design/environments.md §6.4).
test("a roster key on the manifest never changes the registered ollama model list", () => {
	const withoutRoster = modelsFromManifest(
		{ models: [{ id: "ollama/glm-5.2:cloud", backend: "ollama", name: "GLM 5.2" }] },
		TAG,
	);
	const withRoster = modelsFromManifest(
		{
			models: [{ id: "ollama/glm-5.2:cloud", backend: "ollama", name: "GLM 5.2" }],
			roster: { main: "ollama/glm-5.2:cloud", agents: { engineer: "ollama/glm-5.2:cloud" } },
		},
		TAG,
	);
	assert.deepEqual(withRoster, withoutRoster);
});

// extensions/ollama-bridge.ts reads the SAME additive roster shape the other
// two readers do, from the SAME file — never a second, divergent read.
test("readRoster resolves the roster from <agentDir>/inference.json", () => {
	const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "ollama-bridge-roster-"));
	const saved = process.env.PI_TEST_AGENT_DIR;
	try {
		process.env.PI_TEST_AGENT_DIR = agentDir;
		assert.equal(readRoster(), undefined, "absent inference.json -> no roster");
		fs.writeFileSync(
			path.join(agentDir, "inference.json"),
			JSON.stringify({ version: 1, backends: {}, models: [], roster: { main: "ollama/glm-5.2:cloud", agents: {} } }),
		);
		assert.deepEqual(readRoster(), { main: "ollama/glm-5.2:cloud", agents: {} });
		fs.writeFileSync(path.join(agentDir, "inference.json"), "{ not json");
		assert.equal(readRoster(), undefined, "unparseable manifest -> no roster, never throws");
	} finally {
		if (saved === undefined) delete process.env.PI_TEST_AGENT_DIR;
		else process.env.PI_TEST_AGENT_DIR = saved;
	}
});
