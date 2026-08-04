// Self-test for the U00c (W0) semantic-diff guard.
//
// Three tiers:
//   1. Unit tests of the pure engine (scripts/semantic-diff/lib/engine.mjs)
//      against small in-memory/tmp-dir fixtures — fast, no real repo needed.
//   2. A PLANTED CORRUPTION self-test: a fixture that mimics the exact failure
//      mode this framework exists for — a production value AND its
//      "consumer" witness get renamed together in one commit (lockstep
//      corruption) — and proves the guard still fails because the pin's
//      `expected` is a third, independently fixed literal.
//   3. An integration run against the REAL repository with the REAL W0 rules,
//      proving every pin this task ships is actually true today.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

import { checkRuleDrift, evaluateCheck, evaluatePins, extractRegion, extractSet, loadManifest, loadRules } from "../scripts/semantic-diff/lib/engine.mjs";

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

// --- tier 3: the real repo, the real W0 rules --------------------------------

test("the shipped rules/lifecycle.rules.mjs is present and empty (Story04's reserved, untouched extension point)", async () => {
	const mod = await import(path.join(REAL_RULES_DIR, "lifecycle.rules.mjs"));
	assert.deepEqual(mod.default, []);
});

test("every real W0 pin holds against THIS repository right now (no manifest waivers needed)", async () => {
	const pins = await loadRules(REAL_RULES_DIR);
	assert.ok(pins.length >= 15, "W0 should ship a meaningful pin set across all documented domains");
	const domains = new Set(pins.map((p) => p.domain));
	for (const expected of ["memory-rpc", "ports", "sandbox-scope", "permissions", "stdio", "subprocess-argv", "config-keys"]) {
		assert.ok(domains.has(expected), `missing domain shard: ${expected}`);
	}
	// lifecycle ships zero pins at W0 (Story04's reserved extension point), so it
	// never appears in a pin-derived domain set — assert the shard FILE exists
	// instead of asking pins (there are none) to name it.
	assert.ok(fs.existsSync(path.join(REAL_RULES_DIR, "lifecycle.rules.mjs")), "lifecycle.rules.mjs shard must exist for Story04");

	const manifest = loadManifest(path.join(REPO_ROOT, "scripts", "semantic-diff", "intended-changes.json"));
	const report = evaluatePins(pins, REPO_ROOT, manifest);
	if (!report.ok) {
		const failures = report.pins.filter((p) => !p.ok).map((p) => `${p.id}: ${JSON.stringify(p.checks.filter((c) => !c.ok))}`);
		assert.fail(`real W0 pins do not hold:\n${failures.join("\n")}`);
	}
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

test("the CLI exits 0 against the real repo and exits 1 against a fixture with a planted corruption", () => {
	const cliOut = execFileSync("node", [CLI, "--root", REPO_ROOT], { encoding: "utf8" });
	assert.match(cliOut, /semantic-diff: PASS/);

	// Build a full standalone fixture repo tree containing ONLY the real rules
	// + a deliberately corrupted copy of one pinned production file, and run
	// the actual CLI against it via --root, to prove the wiring end-to-end
	// (not just the engine functions in-process).
	const root = mkTmpDir();
	fs.cpSync(path.join(REPO_ROOT, "services"), path.join(root, "services"), { recursive: true });
	fs.cpSync(path.join(REPO_ROOT, "extensions"), path.join(root, "extensions"), { recursive: true });
	// Corrupt the memory RPC method table: rename "remember" -> "rememberFact".
	const memGo = path.join(root, "services/host/memory.go");
	fs.writeFileSync(memGo, fs.readFileSync(memGo, "utf8").replace('"remember": func(p jsonObj)', '"rememberFact": func(p jsonObj)'));

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
