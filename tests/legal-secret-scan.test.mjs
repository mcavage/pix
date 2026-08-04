import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// AC-REL-01: secret-scan pattern rules + a small-fixture proof that a
// full-history scan catches a secret buried in an amended-away commit (the
// exact case a HEAD-only scan misses). Kept FAST on purpose (< 1s total) so
// it runs in the timed fast gate (scripts/gate.sh's `node --test
// tests/*.test.mjs`, non-recursive glob).
//
// The actual full-history scan of THIS repo's real history — which takes
// ~20s and would blow the fast gate's budget — lives in
// tests/slow/legal-secret-scan-full-history.test.mjs, run by
// .github/workflows/legal.yml instead. See scripts/check-secret-history.sh.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const scanScript = path.join(repoRoot, "scripts/legal/secret-scan.mjs");

const scanMod = await import(scanScript);

test("scanText() flags each known secret shape and does not false-positive on ordinary text", () => {
	assert.ok(scanMod.scanText("AKIAABCDEFGHIJKLMNOP").some((f) => f.ruleId === "aws-access-key-id"));
	assert.ok(scanMod.scanText("-----BEGIN RSA PRIVATE KEY-----").some((f) => f.ruleId === "private-key-block"));
	assert.ok(scanMod.scanText("https://user:hunter2@example.com/x").some((f) => f.ruleId === "url-embedded-basic-auth"));
	assert.deepEqual(scanMod.scanText("function add(a, b) { return a + b; }"), []);
});

test("secret-scan.mjs --self-test exits 0", () => {
	const res = execFileSync("node", [scanScript, "--self-test"], { encoding: "utf8" });
	assert.match(res, /self-test: \d+ rule\(s\) OK/);
});

test("full-history scan catches a secret buried in an amended-away commit", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "secret-scan-fixture-"));
	const git = (args) => execFileSync("git", args, { cwd: tmp, encoding: "utf8" });
	git(["init", "-q"]);
	git(["config", "user.email", "test@example.com"]);
	git(["config", "user.name", "Test"]);
	fs.writeFileSync(path.join(tmp, "config.py"), 'AWS_KEY = "AKIAABCDEFGHIJKLMNOP"\n');
	git(["add", "."]);
	git(["commit", "-q", "-m", "oops, a real key"]);
	// "Fix" it in a later commit — a HEAD-only scan would now see nothing.
	fs.writeFileSync(path.join(tmp, "config.py"), 'AWS_KEY = os.environ["AWS_KEY"]\n');
	git(["add", "."]);
	git(["commit", "-q", "-m", "remove hardcoded key"]);

	const outPath = path.join(tmp, "report.json");
	try {
		execFileSync("node", [scanScript, "--scan", tmp, "--out", outPath], { encoding: "utf8" });
		assert.fail("expected the scan to fail closed on the buried secret");
	} catch (err) {
		assert.notEqual(err.status, 0);
	}
	const report = JSON.parse(fs.readFileSync(outPath, "utf8"));
	assert.ok(report.findings_count >= 1);
	assert.ok(report.findings.some((f) => f.ruleId === "aws-access-key-id"));
	fs.rmSync(tmp, { recursive: true, force: true });
});

test("the allowlist file's own path is excluded from the scan (self-referential fixed-point guard)", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "secret-scan-selfexclude-"));
	const git = (args) => execFileSync("git", args, { cwd: tmp, encoding: "utf8" });
	git(["init", "-q"]);
	git(["config", "user.email", "test@example.com"]);
	git(["config", "user.name", "Test"]);
	fs.mkdirSync(path.join(tmp, "scripts/legal"), { recursive: true });
	fs.writeFileSync(
		path.join(tmp, "scripts/legal/secret-scan-allowlist.txt"),
		'deadbeef  # aws-access-key-id in x.py — reviewed: fixture ("AKIAABCDEFGHIJKLMNOP")\n'
	);
	git(["add", "."]);
	git(["commit", "-q", "-m", "allowlist with a quoted example"]);

	const outPath = path.join(tmp, "report.json");
	const res = execFileSync("node", [scanScript, "--scan", tmp, "--out", outPath], { encoding: "utf8" });
	assert.match(res, /no secrets found/);
	const report = JSON.parse(fs.readFileSync(outPath, "utf8"));
	assert.equal(report.findings_count, 0);
	fs.rmSync(tmp, { recursive: true, force: true });
});

test("full-history scan passes clean on a fixture repo with no secrets", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "secret-scan-clean-"));
	const git = (args) => execFileSync("git", args, { cwd: tmp, encoding: "utf8" });
	git(["init", "-q"]);
	git(["config", "user.email", "test@example.com"]);
	git(["config", "user.name", "Test"]);
	fs.writeFileSync(path.join(tmp, "README.md"), "# nothing secret here\n");
	git(["add", "."]);
	git(["commit", "-q", "-m", "init"]);

	const outPath = path.join(tmp, "report.json");
	const res = execFileSync("node", [scanScript, "--scan", tmp, "--out", outPath], { encoding: "utf8" });
	assert.match(res, /no secrets found/);
	const report = JSON.parse(fs.readFileSync(outPath, "utf8"));
	assert.equal(report.findings_count, 0);
	fs.rmSync(tmp, { recursive: true, force: true });
});
