// U00c (W0): the semantic-diff guard engine.
//
// WHY THIS EXISTS: a compiler and a test suite can agree on the same
// mechanically introduced bug. The classic failure is a bulk codemod (rename,
// package extraction, generated edit) that changes a real contract — a
// JSON-RPC method name, a reserved port, a subprocess argv shape, a file
// permission mode — AND, in the SAME commit, updates every place that asserted
// the old value so they now assert the new one. Every test that compares
// production code against another file IN the same working tree still passes,
// because both sides moved together ("lockstep corruption"). See
// skills/architecture-audit/SKILL.md Phase 3.
//
// THE FIX: pin the expected value as a THIRD, independently-reviewed witness —
// a literal baked into scripts/semantic-diff/rules/*.rules.mjs — that ordinary
// application refactors have no reason to touch. A pin can also cross-check
// TWO application files against that same fixed literal (e.g. a server's
// method map and a client's call sites): if a codemod renames the method
// everywhere consistently, both witnesses now disagree with the fixed pin and
// the guard fails on both.
//
// THE SECOND LAYER: the pins themselves must not be casually or automatically
// edited either — that would just move the lockstep-corruption problem one
// level up. Any change to a pin's expected value is required to come with a
// matching, rationale-bearing entry in intended-changes.json (the "explicit
// intended-change manifest"). checkRuleDrift() compares the rules files'
// checkable content against a git base revision and fails if a pin moved
// without a manifest entry describing exactly that transition.
//
// This module is pure/testable: no process.exit, no console output. The CLI
// (../../check-semantic-diff.mjs) is the only place that prints and exits.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

/** @typedef {{file: string, kind: "contains"|"notContains"|"set"|"equals", region?: {start: string, end?: string}, pattern?: string, patterns?: string[], values?: string[], expected?: string|string[]}} Check */
/** @typedef {{id: string, domain?: string, description?: string, checks: Check[], activation?: string}} Pin */

// --- region + regex extraction -----------------------------------------------

/**
 * Slices `content` down to the region between two literal (non-regex) markers.
 * `end` omitted means "to end of file". Throws when a marker is not found —
 * a missing anchor means the pinned code moved and the pin itself needs
 * attention, which is exactly the kind of drift this framework exists to
 * surface loudly rather than silently skip.
 */
export function extractRegion(content, start, end) {
	const startIdx = content.indexOf(start);
	if (startIdx === -1) {
		throw new Error(`anchor not found: ${JSON.stringify(start)}`);
	}
	const from = startIdx + start.length;
	if (!end) return content.slice(from);
	const endIdx = content.indexOf(end, from);
	if (endIdx === -1) {
		throw new Error(`end anchor not found: ${JSON.stringify(end)}`);
	}
	return content.slice(from, endIdx);
}

function toGlobalRegex(pattern) {
	// Accept either a RegExp source string ("literal\\("") or a RegExp instance;
	// always re-flag with "g" so callers get every match, not just the first.
	const src = pattern instanceof RegExp ? pattern.source : pattern;
	return new RegExp(src, "g");
}

/** Collects every capture-group-1 match of `pattern` in `content` into a sorted, de-duplicated array. */
export function extractSet(content, pattern) {
	const re = toGlobalRegex(pattern);
	const found = new Set();
	let m;
	while ((m = re.exec(content))) {
		found.add(m[1] ?? m[0]);
		if (m[0].length === 0) re.lastIndex++; // guard a zero-width pattern from spinning forever
	}
	return [...found].sort();
}

/** First capture-group-1 match of `pattern` in `content`, or null. */
export function extractOne(content, pattern) {
	const re = toGlobalRegex(pattern);
	const m = re.exec(content);
	return m ? (m[1] ?? m[0]) : null;
}

function sameSet(a, b) {
	const as = [...new Set(a)].sort();
	const bs = [...new Set(b)].sort();
	return as.length === bs.length && as.every((v, i) => v === bs[i]);
}

// --- single-check evaluation --------------------------------------------------

/**
 * Evaluates one Check against file content already read from disk.
 * Returns { ok, actual, expected, reason? }. Never throws for an ordinary
 * mismatch; throws only when a region anchor is missing (broken pin).
 */
