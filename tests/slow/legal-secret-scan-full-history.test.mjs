import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// AC-REL-01: the REAL full-history scan of this repo (~20s — deliberately
// NOT under tests/*.test.mjs, so it never counts against scripts/gate.sh's
// timed budget; see the header comment there and in
// .github/workflows/legal.yml, which is what actually runs this file).

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const scanScript = path.join(repoRoot, "scripts/legal/secret-scan.mjs");

test(
	"full-history scan of THIS repo's real git history (with the reviewed allowlist) reports no findings",
	{ timeout: 120_000 },
	() => {
		const allowlistPath = path.join(repoRoot, "scripts/legal/secret-scan-allowlist.txt");
		const outPath = path.join(os.tmpdir(), `pix-secret-scan-report-${process.pid}.json`);
		try {
			execFileSync(
				"node",
				[scanScript, "--scan", repoRoot, "--out", outPath, "--allowlist", allowlistPath],
				{ encoding: "utf8", maxBuffer: 1024 * 1024 * 64, timeout: 110_000 }
			);
		} catch (err) {
			const report = fs.existsSync(outPath) ? fs.readFileSync(outPath, "utf8") : "(no report written)";
			assert.fail(`full-history scan found something not in the allowlist — see ${outPath}\n${report.slice(0, 4000)}`);
		}
		const report = JSON.parse(fs.readFileSync(outPath, "utf8"));
		assert.equal(report.findings_count, 0);
	}
);

test("scripts/check-secret-history.sh passes on the real tree and writes an artifact", { timeout: 120_000 }, () => {
	const outDir = fs.mkdtempSync(path.join(os.tmpdir(), "secret-history-out-"));
	const res = execFileSync("bash", ["scripts/check-secret-history.sh"], {
		cwd: repoRoot,
		encoding: "utf8",
		env: { ...process.env, SECRET_SCAN_OUT_DIR: outDir },
		timeout: 110_000,
	});
	assert.match(res, /report at/);
	const reportPath = path.join(outDir, "report.json");
	assert.ok(fs.existsSync(reportPath));
	const report = JSON.parse(fs.readFileSync(reportPath, "utf8"));
	assert.equal(report.findings_count, 0);
	fs.rmSync(outDir, { recursive: true, force: true });
});
