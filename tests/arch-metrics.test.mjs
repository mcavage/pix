import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// Covers AC-GATE-03/04 (Story00, W0): derived architecture metrics + a
// shrink-only per-package budget ratchet, wired as a CHEAP gate segment (no
// `go test`, no coverage — that lives in the untimed `metrics` CI job) so the
// timed fast gate stays under its budget.
//
// This test builds and runs the REAL tool (no mocks) against:
//   1. a tiny real fixture tree, to pin the derived metrics' semantics, and
//   2. the actual services/host tree + the COMMITTED budgets.json, to prove
//      the current baseline does not fail its own shrink-only gate.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const toolDir = path.join(repoRoot, "scripts/arch-metrics");
const budgetsPath = path.join(toolDir, "budgets.json");
const hostRoot = path.join(repoRoot, "services/host");

function buildTool() {
	const bin = path.join(os.tmpdir(), `pix-arch-metrics-test-${process.pid}`);
	execFileSync("go", ["build", "-o", bin, "."], { cwd: toolDir, stdio: "pipe" });
	return bin;
}

function run(bin, args) {
	try {
		const stdout = execFileSync(bin, args, { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
		return { code: 0, stdout };
	} catch (err) {
		return { code: err.status ?? 1, stdout: err.stdout?.toString() ?? "", stderr: err.stderr?.toString() ?? "" };
	}
}

test("arch-metrics builds", () => {
	const bin = buildTool();
	assert.ok(fs.existsSync(bin));
	fs.rmSync(bin, { force: true });
});

test("current services/host tree passes its own committed shrink-only budget", () => {
	const bin = buildTool();
	const result = run(bin, ["-root", hostRoot, "-budgets", budgetsPath]);
	assert.equal(result.code, 0, `expected the CURRENT baseline to pass its own budget, got:\n${result.stderr}`);
	fs.rmSync(bin, { force: true });
});

test("shrink-only check fails loud against an artificially tightened budget", () => {
	const bin = buildTool();
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "arch-metrics-tight-"));
	const tight = path.join(tmp, "budgets.json");
	// Every real package must have prod_loc > 0, so a budget of prod_loc: 0
	// for every recorded package is guaranteed to violate.
	const current = JSON.parse(execFileSync(bin, ["-root", hostRoot]).toString());
	const packages = {};
	for (const pkg of Object.keys(current.packages)) {
		packages[pkg] = { prod_loc: 0, exports: 0, globals: 0, edges: 0, exits: 0 };
	}
	fs.writeFileSync(tight, JSON.stringify({ schema: 1, packages }));

	const result = run(bin, ["-root", hostRoot, "-budgets", tight]);
	assert.notEqual(result.code, 0, "expected a nonzero exit against an impossible budget");
	assert.match(result.stderr, /shrink-only budget violations/);
	fs.rmSync(bin, { force: true });
	fs.rmSync(tmp, { recursive: true, force: true });
});

test("a package absent from the budgets file never fails (seeding is a separate, non-breaking step)", () => {
	const bin = buildTool();
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "arch-metrics-empty-"));
	const empty = path.join(tmp, "budgets.json");
	fs.writeFileSync(empty, JSON.stringify({ schema: 1, packages: {} }));

	const result = run(bin, ["-root", hostRoot, "-budgets", empty]);
	assert.equal(result.code, 0, `expected no violations against an empty budgets file, got:\n${result.stderr}`);
	fs.rmSync(bin, { force: true });
	fs.rmSync(tmp, { recursive: true, force: true });
});

test("budgets.json is tracked, valid, and covers every current package", () => {
	assert.ok(fs.existsSync(budgetsPath), "scripts/arch-metrics/budgets.json must be committed");
	const budgets = JSON.parse(fs.readFileSync(budgetsPath, "utf8"));
	assert.equal(budgets.schema, 1);
	const bin = buildTool();
	const current = JSON.parse(execFileSync(bin, ["-root", hostRoot]).toString());
	for (const pkg of Object.keys(current.packages)) {
		assert.ok(pkg in budgets.packages, `budgets.json is missing package ${pkg} — run the seeding command in scripts/arch-metrics`);
	}
	fs.rmSync(bin, { force: true });
});

test("the fast gate wires arch-metrics as a cheap, non-coverage segment", () => {
	const gate = fs.readFileSync(path.join(repoRoot, "scripts/gate.sh"), "utf8");
	assert.match(gate, /arch[_-]metrics/, "scripts/gate.sh must run the arch-metrics shrink-only check");
	assert.doesNotMatch(gate, /arch-metrics[^\n]*-coverage/, "the timed gate must not run the expensive coverage path");
});

test("CI has an untimed job for full corpus/coverage/LOC, separate from the timed gate", () => {
	const workflow = fs.readFileSync(path.join(repoRoot, ".github/workflows/test.yml"), "utf8");
	assert.match(workflow, /-coverage/, "the untimed metrics job must run the coverage-merging path");
	assert.match(workflow, /go test -cover/, "the untimed metrics job must run the full corpus with coverage");
});

test("CI has a required macos-latest build/test job", () => {
	const workflow = fs.readFileSync(path.join(repoRoot, ".github/workflows/test.yml"), "utf8");
	assert.match(workflow, /runs-on:\s*macos-latest/, "expected a job pinned to macos-latest");
});