export function evaluateCheck(check, content) {
	const scoped = check.region ? extractRegion(content, check.region.start, check.region.end) : content;

	switch (check.kind) {
		case "contains": {
			const missing = (check.values ?? []).filter((v) => !scoped.includes(v));
			return missing.length === 0
				? { ok: true, actual: check.values, expected: check.values }
				: { ok: false, actual: `missing: ${JSON.stringify(missing)}`, expected: check.values };
		}
		case "notContains": {
			const present = (check.values ?? []).filter((v) => scoped.includes(v));
			return present.length === 0
				? { ok: true, actual: [], expected: [] }
				: { ok: false, actual: `forbidden text present: ${JSON.stringify(present)}`, expected: `none of: ${JSON.stringify(check.values)}` };
		}
		case "set": {
			const patterns = check.patterns ?? (check.pattern ? [check.pattern] : []);
			const found = new Set();
			for (const p of patterns) for (const v of extractSet(scoped, p)) found.add(v);
			const actual = [...found].sort();
			const expected = [...(check.expected ?? [])].sort();
			return sameSet(actual, expected) ? { ok: true, actual, expected } : { ok: false, actual, expected };
		}
		case "equals": {
			const actual = extractOne(scoped, check.pattern);
			return actual === check.expected
				? { ok: true, actual, expected: check.expected }
				: { ok: false, actual, expected: check.expected };
		}
		default:
			throw new Error(`unknown check kind: ${check.kind}`);
	}
}

// --- manifest -----------------------------------------------------------------

/**
 * Loads the intended-change manifest. Missing file = empty manifest (W0
 * ships with none). Malformed JSON is a hard error: a broken manifest must
 * not silently stop waiving anything.
 */
export function loadManifest(manifestPath) {
	if (!fs.existsSync(manifestPath)) return [];
	const raw = fs.readFileSync(manifestPath, "utf8");
	const parsed = JSON.parse(raw);
	if (!Array.isArray(parsed)) throw new Error(`intended-change manifest must be a JSON array: ${manifestPath}`);
	for (const entry of parsed) {
		if (!entry.id || typeof entry.id !== "string") throw new Error(`manifest entry missing string "id": ${JSON.stringify(entry)}`);
		if (!entry.rationale || !String(entry.rationale).trim()) throw new Error(`manifest entry ${entry.id} missing non-empty "rationale"`);
		if (!entry.evidence || !String(entry.evidence).trim()) throw new Error(`manifest entry ${entry.id} missing non-empty "evidence"`);
		if (!Array.isArray(entry.changes) || entry.changes.length === 0) throw new Error(`manifest entry ${entry.id} missing non-empty "changes"`);
	}
	return parsed;
}

// --- activation (Story04 staged-pin schema) -----------------------------------
//
// A pin may carry `activation: "<key>"` (see rules/lifecycle.rules.mjs) to mark
// it as STAGED: it describes a contract for behavior that does not exist in
// production yet (a future story), so evaluating it for real today would
// permanently redden the gate for work nobody has landed. A staged pin is
// skipped entirely (no file I/O, no fs.existsSync, not counted toward
// report.ok, never consumes a manifest waiver) UNLESS its `activation` key is
// present in the loaded activation set — same shape and same discipline as
// the intended-change manifest: an explicit, rationale-bearing entry that
// must be added in the SAME commit that wires the real behavior, never
// silently. It is reported in the pin list with `pending: true` so its
// existence stays legible in `--json`/CLI output instead of vanishing.

/**
 * Loads the activation manifest: which staged-pin `activation` keys are
 * turned ON. Missing file = empty (nothing activated — the W0/interim
 * default). Malformed JSON, a non-array, or an entry missing a non-empty
 * `key`/`rationale`/`evidence` is a hard error, mirroring loadManifest: a
 * broken activation file must not silently fail to activate (or
 * silently fail to enforce) anything.
 */
export function loadActivation(activationPath) {
	if (!fs.existsSync(activationPath)) return [];
	const raw = fs.readFileSync(activationPath, "utf8");
	const parsed = JSON.parse(raw);
	if (!Array.isArray(parsed)) throw new Error(`activation manifest must be a JSON array: ${activationPath}`);
	for (const entry of parsed) {
		if (!entry.key || typeof entry.key !== "string") throw new Error(`activation entry missing string "key": ${JSON.stringify(entry)}`);
		if (!entry.rationale || !String(entry.rationale).trim()) throw new Error(`activation entry ${entry.key} missing non-empty "rationale"`);
		if (!entry.evidence || !String(entry.evidence).trim()) throw new Error(`activation entry ${entry.key} missing non-empty "evidence"`);
	}
	return parsed;
}

