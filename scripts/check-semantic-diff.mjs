#!/usr/bin/env node
// U00c (W0): semantic-diff guard CLI.
//
// Pins real, currently-true contracts as fixed literals independent of the
// production code they check, so a codemod that moves a contract AND
// whatever else asserted it (lockstep corruption) still gets caught. See
// scripts/semantic-diff/README.md for the full design and pin schema, and
// skills/architecture-audit/SKILL.md's "Phase 3: semantic-diff pass" for the
// motivating idea.
//
// Usage:
//   node scripts/check-semantic-diff.mjs [--root DIR] [--base REF] [--json] [--no-git]
//
// Exit 0 = every pin holds (directly or via a documented intended-change
// manifest waiver) and no rule drifted without one. Exit 1 = a real
// violation. This script is deliberately NOT wired into scripts/gate.sh at
// W0 — see tests/check-semantic-diff.test.mjs, which runs it directly.

import path from "node:path";
import { fileURLToPath } from "node:url";
import { checkRuleDrift, evaluatePins, loadManifest, loadRules } from "./semantic-diff/lib/engine.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(__dirname, "..");
const RULES_DIR = path.join(__dirname, "semantic-diff", "rules");
const MANIFEST_PATH = path.join(__dirname, "semantic-diff", "intended-changes.json");

function parseArgs(argv) {
	const opts = { root: REPO_ROOT, base: "HEAD", json: false, git: true };
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (a === "--root") opts.root = path.resolve(argv[++i]);
		else if (a === "--base") opts.base = argv[++i];
		else if (a === "--json") opts.json = true;
		else if (a === "--no-git") opts.git = false;
		else if (a === "--help" || a === "-h") opts.help = true;
		else throw new Error(`unknown argument: ${a}`);
	}
	return opts;
}

function printReport(report, drift, opts) {
	if (opts.json) {
		console.log(JSON.stringify({ ...report, drift }, null, 2));
		return;
	}

	console.log("semantic-diff guard (U00c W0)");
	console.log("");
	const byDomain = new Map();
	for (const pin of report.pins) {
		if (!byDomain.has(pin.domain)) byDomain.set(pin.domain, []);
		byDomain.get(pin.domain).push(pin);
	}
	for (const [domain, pins] of [...byDomain.entries()].sort()) {
		console.log(`[${domain}]`);
		for (const pin of pins) {
			const glyph = pin.ok ? "PASS" : "FAIL";
			console.log(`  ${glyph}  ${pin.id}`);
			for (const check of pin.checks) {
				if (check.ok && !check.waived) continue; // quiet on the ordinary passing case
				if (check.waived) {
					console.log(`        waived (${check.file}): ${check.waiver.rationale} [${check.waiver.evidence}]`);
				} else {
					console.log(`        ${check.file} (${check.kind}): expected ${JSON.stringify(check.expected)}, got ${JSON.stringify(check.actual)}`);
				}
			}
		}
	}
	console.log("");

	if (report.unusedManifestEntries.length) {
		console.log(`note: unused intended-change manifest entries (no matching mismatch found): ${report.unusedManifestEntries.join(", ")}`);
	}

	if (drift.skipped) {
		console.log("rule-drift-vs-git: skipped (no usable git base — fresh checkout or no prior committed rules)");
	} else if (drift.ok) {
		console.log("rule-drift-vs-git: OK (no pin changed without a matching intended-change manifest entry)");
	} else {
		console.log("rule-drift-vs-git: FAIL");
		for (const d of drift.drifted) {
			console.log(`  ${d.id} (${d.file}): ${d.reason}`);
		}
	}
	console.log("");
	console.log(report.ok && drift.ok ? "semantic-diff: PASS" : "semantic-diff: FAIL");
}

async function main() {
	const opts = parseArgs(process.argv.slice(2));
	if (opts.help) {
		console.log("Usage: node scripts/check-semantic-diff.mjs [--root DIR] [--base REF] [--json] [--no-git]");
		return;
	}

	const pins = await loadRules(RULES_DIR);
	const manifest = loadManifest(MANIFEST_PATH);
	const report = evaluatePins(pins, opts.root, manifest);
	const drift = opts.git ? await checkRuleDrift(RULES_DIR, opts.root, opts.base, manifest) : { ok: true, skipped: true, drifted: [] };

	printReport(report, drift, opts);
	process.exitCode = report.ok && drift.ok ? 0 : 1;
}

main().catch((err) => {
	console.error(`semantic-diff guard crashed: ${err.stack || err.message}`);
	process.exitCode = 1;
});
