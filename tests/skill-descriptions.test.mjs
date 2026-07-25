// U-W0b.07: cap on the YAML frontmatter `description` field in every public
// skills/*/SKILL.md. Descriptions are what gets loaded into every prompt as
// the skill catalog (see extensions/help.ts and pi's own skill-discovery),
// so a runaway description is a standing prompt-size tax paid on every turn,
// not a one-time doc nit. Enforced here, not by hand-review, because there is
// no other gate on skill authoring before a SKILL.md lands.
//
// Also reconciles the .dockerignore skills/ allowlist against the actual
// skills/ directories on disk (see .dockerignore's "Open-core boundary:
// skills" block): an allowlist entry for a skill that was never created (or
// a skill directory missing from the allowlist) is silent drift that only
// shows up as a missing skill in a built image, long after the fact.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const skillsDir = path.join(repoRoot, "skills");
const MAX_DESCRIPTION_LEN = 200;

function listSkillDirs() {
	return fs
		.readdirSync(skillsDir, { withFileTypes: true })
		.filter((e) => e.isDirectory())
		.map((e) => e.name)
		.sort();
}

// Pulls the raw `description:` value out of a SKILL.md's YAML frontmatter.
// Every public SKILL.md uses a single-line plain scalar (no leading quote,
// no ": " inside it — that combination is invalid YAML plain-scalar syntax),
// so a straight substring is exact and needs no YAML dependency in this repo.
function readDescription(skillName) {
	const file = path.join(skillsDir, skillName, "SKILL.md");
	const text = fs.readFileSync(file, "utf8");
	const fmMatch = text.match(/^---\n([\s\S]*?)\n---\n/);
	assert.ok(fmMatch, `${skillName}/SKILL.md must have --- frontmatter --- `);
	const line = fmMatch[1].split("\n").find((l) => l.startsWith("description:"));
	assert.ok(line, `${skillName}/SKILL.md frontmatter must have a description: field`);
	const value = line.slice("description:".length).trim();
	assert.ok(
		!value.startsWith("'") && !value.startsWith('"'),
		`${skillName}: description is a quoted/block YAML scalar; update readDescription() to unquote it before length-checking`,
	);
	assert.ok(
		!/: /.test(value),
		`${skillName}: description contains ": " which is invalid in a plain YAML scalar (would need quoting)`,
	);
	return value;
}

test("every public skills/*/SKILL.md description is <=200 chars", () => {
	const overLimit = [];
	for (const name of listSkillDirs()) {
		const description = readDescription(name);
		if (description.length > MAX_DESCRIPTION_LEN) {
			overLimit.push(`${name} (${description.length} chars)`);
		}
	}
	assert.deepEqual(overLimit, [], `descriptions over ${MAX_DESCRIPTION_LEN} chars: ${overLimit.join(", ")}`);
});

test("no skill description is empty", () => {
	for (const name of listSkillDirs()) {
		assert.ok(readDescription(name).length > 0, `${name}: description must not be empty`);
	}
});

// ── .dockerignore reconciliation ────────────────────────────────────────────

// Parses the `!skills/<name>` re-include lines out of the "Open-core
// boundary: skills" allowlist block in .dockerignore (the block starts at
// the `skills/*` DEFAULT-DENY line and runs until the next blank line).
function readDockerignoreSkillAllowlist() {
	const text = fs.readFileSync(path.join(repoRoot, ".dockerignore"), "utf8");
	const lines = text.split("\n");
	const denyIdx = lines.indexOf("skills/*");
	assert.ok(denyIdx >= 0, ".dockerignore must have a bare `skills/*` DEFAULT-DENY line");
	const allowed = [];
	for (let i = denyIdx + 1; i < lines.length; i++) {
		const line = lines[i];
		if (line.trim() === "") break;
		const m = line.match(/^!skills\/(\S+)$/);
		assert.ok(m, `unexpected line in the skills/ allowlist block: ${JSON.stringify(line)}`);
		allowed.push(m[1]);
	}
	return allowed;
}

test(".dockerignore skills/ allowlist matches the actual skills/ directories on disk", () => {
	const onDisk = listSkillDirs();
	const allowlisted = readDockerignoreSkillAllowlist();

	const allowlistedButMissing = allowlisted.filter((name) => !onDisk.includes(name));
	assert.deepEqual(
		allowlistedButMissing,
		[],
		`.dockerignore allowlists a skill with no matching skills/<name> directory: ${allowlistedButMissing.join(", ")}`,
	);

	const onDiskButNotAllowlisted = onDisk.filter((name) => !allowlisted.includes(name));
	assert.deepEqual(
		onDiskButNotAllowlisted,
		[],
		`skills/ has a directory not re-included in the .dockerignore allowlist (won't ship in the public image): ${onDiskButNotAllowlisted.join(", ")}`,
	);
});