/** Turns a loaded activation manifest (array of {key,...}) into the Set evaluatePins expects. */
export function activationKeySet(activation) {
	return new Set(activation.map((e) => e.key));
}

// Builds a check identical to `check` except its expected-value field(s) are
// swapped for the manifest's declared `to`, so we can ask "does the CURRENT
// file content actually satisfy THIS declared destination value" — the same
// evaluateCheck() the rest of the engine uses, so there is exactly one
// definition of what each check kind means.
function checkWithExpected(check, to) {
	switch (check.kind) {
		case "contains":
		case "notContains":
			return { ...check, values: Array.isArray(to) ? to : [to] };
		case "set":
			return { ...check, expected: Array.isArray(to) ? to : [to] };
		case "equals":
			return { ...check, expected: to };
		default:
			return null;
	}
}

/**
 * Finds a manifest entry for `pinId` whose `changes[]` declares this exact
 * check's file (and kind, when the entry specifies one) with a `to` value
 * that the CURRENT file content actually satisfies right now. This is kind-
 * generic (works for contains/notContains/set/equals alike) because it
 * re-runs the real evaluateCheck() against the manifest's claimed
 * destination rather than string-comparing a diagnostic message.
 */
function findWaiver(manifest, pinId, check, content) {
	for (const entry of manifest) {
		if (entry.id !== pinId) continue;
		for (const change of entry.changes) {
			if (change.file !== check.file) continue;
			if (change.kind && change.kind !== check.kind) continue;
			const toCheck = checkWithExpected(check, change.to);
			if (!toCheck) continue;
			try {
				if (evaluateCheck(toCheck, content).ok) return entry;
			} catch {
				// a broken region anchor on the waiver path is not this function's job
				// to report; the primary evaluateCheck() call already surfaced it.
			}
		}
	}
	return null;
}

// --- rule loading --------------------------------------------------------------

export async function loadRules(rulesDir) {
	const files = fs
		.readdirSync(rulesDir)
		.filter((f) => f.endsWith(".rules.mjs"))
		.sort();
	/** @type {Pin[]} */
	const pins = [];
	for (const f of files) {
		const mod = await import(pathToFileURL(path.join(rulesDir, f)).href);
		const domainPins = mod.default ?? [];
		for (const pin of domainPins) {
			pins.push({ domain: f.replace(/\.rules\.mjs$/, ""), ...pin });
		}
	}
	return pins;
}

// --- full run --------------------------------------------------------------

/**
 * Runs every pin against `root`, applying manifest waivers. A pin carrying
 * `activation: "<key>"` whose key is not present in `activeKeys` is STAGED:
 * skipped entirely (no fs access, ok:true, pending:true) rather than
 * evaluated — see the "activation (Story04 staged-pin schema)" section above
 * loadActivation. Returns a report:
 * { ok, pins: [{id, domain, ok, pending?, checks: [{file, kind, ok, waived, actual, expected, entry?}]}], unusedManifestEntries }
 */
export function evaluatePins(pins, root, manifest = [], activeKeys = new Set()) {
	const usedManifestIds = new Set();
	const results = pins.map((pin) => {
		if (pin.activation && !activeKeys.has(pin.activation)) {
			return { id: pin.id, domain: pin.domain, description: pin.description, ok: true, pending: true, activation: pin.activation, checks: [] };
		}
		const checks = pin.checks.map((check) => {
			const abs = path.join(root, check.file);
			if (!fs.existsSync(abs)) {
				return { file: check.file, kind: check.kind, ok: false, actual: "FILE MISSING", expected: check.expected ?? check.values };
			}
			const content = fs.readFileSync(abs, "utf8");
			let evaluated;
			try {
				evaluated = evaluateCheck(check, content);
			} catch (err) {
				return { file: check.file, kind: check.kind, ok: false, actual: `ANCHOR ERROR: ${err.message}`, expected: check.expected ?? check.values };
			}
			if (evaluated.ok) return { file: check.file, kind: check.kind, ...evaluated, waived: false };

			const waiver = findWaiver(manifest, pin.id, check, content);
			if (waiver) {
				usedManifestIds.add(waiver.id);
				return { file: check.file, kind: check.kind, ...evaluated, ok: true, waived: true, waiver: { rationale: waiver.rationale, evidence: waiver.evidence } };
			}
			return { file: check.file, kind: check.kind, ...evaluated, waived: false };
		});
		return { id: pin.id, domain: pin.domain, description: pin.description, ok: checks.every((c) => c.ok), checks };
	});

	const unusedManifestEntries = manifest.filter((e) => !usedManifestIds.has(e.id)).map((e) => e.id);
	return { ok: results.every((r) => r.ok), pins: results, unusedManifestEntries };
}

