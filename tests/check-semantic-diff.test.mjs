// Self-test for the U00c (W0) semantic-diff guard, extended by U04g.
//
// Tiers:
//   1. Unit tests of the pure engine (scripts/semantic-diff/lib/engine.mjs)
//      against small in-memory/tmp-dir fixtures — fast, no real repo needed.
//   2. A PLANTED CORRUPTION self-test: a fixture that mimics the exact failure
//      mode this framework exists for — a production value AND its
//      "consumer" witness get renamed together in one commit (lockstep
//      corruption) — and proves the guard still fails because the pin's
//      `expected` is a third, independently fixed literal.
//   2b. (U04g) An ACTIVATION self-test: a staged pin (`activation: "<key>"`)
//      is skipped (pending, gate-green) when its key is not active, and
//      evaluated for real — passing or failing on the fixture's actual
//      content — once activated. Proves the Story04 staged-pin schema
//      genuinely turns behavior checks on rather than only decorating them.
//   3. An integration run against the REAL repository with the REAL rules,
//      proving every ACTIVE pin holds today, every STAGED (Story04) pin is
//      pending under the shipped (empty) activation.json, and every STAGED
//      pin genuinely fails right now if forcibly activated — i.e. each is a
//      real, currently-true "not yet implemented" TODO, never a vacuous
//      placeholder that would silently rot into a no-op.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import { activationKeySet, checkRuleDrift, entriesExplainingDrift, evaluateCheck, evaluatePins, extractRegion, extractSet, loadActivation, loadManifest, loadRules, resolveDefaultBase, staleManifestEntries } from "../scripts/semantic-diff/lib/engine.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(__dirname, "..");
const REAL_RULES_DIR = path.join(REPO_ROOT, "scripts", "semantic-diff", "rules");
const CLI = path.join(REPO_ROOT, "scripts", "check-semantic-diff.mjs");

function mkTmpDir() {
	return fs.mkdtempSync(path.join(os.tmpdir(), "semdiff-test-"));
}

// --- tier 1: engine unit tests ------------------------------------------------

test("extractRegion slices between two literal anchors", () => {
	const content = "before\nSTART\nmiddle text\nEND\nafter";
	assert.equal(extractRegion(content, "START\n", "\nEND"), "middle text");
});

test("extractRegion with no end anchor runs to end of file", () => {
	const content = "before\nSTART\nrest of the file";
	assert.equal(extractRegion(content, "START\n"), "rest of the file");
});

test("extractRegion throws when the start anchor is missing (rotted pin, not a silent skip)", () => {
	assert.throws(() => extractRegion("no markers here", "START"), /anchor not found/);
});

test("extractRegion throws when the end anchor is missing but the start anchor was found", () => {
	assert.throws(() => extractRegion("START only, no end marker here", "START", "END"), /end anchor not found/);
});

test("extractSet dedupes and sorts capture-group matches", () => {
	assert.deepEqual(extractSet('rpc("b") rpc("a") rpc("b")', 'rpc\\("(\\w+)"\\)'), ["a", "b"]);
});

test("evaluateCheck: contains reports every missing value", () => {
	const r = evaluateCheck({ kind: "contains", values: ["foo", "bar"] }, "has foo only");
	assert.equal(r.ok, false);
	assert.match(r.actual, /bar/);
});

test("evaluateCheck: notContains flags forbidden text", () => {
	const ok = evaluateCheck({ kind: "notContains", values: ["fmt.Println("] }, "clean file");
	assert.equal(ok.ok, true);
	const bad = evaluateCheck({ kind: "notContains", values: ["fmt.Println("] }, "oops fmt.Println(\"x\")");
	assert.equal(bad.ok, false);
});

test("evaluateCheck: set is order-independent and catches additions AND removals", () => {
	const content = '"a": func(\n"b": func(\n"c": func(\n';
	const same = evaluateCheck({ kind: "set", pattern: '"(\\w+)":\\s*func\\(', expected: ["c", "b", "a"] }, content);
	assert.equal(same.ok, true);
	const added = evaluateCheck({ kind: "set", pattern: '"(\\w+)":\\s*func\\(', expected: ["a", "b"] }, content);
	assert.equal(added.ok, false);
	const removed = evaluateCheck({ kind: "set", pattern: '"(\\w+)":\\s*func\\(', expected: ["a", "b", "c", "d"] }, content);
	assert.equal(removed.ok, false);
});

test("evaluateCheck: equals matches the first capture group exactly", () => {
	const r = evaluateCheck({ kind: "equals", pattern: 'PORT", "(\\d+)"', expected: "11435" }, 'env("MEMORY_PORT", "11435")');
	assert.equal(r.ok, true);
});

test("loadManifest requires non-empty id/rationale/evidence/changes on every entry", () => {
	const dir = mkTmpDir();
	const p = path.join(dir, "manifest.json");
	fs.writeFileSync(p, JSON.stringify([{ id: "x" }]));
	assert.throws(() => loadManifest(p), /rationale/);
});

test("loadManifest tolerates a missing file (empty manifest, the W0 default)", () => {
	assert.deepEqual(loadManifest(path.join(mkTmpDir(), "does-not-exist.json")), []);
});

// --- activation (U04g staged-pin schema): loadActivation/activationKeySet ---

test("loadActivation tolerates a missing file (empty activation set, nothing turned on)", () => {
	assert.deepEqual(loadActivation(path.join(mkTmpDir(), "does-not-exist.json")), []);
});

