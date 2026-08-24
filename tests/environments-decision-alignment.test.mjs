// Traceability gate for the E1.0 pre-merge review BLOCK: the reviewer could
// not verify complete D1-D24 reconciliation because nothing in the live
// design doc traced the PRD's 24 decisions to where each one actually lives.
//
// The fix is not a second copy of the prose (that duplicates drift risk
// instead of removing it) — it is one small index table, `docs/design/
// environments.md` §17, naming every decision ID once and pointing at the
// section that carries its actual mechanism. This test is the anti-drift
// assertion for that table: D1-D24 each appear exactly once, in order, with
// no gap, no duplicate, and no dangling section pointer.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const designDoc = fs.readFileSync(path.join(repoRoot, "docs", "design", "environments.md"), "utf8");

const TABLE_HEADING = /^## 17\. Decision alignment \(D1[\u2013-]D24\)\s*$/m;

function alignmentSection(doc) {
	const start = doc.search(TABLE_HEADING);
	assert.notEqual(start, -1, "docs/design/environments.md: missing the '## 17. Decision alignment (D1\u2013D24)' section");
	const rest = doc.slice(start);
	const next = rest.slice(1).search(/^## /m);
	return next === -1 ? rest : rest.slice(0, next + 1);
}

// A heading is either top-level ("## 3. Ownership boundary") or a sub-heading
// ("### 3.1 sbx owns"). Top-level numbers carry a trailing period before the
// title; sub-numbers do not. This checks the referenced section still exists,
// so a renumbered or deleted heading breaks the pointer instead of silently
// going stale.
function headingExists(doc, num) {
	const escaped = num.replace(/\./g, "\\.");
	const pattern = num.includes(".")
		? new RegExp(`^#{2,4}\\s+${escaped}\\s+\\S`, "m")
		: new RegExp(`^#{2,4}\\s+${escaped}\\.\\s+\\S`, "m");
	return pattern.test(doc);
}

test("design doc has a decision-alignment table for D1-D24", () => {
	const section = alignmentSection(designDoc);
	assert.match(section, /\|\s*ID\s*\|\s*Decision\s*\|\s*Section\s*\|/, "table must have ID/Decision/Section columns");
});

test("D1-D24 each appear exactly once in the alignment table, in order, with no gap or duplicate", () => {
	const section = alignmentSection(designDoc);
	const rows = [...section.matchAll(/^\|\s*(D\d{1,2})\s*\|\s*(.+?)\s*\|\s*(\S.*?)\s*\|\s*$/gm)];
	const ids = rows.map((r) => r[1]);

	const expected = Array.from({ length: 24 }, (_, i) => `D${i + 1}`);
	assert.deepEqual(ids, expected, "the table must list D1..D24, in order, with no gap or duplicate");

	for (const row of rows) {
		assert.notEqual(row[2].trim(), "", `${row[1]}: decision text must not be empty`);
	}
});

test("every alignment-table section pointer names a section heading that actually exists", () => {
	const section = alignmentSection(designDoc);
	const rows = [...section.matchAll(/^\|\s*(D\d{1,2})\s*\|\s*(.+?)\s*\|\s*(\S.*?)\s*\|\s*$/gm)];
	assert.ok(rows.length > 0, "no alignment rows parsed; regex or table shape drifted");

	const missing = [];
	for (const [, id, , sectionRefs] of rows) {
		const nums = [...sectionRefs.matchAll(/\u00a7([0-9]+(?:\.[0-9]+)?)/g)].map((m) => m[1]);
		if (nums.length === 0) missing.push(`${id}: no \u00a7section reference found in "${sectionRefs}"`);
		for (const num of nums) {
			if (!headingExists(designDoc, num)) missing.push(`${id}: section \u00a7${num} has no matching heading`);
		}
	}
	assert.deepEqual(missing, [], `dangling section pointers:\n${missing.join("\n")}`);
});

test("no D-id reference leaks outside the alignment table into other prose in the design doc", () => {
	// This is what actually prevents drift: a future edit that pastes "D7"
	// into some other section's prose instead of updating the one table row
	// would otherwise go unnoticed. The table itself (heading included, since
	// the heading names D1 and D24 as the range) is carved out; everything
	// else in the document must be D-id-free.
	const section = alignmentSection(designDoc);
	const outside = designDoc.slice(0, designDoc.indexOf(section)) + designDoc.slice(designDoc.indexOf(section) + section.length);
	const leaked = [...outside.matchAll(/\bD\d{1,2}\b/g)].map((m) => m[0]);
	assert.deepEqual(leaked, [], `D-id reference(s) found outside the §17 alignment table: ${leaked.join(", ")}`);
});
