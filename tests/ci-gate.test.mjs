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

test("the timing ceiling is advisory locally and enforced in exactly one place: CI", () => {
	// The local default is 0 (off): a correct-but-slow suite must not fail a
	// developer's build on wall time alone, which is what the stale 34000 ms
	// ceiling had started doing. The gate still measures and still reports
	// every over-GATE_SLOW_MS test, so the signal survives the un-gating.
	assert.match(gate, /GATE_BUDGET_MS="\$\{GATE_BUDGET_MS:-0\}"/);
	// CI remains the one place a genuine cliff is caught before merge, and it
	// must stay an explicit number rather than inheriting the off-by-default.
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

test("the macos job runs the full suite too, never gaining a -short flag", () => {
	// macos is the only macOS CI signal (see the workflow header comment); if it
	// ever picked up -short it would silently stop covering every
	// testing.Short()-gated test on that platform, the same gap race/metrics
	// guard against above.
	const macosJob = workflow.slice(workflow.indexOf("macos:"));
	assert.match(macosJob, /go test \.\/\.\.\./);
	assert.doesNotMatch(macosJob, /-short/);
});