test("loadActivation requires non-empty key/rationale/evidence on every entry", () => {
	const dir = mkTmpDir();
	const p = path.join(dir, "activation.json");
	fs.writeFileSync(p, JSON.stringify([{ key: "story04" }]));
	assert.throws(() => loadActivation(p), /rationale/);
});

test("loadActivation rejects a non-array root", () => {
	const dir = mkTmpDir();
	const p = path.join(dir, "activation.json");
	fs.writeFileSync(p, JSON.stringify({ story04: true }));
	assert.throws(() => loadActivation(p), /must be a JSON array/);
});

test("activationKeySet turns a loaded activation array into a Set of its keys", () => {
	const activation = [
		{ key: "story04", rationale: "landed the reaper", evidence: "PR #1" },
		{ key: "other", rationale: "landed X", evidence: "PR #2" },
	];
	const set = activationKeySet(activation);
	assert.equal(set.has("story04"), true);
	assert.equal(set.has("other"), true);
	assert.equal(set.has("never-activated"), false);
});

// --- tier 2: planted corruption self-test ------------------------------------

function writeFixture(dir, files) {
	for (const [rel, content] of Object.entries(files)) {
		const abs = path.join(dir, rel);
		fs.mkdirSync(path.dirname(abs), { recursive: true });
		fs.writeFileSync(abs, content);
	}
}

// One pin, two independent witnesses (a "server" and a "client"), pinned
// against the SAME fixed expected value — the exact shape memory-rpc.rules.mjs
// uses for real.
const CORRUPTION_PIN = {
	id: "fixture.method-name",
	domain: "fixture",
	checks: [
		{ file: "server.go", kind: "contains", values: ['"remember"'] },
		{ file: "client.ts", kind: "contains", values: ['rpc("remember"'] },
	],
};

test("planted corruption: an untouched fixture passes both witnesses", () => {
	const root = mkTmpDir();
	writeFixture(root, {
		"server.go": 'methods["remember"] = rememberHandler',
		"client.ts": 'await rpc("remember", {content})',
	});
	const report = evaluatePins([CORRUPTION_PIN], root, []);
	assert.equal(report.ok, true);
});

test("planted corruption: renaming ONLY the production side is caught immediately", () => {
	const root = mkTmpDir();
	writeFixture(root, {
		"server.go": 'methods["rememberFact"] = rememberHandler', // corrupted
		"client.ts": 'await rpc("remember", {content})', // untouched
	});
	const report = evaluatePins([CORRUPTION_PIN], root, []);
	assert.equal(report.ok, false);
	const pin = report.pins.find((p) => p.id === "fixture.method-name");
	const serverCheck = pin.checks.find((c) => c.file === "server.go");
	assert.equal(serverCheck.ok, false);
});

test("LOCKSTEP CORRUPTION: renaming production AND its consumer witness TOGETHER still fails, because `expected` is a third fixed literal neither file controls", () => {
	const root = mkTmpDir();
	// A bad codemod renames "remember" -> "rememberFact" EVERYWHERE, consistently.
	// Both files agree with EACH OTHER. An ordinary cross-check between them
	// would see no problem at all.
	writeFixture(root, {
		"server.go": 'methods["rememberFact"] = rememberHandler',
		"client.ts": 'await rpc("rememberFact", {content})',
	});
	const report = evaluatePins([CORRUPTION_PIN], root, []);
	assert.equal(report.ok, false, "lockstep-corrupted fixture must still fail: the pin's expected value never moved");
	const pin = report.pins.find((p) => p.id === "fixture.method-name");
	assert.equal(pin.checks.every((c) => !c.ok), true, "BOTH witnesses must independently disagree with the fixed pin");
});

test("an intended-change manifest entry waives an EXACT, documented transition", () => {
	const root = mkTmpDir();
	writeFixture(root, {
		"server.go": 'methods["rememberFact"] = rememberHandler',
		"client.ts": 'await rpc("rememberFact", {content})',
	});
	const manifest = [
		{
			id: "fixture.method-name",
			rationale: "renamed remember -> rememberFact for clarity",
			evidence: "PR #999",
			changes: [
				{ file: "server.go", kind: "contains", from: ['"remember"'], to: ['"rememberFact"'] },
				{ file: "client.ts", kind: "contains", from: ['rpc("remember"'], to: ['rpc("rememberFact"'] },
			],
		},
	];
	const report = evaluatePins([CORRUPTION_PIN], root, manifest);
	assert.equal(report.ok, true, "a fully-documented, exact-match transition must be waived, not blocked forever");
	const pin = report.pins.find((p) => p.id === "fixture.method-name");
	assert.equal(pin.checks.every((c) => c.waived), true);
	assert.equal(report.unusedManifestEntries.length, 0);
});

test("a manifest entry whose `to` does not match reality does not waive anything (typo/drift protection)", () => {
	const root = mkTmpDir();
	writeFixture(root, {
		"server.go": 'methods["rememberFact"] = rememberHandler',
		"client.ts": 'await rpc("rememberThing", {content})', // inconsistent rename: neither the old nor the declared new value
	});
	const manifest = [
		{
			id: "fixture.method-name",
			rationale: "renamed remember -> rememberFact",
			evidence: "PR #999",
			changes: [
				{ file: "server.go", kind: "contains", from: ['"remember"'], to: ['"rememberFact"'] },
				{ file: "client.ts", kind: "contains", from: ['rpc("remember"'], to: ['rpc("rememberFact"'] }, // does not match client.ts's actual content
			],
		},
	];
	const report = evaluatePins([CORRUPTION_PIN], root, manifest);
	assert.equal(report.ok, false, "client.ts matches neither the original expected value nor the manifest's declared destination; that is a real, unresolved break");
});

