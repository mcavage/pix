import assert from "node:assert/strict";
import { register } from "node:module";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const { compatForBackend } = await import("../extensions/inference.ts");

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
