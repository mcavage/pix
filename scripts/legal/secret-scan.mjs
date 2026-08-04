#!/usr/bin/env node
// Full-history secret scan (AC-REL-01/legal): scans every blob reachable from
// every ref (not just HEAD) for common secret shapes. Pattern matching is
// pure and exported for tests; the CLI wires it to `git rev-list --objects
// --all` + `git cat-file --batch` so nothing is skipped because it was
// rewritten out of the current tree.
//
// Usage:
//   node secret-scan.mjs --self-test          # pattern-matching unit checks
//   node secret-scan.mjs --scan <repo-dir> --out <report.json>
//     Scans the FULL history of <repo-dir> and writes a JSON report. Exits
//     non-zero (fail-closed) if ANY finding is not in the allowlist.
//   node secret-scan.mjs --scan <repo-dir> --out <report.json> --allowlist <file>
//     A newline-delimited allowlist of `sha256(commit:path:snippet)` hashes
//     for confirmed false positives (test fixtures, docs examples) — see
//     scripts/legal/secret-scan-allowlist.txt. Empty by default: nothing is
//     pre-allowlisted, so a genuinely clean history stays provably clean.

import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

// Each rule: { id, label, regex }. Kept narrow and high-signal on purpose —
// broad "looks like base64" rules drown a fail-closed gate in noise until
// someone disables it. Extend this list as new provider token shapes appear.
export const RULES = [
	{ id: "aws-access-key-id", label: "AWS access key ID", regex: /\bAKIA[0-9A-Z]{16}\b/g },
	{ id: "aws-secret-key", label: "AWS secret access key (assignment)", regex: /aws_secret_access_key\s*=\s*['"]?[A-Za-z0-9\/+=]{40}['"]?/gi },
	{ id: "github-pat", label: "GitHub token", regex: /\bgh[pousr]_[A-Za-z0-9]{36,255}\b/g },
	{ id: "slack-token", label: "Slack token", regex: /\bxox[baprs]-[A-Za-z0-9-]{10,72}\b/g },
	{ id: "google-api-key", label: "Google API key", regex: /\bAIza[0-9A-Za-z\-_]{35}\b/g },
	{ id: "private-key-block", label: "PEM private key block", regex: /-----BEGIN (RSA |EC |OPENSSH |DSA |)PRIVATE KEY-----/g },
	{ id: "generic-bearer", label: "hardcoded Bearer token", regex: /Bearer\s+[A-Za-z0-9_\-.]{20,}/g },
	{ id: "op-service-account-token", label: "1Password service account token", regex: /\bops_eyJ[A-Za-z0-9_\-.]{20,}/g },
	{
		id: "url-embedded-basic-auth",
		label: "credentials embedded in a URL",
		regex: /https?:\/\/[^\s\/:@]+:[^\s\/:@]+@[^\s\/]+/g,
	},
];

export function scanText(text) {
	const findings = [];
	for (const rule of RULES) {
		const re = new RegExp(rule.regex.source, rule.regex.flags);
		let m;
		while ((m = re.exec(text))) {
			findings.push({ ruleId: rule.id, label: rule.label, match: m[0] });
			if (!rule.regex.global) break;
		}
	}
	return findings;
}

export function fingerprint(ref, path, match) {
	return createHash("sha256").update(`${ref}:${path}:${match}`).digest("hex");
}

function selfTest() {
	const cases = [
		["AKIAABCDEFGHIJKLMNOP", "aws-access-key-id"],
		["aws_secret_access_key = 'wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY'", "aws-secret-key"],
		["ghp_" + "a".repeat(36), "github-pat"],
		["xoxb-1234567890-abcdefghijklmnop", "slack-token"],
		["AIza" + "a".repeat(35), "google-api-key"],
		["-----BEGIN RSA PRIVATE KEY-----", "private-key-block"],
		["Authorization: Bearer abcdefghijklmnopqrstu", "generic-bearer"],
		["ops_eyJhbGciOiJIUzI1NiJ9abcdefghijklmno", "op-service-account-token"],
		["https://user:hunter2@example.com/path", "url-embedded-basic-auth"],
	];
	let failed = 0;
	for (const [text, expectedId] of cases) {
		const findings = scanText(text);
		if (!findings.some((f) => f.ruleId === expectedId)) {
			console.error(`self-test FAILED: expected rule "${expectedId}" to match: ${text}`);
			failed++;
		}
	}
	const clean = scanText("just some ordinary source code, nothing to see here");
	if (clean.length !== 0) {
		console.error("self-test FAILED: ordinary text produced a false positive", clean);
		failed++;
	}
	if (failed) {
		console.error(`secret-scan self-test: ${failed} failure(s)`);
		process.exit(1);
	}
	console.log(`secret-scan self-test: ${cases.length} rule(s) OK, no false positive on clean text`);
}

function loadAllowlist(path) {
	if (!path || !existsSync(path)) return new Set();
	// One fingerprint per line, optionally followed by whitespace + a
	// `# reviewed: ...` comment explaining WHY it's a false positive (not a
	// blanket bypass — a different match value in the same file gets a
	// different fingerprint and is NOT allowlisted).
	return new Set(
		readFileSync(path, "utf8")
			.split("\n")
			.map((l) => l.trim())
			.filter((l) => l && !l.startsWith("#"))
			.map((l) => l.split(/\s+/)[0])
	);
}

// Scans full history: every blob reachable from every ref, with the
// commit+path that introduced it (best-effort attribution via
// `git log --all --diff-filter=A --name-only` per blob is too slow at this
// scale, so we attribute by walking `git rev-list --objects --all` output,
// which already pairs each blob with the path it was recorded at in at
// least one tree).
function scanRepo(repoDir, allowlistPath) {
	const objects = execFileSync(
		"git",
		["rev-list", "--objects", "--all"],
		{ cwd: repoDir, encoding: "utf8", maxBuffer: 1024 * 1024 * 256 }
	);
	const allowlist = loadAllowlist(allowlistPath);
	const findings = [];
	const lines = objects.split("\n").filter(Boolean);

	for (const line of lines) {
		const sp = line.indexOf(" ");
		if (sp === -1) continue; // a commit/tree with no path (root tree, etc.)
		const sha = line.slice(0, sp);
		const path = line.slice(sp + 1);

		const catFile = spawnSync("git", ["cat-file", "-t", sha], { cwd: repoDir, encoding: "utf8" });
		if (catFile.stdout.trim() !== "blob") continue;

		const sizeRes = spawnSync("git", ["cat-file", "-s", sha], { cwd: repoDir, encoding: "utf8" });
		const size = parseInt(sizeRes.stdout.trim(), 10);
		if (!Number.isFinite(size) || size > 2 * 1024 * 1024) continue; // skip huge/binary blobs

		const content = spawnSync("git", ["cat-file", "blob", sha], {
			cwd: repoDir,
			encoding: "utf8",
			maxBuffer: 1024 * 1024 * 4,
		});
		if (content.status !== 0 || content.stdout == null) continue;
		// A NUL byte is the cheap binary-file signal; skip (secrets live in text).
		if (content.stdout.includes("\u0000")) continue;

		for (const f of scanText(content.stdout)) {
			const fp = fingerprint(sha, path, f.match);
			if (allowlist.has(fp)) continue;
			findings.push({ blob: sha, path, ...f, fingerprint: fp });
		}
	}
	return findings;
}

function main() {
	const args = process.argv.slice(2);
	if (args.includes("--self-test")) {
		selfTest();
		return;
	}
	const scanIdx = args.indexOf("--scan");
	if (scanIdx === -1) {
		console.error("usage: secret-scan.mjs --self-test | --scan <repo-dir> --out <report.json> [--allowlist <file>]");
		process.exit(2);
	}
	const repoDir = args[scanIdx + 1];
	const outIdx = args.indexOf("--out");
	const outPath = outIdx !== -1 ? args[outIdx + 1] : null;
	const allowlistIdx = args.indexOf("--allowlist");
	const allowlistPath = allowlistIdx !== -1 ? args[allowlistIdx + 1] : null;

	const findings = scanRepo(repoDir, allowlistPath);
	const report = {
		schema: 1,
		scanned_at: new Date().toISOString(),
		findings_count: findings.length,
		findings,
	};
	if (outPath) writeFileSync(outPath, JSON.stringify(report, null, 2));

	if (findings.length > 0) {
		console.error(`secret-scan: FAIL-CLOSED — ${findings.length} finding(s) across full history:`);
		for (const f of findings.slice(0, 20)) {
			console.error(`  - ${f.label} in ${f.path} (blob ${f.blob.slice(0, 12)})`);
		}
		process.exit(1);
	}
	console.log(`secret-scan: no secrets found across full history (${outPath || "no report written"})`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	main();
}