// --- tier 2b: activation (Story04 staged-pin) self-test ---------------------

// A staged pin whose fixture does NOT yet satisfy it — modeling a Story04
// contract (e.g. the orphan reaper) that has no production code yet.
const STAGED_PIN = {
	id: "fixture.staged-thing",
	domain: "fixture",
	activation: "story04",
	checks: [{ file: "future.go", kind: "contains", values: ["notYetWritten()"] }],
};

test("a staged pin is PENDING (not evaluated, not failed) when its activation key is not active", () => {
	const root = mkTmpDir(); // future.go deliberately does not exist here
	const report = evaluatePins([STAGED_PIN], root, [], new Set());
	assert.equal(report.ok, true, "a staged pin must never fail the gate while its key is inactive");
	const pin = report.pins.find((p) => p.id === "fixture.staged-thing");
	assert.equal(pin.pending, true);
	assert.equal(pin.activation, "story04");
	assert.deepEqual(pin.checks, [], "a pending pin must not touch the filesystem or report check detail");
});

test("the SAME staged pin, activated, is evaluated for real and FAILS against the unimplemented fixture", () => {
	const root = mkTmpDir(); // still no future.go
	const report = evaluatePins([STAGED_PIN], root, [], new Set(["story04"]));
	assert.equal(report.ok, false, "activating a pin describing not-yet-built behavior must surface as a real failure, proving it isn't a vacuous placeholder");
	const pin = report.pins.find((p) => p.id === "fixture.staged-thing");
	assert.equal(pin.pending, undefined);
	assert.equal(pin.checks[0].actual, "FILE MISSING");
});

test("the SAME staged pin, activated, PASSES once the fixture actually implements the behavior", () => {
	const root = mkTmpDir();
	writeFixture(root, { "future.go": "func notYetWritten() {}" });
	const report = evaluatePins([STAGED_PIN], root, [], new Set(["story04"]));
	assert.equal(report.ok, true, "once Story04 lands the behavior, activating the pin must pass");
});

test("activation only turns on pins naming that exact key — an unrelated activated key leaves it pending", () => {
	const root = mkTmpDir();
	const report = evaluatePins([STAGED_PIN], root, [], new Set(["some-other-story"]));
	const pin = report.pins.find((p) => p.id === "fixture.staged-thing");
	assert.equal(pin.pending, true);
});

test("an unstaged pin (no `activation` field) is always evaluated regardless of the active key set", () => {
	const root = mkTmpDir();
	writeFixture(root, { "server.go": 'methods["remember"] = rememberHandler', "client.ts": 'await rpc("remember", {content})' });
	const report = evaluatePins([CORRUPTION_PIN], root, [], new Set());
	assert.equal(report.ok, true);
	assert.equal(report.pins[0].pending, undefined);
});

test("checkRuleDrift treats flipping a pin's `activation` field alone (no check change) as drift requiring a manifest entry", async () => {
	const root = makeScratchRepo();
	const before = `export default [\n  { id: "fixture.pin", checks: [{ file: "a.txt", kind: "contains", values: ["v1"] }] },\n];\n`;
	const after = `export default [\n  { id: "fixture.pin", activation: "story04", checks: [{ file: "a.txt", kind: "contains", values: ["v1"] }] },\n];\n`;
	writeFixture(root, { "rules/fixture.rules.mjs": before });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1, unstaged");

	writeFixture(root, { "rules/fixture.rules.mjs": after });
	const noWaiver = await checkRuleDrift(path.join(root, "rules"), root, "HEAD", []);
	assert.equal(noWaiver.ok, false, "staging a previously-active pin with no manifest entry must be treated as drift, same as any other checkable-content change");

	const waiver = [{ id: "fixture.pin", rationale: "staged behind story04 pending the reaper landing", evidence: "PR #2", changes: [{ file: "a.txt", kind: "contains", from: ["v1"], to: ["v1"] }] }];
	const withWaiver = await checkRuleDrift(path.join(root, "rules"), root, "HEAD", waiver);
	assert.equal(withWaiver.ok, true, "the same staging move, documented with a matching manifest entry, must pass");
});

// --- rule-drift-vs-git -------------------------------------------------------

function git(cwd, ...args) {
	return execFileSync("git", args, { cwd, encoding: "utf8" });
}

function makeScratchRepo() {
	const root = mkTmpDir();
	git(root, "init", "-q");
	git(root, "config", "user.email", "test@example.com");
	git(root, "config", "user.name", "semdiff test");
	return root;
}

const RULE_FILE_V1 = `export default [
  { id: "fixture.pin", checks: [{ file: "a.txt", kind: "contains", values: ["v1"] }] },
];
`;
const RULE_FILE_V2 = `export default [
  { id: "fixture.pin", checks: [{ file: "a.txt", kind: "contains", values: ["v2"] }] },
];
`;

test("checkRuleDrift is a no-op (ok, skipped) with no git base to compare against", async () => {
	const root = mkTmpDir();
	const rulesDir = path.join(root, "rules");
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1 });
	const result = await checkRuleDrift(rulesDir, root, "HEAD", []);
	assert.equal(result.skipped, true);
	assert.equal(result.ok, true);
});

test("checkRuleDrift FAILS when a pin's expected value changed with no matching manifest entry", async () => {
	const root = makeScratchRepo();
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1");

	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V2 }); // uncommitted, working-tree drift

	const result = await checkRuleDrift(path.join(root, "rules"), root, "HEAD", []);
	assert.equal(result.skipped, false);
	assert.equal(result.ok, false);
	assert.equal(result.drifted[0].id, "fixture.pin");
});

