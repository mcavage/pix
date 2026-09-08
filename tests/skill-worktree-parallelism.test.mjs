// Independent units are parallel by default through isolated worktrees.
// Preserve that invariant without forcing a bounded change into fake shards
// or coupling the rule to deliver's retired phase/table layout.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const TARGETS = {
	"delegation-guide": path.join(repoRoot, "skills", "delegation-guide", "SKILL.md"),
	deliver: path.join(repoRoot, "skills", "deliver", "SKILL.md"),
};

function read(name) {
	return fs.readFileSync(TARGETS[name], "utf8");
}

// Markdown wraps multi-word rules across lines; whitespace is not semantics.
function normalize(text) {
	return text.replace(/\s+/g, " ");
}

const REQUIRED_PHRASES = [
	{ id: "parallel-by-default", phrase: "parallel by default" },
	{ id: "dependency-dag", phrase: "dependency dag" },
	{ id: "one-worktree-per-unit", phrase: "worktree per concurrent unit" },
	{ id: "one-parallel-call", phrase: "whole ready wave in one parallel" },
	{ id: "merge-after-collection", phrase: "merge reviewed commits after collecting results" },
	{ id: "serialize-real-edges-only", phrase: "real dependency edge or file-conflict edge" },
];

const FORBIDDEN_PHRASES = ["serialize by default", "single shared working tree", "run one at a time in the shared"];

for (const [name] of Object.entries(TARGETS)) {
	test(`skills/${name}/SKILL.md states the worktree-parallel-by-default invariant`, () => {
		const text = normalize(read(name).toLowerCase());
		const missing = REQUIRED_PHRASES.filter((r) => !text.includes(r.phrase)).map((r) => r.id);
		assert.deepEqual(missing, [], `${name}/SKILL.md is missing invariant phrase(s): ${missing.join(", ")}`);
	});

	test(`skills/${name}/SKILL.md does not default to a single shared working tree`, () => {
		const text = normalize(read(name).toLowerCase());
		const present = FORBIDDEN_PHRASES.filter((p) => text.includes(p));
		assert.deepEqual(
			present,
			[],
			`${name}/SKILL.md contains anti-pattern wording that defaults to a shared tree: ${present.join(", ")}`,
		);
	});
}

test("deliver rejects shared-tree serialization without manufacturing bounded-work handoffs", () => {
	const text = normalize(read("deliver"));
	assert.match(text, /a shared working tree is never a reason to serialize/i);
	assert.match(text, /do not shard a bounded one-unit change just to create handoffs/i);
	assert.match(text, /whole ready wave in one parallel.*same turn/i);
	assert.match(text, /otherwise integrate returned patches without committing/i);
});
