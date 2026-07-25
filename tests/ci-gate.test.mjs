import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const workflow = fs.readFileSync(new URL("../.github/workflows/test.yml", import.meta.url), "utf8");
const gate = fs.readFileSync(new URL("../scripts/gate.sh", import.meta.url), "utf8");

test("PR branches do not launch duplicate push and pull_request workflows", () => {
	assert.match(workflow, /push:\n\s+branches: \["main"\]\n\s+pull_request:/);
});

test("Node test workers cannot strand CI after results complete", () => {
	assert.match(gate, /node --test --test-force-exit --test-concurrency=4/);
});

test("local and CI timing budgets remain explicit", () => {
	assert.match(gate, /GATE_BUDGET_MS="\$\{GATE_BUDGET_MS:-12000\}"/);
	assert.match(workflow, /GATE_BUDGET_MS: "75000"/);
});