test("checkRuleDrift PASSES the same drift once a matching intended-change manifest entry exists", async () => {
	const root = makeScratchRepo();
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1");

	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V2 });

	const manifest = [{ id: "fixture.pin", rationale: "v1 -> v2, documented", evidence: "PR #1", changes: [{ file: "a.txt", kind: "contains", from: ["v1"], to: ["v2"] }] }];
	const result = await checkRuleDrift(path.join(root, "rules"), root, "HEAD", manifest);
	assert.equal(result.ok, true);
});

// Doc assertion: README.md's "Landing a rule change: same commit vs. a
// subsequent commit" section, encoded as behavior so the doc claim can't
// silently go stale. Models the full two-commit workflow: (1) rule change +
// waiver land together in one commit and are safe to commit, (2) a LATER,
// separate commit that only deletes the now-stale waiver is safe because the
// fingerprint no longer differs from the new base, (3) doing the rule change
// and the waiver removal in the SAME commit fails.
test("README workflow: rule change + waiver together (same commit) passes, stale-waiver removal in a SUBSEQUENT commit passes, but removing the waiver in the SAME commit as the rule change fails", async () => {
	const root = makeScratchRepo();
	const rulesDir = path.join(root, "rules");
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1 base");

	// (3) SAME-commit attempt: rule changed, but the manifest that would waive
	// it is absent (never added, or stripped in this same working-tree state) — fails.
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V2 });
	const sameCommitNoWaiver = await checkRuleDrift(rulesDir, root, "HEAD", []);
	assert.equal(sameCommitNoWaiver.ok, false, "rule change with no waiver present in this same commit's final state must fail");

	// (1) The correct move: land the rule change WITH its waiver, together, in one commit.
	const waiver = [{ id: "fixture.pin", rationale: "v1 -> v2, documented", evidence: "PR #1", changes: [{ file: "a.txt", kind: "contains", from: ["v1"], to: ["v2"] }] }];
	const sameCommitWithWaiver = await checkRuleDrift(rulesDir, root, "HEAD", waiver);
	assert.equal(sameCommitWithWaiver.ok, true, "rule change + waiver together in the same commit must pass");

	// Actually commit that state (rule=V2, manifest carries the waiver) as the new base.
	writeFixture(root, { "intended-changes.json": JSON.stringify(waiver) });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v2 + waiver");

	// (2) A SUBSEQUENT, separate commit deletes the now-stale waiver; the rule
	// file itself is untouched — no fingerprint change vs. the new base, so this
	// is safe regardless of the (now empty) manifest.
	const subsequentRemoval = await checkRuleDrift(rulesDir, root, "HEAD", []);
	assert.equal(subsequentRemoval.ok, true, "deleting a stale waiver in a later commit, with the rule unchanged, must pass");
});

test("checkRuleDrift ignores an unrelated (non-checkable) description-only edit", async () => {
	const root = makeScratchRepo();
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1");

	// same `expected`, just reformatted/commented — not a real contract change.
	writeFixture(root, {
		"rules/fixture.rules.mjs": `// just a comment, no behavior change\n${RULE_FILE_V1}`,
	});
	const result = await checkRuleDrift(path.join(root, "rules"), root, "HEAD", []);
	assert.equal(result.ok, true);
});

// --- unused-manifest-entries FAILS the guard, and staleManifestEntries -----
//
// Requirement: an intended-change manifest entry that neither waives a
// currently-failing evaluatePins check NOR explains a real checkRuleDrift
// fingerprint change vs. the base is dead weight — historically it was only
// ever printed as a "note", never enforced, and this repo had ~20 of them
// accumulate. staleManifestEntries() is the single source of truth the CLI
// uses to fail on that; these tests watch the mechanism actually catch it,
// not just report it.

test("staleManifestEntries: an entry unused by evaluatePins AND not explaining any real drift is reported stale", () => {
	const report = { unusedManifestEntries: ["some.pin"] };
	const drift = { skipped: false, consumedManifestIds: [] };
	assert.deepEqual(staleManifestEntries(report, drift), ["some.pin"]);
});

test("staleManifestEntries: an entry unused by evaluatePins BUT consumed by checkRuleDrift (a real transition landing this commit) is NOT stale", () => {
	const report = { unusedManifestEntries: ["some.pin", "other.pin"] };
	const drift = { skipped: false, consumedManifestIds: ["some.pin"] };
	assert.deepEqual(staleManifestEntries(report, drift), ["other.pin"]);
});

test("staleManifestEntries: with no usable git base (skipped), nothing can be proven stale", () => {
	const report = { unusedManifestEntries: ["some.pin"] };
	const drift = { skipped: true, consumedManifestIds: [] };
	assert.deepEqual(staleManifestEntries(report, drift), []);
});

test("entriesExplainingDrift: credits an entry that is unused as a waiver but not flagged stale (i.e. consumed by real drift)", () => {
	const report = { unusedManifestEntries: ["some.pin", "other.pin"] };
	const drift = { skipped: false, consumedManifestIds: ["some.pin"] };
	const stale = staleManifestEntries(report, drift);
	assert.deepEqual(stale, ["other.pin"]);
	assert.deepEqual(entriesExplainingDrift(report, drift, stale), ["some.pin"]);
});

