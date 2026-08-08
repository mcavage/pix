// U-Copy-Phase10 Guard — Primary surfaces must use direct punctuation, not em dashes.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const PRIMARY_SURFACES = [
	"README.md",
	"AGENTS.md",
	"docs/getting-started.md",
	"docs/reference.md",
	"pi-kit/spec.yaml",
	"install.sh",
];

// A primary surface that no longer exists is a STALE LIST, not a style
// violation. Conflating the two made deleting docs/MIGRATION.md report as "this
// file contains em dashes", which is both false and a confusing thing to debug.
// Kept as its own assertion so the list stays honest, with a message that says
// what to actually do.
test("the primary-surface list has no stale entries", () => {
	const missing = PRIMARY_SURFACES.filter((p) => !fs.existsSync(path.join(repoRoot, p)));
	assert.deepEqual(missing, [], `PRIMARY_SURFACES names file(s) that no longer exist: ${missing.join(", ")}. Remove them from the list, or restore the files.`);
});

test("primary surfaces carry no em dashes (—)", () => {
	const violations = [];

	for (const relPath of PRIMARY_SURFACES) {
		const fullPath = path.join(repoRoot, relPath);
		if (!fs.existsSync(fullPath)) continue; // covered by the stale-list test above
		const content = fs.readFileSync(fullPath, "utf8");
		const lines = content.split("\n");
		for (let i = 0; i < lines.length; i++) {
			if (lines[i].includes("—")) {
				violations.push(`${relPath}:${i + 1}: ${lines[i].trim()}`);
			}
		}
	}

	// Also check descriptions in public skills/*/SKILL.md
	const skillsDir = path.join(repoRoot, "skills");
	if (fs.existsSync(skillsDir)) {
		const skillEntries = fs.readdirSync(skillsDir, { withFileTypes: true });
		for (const entry of skillEntries) {
			if (entry.isDirectory()) {
				const skillFile = path.join(skillsDir, entry.name, "SKILL.md");
				if (fs.existsSync(skillFile)) {
					const content = fs.readFileSync(skillFile, "utf8");
					const descMatch = content.match(/^description:\s*(.*)$/m);
					if (descMatch && descMatch[1].includes("—")) {
						violations.push(`skills/${entry.name}/SKILL.md description: ${descMatch[1]}`);
					}
				}
			}
		}
	}

	assert.deepEqual(
		violations,
		[],
		`Primary surfaces contain em dashes (—). Replace with direct punctuation:\n  ${violations.join("\n  ")}`,
	);
});