// --- rule-drift-vs-git guard --------------------------------------------------

function gitShow(root, ref, relFile) {
	try {
		// stdio: pipe suppresses git's own "fatal: path ... not in 'HEAD'" noise on
		// stderr for the (extremely common) case of a brand-new rules file — that
		// is an expected, silently-handled outcome here, not a diagnostic worth
		// surfacing.
		return execFileSync("git", ["show", `${ref}:${relFile}`], { cwd: root, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
	} catch {
		return null; // not a git repo, no such ref, or file didn't exist at that ref (new pin file)
	}
}

/** The parts of a pin that matter for drift detection — id + the checkable content, never free-text description. */
function pinFingerprint(pin) {
	return JSON.stringify({
		activation: pin.activation ?? null,
		checks: pin.checks.map((c) => ({ file: c.file, kind: c.kind, region: c.region ?? null, pattern: c.pattern ?? null, patterns: c.patterns ?? null, values: c.values ?? null, expected: c.expected ?? null })),
	});
}

/**
 * Compares the CURRENT rules directory against its content at git ref `base`.
 * Any pin whose checkable fingerprint changed must have a matching
 * intended-change manifest entry (by id) — otherwise the pin itself may have
 * been corrupted in lockstep with the code it is meant to police.
 *
 * Returns { ok, skipped, drifted: [{id, reason}] }. skipped=true (ok=true)
 * when there is no usable git base (not a repo, or the ref/file is new) —
 * this layer is defense-in-depth, not a hard requirement for a fresh checkout
 * or a repo with no history for these files yet.
 */
export async function checkRuleDrift(rulesDir, root, base, manifest) {
	const manifestIds = new Set(manifest.map((e) => e.id));
	const relRulesDir = path.relative(root, rulesDir);
	const files = fs
		.readdirSync(rulesDir)
		.filter((f) => f.endsWith(".rules.mjs"))
		.sort();

	let anyBase = false;
	const drifted = [];
	for (const f of files) {
		const relFile = path.join(relRulesDir, f);
		const oldSrc = gitShow(root, base, relFile);
		if (oldSrc === null) continue; // no base version to compare against yet
		anyBase = true;

		const tmp = path.join(os.tmpdir(), `semdiff-base-${process.pid}-${Math.random().toString(36).slice(2)}.mjs`);
		fs.writeFileSync(tmp, oldSrc);
		let oldPins;
		try {
			const mod = await import(pathToFileURL(tmp).href);
			oldPins = mod.default ?? [];
		} finally {
			fs.rmSync(tmp, { force: true });
		}

		const newSrc = fs.readFileSync(path.join(root, relFile), "utf8");
		const newTmp = path.join(os.tmpdir(), `semdiff-cur-${process.pid}-${Math.random().toString(36).slice(2)}.mjs`);
		fs.writeFileSync(newTmp, newSrc);
		let newPins;
		try {
			const mod = await import(pathToFileURL(newTmp).href);
			newPins = mod.default ?? [];
		} finally {
			fs.rmSync(newTmp, { force: true });
		}

		const oldById = new Map(oldPins.map((p) => [p.id, p]));
		for (const pin of newPins) {
			const before = oldById.get(pin.id);
			if (!before) continue; // brand-new pin: nothing to have drifted from
			if (pinFingerprint(before) === pinFingerprint(pin)) continue; // unchanged
			if (manifestIds.has(pin.id)) continue; // documented, explicit change
			drifted.push({ id: pin.id, file: relFile, reason: "expected value changed with no matching intended-change manifest entry" });
		}
	}

	if (!anyBase) return { ok: true, skipped: true, drifted: [] };
	return { ok: drifted.length === 0, skipped: false, drifted };
}
