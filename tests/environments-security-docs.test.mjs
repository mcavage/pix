// Anti-drift pins for the Story 0 accepted security/QA doc findings. These are
// narrow content assertions, not a reimplementation of the docs: each pins one
// honest-scope claim so a later edit cannot silently overclaim proof or
// implementation, the same pattern tests/onboarding-skill-guidance.test.mjs
// uses for skill prose.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const upstreamDoc = fs.readFileSync(
	path.join(repoRoot, "docs", "upstream", "sbx-0.39-environments.md"),
	"utf8",
);
const designDoc = fs.readFileSync(path.join(repoRoot, "docs", "design", "environments.md"), "utf8");
const uatDoc = fs.readFileSync(path.join(repoRoot, "docs", "design", "self-development-uat.md"), "utf8");

test("upstream env doc: list-concatenation/key-merge composition is an explicit non-claim (A6), not proven by Story 0", () => {
	assert.match(upstreamDoc, /multi-file list-concatenation merge semantics \(A6\)/);
	assert.match(upstreamDoc, /never composed two environment files in\s+argument order/);
	assert.match(upstreamDoc, /Story 1\s+parser\/composition test obligation/);
	// It must land as a "left as later-story responsibility" item too, not just
	// a passing mention buried in section 6.
	assert.match(
		upstreamDoc,
		/multi-file list-concatenation and key-merge semantics \(A6\): no fixture in\s+this run composed more than one file at create time/,
	);
});

test("upstream env doc: the existing A3 digest-equality non-claim survives untouched", () => {
	assert.match(
		upstreamDoc,
		/Do not claim\s+digest equality anywhere in this codebase; it is not observable with this sbx\s+release\./,
	);
	// The new A6 note cross-references A3's honest posture rather than
	// duplicating or contradicting it.
	assert.match(upstreamDoc, /the same honest posture section 7 below takes for digest equality\s+\(A3\)/);
});

test("design doc trust section: every authored ${VAR} reference must appear in host trust review with source var + destination field", () => {
	assert.match(
		designDoc,
		/Every\s+authored `\$\{VAR\}` reference in the environment file must appear as its own\s+line item in the host trust review, naming the source host variable and the\s+destination field it resolves into/,
	);
	assert.match(
		designDoc,
		/The review must never display or persist the\s+resolved value, only the reference and its destination\./,
	);
	assert.match(
		designDoc,
		/- authored `\$\{VAR\}` interpolation references, by source variable name and\s+destination field only, never the resolved value \(Story 1\)/,
	);
});

test("design doc trust section: creation fingerprint must use keyed digest/HMAC of resolved interpolated values, never a raw hash", () => {
	assert.match(
		designDoc,
		/it must never hash a low-entropy\s+resolved value directly/,
	);
	assert.match(
		designDoc,
		/the unresolved expression plus a keyed digest \(HMAC, or an equally concrete\s+non-reversible keyed construction\) of the resolved value/,
	);
});

test("design doc trust section: the interpolation review/fingerprint requirement is scoped to Story 1, not claimed as shipped", () => {
	assert.match(
		designDoc,
		/\*\*Story 1 requirement, not yet implemented:\*\* resolving `\$\{VAR\}` before/,
	);
	assert.match(
		designDoc,
		/Neither the review line-items nor\s+the keyed digest exist yet; both are Story 1 obligations, not implemented\s+behavior\./,
	);
	// It also shows up in Story 1's own Changes/Acceptance lists, not only the
	// trust-section prose.
	assert.match(
		designDoc,
		/add host trust review line items for every authored `\$\{VAR\}` reference/,
	);
	assert.match(
		designDoc,
		/host trust review lists every authored `\$\{VAR\}` reference by source\s+variable and destination field, never a resolved value/,
	);
});

test("self-development-uat doc: documents that candidate code runs with the operator shell's inherited authenticated environment", () => {
	assert.match(uatDoc, /## Security boundary: operator environment inheritance/);
	assert.match(
		uatDoc,
		/`uat-worker` executes submitted candidate code on the host as a child process\s+of the operator's own `pix run --dev` invocation/,
	);
	assert.match(uatDoc, /inherits the operator shell's full environment, unfiltered/);
});

test("self-development-uat doc: warns operators not to launch --dev from a shell exporting long-lived secrets", () => {
	assert.match(
		uatDoc,
		/Operators must not launch `pix run --dev` from a shell that has exported\s+long-lived provider or cloud API secrets\./,
	);
});

test("self-development-uat doc: never claims env scrubbing exists, and non-goals explicitly disclaim it", () => {
	assert.doesNotMatch(uatDoc, /scrubs the (operator|inherited) environment/i);
	assert.doesNotMatch(uatDoc, /environment variables are (filtered|redacted|scrubbed) before/i);
	assert.match(
		uatDoc,
		/does not scrub, allowlist, or redact environment variables before/,
	);
	assert.match(
		uatDoc,
		/scrubbing, filtering, or sandboxing the operator shell's environment before\s+candidate execution \(none exists today/,
	);
});

test("self-development-uat doc: production/non-dev sessions never receive the UAT surface", () => {
	assert.match(
		uatDoc,
		/A production or non-dev session\s+never spawns a worker, never receives `uat_capabilities`, and never executes\s+submitted candidate code on the host\./,
	);
});
