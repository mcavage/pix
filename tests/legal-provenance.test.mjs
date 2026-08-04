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

test("verify-provenance.sh fails closed when a version's recorded digest would change (immutability)", () => {
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
