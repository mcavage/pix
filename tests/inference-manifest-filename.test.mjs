// Cross-language anti-drift guard for the generated inference manifest's
// filename.
//
// The host (Go, services/host/inference/live.go, SynthesizeInferenceKit)
// writes the manifest into the create-time mixin; the sandbox (TypeScript,
// extensions/inference.ts + extensions/ollama-bridge.ts) reads it back by a
// HARDCODED path.join(getAgentDir(), "inference.json") literal on each side.
// There is no shared header the two languages both import (TS extensions are
// standalone factories pi loads directly — see AGENTS.md's "never put a
// non-extension .ts in extensions/" rule — so a cross-language constant would
// need its own shipping-path wiring for a one-line filename). That means
// nothing in the TYPE system catches a rename on one side: it shipped once as
// `files/home/.pi/agent/json` on the Go side against `inference.json` reads
// on the TS side, and the failure mode was silent (present file, wrong name,
// nothing read it, fallback Anthropic 401 with no signal pointing here).
//
// This test is the guard: it greps the Go writer's literal filename constant
// and the TS readers' literal path segments and asserts all three are the
// same string, byte for byte. A future rename of either side without the
// other fails THIS test, not a live sandbox weeks later.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const liveGo = fs.readFileSync(path.join(repoRoot, "services/host/inference/live.go"), "utf8");
const inferenceTs = fs.readFileSync(path.join(repoRoot, "extensions/inference.ts"), "utf8");
const ollamaBridgeTs = fs.readFileSync(path.join(repoRoot, "extensions/ollama-bridge.ts"), "utf8");

test("Go writer declares an inferenceManifestFilename constant", () => {
	const m = liveGo.match(/const inferenceManifestFilename = "([^"]+)"/);
	assert.ok(m, 'expected `const inferenceManifestFilename = "..."` in live.go');
	assert.equal(m[1], "inference.json");
});

test("Go writer's os.WriteFile call uses the named constant, not a re-spelled literal", () => {
	// Guards against the SAME drift one level down: introducing the constant
	// but writing os.WriteFile(filepath.Join(agentDir, "inference.json"), ...)
	// (a literal) instead of os.WriteFile(filepath.Join(agentDir,
	// inferenceManifestFilename), ...) would still pass every other check
	// here while leaving the constant an unused, lying source of truth.
	assert.match(
		liveGo,
		/os\.WriteFile\(filepath\.Join\(agentDir, inferenceManifestFilename\), append\(b, '\\n'\), 0o600\)/,
		"expected the manifest write to reference inferenceManifestFilename by name",
	);
	// And the historical bug's exact literal must never come back.
	assert.doesNotMatch(
		liveGo,
		/filepath\.Join\(agentDir,\s*"json"\)/,
		'the manifest must never be written to a bare "json" file again',
	);
});

test("extensions/inference.ts reads the same filename the Go writer declares", () => {
	const goFilename = liveGo.match(/const inferenceManifestFilename = "([^"]+)"/)[1];
	const m = inferenceTs.match(/path\.join\(getAgentDir\(\), "([^"]+)"\)/);
	assert.ok(m, 'expected `path.join(getAgentDir(), "...")` in extensions/inference.ts');
	assert.equal(m[1], goFilename);
});

test("extensions/ollama-bridge.ts reads the same filename the Go writer declares", () => {
	const goFilename = liveGo.match(/const inferenceManifestFilename = "([^"]+)"/)[1];
	const m = ollamaBridgeTs.match(/join\(getAgentDir\(\), "([^"]+)"\)/);
	assert.ok(m, 'expected `join(getAgentDir(), "...")` in extensions/ollama-bridge.ts');
	assert.equal(m[1], goFilename);
});

test("both TS readers agree with each other on the manifest path segment", () => {
	const a = inferenceTs.match(/path\.join\(getAgentDir\(\), "([^"]+)"\)/)[1];
	const b = ollamaBridgeTs.match(/join\(getAgentDir\(\), "([^"]+)"\)/)[1];
	assert.equal(a, b, "extensions/inference.ts and extensions/ollama-bridge.ts must read the identical manifest filename");
});

// A present-but-invalid manifest must be diagnosed loudly (stderr), never
// silently swallowed the way the filename bug's absence WAS silently
// swallowed — that silence is what let the bug ship unnoticed. This does not
// re-run the extensions (they load through pi's own runtime); it asserts the
// diagnostic-writing code path exists in source, on both readers, so a future
// edit that deletes the warning without replacing it fails a review-visible
// test instead of only a live sandbox.
test("extensions/inference.ts warns loudly (not silently) on a present-but-invalid manifest", () => {
	assert.match(inferenceTs, /process\.stderr\.write\(\s*`\[inference\][^`]*present[^`]*/s);
});

test("extensions/ollama-bridge.ts warns loudly (not silently) on a present-but-invalid manifest", () => {
	assert.match(ollamaBridgeTs, /process\.stderr\.write\(`\[ollama-bridge\][^`]*present[^`]*/);
});
