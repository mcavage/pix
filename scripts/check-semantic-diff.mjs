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
//   node scripts/check-semantic-diff.mjs [--root DIR] [--base REF] [--json] [--no-git] [--activate KEY]...
//
// Exit 0 = every pin holds (directly or via a documented intended-change
// manifest waiver), no rule drifted vs. the base without a matching
// intended-change entry, and no manifest entry is stale (unused by both
// mechanisms). Exit 1 = a real violation. This script is deliberately NOT a
// standalone scripts/gate.sh segment — tests/check-semantic-diff.test.mjs
// runs it directly, and that test file is itself part of the timed gate's
// `node --test tests/*.test.mjs` step.
//
// The rule-drift base defaults (with no --base flag) to resolveDefaultBase()
// in scripts/semantic-diff/lib/engine.mjs: the CI-provided
// SEMANTIC_DIFF_BASE_SHA (the PR's actual base sha, or the pre-push sha on a
// direct push — see .github/workflows/test.yml's `gate` job), else
// merge-base(HEAD, origin/main), else HEAD~1, else a literal "HEAD" only for
// a brand-new single-commit repo — NEVER a bare "HEAD" when anything more
// meaningful is available, because comparing the current rules against HEAD
// compares them against themselves in exactly the case that matters (a
// clean, already-committed checkout, which is every CI run).
//
// A pin may be STAGED (carries `activation: "<key>"`, see
// scripts/semantic-diff/rules/lifecycle.rules.mjs) for a contract that
// describes future behavior — it is skipped (reported PEND, never fails)
// unless its key is listed in scripts/semantic-diff/activation.json or
// passed via --activate. --activate lets a developer preview what a staged
// pin would check without editing activation.json.

import path from "node:path";
import { fileURLToPath } from "node:url";
import { activationKeySet, checkRuleDrift, entriesExplainingDrift, evaluatePins, loadActivation, loadManifest, loadRules, resolveDefaultBase, staleManifestEntries } from "./semantic-diff/lib/engine.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(__dirname, "..");
const RULES_DIR = path.join(__dirname, "semantic-diff", "rules");
const MANIFEST_PATH = path.join(__dirname, "semantic-diff", "intended-changes.json");
const ACTIVATION_PATH = path.join(__dirname, "semantic-diff", "activation.json");

function parseArgs(argv) {
	const opts = { root: REPO_ROOT, base: null, json: false, git: true, activate: [] };
	for (let i = 0; i < argv.length; i++) {
		const a = argv[i];
		if (a === "--root") opts.root = path.resolve(argv[++i]);
		else if (a === "--base") opts.base = argv[++i];
		else if (a === "--json") opts.json = true;
		else if (a === "--no-git") opts.git = false;
		else if (a === "--activate") opts.activate.push(argv[++i]);
		else if (a === "--help" || a === "-h") opts.help = true;
		else throw new Error(`unknown argument: ${a}`);
	}
	return opts;
}

function printReport(report, drift, stale, opts) {
	if (opts.json) {
		console.log(JSON.stringify({ ...report, drift, staleManifestEntries: stale }, null, 2));
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
			const glyph = pin.pending ? "PEND" : pin.ok ? "PASS" : "FAIL";
			console.log(`  ${glyph}  ${pin.id}${pin.pending ? ` (activation: ${pin.activation})` : ""}`);
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

	const usedForDrift = entriesExplainingDrift(report, drift, stale);
	if (usedForDrift.length) {
		console.log(`note: intended-change manifest entries not needed as an evaluatePins waiver, but explaining real rule-drift this run (fine, not failing): ${usedForDrift.join(", ")}`);
	}
	if (stale.length) {
		console.log(`FAIL: stale intended-change manifest entries — no matching mismatch AND no matching rule-drift vs. base (delete them): ${stale.join(", ")}`);
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
	console.log(report.ok && drift.ok && stale.length === 0 ? "semantic-diff: PASS" : "semantic-diff: FAIL");
}

async function main() {
	const opts = parseArgs(process.argv.slice(2));
	if (opts.help) {
		console.log("Usage: node scripts/check-semantic-diff.mjs [--root DIR] [--base REF] [--json] [--no-git] [--activate KEY]...");
		return;
	}

	const pins = await loadRules(RULES_DIR);
	const manifest = loadManifest(MANIFEST_PATH);
	const activation = loadActivation(ACTIVATION_PATH);
	const activeKeys = activationKeySet(activation);
	for (const key of opts.activate) activeKeys.add(key);
	const report = evaluatePins(pins, opts.root, manifest, activeKeys);
	const baseRef = opts.base ?? (opts.git ? resolveDefaultBase(opts.root, RULES_DIR) : "HEAD");
	const drift = opts.git ? await checkRuleDrift(RULES_DIR, opts.root, baseRef, manifest) : { ok: true, skipped: true, drifted: [], consumedManifestIds: [] };
	const stale = staleManifestEntries(report, drift);

	printReport(report, drift, stale, opts);
	process.exitCode = report.ok && drift.ok && stale.length === 0 ? 0 : 1;
}

main().catch((err) => {
	console.error(`semantic-diff guard crashed: ${err.stack || err.message}`);
	process.exitCode = 1;
});
