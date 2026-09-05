// Onboarding guidance is model-executed prose, so pin its load-bearing state
// predicates here. Host-state tests prove the booleans; these checks prove the
// skill interprets them without inventing mandatory setup work.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const skill = fs.readFileSync(path.join(repoRoot, "skills", "onboarding", "SKILL.md"), "utf8");

test("onboarding requires setup only when no inference route is resolved", () => {
	assert.match(skill, /No usable inference.*`keys\.resolved` is false/s);
	assert.match(skill, /Individual\s+provider booleans may remain false/s);
	assert.doesNotMatch(skill, /Any model key false/);
});

test("onboarding distinguishes disabled memory from enabled-but-stopped memory", () => {
	assert.match(
		skill,
		/Memory is enabled but stopped.*`memory\.enabled` is true and\s+`memory\.up` is false/s,
	);
	assert.match(skill, /If memory is\s+disabled, do not call it broken or tell the user to start it/s);
});

// Pix v2 deleted packs outright (docs/design/pix-v2-architecture.md §14):
// there is no `pack.toml`, no `pack use`, and the trusted host-state payload
// (services/host/workflow/launch/hoststate.go's HostState) carries no `pack`
// field at all. Onboarding must not invent one.
test("onboarding never claims or invents a pack", () => {
	assert.doesNotMatch(skill, /pack\.active/);
	assert.doesNotMatch(skill, /pack\.exists/);
	assert.doesNotMatch(skill, /pack\.toml/);
	assert.doesNotMatch(skill, /`pix pack/);
	assert.match(skill, /Pix v2 has no pack/s);
});
