import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// AC-REL-04: SBOM/provenance config — immutable version/digest recording and
// verification after manifest assembly, plus the base-image digest resolver's
// pure JSON-parsing path (no live Docker/network needed to test it).

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const resolveScript = path.join(repoRoot, "scripts/release/resolve-base-digest.sh");
const verifyScript = path.join(repoRoot, "scripts/release/verify-provenance.sh");
const tagScript = path.join(repoRoot, "scripts/release/tag-availability.sh");
const publishWorkflow = fs.readFileSync(path.join(repoRoot, ".github/workflows/publish.yml"), "utf8");

const DIGEST_A = "sha256:" + "a".repeat(64);
const DIGEST_B = "sha256:" + "b".repeat(64);

function run(script, args, opts = {}) {
	return spawnSync("bash", [script, ...args], { encoding: "utf8", ...opts });
}

test("resolve-base-digest.sh --parse extracts the digest from an imagetools inspect JSON fixture", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "digest-fixture-"));
	const jsonFile = path.join(tmp, "inspect.json");
	fs.writeFileSync(
		jsonFile,
		JSON.stringify({
			manifest: { digest: DIGEST_A, mediaType: "application/vnd.oci.image.index.v1+json" },
			name: "dhi.io/node:25-debian13-dev",
		})
	);
	try {
		const res = run(resolveScript, ["--parse", jsonFile]);
		assert.equal(res.status, 0, res.stderr);
		assert.equal(res.stdout.trim(), DIGEST_A);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

test("resolve-base-digest.sh --parse fails closed on a fixture with no digest", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "digest-fixture-"));
	const jsonFile = path.join(tmp, "inspect.json");
	fs.writeFileSync(jsonFile, JSON.stringify({ name: "dhi.io/node:25-debian13-dev" }));
	try {
		const res = run(resolveScript, ["--parse", jsonFile]);
		assert.notEqual(res.status, 0);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

test("resolve-base-digest.sh live mode fails closed (not fabricated) when docker is unavailable", () => {
	// Keep enough PATH for bash/mktemp/grep/sed to resolve, but exclude any
	// directory that might contain a real `docker` binary.
	const res = run(resolveScript, ["dhi.io/node:25-debian13-dev"], {
		env: { ...process.env, PATH: "/usr/bin:/bin" },
	});
	assert.notEqual(res.status, 0);
	assert.match(res.stdout + res.stderr, /docker is not installed|refusing to fabricate/);
});

test("verify-provenance.sh rejects a non-semver version", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "provenance-"));
	try {
		const res = run(verifyScript, ["not-a-version", DIGEST_A, "abc123", tmp]);
		assert.notEqual(res.status, 0);
		assert.match(res.stderr, /not semver/);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

test("verify-provenance.sh rejects a malformed digest", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "provenance-"));
	try {
		const res = run(verifyScript, ["1.2.3", "sha256:not-hex", "abc123", tmp]);
		assert.notEqual(res.status, 0);
		assert.match(res.stderr, /not a well-formed sha256 digest/);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

test("verify-provenance.sh records a fresh version and is idempotent on a re-run with the same digest", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "provenance-"));
	try {
		const first = run(verifyScript, ["1.2.3", DIGEST_A, "abc123", tmp]);
		assert.equal(first.status, 0, first.stderr);
		const record = JSON.parse(fs.readFileSync(path.join(tmp, "1.2.3.json"), "utf8"));
		assert.equal(record.version, "1.2.3");
		assert.equal(record.digest, DIGEST_A);
		assert.equal(record.git_sha, "abc123");

		const second = run(verifyScript, ["1.2.3", DIGEST_A, "abc123", tmp]);
		assert.equal(second.status, 0, second.stderr);
		assert.match(second.stdout, /no-op/);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

test("verify-provenance.sh fails closed when a version's recorded digest would change (immutability WITHIN a run / against a restored record)", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "provenance-"));
	try {
		const first = run(verifyScript, ["1.2.3", DIGEST_A, "abc123", tmp]);
		assert.equal(first.status, 0, first.stderr);

		const second = run(verifyScript, ["1.2.3", DIGEST_B, "def456", tmp]);
		assert.notEqual(second.status, 0);
		assert.match(second.stderr, /IMMUTABILITY VIOLATION/);

		// The original record must be untouched.
		const record = JSON.parse(fs.readFileSync(path.join(tmp, "1.2.3.json"), "utf8"));
		assert.equal(record.digest, DIGEST_A);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

// --- cross-run tag immutability ----------------------------------------------
// The gap these close: `version` picks the next version with no v<version> GIT
// tag, but that tag is created by `bump` — AFTER `merge` pushed
// pix:<version>. A run that published and then died left the git tag free and
// the Docker tag taken, so the next run re-selected that version and
// `imagetools create` overwrote a published tag. out/provenance/<v>.json
// cannot catch it (fresh runner workspace, no prior record), so the check has
// to ask the registry — and must never turn "I could not tell" into "free".

function classify(code, stderrText) {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "tag-avail-"));
	const errFile = path.join(tmp, "stderr.txt");
	fs.writeFileSync(errFile, stderrText);
	try {
		const res = run(tagScript, ["--classify", String(code), errFile]);
		return { status: res.status, verdict: res.stdout.trim(), stderr: res.stderr };
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
}

test("tag-availability.sh: a successful inspect means the tag is TAKEN", () => {
	const res = classify(0, "");
	assert.equal(res.status, 0);
	assert.equal(res.verdict, "taken");
});

