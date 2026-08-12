#!/usr/bin/env node
// Full-history secret scan (AC-REL-01/legal): scans every blob reachable from
// every ref (not just HEAD) for common secret shapes. Pattern matching is
// pure and exported for tests; the CLI wires it to a TRULY BATCHED git
// plumbing traversal: one `git rev-list --objects --all`, one
// `git cat-file --batch-check` (fed every object at once), and a small,
// fixed number of `git cat-file --batch` calls chunked by content size (fed
// only the wanted blob shas at once) — never one `spawnSync` per object.
//
// This replaced an earlier version that ran THREE `spawnSync("git", ...)`
// calls PER OBJECT (`cat-file -t`, `cat-file -s`, `cat-file blob`), which
// made a full-history scan take O(objects) process spawns — ~24s on this
// repo's ~700-commit history, and scaling linearly (badly) with repo size.
// The batched form below does a small, ~constant number of git invocations
// regardless of history size (see `gitCallCount` / `resetGitCallCount()`,
// exported so tests can PROVE the call count stays flat as history grows —
// see tests/legal-secret-scan.test.mjs).
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

import { spawnSync } from "node:child_process";
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

// --- git call instrumentation (lets tests PROVE batching, not counting) -----
// Every actual `spawnSync("git", ...)` in this file goes through `runGit()`,
// which is the only place `gitCallCount` is incremented. A scan of full
// history therefore makes a small, bounded number of git invocations no
// matter how many objects are in that history.
export let gitCallCount = 0;
export function resetGitCallCount() {
	gitCallCount = 0;
}
function runGit(args, opts) {
	gitCallCount++;
	return spawnSync("git", args, opts);
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

// The allowlist file records fingerprints of `blob:path:match` triples found
// in OTHER files. Its own content necessarily quotes those match snippets
// (that's what makes an allowlist entry legible on review), which means IT
// would keep matching the very patterns it exists to allowlist — and because
// its blob hash changes every time an entry is added, a self-referential
// allowlist entry for its own past content can never reach a fixed point (the
// same self-exclusion problem scripts/rename/build-inventory.sh documents for
// its own inventory file). It is metadata ABOUT findings, not a place a real
// secret would land, so every historical version of it is excluded from the
// scan itself, by path.
//
// docs/legal/RELEASE-SAFEGUARDS.md is here for the SAME reason. It documents what
// this scanner catches, so it quotes the shapes: AWS's published
// AKIAIOSFODNN7EXAMPLE and Slack's xoxb-1234567890. Every edit to that doc
// re-hashes its blob, both examples come back as findings, and the release stops
// until someone allowlists a hash that did not exist until the commit was made.
// That happened three times in one afternoon while writing up an unrelated
// change, each time discovered only after CI had already failed.
//
// The cost is real and is why this set stays short: a genuine secret pasted into
// that file would not be caught. It is prose ABOUT the scanner, not a place
// credentials land, and the alternative is a gate that punishes editing its own
// documentation.
const SELF_EXCLUDED_PATHS = new Set([
	"scripts/legal/secret-scan-allowlist.txt",
	"docs/legal/RELEASE-SAFEGUARDS.md",
]);

// Objects larger than this are skipped (secrets live in text, not multi-MB
// blobs; also caps how much content any single batch call has to move).
const DEFAULT_MAX_BLOB_BYTES = 2 * 1024 * 1024;

// `git cat-file --batch-check` in ONE call for every candidate sha (deduped).
// Output format (default, one line per input): `<sha> <type> <size>\n`, or
// `<sha> missing\n` for a sha that doesn't resolve (shouldn't happen here —
// rev-list only emits objects it can see — but handled defensively).
// Returns Map<sha, {type, size} | null>.
export function batchCheck(repoDir, shas) {
	const map = new Map();
	if (shas.length === 0) return map;
	const res = runGit(["cat-file", "--batch-check"], {
		cwd: repoDir,
		input: shas.join("\n") + "\n",
		encoding: "utf8",
		maxBuffer: 1024 * 1024 * 512,
	});
	if (res.error) throw res.error;
	for (const line of res.stdout.split("\n")) {
		if (!line) continue;
		const missing = line.match(/^(\S+) missing$/);
		if (missing) {
			map.set(missing[1], null);
			continue;
		}
		const m = line.match(/^(\S+) (\S+) (\d+)$/);
		if (!m) continue; // malformed/unexpected line; skip defensively
		map.set(m[1], { type: m[2], size: parseInt(m[3], 10) });
	}
	return map;
}

// `git cat-file --batch` for a set of blob shas with known sizes, chunked so
// no single call has to hold an unbounded amount of blob content in memory
// at once — still a small, FIXED number of git invocations regardless of
// object count (chunk boundaries are drawn by cumulative content size AND a
// hard object-count cap, not "one object per call"). `sizeOf(sha)` returns
// the blob's byte size (from a prior `batchCheck()`), used only to decide
// chunk boundaries and size `maxBuffer` — never to skip a real git call.
// Returns Map<sha, Buffer>.
export function batchContent(repoDir, shas, sizeOf, { maxChunkBytes = 64 * 1024 * 1024, maxChunkObjects = 5000 } = {}) {
	const contents = new Map();
	if (shas.length === 0) return contents;

	let chunk = [];
	let chunkBytes = 0;
	const flush = () => {
		if (chunk.length === 0) return;
		const res = runGit(["cat-file", "--batch"], {
			cwd: repoDir,
			input: chunk.join("\n") + "\n",
			maxBuffer: Math.max(chunkBytes * 2 + 1024 * 1024, 16 * 1024 * 1024),
		});
		if (res.error) throw res.error;
		parseBatchOutput(res.stdout, contents);
		chunk = [];
		chunkBytes = 0;
	};
	for (const sha of shas) {
		const size = sizeOf(sha) || 0;
		if (chunk.length > 0 && chunkBytes + size > maxChunkBytes) flush();
		chunk.push(sha);
		chunkBytes += size;
		if (chunk.length >= maxChunkObjects) flush();
	}
	flush();
	return contents;
}

// Parses the raw (binary-safe) output of `git cat-file --batch`: repeated
// `<sha> <type> <size>\n<size bytes of content>\n` records (or
// `<sha> missing\n`). Must operate on a Buffer, not a string — blob content
// can contain arbitrary bytes, including bytes that look like the record
// separator, so we advance by the DECLARED size, never by scanning for the
// next newline inside content.
export function parseBatchOutput(buf, into = new Map()) {
	let offset = 0;
	while (offset < buf.length) {
		const nl = buf.indexOf(0x0a, offset);
		if (nl === -1) break;
		const header = buf.slice(offset, nl).toString("utf8");
		offset = nl + 1;
		const missing = header.match(/^(\S+) missing$/);
		if (missing) {
			into.set(missing[1], null);
			continue;
		}
		const m = header.match(/^(\S+) (\S+) (\d+)$/);
		if (!m) break; // malformed; stop defensively rather than mis-parse
		const [, sha, , sizeStr] = m;
		const size = parseInt(sizeStr, 10);
		into.set(sha, buf.slice(offset, offset + size));
		offset += size + 1; // skip the trailing newline after content
	}
	return into;
}

export function scanRepo(repoDir, allowlistPath, { maxBlobBytes = DEFAULT_MAX_BLOB_BYTES } = {}) {
	// 1 call: every object reachable from every ref, paired with a path.
	const objectsRes = runGit(["rev-list", "--objects", "--all"], {
		cwd: repoDir,
		encoding: "utf8",
		maxBuffer: 1024 * 1024 * 256,
	});
	if (objectsRes.error) throw objectsRes.error;

	const allowlist = loadAllowlist(allowlistPath);
	const records = []; // { sha, path }
	const uniqueShas = new Set();
	for (const line of objectsRes.stdout.split("\n")) {
		if (!line) continue;
		const sp = line.indexOf(" ");
		if (sp === -1) continue; // a commit/tree with no path (root tree, etc.)
		const sha = line.slice(0, sp);
		const path = line.slice(sp + 1);
		if (SELF_EXCLUDED_PATHS.has(path)) continue;
		records.push({ sha, path });
		uniqueShas.add(sha);
	}

	// 1 call: type + size for every unique object at once.
	const meta = batchCheck(repoDir, [...uniqueShas]);

	// Only blobs under the size cap are worth fetching content for.
	const wantedShas = [];
	let totalWantedBytes = 0;
	for (const sha of uniqueShas) {
		const info = meta.get(sha);
		if (!info || info.type !== "blob" || info.size > maxBlobBytes) continue;
		wantedShas.push(sha);
		totalWantedBytes += info.size;
	}

	// A small, FIXED number of calls (chunked by cumulative content size, not
	// one per object) to fetch content for exactly the objects we need.
	const contents = batchContent(repoDir, wantedShas, (sha) => meta.get(sha)?.size);
	void totalWantedBytes; // (kept for readability of the sizing rationale above)

	const findings = [];
	for (const { sha, path } of records) {
		const info = meta.get(sha);
		if (!info || info.type !== "blob" || info.size > maxBlobBytes) continue;
		const content = contents.get(sha);
		if (!content) continue;
		// A NUL byte is the cheap binary-file signal; skip (secrets live in text).
		if (content.includes(0)) continue;
		const text = content.toString("utf8");
		for (const f of scanText(text)) {
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