test("entriesExplainingDrift never claims an entry explains drift when drift.skipped, even though it is unused-as-waiver and (per staleManifestEntries) not flagged stale either", () => {
	// This is exactly the historical bug: a skipped drift check (no usable git
	// base) means staleManifestEntries() also can't prove anything, so its
	// output alone can't distinguish "genuinely explained by drift" from
	// "nothing was ever checked". entriesExplainingDrift must not paper over
	// that by reporting the entry as drift-explaining just because it wasn't
	// caught as stale.
	const report = { unusedManifestEntries: ["some.pin"] };
	const drift = { skipped: true, consumedManifestIds: [] };
	const stale = staleManifestEntries(report, drift);
	assert.deepEqual(stale, [], "sanity: skipped drift proves nothing stale either");
	assert.deepEqual(entriesExplainingDrift(report, drift, stale), [], "a skipped drift check must never be reported as explaining anything");
});

// Full pipeline (evaluatePins + checkRuleDrift + staleManifestEntries), the
// same three calls the CLI itself makes, against a self-contained fixture —
// proves the mechanism end-to-end without the CLI's REPO_ROOT-fixed rules/
// manifest paths getting in the way of a synthetic `--root`.
test("pipeline FAILS on a stale manifest entry (no waiver use, no matching drift vs. the given base)", async () => {
	const root = makeScratchRepo();
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1, "a.txt": "v1" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "base: v1, no manifest entry needed");

	// A manifest entry claiming a transition that never happened against THIS
	// base: the rule never changed, so it can never be "consumed" by drift, and
	// the underlying check already passes directly, so it is never used as a
	// waiver either. That is exactly a stale entry.
	const manifest = [{ id: "fixture.pin", rationale: "stale, nothing changed", evidence: "nowhere", changes: [{ file: "a.txt", kind: "contains", from: ["v1"], to: ["v1"] }] }];

	const pins = await loadRules(path.join(root, "rules"));
	const report = evaluatePins(pins, root, manifest, new Set());
	const drift = await checkRuleDrift(path.join(root, "rules"), root, "HEAD", manifest);
	const stale = staleManifestEntries(report, drift);
	assert.deepEqual(stale, ["fixture.pin"], "an entry that neither waives a failing check nor explains real drift must be reported stale");
});

test("pipeline PASSES when the SAME manifest entry lands in the SAME commit as the rule-drift it documents (base predates the change)", async () => {
	const root = makeScratchRepo();
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1, "a.txt": "v1" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "base: v1");
	const baseSha = git(root, "rev-parse", "HEAD").trim();

	// Now land the transition for real: the rule's expected value moves to v2
	// AND the file it checks moves to v2, with a manifest entry documenting it.
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V2, "a.txt": "v2" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "land v2 + waiver");
	const manifest = [{ id: "fixture.pin", rationale: "v1 -> v2, documented", evidence: "PR #1", changes: [{ file: "a.txt", kind: "contains", from: ["v1"], to: ["v2"] }] }];

	const pins = await loadRules(path.join(root, "rules"));
	const report = evaluatePins(pins, root, manifest, new Set());
	const drift = await checkRuleDrift(path.join(root, "rules"), root, baseSha, manifest);
	const stale = staleManifestEntries(report, drift);
	assert.equal(report.ok, true);
	assert.equal(drift.ok, true);
	assert.deepEqual(stale, [], "a manifest entry consumed by real drift this run must not be flagged stale");
});

// --- resolveDefaultBase: a meaningful base, not a self-compare against HEAD -

