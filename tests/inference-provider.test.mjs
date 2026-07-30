import assert from "node:assert/strict";
import { register } from "node:module";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const { compatForBackend, compatForModel, providerBaseURL } = await import("../extensions/inference.ts");

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
