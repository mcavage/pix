// Traceability gate for the E1.0 pre-merge review BLOCK: the reviewer could
// not verify complete D1-D24 reconciliation because nothing in the live
// design doc traced the PRD's 24 decisions to where each one actually lives.
//
// Wave B closeout finding: the first version of this gate only checked that
// `docs/design/environments.md` §17 was internally consistent (24 rows, in
// order, pointers resolve). It never checked the table against the actual
// PRD (`PRD: native sandbox environments` §2, "Fixed decisions and taste
// calls"). The design doc had invented its OWN 24 decisions and slapped the
// D1-D24 labels on them, which collide with the real PRD's D1-D24 (verb
// naming, `--effective`, exit codes, the recreate log, ...) without being the
// same decisions at all. A reader who trusted the label got the wrong
// content.
//
// The fix pins the PRD's decision text as the source of truth here (PRD
// files are project working documents outside this repo, not something a
// portable repo test can read off disk), then PARSES it with the same table
// regex used against the design doc, so both sides go through one code path
// and cannot silently diverge in shape. D1-D24 each appear exactly once, in
// order, with the design doc's row text carrying the PRD's decision as at
// least a recognizable prefix, and with every section pointer resolving to a
// heading that still exists.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const designDoc = fs.readFileSync(path.join(repoRoot, "docs", "design", "environments.md"), "utf8");

// Verbatim from "PRD: native sandbox environments" §2, "Fixed decisions and
// taste calls" (the "Decision" column only; "Why" and "Rejected alternative"
// are that PRD's own columns and are not reproduced here). D21-D24 are the
// three decisions the PRD's own §9 records as "closed decisions that were
// previously open"; their one-line text below matches both places the PRD
// states them.
const PRD_DECISIONS_TABLE = `
| # | Decision |
| --- | --- |
| D1 | One authored sandbox grammar, \`.sbxenv.yaml\`, owned upstream; one narrow sidecar \`pix.toml\` for Pi/Pix gaps only |
| D2 | The verb is **\`pix env forget NAME\`**, not \`env rm\` |
| D3 | \`pix env rm\` is dispatched to a pointer error that performs nothing and names all three removal objects |
| D4 | \`pix env edit NAME pix\\|sbxenv\` is an exact positional enum. TTY with no token prints a two-line selection; non-TTY with no token exits 2 |
| D5 | After \`edit\`, print \`pix env review NAME\`. No inline \`[y/N]\` |
| D6 | The optional \`--review\` pre-commitment flag is **P1** |
| D7 | \`env show\` is a lossy summary by default, \`--effective\` renders the byte-identical document, \`--path\` prints only the path |
| D8 | \`--effective\` renders with **no sandbox in existence** |
| D9 | An unknown explicit \`--env\` exits non-zero and launches nothing. Never falls back to the default |
| D10 | Zero-path \`pix env add NAME\` is refused when \`$PWD\` contains a \`.sbxenv.yaml\`, naming both register and scaffold intents |
| D11 | The recreate line is \`pix rm NAME && pix run --env ENV\`. Pix never asks the user to invent \`--name\` |
| D12 | The recreate refusal names the drifted facets by canonical key path |
| D13 | \`pix setup\` has no environment step, no prompt, no probe. One closing \`pix help env\` pointer, plus one conditional launch hint |
| D14 | Exact-name suggestions are **informational data**, never an offer. \`closest: home\` is allowed; \`did you mean home? [Y/n]\` is not |
| D15 | Trust review shows counts **plus** host commands/services, credential destinations, and mount expansion by default; full argv and digests behind \`--verbose\` |
| D16 | \`pix env review NAME\` stays the explicit audit boundary. Non-TTY fails closed without \`--yes\` |
| D17 | The no-environment state is named \`none\`; the prose is *built-in defaults* |
| D18 | Model selection is a literal table. No score, price, benchmark, status taxonomy, or \`WHY\` column |
| D19 | Exit codes: **2** for usage errors and refusals, non-zero-not-2 for operational failure, **0** only for a completed operation (including "printed the path because \`$EDITOR\` was unset") |
| D20 | No \`pix env current\` verb |
| D21 | \`pix reset\` invalidates **every** environment trust acceptance, scaffolded or externally registered, and deletes **no** environment source |
| D22 | The \`I4\` recreate log keeps at most **100** records, oldest dropped. \`pix doctor\` prints **one line, only when the count is nonzero**; full facet key paths need explicit \`pix doctor --recreates\` |
| D23 | Drift attribution reads the adapter's **pre-composition semantic tree**. Facets are attributed by stable identity where the native schema has one (\`mcp.servers[<name>]\`, bindings by service/domain, host services by port); indexed paths (\`mounts[2]\`) only where the schema has no identity. Never an opaque hash-only message |
| D24 | A new \`pix env\` verb is **not** how diagnostics ship. \`--recreates\` is a flag on the existing \`doctor\` verb |
`;

