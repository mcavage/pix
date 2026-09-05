import assert from "node:assert/strict";
import { register } from "node:module";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const { compatForBackend, compatForModel, hasValidLimits, providerBaseURL, suppressUnusedDirectCredentialSentinels } = await import("../extensions/inference.ts");

test("generated providers require catalog-owned context and output limits", () => {
	assert.equal(hasValidLimits({ context_window: 1050000, max_tokens: 128000 }), true);
	assert.equal(hasValidLimits({ context_window: undefined, max_tokens: 128000 }), false);
	assert.equal(hasValidLimits({ context_window: 131072, max_tokens: undefined }), false);
	assert.equal(hasValidLimits({ context_window: 1000, max_tokens: 1001 }), false);
});

test("generated Responses gateways suppress the underscore session_id header", () => {
	assert.deepEqual(
		compatForBackend({ driver: "openai-compatible", protocol: "openai-responses", auth: "sbx-session" }),
		{ sessionAffinityFormat: "openai-nosession" },
	);
});

test("non-Responses providers retain their native transport behavior", () => {
	for (const protocol of ["openai-completions", "anthropic-messages", "google-generative-ai", undefined]) {
		assert.equal(compatForBackend({ driver: "openai-compatible", protocol, auth: "none" }), undefined);
	}
});

test("Anthropic SDK gateways do not duplicate the v1 path", () => {
	assert.equal(
		providerBaseURL({
			driver: "openai-compatible",
			protocol: "anthropic-messages",
			auth: "sbx-session",
			base_url: "https://gateway.example/inference/anthropic/v1",
		}),
		"https://gateway.example/inference/anthropic",
	);
	assert.equal(
		providerBaseURL({
			driver: "openai-compatible",
			protocol: "openai-responses",
			auth: "sbx-session",
			base_url: "https://gateway.example/inference/openai/v1",
		}),
		"https://gateway.example/inference/openai/v1",
	);
});

test("catalog metadata selects Anthropic adaptive thinking without guessing model names", () => {
	assert.deepEqual(
		compatForModel(
			{ driver: "openai-compatible", protocol: "anthropic-messages", auth: "sbx-session" },
			{ id: "gateway/flagship", catalog_model: "anthropic/future-model", backend: "gateway", name: "Future", adaptive_thinking: true },
		),
		{ forceAdaptiveThinking: true },
	);
	assert.equal(
		compatForModel(
			{ driver: "openai-compatible", protocol: "anthropic-messages", auth: "sbx-session" },
			{ id: "gateway/legacy", catalog_model: "anthropic/legacy", backend: "gateway", name: "Legacy" },
		),
		undefined,
	);
});

test("exclusive gateways hide native proxy sentinels from direct-search extensions", () => {
	const saved = Object.fromEntries(["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"].map((k) => [k, process.env[k]]));
	try {
		process.env.ANTHROPIC_API_KEY = "proxy-managed";
		process.env.OPENAI_API_KEY = "proxy-managed";
		process.env.GEMINI_API_KEY = "real-user-value";
		suppressUnusedDirectCredentialSentinels({
			version: 1,
			backends: { gateway: { driver: "openai-compatible", auth: "sbx-session" } },
			models: [],
		});
		assert.equal(process.env.ANTHROPIC_API_KEY, undefined);
		assert.equal(process.env.OPENAI_API_KEY, undefined);
		assert.equal(process.env.GEMINI_API_KEY, "real-user-value", "never delete a real value");
	} finally {
		for (const [key, value] of Object.entries(saved)) {
			if (value === undefined) delete process.env[key];
			else process.env[key] = value;
		}
	}
});

test("native runtime keeps its own proxy-managed credential sentinel", () => {
	const saved = process.env.OPENAI_API_KEY;
	try {
		process.env.OPENAI_API_KEY = "proxy-managed";
		suppressUnusedDirectCredentialSentinels({
			version: 1,
			backends: { openai: { driver: "native", auth: "1password", key_env: "OPENAI_API_KEY" } },
			models: [],
		});
		assert.equal(process.env.OPENAI_API_KEY, "proxy-managed");
	} finally {
		if (saved === undefined) delete process.env.OPENAI_API_KEY;
		else process.env.OPENAI_API_KEY = saved;
	}
});

test("gateway registration preserves canonical prices, cache tiers and thinking levels", async () => {
	const fs = await import("node:fs");
	const os = await import("node:os");
	const path = await import("node:path");
	const { builtinModels } = await import("./stubs/pi-ai.mjs");
	const cost = { input: 10, output: 50, cacheRead: 1, cacheWrite: 12.5,
		tiers: [{ inputTokensAbove: 272000, input: 20, output: 75, cacheRead: 2, cacheWrite: 25 }] };
	const thinkingLevelMap = { off: null, minimal: null, high: "high", max: "max" };
	builtinModels.set("openai/gpt-6-astra", { cost, thinkingLevelMap, input: ["text", "image"] });
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-pricing-"));
	const previous = process.env.PI_TEST_AGENT_DIR;
	try {
		process.env.PI_TEST_AGENT_DIR = dir;
		fs.writeFileSync(path.join(dir, "inference.json"), JSON.stringify({
			version: 1,
			backends: { work: { driver: "openai-compatible", protocol: "openai-responses", base_url: "https://gateway.example/v1", auth: "sbx-session" } },
			models: [{ id: "work/gpt-6-astra", catalog_model: "openai/gpt-6-astra", backend: "work", context_window: 1050000, max_tokens: 128000 }],
		}));
		const registered = [];
		const { default: inference } = await import("../extensions/inference.ts");
		inference({ registerProvider: (name, config) => registered.push({ name, config }) });
		assert.equal(registered.length, 1);
		const model = registered[0].config.models[0];
		assert.equal(model.id, "gpt-6-astra");
		assert.deepEqual(model.cost, cost);
		assert.deepEqual(model.thinkingLevelMap, thinkingLevelMap);
		assert.deepEqual(model.input, ["text", "image"]);
	} finally {
		if (previous === undefined) delete process.env.PI_TEST_AGENT_DIR;
		else process.env.PI_TEST_AGENT_DIR = previous;
		builtinModels.delete("openai/gpt-6-astra");
		fs.rmSync(dir, { recursive: true, force: true });
	}
});
