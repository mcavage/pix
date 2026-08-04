import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

// AC-REL-01: legal.yml is a wholly NEW, standalone workflow — it must not
// fold into test.yml/publish.yml's job graph, and the secret-scan job must
// fetch full history (fetch-depth 0 + every remote ref), or it only proves
// the checked-out tip is clean.

const legalWorkflow = fs.readFileSync(new URL("../.github/workflows/legal.yml", import.meta.url), "utf8");
const testWorkflow = fs.readFileSync(new URL("../.github/workflows/test.yml", import.meta.url), "utf8");
const publishWorkflow = fs.readFileSync(new URL("../.github/workflows/publish.yml", import.meta.url), "utf8");

test("legal.yml defines its own third-party-notices, secret-scan, and sbom jobs", () => {
	assert.match(legalWorkflow, /jobs:/);
	assert.match(legalWorkflow, /third-party-notices:/);
	assert.match(legalWorkflow, /secret-scan:/);
	assert.match(legalWorkflow, /\n  sbom:/);
});

test("legal.yml's secret-scan job fetches FULL history, not just the checked-out tip", () => {
	assert.match(legalWorkflow, /fetch-depth: 0/);
	assert.match(legalWorkflow, /fetch origin '\+refs\/heads\/\*/);
	assert.match(legalWorkflow, /check-secret-history\.sh/);
});

test("legal.yml uploads the secret-scan report even on failure", () => {
	assert.match(legalWorkflow, /secret-scan[\s\S]*?if: always\(\)[\s\S]*?secret-scan-report/);
});

test("legal.yml runs check-third-party-notices.sh", () => {
	assert.match(legalWorkflow, /check-third-party-notices\.sh/);
});

test("legal.yml does not duplicate test.yml/publish.yml job names (stays a disjoint workflow)", () => {
	const jobsSection = legalWorkflow.slice(legalWorkflow.indexOf("\njobs:"));
	const legalJobNames = [...jobsSection.matchAll(/^  ([a-z0-9-]+):\n/gm)].map((m) => m[1]);
	assert.deepEqual(legalJobNames.sort(), ["sbom", "secret-scan", "third-party-notices"]);
	for (const name of legalJobNames) {
		assert.ok(!testWorkflow.includes(`\n  ${name}:`), `${name} unexpectedly duplicated in test.yml`);
		assert.ok(!publishWorkflow.includes(`\n  ${name}:`), `${name} unexpectedly duplicated in publish.yml`);
	}
});
