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
	assert.match(gate, /GATE_BUDGET_MS="\$\{GATE_BUDGET_MS:-34000\}"/);
	assert.match(workflow, /GATE_BUDGET_MS: "75000"/);
});

test("the fast gate's Go test segment runs -short, so it never blocks on a test that is slow by design", () => {
	assert.match(gate, /go test -count=1 -v -short \.\/\.\.\./);
});

test("every test skipped under -short is still run in full by an untimed CI job (race or metrics), never dropped", () => {
	// The `race` job is go test -race with NO -short: every t.Skip(testing.Short())
	// test still runs there. The `metrics` job is go test -cover with NO -short:
	// same coverage, plus the derived LOC/exports report. Neither job may gain a
	// -short flag, or the tests marked slow-by-design would stop running anywhere.
	const raceJob = workflow.slice(workflow.indexOf("race:"), workflow.indexOf("metrics:"));
	const metricsJob = workflow.slice(workflow.indexOf("metrics:"), workflow.indexOf("macos:"));
	assert.match(raceJob, /go test -race \.\/\.\.\./);
	assert.doesNotMatch(raceJob, /-short/);
	assert.match(metricsJob, /go test -cover \.\/\.\.\./);
	assert.doesNotMatch(metricsJob, /-short/);
});
