// U-W0b.13 (AC-P0-111, AC-P0-112) — the always-on prompt byte budget.
//
// Gated: the project-owned slice, because this repo writes every byte of it.
// Reported: everything host-owned (an ancestor AGENTS.md on the user's machine,
// pi's own catalog formatting and tool schemas). Gating a number you do not
// control produces a waiver, and a waived gate teaches that gates are
// negotiable.
//
// ONE BUDGET IS CURRENTLY MISSED, ON PURPOSE AND IN THE OPEN. See
// KNOWN_OVER_BUDGET below: project context is 41.7 KB against an 8 KB target.
// That is not a rounding error a prose edit closes, and it is exactly the
// condition D7 attached to `pix context compile` — so it is recorded here as a
// named, ratcheted gap rather than hidden behind a relaxed budget.
import assert from "node:assert/strict";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { BUDGETS, KB, measureContext } from "../scripts/measure-context.mjs";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const result = measureContext(repoRoot);
const budget = (id) => result.budgets.find((b) => b.id === id);
const segment = (id) => result.segments.find((s) => s.id === id);

/**
 * Segments over their AC-P0-111 budget today, each with a non-regression
 * ceiling and the reason it is not simply fixed.
 *
 * The ceiling is a RATCHET: the segment may shrink freely, and the moment it
 * drops under its real budget this test fails and tells you to delete the
 * entry. It may never grow. What it must never become is a second budget that
 * quietly tracks reality upward.
 */
const KNOWN_OVER_BUDGET = [
	{
		id: "project-context",
		ceiling: 42_300,
		why:
			"AGENTS.md (37.5 KB) + the kit agentInstructions (3.9 KB) against an 8 KB budget. " +
			"U-W0b.08 removed the 15 KB launcher CLI reference; the remainder is repo layout, build/load/run, " +
			"extension and skill authoring rules, and the hard-won gotchas — all of it load-bearing prose that a " +
			"byte trim cannot delete, only RESTRUCTURE into always/on-demand/reference tiers. That restructuring is " +
			"`pix context compile` (AC-P1-701), and this miss is the D7 condition that promotes it back to P0. " +
			"Ratcheted +300 B for sbx v0.38's noninteractive-rm safety invariant #7 update (a genuine, reviewed " +
			"content addition, not drift — see docs/upstream/sbx-0.38-noninteractive-rm.md).",
	},
	{
		id: "project-owned-total",
		ceiling: 52_000,
		why: "Follows from project-context above; the other two sub-budgets are met.",
	},
];

test("skill catalog is under its 8 KB budget", () => {
	const b = budget("skill-catalog");
	assert.equal(b.budget, 8 * KB);
	assert.ok(!b.over, `skill catalog is ${b.bytes} B, over ${b.budget} B. 39 entries x <=200-char descriptions is the control (tests/skill-descriptions.test.mjs); a new skill costs ~200 B of every turn forever.`);
});

test("extension prompt snippets and guidelines are under their 2 KB budget", () => {
	const b = budget("extension-snippets");
	assert.equal(b.budget, 2 * KB);
	assert.ok(!b.over, `extension snippets are ${b.bytes} B, over ${b.budget} B: a promptSnippet/promptGuideline is on every turn the tool is active.`);
});

test("every gated segment is either under budget or a named, ratcheted exception", () => {
	const unexplained = result.budgets
		.filter((b) => b.over)
		.filter((b) => !KNOWN_OVER_BUDGET.some((k) => k.id === b.id))
		.map((b) => `${b.id}: ${b.bytes} B > ${b.budget} B`);
	assert.deepEqual(
		unexplained,
		[],
		`always-on prompt segment(s) went over budget with no recorded reason:\n  ${unexplained.join("\n  ")}\n` +
			"Trim the segment, or add it to KNOWN_OVER_BUDGET with a ceiling and a reason a reviewer can argue with.",
	);
});

test("known-over-budget segments have not grown", () => {
	for (const known of KNOWN_OVER_BUDGET) {
		const b = budget(known.id);
		assert.ok(b, `KNOWN_OVER_BUDGET names ${known.id}, which is not a measured budget`);
		assert.ok(
			b.bytes <= known.ceiling,
			`${known.id} grew to ${b.bytes} B, past its ${known.ceiling} B non-regression ceiling. ` +
				"This content is on EVERY turn of every session: move it into docs/ and leave a pointer.",
		);
	}
});

test("a known-over-budget segment that came under budget is removed from the list", () => {
	const fixed = KNOWN_OVER_BUDGET.filter((k) => !budget(k.id).over).map((k) => k.id);
	assert.deepEqual(
		fixed,
		[],
		`${fixed.join(", ")} now meets its AC-P0-111 budget. Delete the KNOWN_OVER_BUDGET entry so the real budget gates it from here on.`,
	);
});

test("the AC-P0-111 budgets are the PRD's numbers, not whatever the tree happens to be", () => {
	assert.deepEqual(BUDGETS, {
		"project-context": 8 * KB,
		"skill-catalog": 8 * KB,
		"extension-snippets": 2 * KB,
		"project-owned-total": 18 * KB,
	});
	assert.equal(
		BUDGETS["project-context"] + BUDGETS["skill-catalog"] + BUDGETS["extension-snippets"],
		BUDGETS["project-owned-total"],
		"the three sub-budgets must add up to the 18 KB total",
	);
});

// AC-P0-112: attribution, not a single number.
test("every segment names an owner, and host-owned segments are never gated", () => {
	assert.ok(result.segments.length >= 6);
	for (const s of result.segments) {
		assert.ok(["project", "host", "pi"].includes(s.owner), `${s.id} has no owner`);
		if (s.owner !== "project") assert.equal(s.gated, false, `${s.id} is ${s.owner}-owned and must not be gated`);
	}
	// The specific attributions AC-P0-112 calls for.
	for (const id of ["project-context", "skill-catalog", "extension-snippets", "recall-net-new", "ancestor-context"]) {
		assert.ok(segment(id), `missing segment: ${id}`);
	}
});

test("an unmeasured ancestor AGENTS.md reports as unmeasured, not as zero", () => {
	const ancestor = segment("ancestor-context");
	assert.equal(ancestor.owner, "host");
	assert.equal(ancestor.gated, false);
	assert.equal(ancestor.measured, false);
	assert.match(ancestor.label, /not measured on this host/);

	// And it is attributed properly when there IS one.
	const withAncestor = measureContext(repoRoot, { ancestor: path.join(repoRoot, "AGENTS.md") });
	const a = withAncestor.segments.find((s) => s.id === "ancestor-context");
	assert.equal(a.measured, true);
	assert.ok(a.bytes > 0);
	assert.equal(a.gated, false, "an ancestor AGENTS.md is host-owned; this repo cannot rewrite it");
});

test("recall net-new is the enforced ceiling, the one remaining channel", () => {
	const recall = segment("recall-net-new");
	// The knowledge-recall channel was retired (W2 U03A); memory is the only one left.
	assert.equal(recall.bytes, 1 * KB, "one channel x the 1 KB per-turn cap in lib/recall-message.ts");
	assert.match(recall.label, /1 channels x 1024 B/);
});

test("the measurement is deterministic", () => {
	const a = measureContext(repoRoot);
	const b = measureContext(repoRoot);
	assert.deepEqual(a.budgets, b.budgets);
	assert.equal(a.coldTurnTotal, b.coldTurnTotal);
});
