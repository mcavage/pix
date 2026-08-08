import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

// AC-REL-01: the legal gate (full-history secret scan, third-party/license
// class gate, repo SBOM) must GATE the publish path, not run beside it.
// GitHub does not order two workflows against each other, so while legal.yml
// was only a standalone workflow, a push to main could publish
// pix:<version> while the gate was still running — or after it had failed.
// legal.yml now also declares `on: workflow_call` and publish.yml calls it as
// the `legal-gate` job that both `build` and `merge` need.
//
// The parsing below is deliberately structural (top-level job ids and their
// `needs:` edges) rather than a substring match: the point of these tests is
// the dependency GRAPH, and a regex over the whole file cannot tell which job
// a `needs:` line belongs to.

const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");
const legalWorkflow = read("../.github/workflows/legal.yml");
const testWorkflow = read("../.github/workflows/test.yml");
const publishWorkflow = read("../.github/workflows/publish.yml");

// parseJobs(yamlText) -> Map<jobId, {needs:string[], uses:string|null, body:string}>
// Handles the two `needs` spellings GitHub accepts (flow sequence and block
// sequence) and ignores anything indented deeper than a job's own keys.
function parseJobs(text) {
	const lines = text.split("\n");
	const start = lines.findIndex((l) => /^jobs:\s*$/.test(l));
	assert.notEqual(start, -1, "workflow has no top-level `jobs:` mapping");
	const jobs = new Map();
	let current = null;
	for (let i = start + 1; i < lines.length; i++) {
		const line = lines[i];
		if (/^\S/.test(line) && line.trim() !== "") break; // next top-level key
		const jobHeader = line.match(/^ {2}([A-Za-z0-9_-]+):\s*$/);
		if (jobHeader) {
			current = { id: jobHeader[1], needs: [], uses: null, body: "" };
			jobs.set(current.id, current);
			continue;
		}
		if (!current) continue;
		current.body += line + "\n";
		const inlineNeeds = line.match(/^ {4}needs:\s*\[(.*)\]\s*$/);
		if (inlineNeeds) {
			current.needs = inlineNeeds[1]
				.split(",")
				.map((s) => s.trim())
				.filter(Boolean);
			continue;
		}
		const scalarNeeds = line.match(/^ {4}needs:\s*([A-Za-z0-9_-]+)\s*$/);
		if (scalarNeeds) {
			current.needs = [scalarNeeds[1]];
			continue;
		}
		if (/^ {4}needs:\s*$/.test(line)) {
			for (let j = i + 1; j < lines.length; j++) {
				const item = lines[j].match(/^ {6}-\s*([A-Za-z0-9_-]+)\s*$/);
				if (!item) break;
				current.needs.push(item[1]);
				i = j;
			}
			continue;
		}
		const uses = line.match(/^ {4}uses:\s*(\S+)\s*$/);
		if (uses) current.uses = uses[1];
	}
	return jobs;
}

const legalJobs = parseJobs(legalWorkflow);
const publishJobs = parseJobs(publishWorkflow);

// Transitive closure of a job's `needs`.
function ancestors(jobs, id, seen = new Set()) {
	for (const dep of jobs.get(id)?.needs ?? []) {
		if (seen.has(dep)) continue;
		seen.add(dep);
		ancestors(jobs, dep, seen);
	}
	return seen;
}

test("legal.yml defines the three gate jobs and nothing else", () => {
	assert.deepEqual([...legalJobs.keys()].sort(), ["sbom", "secret-scan", "third-party-notices"]);
});

test("legal.yml is callable as a reusable workflow AND still runs on PRs", () => {
	const on = legalWorkflow.slice(legalWorkflow.indexOf("\non:"), legalWorkflow.indexOf("\njobs:"));
	assert.match(on, /workflow_call:/);
	assert.match(on, /pull_request:/);
	assert.match(on, /push:/);
});

test("publish.yml calls legal.yml as an in-graph job (not a workflow racing it)", () => {
	const gate = publishJobs.get("legal-gate");
	assert.ok(gate, "publish.yml has no legal-gate job");
	assert.equal(gate.uses, "./.github/workflows/legal.yml");
});

test("NOTHING is pushed before the legal gate: build and merge both depend on it", () => {
	// `build` pushes layers by digest; `merge` creates the versioned tag. Both
	// must wait, and `merge` names it directly so the edge is explicit at the
	// step that mutates a public tag.
	for (const job of ["build", "merge"]) {
		assert.ok(publishJobs.has(job), `publish.yml lost its ${job} job`);
		assert.ok(
			publishJobs.get(job).needs.includes("legal-gate"),
			`${job} does not list legal-gate in needs: [${publishJobs.get(job).needs}]`
		);
	}
});

test("every publish job that can push, tag, or release is downstream of legal-gate", () => {
	// Anything that mutates something public: the image tags, the git tag, the
	// GitHub release, the tap PR. patch-smoke/version only read.
	for (const job of ["build", "merge", "provenance", "bump", "release-binaries", "bump-tap"]) {
		assert.ok(publishJobs.has(job), `publish.yml lost its ${job} job`);
		assert.ok(
			ancestors(publishJobs, job).has("legal-gate"),
			`${job} is not transitively gated by legal-gate`
		);
	}
});

test("the legal gate itself waits on nothing in publish (it cannot be gated by the thing it gates)", () => {
	assert.deepEqual(publishJobs.get("legal-gate").needs, []);
});

test("legal.yml's secret-scan job fetches FULL history, not just the checked-out tip", () => {
	const secretScan = legalJobs.get("secret-scan").body;
	assert.match(secretScan, /fetch-depth: 0/);
	assert.match(secretScan, /fetch origin '\+refs\/heads\/\*/);
	assert.match(secretScan, /check-secret-history\.sh/);
});

test("legal.yml uploads the secret-scan report even on failure", () => {
	assert.match(legalJobs.get("secret-scan").body, /if: always\(\)[\s\S]*?secret-scan-report/);
});

test("legal.yml runs check-third-party-notices.sh and asserts a non-empty repo SBOM", () => {
	assert.match(legalJobs.get("third-party-notices").body, /check-third-party-notices\.sh/);
	assert.match(legalJobs.get("sbom").body, /packages \| length > 0/);
});

test("legal.yml's jobs are defined once, in legal.yml (publish reuses, never duplicates)", () => {
	for (const name of legalJobs.keys()) {
		assert.ok(!testWorkflow.includes(`\n  ${name}:`), `${name} unexpectedly duplicated in test.yml`);
		assert.ok(!publishJobs.has(name), `${name} unexpectedly redefined in publish.yml`);
	}
});