test("tag-availability.sh: registry 'not found' shapes mean FREE", () => {
	for (const text of [
		"ERROR: docker.io/mcavage/pix:0.1.25: not found",
		"ERROR: manifest unknown",
		'{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}',
		"ERROR: unexpected status: 404 Not Found",
	]) {
		const res = classify(1, text);
		assert.equal(res.status, 0, `${text} -> ${res.stderr}`);
		assert.equal(res.verdict, "free", text);
	}
});

test("tag-availability.sh: FAILS CLOSED on auth/network/unknown — never 'free'", () => {
	for (const text of [
		"ERROR: unauthorized: authentication required",
		"ERROR: pull access denied, repository does not exist or may require authorization",
		"ERROR: dial tcp: lookup registry-1.docker.io: no such host",
		"ERROR: failed to do request: TLS handshake timeout",
		"ERROR: something nobody has seen before",
	]) {
		const res = classify(1, text);
		assert.equal(res.status, 2, `expected UNDECIDED for: ${text}`);
		assert.notEqual(res.verdict, "free", text);
		assert.match(res.stderr, /UNDECIDED/);
	}
});

test("tag-availability.sh live mode fails closed (not fabricated) when docker is unavailable", () => {
	const res = run(tagScript, ["docker.io/mcavage/pix", "1.2.3"], {
		env: { ...process.env, PATH: "/usr/bin:/bin" },
	});
	assert.equal(res.status, 2);
	assert.match(res.stdout + res.stderr, /docker is not installed|refusing to fabricate/);
	assert.doesNotMatch(res.stdout, /free/);
});

test("publish.yml asks the registry BOTH when selecting a version and again before mutating a tag", () => {
	const versionJob = publishWorkflow.slice(
		publishWorkflow.indexOf("\n  version:"),
		publishWorkflow.indexOf("\n  patch-smoke:")
	);
	const mergeJob = publishWorkflow.slice(
		publishWorkflow.indexOf("\n  merge:"),
		publishWorkflow.indexOf("\n  provenance:")
	);
	assert.match(versionJob, /tag-availability\.sh/);
	assert.match(versionJob, /Selecting the next patch instead of overwriting it/);
	// Both call sites must capture the verdict with an ASSIGNMENT. Inside an
	// `if [ "$(...)" = ... ]` the condition context suppresses `set -e`, so the
	// script's fail-closed exit 2 would read as an empty string, i.e. "not
	// taken", i.e. fail OPEN. Proven below against the real shell.
	for (const [label, job] of [["version", versionJob], ["merge", mergeJob]]) {
		assert.match(job, /verdict="\$\(bash scripts\/release\/tag-availability\.sh/, `${label} job does not assign the verdict`);
		assert.doesNotMatch(
			job,
			/if \[ "\$\(bash scripts\/release\/tag-availability\.sh/,
			`${label} job calls tag-availability.sh inside a condition, which suppresses set -e (fail-open)`
		);
	}
	// The pre-mutation check must come BEFORE `imagetools create` in the job.
	assert.match(mergeJob, /tag-availability\.sh/);
	assert.ok(
		mergeJob.indexOf("tag-availability.sh") < mergeJob.indexOf("imagetools create"),
		"the registry re-check must run before the manifest is created"
	);
	assert.match(mergeJob, /refusing to overwrite a published tag/);
	// merge proceeds ONLY on an explicit "free"; anything else stops it.
	assert.match(mergeJob, /if \[ "\$verdict" != "free" \]/);
});

test("docs do NOT claim the ephemeral provenance record enforces cross-run immutability", () => {
	const findings = fs.readFileSync(path.join(repoRoot, "docs/legal/FINDINGS.md"), "utf8");
	const safeguards = fs.readFileSync(
		path.join(repoRoot, "docs/legal/RELEASE-SAFEGUARDS.md"),
		"utf8"
	);
	// The retired claim: cross-run uniqueness resting on the version selector's
	// git-tag scan alone. It was false — the git tag is created after the push.
	assert.doesNotMatch(
		findings,
		/uniqueness across runs comes from the\s+version selector/,
		"FINDINGS.md still rests cross-run uniqueness on the git-tag selector"
	);
	assert.match(findings, /enforces nothing across runs/);
	assert.match(findings, /tag-availability\.sh/);
	assert.match(safeguards, /does not, and cannot, enforce immutability across runs/);
	assert.match(safeguards, /registry is the only durable state/i);
	// And the honest scope note that neither mechanism is a signature.
	assert.match(findings, /Neither is a signature/);
});

test("a fail-closed verdict inside an `if [ \"$(...)\" ]` really would fail OPEN (why the assignment above matters)", () => {
	// This is the shell semantic the workflow assertions above encode. Proven,
	// not asserted from memory: a command substitution in a CONDITION is exempt
	// from `set -e`, so exit 2 becomes "" and the else-branch runs.
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "set-e-semantics-"));
	const script = path.join(tmp, "sem.sh");
	try {
		fs.writeFileSync(
			script,
			[
				"set -euo pipefail",
				"probe() { return 2; }",
				'if [ "$(probe)" = "taken" ]; then echo COND_TAKEN; else echo COND_FAIL_OPEN; fi',
				'v="$(probe)"',
				"echo ASSIGN_CONTINUED",
			].join("\n") + "\n"
		);
		const res = run(script, []);
		assert.match(res.stdout, /COND_FAIL_OPEN/);
		assert.doesNotMatch(res.stdout, /ASSIGN_CONTINUED/, "the assignment form must abort under set -e");
		assert.equal(res.status, 2);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});