/**
 * Parses a `| Dn | text |` (or `| Dn | text | section |`) table body into
 * `{id, text, sections?}` rows. The internal separators use `[ \t]`, not
 * `\s`, deliberately: `\s` matches a newline, so an optional trailing
 * column would let one row's match run on and swallow the next row whenever
 * that column is absent (as it is in the 2-column PRD fixture below).
 */
function parseDecisionRows(tableSource) {
	return [
		...tableSource.matchAll(
			/^\|[ \t]*(D\d{1,2})[ \t]*\|[ \t]*(.+?)[ \t]*\|(?:[ \t]*(\S.*?)[ \t]*\|)?[ \t]*$/gm,
		),
	].map((m) => ({
		id: m[1],
		text: m[2].trim(),
		sections: m[3],
	}));
}

const prdRows = parseDecisionRows(PRD_DECISIONS_TABLE);

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

// Loosely normalizes markdown emphasis/backticks/whitespace so a PRD row's
// bold/code styling doesn't have to be byte-identical in the design doc's
// table cell for a prefix comparison to be meaningful.
function normalize(s) {
	return s
		.replace(/[`*]/g, "")
		.replace(/\s+/g, " ")
		.trim();
}

test("the pinned PRD decision table itself parses to exactly D1..D24, in order", () => {
	const ids = prdRows.map((r) => r.id);
	const expected = Array.from({ length: 24 }, (_, i) => `D${i + 1}`);
	assert.deepEqual(ids, expected, "PRD_DECISIONS_TABLE fixture drifted from D1..D24; fix the fixture, not the design doc");
});

test("design doc has a decision-alignment table for D1-D24", () => {
	const section = alignmentSection(designDoc);
	assert.match(section, /\|\s*ID\s*\|\s*Decision\s*\|\s*Section\s*\|/, "table must have ID/Decision/Section columns");
});

test("D1-D24 each appear exactly once in the alignment table, in order, with no gap or duplicate", () => {
	const section = alignmentSection(designDoc);
	const rows = parseDecisionRows(section);
	const ids = rows.map((r) => r.id);

	const expected = Array.from({ length: 24 }, (_, i) => `D${i + 1}`);
	assert.deepEqual(ids, expected, "the table must list D1..D24, in order, with no gap or duplicate");

	for (const row of rows) {
		assert.notEqual(row.text, "", `${row.id}: decision text must not be empty`);
	}
});

test("every design-doc decision row's text is drawn from the PRD's actual decision, not an invented substitute", () => {
	const section = alignmentSection(designDoc);
	const designRows = new Map(parseDecisionRows(section).map((r) => [r.id, r.text]));

	const mismatched = [];
	for (const prdRow of prdRows) {
		const designText = designRows.get(prdRow.id);
		if (designText === undefined) {
			mismatched.push(`${prdRow.id}: missing from design doc table`);
			continue;
		}
		const prdNorm = normalize(prdRow.text);
		const designNorm = normalize(designText);
		// The design doc's row must be a recognizable rendering of the PRD's
		// decision: either it reproduces the PRD text verbatim (mod markdown
		// styling), or the PRD text is a prefix of a slightly longer design-doc
		// paraphrase (and vice versa for a PRD sentence trailing a rationale
		// clause the design doc rightly drops). A row that shares neither
		// direction of prefix is a different decision wearing the same ID.
		const PREFIX_LEN = 24;
		const prdHead = prdNorm.slice(0, PREFIX_LEN);
		const designHead = designNorm.slice(0, PREFIX_LEN);
		const related = designNorm.startsWith(prdHead) || prdNorm.startsWith(designHead) || designNorm.includes(prdHead);
		if (!related) {
			mismatched.push(
				`${prdRow.id}: design doc text does not match the PRD decision.\n  PRD:    ${prdRow.text}\n  design: ${designText}`,
			);
		}
	}
	assert.deepEqual(mismatched, [], `design-doc decision text drifted from the PRD:\n${mismatched.join("\n")}`);
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