test("resolveDefaultBase prefers SEMANTIC_DIFF_BASE_SHA when it names a real commit", () => {
	const root = makeScratchRepo();
	writeFixture(root, { "a.txt": "v1" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1");
	const sha = git(root, "rev-parse", "HEAD").trim();

	const prior = process.env.SEMANTIC_DIFF_BASE_SHA;
	process.env.SEMANTIC_DIFF_BASE_SHA = sha;
	try {
		assert.equal(resolveDefaultBase(root), sha);
	} finally {
		if (prior === undefined) delete process.env.SEMANTIC_DIFF_BASE_SHA;
		else process.env.SEMANTIC_DIFF_BASE_SHA = prior;
	}
});

test("resolveDefaultBase ignores a bogus SEMANTIC_DIFF_BASE_SHA and falls through", () => {
	const root = makeScratchRepo();
	writeFixture(root, { "a.txt": "v1" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1");

	const prior = process.env.SEMANTIC_DIFF_BASE_SHA;
	process.env.SEMANTIC_DIFF_BASE_SHA = "not-a-real-sha-at-all";
	try {
		const base = resolveDefaultBase(root);
		assert.notEqual(base, "not-a-real-sha-at-all");
	} finally {
		if (prior === undefined) delete process.env.SEMANTIC_DIFF_BASE_SHA;
		else process.env.SEMANTIC_DIFF_BASE_SHA = prior;
	}
});

test("resolveDefaultBase prefers merge-base(HEAD, origin/main) over a bare HEAD self-compare, and that base actually surfaces drift a literal HEAD never could", async () => {
	const root = makeScratchRepo();
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V1 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1, this is what main has");

	// Fake a remote: a bare clone playing the role of origin, so origin/main exists.
	const remote = mkTmpDir();
	execFileSync("git", ["clone", "--bare", "-q", root, remote]);
	git(root, "remote", "add", "origin", remote);
	git(root, "fetch", "-q", "origin");

	// A feature branch moves the pin to v2 (committed, so HEAD == working tree —
	// the exact case a literal "HEAD" base can never see as drift).
	writeFixture(root, { "rules/fixture.rules.mjs": RULE_FILE_V2 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "feature: v2, no manifest entry");

	const literalHead = await checkRuleDrift(path.join(root, "rules"), root, "HEAD", []);
	assert.equal(literalHead.ok, true, "comparing HEAD to itself trivially finds no drift — this is the bug being fixed");

	const base = resolveDefaultBase(root);
	assert.notEqual(base, "HEAD", "a repo with a tracked remote default branch must resolve to something more meaningful than HEAD");
	const meaningful = await checkRuleDrift(path.join(root, "rules"), root, base, []);
	assert.equal(meaningful.ok, false, "the resolved base must actually be the pre-feature-branch commit, so the undocumented v1->v2 move is caught");
	assert.equal(meaningful.drifted[0].id, "fixture.pin");
});

test("resolveDefaultBase falls back to HEAD~1 with no remote at all, still more meaningful than HEAD", () => {
	const root = makeScratchRepo();
	writeFixture(root, { "a.txt": "v1" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v1");
	writeFixture(root, { "a.txt": "v2" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "v2");

	const base = resolveDefaultBase(root);
	assert.equal(base, "HEAD~1");
});

test("resolveDefaultBase falls back to the literal HEAD only for a brand-new, single-commit repo", () => {
	const root = makeScratchRepo();
	writeFixture(root, { "a.txt": "v1" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "only commit");

	assert.equal(resolveDefaultBase(root), "HEAD");
});

test("resolveDefaultBase rejects a resolvable origin/main merge-base that predates the semantic rules directory entirely, falling through to HEAD~1 (long-lived-branch case)", async () => {
	const root = makeScratchRepo();
	writeFixture(root, { "a.txt": "v1" });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "pre-rules commit, this is all the stale tracked remote ever saw");
	git(root, "branch", "-m", "main"); // deterministic branch name regardless of this host's git init.defaultBranch

	// Fake a remote frozen at the pre-rules commit — mirrors a tracked
	// origin/main that has not been fast-forwarded since long before this
	// long-lived feature branch introduced the semantic-diff rules at all.
	const remote = mkTmpDir();
	execFileSync("git", ["clone", "--bare", "-q", root, remote]);
	git(root, "remote", "add", "origin", remote);
	git(root, "fetch", "-q", "origin");

	// The branch introduces the real rules dir, and later moves a pin, all
	// strictly AFTER the frozen remote snapshot: origin/main's merge-base with
	// HEAD is that pre-rules commit, which cannot see rules/fixture.rules.mjs
	// at all.
	writeFixture(root, { "scripts/semantic-diff/rules/fixture.rules.mjs": RULE_FILE_V1 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "introduce semantic-diff rules, long after origin/main was last synced");
	writeFixture(root, { "scripts/semantic-diff/rules/fixture.rules.mjs": RULE_FILE_V2 });
	git(root, "add", "-A");
	git(root, "commit", "-q", "-m", "later commit moves the pin, no manifest entry for this move");

	const mergeBase = git(root, "merge-base", "HEAD", "origin/main").trim();
	const rulesDir = path.join(root, "scripts", "semantic-diff", "rules");
	const uselessDrift = await checkRuleDrift(rulesDir, root, mergeBase, []);
	assert.equal(uselessDrift.skipped, true, "sanity: the raw merge-base really is useless here — no rules file existed at it, so drift silently self-skips");

	const base = resolveDefaultBase(root);
	assert.notEqual(base, mergeBase, "a merge-base that predates the rules directory entirely must be rejected, not silently used");
	assert.equal(base, "HEAD~1", "falls through to HEAD~1, the next meaningful candidate");

	const drift = await checkRuleDrift(rulesDir, root, base, []);
	assert.equal(drift.skipped, false, "HEAD~1 actually has a prior rules version to compare against, so the undocumented v1->v2 move is now visible instead of silently skipped");
	assert.equal(drift.ok, false, "and it is genuinely undocumented drift");
});

test("post-merge on THIS repo: resolveDefaultBase does not silently degrade to the stale origin/main merge-base (the real long-lived-branch case this fix targets)", (t) => {
	const staleMergeBase = execFileSync("git", ["merge-base", "HEAD", "origin/main"], { cwd: REPO_ROOT, encoding: "utf8" }).trim();

	// The degradation guarded here only EXISTS while a long-lived branch is
	// unlanded: the merge-base predates the rules directory precisely because
	// that branch is what introduced it. Once the branch merges, origin/main
	// carries the rules directory and the merge-base IS HEAD, so there is no
	// stale base left to reject and nothing here to prove. Asserting that as a
	// precondition rather than skipping on it encoded a property of being
	// UNMERGED as a repo invariant, which made this test fail on every push to
	// main. The synthetic-repo test above pins the same engine behavior
	// deterministically, independent of where this checkout sits in history,
	// so skipping here costs no coverage of the rule itself.
	let mergeBaseSeesRules = true;
	try {
		execFileSync("git", ["cat-file", "-e", `${staleMergeBase}:scripts/semantic-diff/rules`], { cwd: REPO_ROOT, stdio: "ignore" });
	} catch {
		mergeBaseSeesRules = false;
	}
	if (mergeBaseSeesRules) {
		t.skip("origin/main's merge-base already carries the rules directory (this checkout is level with, or merged into, main), so no stale base exists to degrade to");
		return;
	}

	const resolved = resolveDefaultBase(REPO_ROOT);
	assert.notEqual(resolved, staleMergeBase, "must not use a merge-base that cannot see the rules directory at all");
	assert.ok(
		resolved === "HEAD~1" || resolved === "HEAD^2",
		`expected a meaningful rules-bearing parent (ordinary branch HEAD~1 or GitHub synthetic-merge PR head HEAD^2), got ${resolved}`,
	);

	const drift = execFileSync("node", [CLI, "--root", REPO_ROOT, "--base", resolved, "--json"], { encoding: "utf8" });
	const parsed = JSON.parse(drift);
	assert.equal(parsed.drift.skipped, false, "the resolved base must actually let rule-drift run, not silently skip");
});

test("the CLI's own default (no --base flag) resolves through resolveDefaultBase against the real repo, not a hardcoded HEAD self-compare", () => {
	const resolved = resolveDefaultBase(REPO_ROOT);
	assert.notEqual(resolved, "HEAD", "this checkout has a tracked origin/main, so the default must not degrade to a self-compare");

	// The CLI's own default-base wiring must reach the exact same resolved ref:
	// running with no --base and running with --base <that ref> explicitly must
	// produce byte-identical output against the real repo's real rules.
	const run = (extra) => {
		try {
			return execFileSync("node", [CLI, "--root", REPO_ROOT, ...extra], { encoding: "utf8" });
		} catch (err) {
			return err.stdout;
		}
	};
	assert.equal(run([]), run(["--base", resolved]));
});

// --- tier 3: the real repo, the real W0 rules --------------------------------

// U04d: Story04 LANDED. Every lifecycle pin — including the three that were
// staged behind `activation: "story04"` (the reaper, the bare non-TTY `pix rm`
// refusal, the -k/--keep flag) — now describes shipped behavior and is
// evaluated for real. The staged-pin MECHANISM is still fully covered, by the
// tier-2b self-tests above, which use synthetic fixture pins precisely so that
// landing a real one cannot leave the mechanism untested.
test("the shipped rules/lifecycle.rules.mjs ships the whole Story04 pin set, all ACTIVE (nothing left staged)", async () => {
	const mod = await import(path.join(REAL_RULES_DIR, "lifecycle.rules.mjs"));
	const pins = mod.default;
	assert.ok(pins.length >= 15, "lifecycle must ship the full U04a/U04c/U04d pin set, not the W0 empty placeholder");
	const staged = pins.filter((p) => p.activation);
	assert.deepEqual(
		staged.map((p) => p.id),
		[],
		"Story04 landed: no lifecycle pin may still hide behind an activation key",
	);
	for (const id of [
		"lifecycle.reaper.no-force-requires-absence",
		"lifecycle.rm.bare-nontty-refusal",
		"lifecycle.rm.keep-short-and-long-flag",
		"lifecycle.teardown.ref-closed-before-the-proof",
		"lifecycle.teardown.absent-probe-before-state-clear",
		"lifecycle.teardown.journal-bounded-0600",
	]) {
		assert.ok(
			pins.some((p) => p.id === id),
			`missing the U04d teardown pin ${id}`,
		);
	}
});

test("every real pin holds against THIS repository right now under the shipped activation.json", async () => {
	const pins = await loadRules(REAL_RULES_DIR);
	assert.ok(pins.length >= 15, "should ship a meaningful pin set across all documented domains");
	const domains = new Set(pins.map((p) => p.domain));
	for (const expected of ["memory-rpc", "ports", "sandbox-scope", "permissions", "stdio", "subprocess-argv", "config-keys", "lifecycle"]) {
		assert.ok(domains.has(expected), `missing domain shard: ${expected}`);
	}

	const manifest = loadManifest(path.join(REPO_ROOT, "scripts", "semantic-diff", "intended-changes.json"));
	const activation = loadActivation(path.join(REPO_ROOT, "scripts", "semantic-diff", "activation.json"));
	assert.deepEqual(
		activation.map((e) => e.key),
		["story04"],
		"U04d turned the story04 key on (with its rationale + evidence) in the same commit that landed the behavior",
	);
	const report = evaluatePins(pins, REPO_ROOT, manifest, activationKeySet(activation));
	if (!report.ok) {
		const failures = report.pins.filter((p) => !p.ok).map((p) => `${p.id}: ${JSON.stringify(p.checks.filter((c) => !c.ok))}`);
		assert.fail(`real pins do not hold:\n${failures.join("\n")}`);
	}
	const pending = report.pins.filter((p) => p.pending);
	assert.deepEqual(pending.map((p) => p.id), [], "nothing ships staged any more: a pending pin here is a contract nobody is checking");
});

// The inverse of the test this replaces: those three pins USED to be provably
// failing TODOs, which is how we knew they were not vacuous. Now they must
// provably HOLD — same three ids, opposite expectation — which is the evidence
// that activating the key was earned by behavior rather than by editing a
// rule until it passed.
test("the three formerly-staged Story04 pins now HOLD against this repo (the behavior landed, the pins were not weakened)", async () => {
	const pins = await loadRules(REAL_RULES_DIR);
	const manifest = loadManifest(path.join(REPO_ROOT, "scripts", "semantic-diff", "intended-changes.json"));
	const landed = ["lifecycle.reaper.no-force-requires-absence", "lifecycle.rm.bare-nontty-refusal", "lifecycle.rm.keep-short-and-long-flag"];
	for (const id of landed) {
		const pin = pins.find((p) => p.id === id);
		assert.ok(pin, `missing ${id}`);
		assert.equal(pin.activation, undefined, `${id} must no longer be staged`);
		const report = evaluatePins([pin], REPO_ROOT, manifest, new Set());
		assert.equal(report.ok, true, `${id} must hold: ${JSON.stringify(report.pins[0].checks.filter((c) => !c.ok))}`);
	}
	// And the reaper still refuses force in the only way that counts: the file
	// contains no forced-removal argv at all.
	const reap = fs.readFileSync(path.join(REPO_ROOT, "services/host/workflow/launch/reap.go"), "utf8");
	assert.ok(!reap.includes('"rm", "-f"'), "the reaper must never compose a forced removal");
});

// U03B regression sentinel: BROKER_PORT/PIX_BROKER_PORT were pinned here
// while the broker was still dormant infrastructure; W2/U03B (commit
// cfd4522) deleted the CredentialBroker plugin seam entirely (see
// services/host/cmd/pix/hostmode_gone_test.go's Go-side sentinel for the
// execution-symbol half of this same deletion). A pin that still expected a
// deleted literal was exactly the U03B gate failure this task fixed — assert
// the ports domain never re-pins the retired broker port so a future patch
// can't silently resurrect the requirement without this test noticing.
test("the ports domain no longer pins the retired broker port (W2/U03B deleted it)", async () => {
	const pins = await loadRules(REAL_RULES_DIR);
	const portsPins = pins.filter((p) => p.domain === "ports");
	assert.ok(portsPins.length > 0, "ports domain must still ship pins");
	for (const pin of portsPins) {
		for (const check of pin.checks) {
			const values = check.values ?? check.expected ?? [];
			const flat = Array.isArray(values) ? values : [values];
			for (const v of flat) {
				assert.ok(!String(v).includes("BROKER_PORT"), `${pin.id} (${check.file}) still pins a broker port literal: ${v}`);
			}
		}
	}
});

// The two U04f-era manifest entries (lifecycle.session.record-before-lifecycle-unlock,
// lifecycle.teardown.journal-bounded-0600) documented transitions that landed
// commits ago; the base this guard now resolves to (HEAD~1, see the
// resolveDefaultBase tests above) already has those pins in their current
// shape, so neither entry is needed as a waiver nor to explain any real
// drift any more. They were removed in a follow-up commit so an unrelated,
// later PR does not fail on a stale manifest it had no part in creating.
test("the shipped intended-changes.json no longer carries the two now-spent U04f manifest entries", () => {
	const manifest = loadManifest(path.join(REPO_ROOT, "scripts", "semantic-diff", "intended-changes.json"));
	const ids = manifest.map((e) => e.id);
	assert.ok(!ids.includes("lifecycle.session.record-before-lifecycle-unlock"), "spent entry must be removed once neither a waiver nor real drift needs it");
	assert.ok(!ids.includes("lifecycle.teardown.journal-bounded-0600"), "spent entry must be removed once neither a waiver nor real drift needs it");
});

test("the CLI exits 0 against the real repo and exits 1 against a fixture with a planted corruption", () => {
	const cliOut = execFileSync("node", [CLI, "--root", REPO_ROOT], { encoding: "utf8" });
	assert.match(cliOut, /semantic-diff: PASS/);
	assert.doesNotMatch(cliOut, /FAIL: stale intended-change manifest entries/, "the shipped intended-changes.json must carry no dead entries");

	// Build a full standalone fixture repo tree containing ONLY the real rules
	// + a deliberately corrupted copy of one pinned production file, and run
	// the actual CLI against it via --root, to prove the wiring end-to-end
	// (not just the engine functions in-process).
	const root = mkTmpDir();
	fs.cpSync(path.join(REPO_ROOT, "services"), path.join(root, "services"), { recursive: true });
	fs.cpSync(path.join(REPO_ROOT, "extensions"), path.join(root, "extensions"), { recursive: true });
	// Corrupt the memory RPC method table: rename "remember" -> "rememberFact".
	// The table lives in serve_plugin.go (memoryStoreMux): U11j collapsed the
	// duplicate in memory.go into it, so both :11435 entry points answer through
	// this one map — which is exactly the file a lockstep rename would hit.
	const muxGo = path.join(root, "services/host/serve_plugin.go");
	fs.writeFileSync(muxGo, fs.readFileSync(muxGo, "utf8").replace('"remember": with(func(', '"rememberFact": with(func('));

	let failed = false;
	try {
		execFileSync("node", [CLI, "--root", root, "--no-git"], { encoding: "utf8" });
	} catch (err) {
		failed = true;
		assert.match(err.stdout, /memory\.rpc\.methods\.server/);
		assert.match(err.stdout, /semantic-diff: FAIL/);
	}
	assert.equal(failed, true, "CLI must exit non-zero on a planted corruption");
});

test("the shipped run is clean and has nothing pending; --activate story04 is now a no-op", () => {
	const plainOut = execFileSync("node", [CLI, "--root", REPO_ROOT, "--no-git"], { encoding: "utf8" });
	assert.match(plainOut, /semantic-diff: PASS/);
	assert.doesNotMatch(plainOut, /PEND {2}lifecycle\./, "Story04 landed: no lifecycle pin may still report as pending");
	assert.match(plainOut, /PASS {2}lifecycle\.reaper\.no-force-requires-absence/);

	// Forcing the key that activation.json already ships changes nothing —
	// which is the point: the pins are active because the behavior landed, not
	// because a CLI flag was passed.
	const activatedOut = execFileSync("node", [CLI, "--root", REPO_ROOT, "--no-git", "--activate", "story04"], { encoding: "utf8" });
	assert.match(activatedOut, /semantic-diff: PASS/);
});
