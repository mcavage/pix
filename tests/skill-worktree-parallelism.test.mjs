// Asserts the orchestration correction: independent units/waves are PARALLEL
// BY DEFAULT through isolated git worktrees, not serialized merely because
// the current working tree is shared. Both `delegation-guide` (the general
// wave-execution rule) and `deliver` (its own DELEGATE phase, which used to
// read as "parallel only when the caller happens to set up worktrees") must
// state this explicitly: identify the dependency DAG, one worktree per
// concurrent unit, launch the whole ready wave in one parallel subagent call,
// merge reviewed commits after collection, and serialize only a real
// dependency or file-conflict edge — never because the tree is shared.
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

// Phrase matching is done against whitespace-collapsed text: markdown prose
// wraps at ~80 cols, so a multi-word invariant phrase routinely spans a line
// break in the source file even though it reads as one phrase to a person.
function normalize(text) {
	return text.replace(/\s+/g, " ");
}

// The invariant, split into the required phrases (case-insensitive substring
// match) that MUST all appear in BOTH skills. Each phrase pins one clause of
// the correction so a future edit can't quietly drop one while keeping the
// others.
const REQUIRED_PHRASES = [
	{ id: "parallel-by-default", phrase: "parallel by default" },
	{ id: "dependency-dag", phrase: "dependency dag" },
	{ id: "one-worktree-per-unit", phrase: "worktree per concurrent unit" },
	{ id: "one-parallel-call", phrase: "whole ready wave in one parallel" },
	{ id: "merge-after-collection", phrase: "merge reviewed commits after collecting results" },
	{ id: "serialize-real-edges-only", phrase: "real dependency edge or file-conflict edge" },
];

// Wording that would reintroduce the anti-pattern this correction fixes: a
// single shared working tree treated as a legitimate reason to run units one
// at a time. These strings must NEVER appear (case-insensitive); the skills
// are written to explicitly disclaim them instead ("never a reason to
// serialize" / "never because they happen to share a working tree").
// Note: "because the tree is shared" is intentionally NOT in this list —
// deliver's Forbidden table legitimately names it as the anti-pattern being
// corrected (see the dedicated test below), not as guidance.
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

// The one place "because the tree is shared" IS expected: deliver's Forbidden
// table names it as the anti-pattern being corrected, not as guidance. Assert
// it appears there paired with the corrected behavior, not as a bare rule.
test("deliver's Forbidden table names the shared-tree anti-pattern with the worktree-per-unit correction", () => {
	const text = read("deliver");
	assert.match(
		text,
		/Serial-when-parallel \| Independent units run one at a time because the tree is shared \| Parallel by default: one worktree per unit/,
	);
});
