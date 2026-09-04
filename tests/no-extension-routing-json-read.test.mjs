// E3.2: extensions resolve the agent/session model roster from the additive
// `roster` field on the generated inference.json v1 manifest (docs/design/
// environments.md §5.2/§6.4/§7) instead of a second, generated routing.json
// artifact. This is the drift sentinel: it fails the moment any sandbox
// extension source re-adds a read of routing.json, in code OR in a stale
// comment that would mislead the next reader. The routing.json ARTIFACT and
// its host-side package are untouched here on purpose — deleting either is
// Wave F's job, not this one's.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const extensionsDir = path.join(repoRoot, "extensions");

const extensionFiles = fs
	.readdirSync(extensionsDir)
	.filter((f) => f.endsWith(".ts"))
	.sort();

test("at least the three roster-reading extensions are present to check", () => {
	for (const f of ["inference.ts", "subagents.ts", "ollama-bridge.ts"]) {
		assert.ok(extensionFiles.includes(f), `expected extensions/${f} to exist`);
	}
});

for (const file of extensionFiles) {
	test(`extensions/${file} never mentions routing.json`, () => {
		const src = fs.readFileSync(path.join(extensionsDir, file), "utf8");
		assert.doesNotMatch(
			src,
			/routing\.json/,
			`extensions/${file} must not read (or reference reading) routing.json; the additive inference.json roster replaces it`,
		);
	});
}

test("lib/inference-roster.ts (the shared roster reader) never mentions routing.json either", () => {
	const src = fs.readFileSync(path.join(repoRoot, "lib", "inference-roster.ts"), "utf8");
	assert.doesNotMatch(src, /routing\.json/);
});

// E3.2 removed the EXTENSION reads; Wave F then deleted what they read. Both
// halves are asserted here so a revert of either one fails loudly: an
// extension reading a file that no longer exists is the same bug as the file
// coming back for an extension to read.
test("the routing.json artifact and its host package are gone (Wave F)", () => {
	assert.ok(!fs.existsSync(path.join(repoRoot, "routing.json")), "routing.json must not exist");
	assert.ok(
		!fs.existsSync(path.join(repoRoot, "services", "host", "routing")),
		"services/host/routing must not exist",
	);
});
