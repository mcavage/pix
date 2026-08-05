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

test("onboarding never claims ordinary setup creates a pack", () => {
	assert.match(skill, /`pack\.exists` is false, say creating one is a `pack\.toml`/s);
	assert.match(skill, /Ordinary `pix\s+setup` does not/s);
	assert.doesNotMatch(skill, /`pix setup` \(or `pix pack new/);
});
