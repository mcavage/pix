import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// Fixture repos must not inherit the developer's ~/.gitconfig. A machine that
// signs commits with a hardware/1Password key makes every fixture commit block
// on an authorization prompt that never comes in a test run — the suite then
// hangs for a minute per commit and times out, with nothing in the output to
// say why. Signing proves nothing about a throwaway repo, so it is turned off
// explicitly rather than left to whatever the host happens to be configured for.
const GIT_HERMETIC = ["-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"];

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
	const git = (args) => execFileSync("git", [...GIT_HERMETIC, ...args], { cwd: tmp, encoding: "utf8" });
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

test("parseBatchOutput() parses a blob record and a missing record from a raw Buffer", () => {
	const content = Buffer.from("hello\nworld", "utf8"); // deliberately contains an internal newline
	const header = Buffer.from(`abc123 blob ${content.length}\n`, "utf8");
	const missing = Buffer.from("def456 missing\n", "utf8");
	const buf = Buffer.concat([header, content, Buffer.from("\n"), missing]);
	const map = scanMod.parseBatchOutput(buf);
	assert.equal(map.get("abc123").toString("utf8"), "hello\nworld");
	assert.equal(map.get("def456"), null);
});

test("batchCheck() and batchContent() report the same type/size/content as per-object `git cat-file`", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "secret-scan-batch-"));
	const git = (args) => execFileSync("git", [...GIT_HERMETIC, ...args], { cwd: tmp, encoding: "utf8" });
	git(["init", "-q"]);
	git(["config", "user.email", "test@example.com"]);
	git(["config", "user.name", "Test"]);
	fs.writeFileSync(path.join(tmp, "a.txt"), "hello world\n");
	fs.writeFileSync(path.join(tmp, "b.txt"), "AKIAABCDEFGHIJKLMNOP\n");
	git(["add", "."]);
	git(["commit", "-q", "-m", "init"]);
	const shaA = git(["rev-parse", "HEAD:a.txt"]).trim();
	const shaB = git(["rev-parse", "HEAD:b.txt"]).trim();

	const meta = scanMod.batchCheck(tmp, [shaA, shaB]);
	assert.equal(meta.get(shaA).type, "blob");
	assert.equal(meta.get(shaA).size, "hello world\n".length);
	assert.equal(meta.get(shaB).type, "blob");

	const contents = scanMod.batchContent(tmp, [shaA, shaB], (sha) => meta.get(sha).size);
	assert.equal(contents.get(shaA).toString("utf8"), "hello world\n");
	assert.equal(contents.get(shaB).toString("utf8"), "AKIAABCDEFGHIJKLMNOP\n");
	fs.rmSync(tmp, { recursive: true, force: true });
});

test("scanRepo() makes a FIXED, small number of git invocations regardless of history size (no per-object spawnSync loop)", () => {
	function makeRepo(fileCount) {
		const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "secret-scan-scale-"));
		const git = (args) => execFileSync("git", [...GIT_HERMETIC, ...args], { cwd: tmp, encoding: "utf8" });
		git(["init", "-q"]);
		git(["config", "user.email", "test@example.com"]);
		git(["config", "user.name", "Test"]);
		for (let i = 0; i < fileCount; i++) {
			fs.writeFileSync(path.join(tmp, `file${i}.txt`), `ordinary content number ${i}\n`);
			git(["add", "."]);
			git(["commit", "-q", "-m", `commit ${i}`]);
		}
		return tmp;
	}

	const small = makeRepo(3);
	const large = makeRepo(40);
	try {
		scanMod.resetGitCallCount();
		scanMod.scanRepo(small, null);
		const smallCalls = scanMod.gitCallCount;

		scanMod.resetGitCallCount();
		scanMod.scanRepo(large, null);
		const largeCalls = scanMod.gitCallCount;

		// The old implementation spawned 3 processes PER OBJECT, so 40 files
		// across 40 commits (~80+ blobs/trees) would cost 200+ calls versus a
		// handful for 3 files. The batched implementation makes the SAME small
		// number of git invocations (rev-list + batch-check + a couple of
		// content-fetch chunks) no matter how many objects are in history.
		assert.ok(smallCalls <= 5, `expected <=5 git calls for the small repo, got ${smallCalls}`);
		assert.ok(largeCalls <= 5, `expected <=5 git calls for the large repo (batched!), got ${largeCalls}`);
		assert.equal(largeCalls, smallCalls, "call count should be flat regardless of history size");
	} finally {
		fs.rmSync(small, { recursive: true, force: true });
		fs.rmSync(large, { recursive: true, force: true });
	}
});

test("the allowlist file's own path is excluded from the scan (self-referential fixed-point guard)", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "secret-scan-selfexclude-"));
	const git = (args) => execFileSync("git", [...GIT_HERMETIC, ...args], { cwd: tmp, encoding: "utf8" });
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
	const git = (args) => execFileSync("git", [...GIT_HERMETIC, ...args], { cwd: tmp, encoding: "utf8" });
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
