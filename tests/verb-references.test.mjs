// A `pix <verb>` written in prose must be a verb that EXISTS.
//
// This is the guard for the class of bug the audit sweep kept finding, and the
// one that survives four rounds of deletion: `pix models route` was deleted,
// and three surfaces went on telling people to run it — printed output, help
// text, and (worst) an error the AGENT reads, which cannot go check. `pix mcp
// bundle`/`register`/`load` outlived their verbs the same way, in
// capabilities.json, which skills read to learn how to wire a provider.
//
// The existing guards could not catch any of it. health_fix_live_test.go covers
// `*Fix` CONSTANTS, and none of these were constants. The corpus covers verbs
// that exist, not references to verbs that don't. So this scans the surfaces
// where the bug actually landed and resolves every `pix <word>` against the
// kong tree, which root.go's own comment calls "the single source of truth".
//
// SCOPE, deliberately narrow: agent-facing and user-facing text where a wrong
// verb sends someone to a dead end. Design docs and CHANGELOG are HISTORY and
// are excluded — "`pix mcp load` was removed" is a true sentence that must not
// fail a gate.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const read = (p) => fs.readFileSync(path.join(repoRoot, p), "utf8");

/**
 * The kong verb TREE, as `{ typeName -> { childVerb -> childTypeName } }` plus
 * the root's own children, harvested from the `cmd:""` struct tags across
 * cmd/pix.
 *
 * A tree, not a flat set, and that distinction is the whole guard: the first
 * version of this test collected every verb name into one Set and checked only
 * the FIRST word after "pix". Both bugs it was written for sailed through —
 * `pix models route` and `pix mcp load` look fine when you only ask whether
 * "models" and "mcp" exist. What is wrong with them is the SECOND word, which
 * only a path resolution can see.
 */
function verbTree() {
	const dir = path.join(repoRoot, "services/host/cmd/pix");
	const types = {};
	for (const entry of fs.readdirSync(dir)) {
		if (!entry.endsWith(".go") || entry.endsWith("_test.go")) continue;
		const src = fs.readFileSync(path.join(dir, entry), "utf8");
		for (const t of src.matchAll(/^type\s+([A-Za-z0-9_]+)\s+struct\s*\{([\s\S]*?)^\}/gm)) {
			const [, typeName, body] = t;
			for (const f of body.matchAll(/^\s*([A-Z][A-Za-z0-9]*)\s+([A-Za-z0-9_\[\]]+)\s+`([^`]*\bcmd:""[^`]*)`/gm)) {
				const [, field, fieldType, tag] = f;
				types[typeName] ??= {};
				types[typeName][field.toLowerCase()] = fieldType.replace(/^\[\]/, "");
				const alias = tag.match(/aliases:"([^"]*)"/);
				if (alias) for (const a of alias[1].split(",")) types[typeName][a.trim().toLowerCase()] = fieldType;
			}
		}
	}
	return types;
}

/**
 * Walks `words` down the tree from the root. Returns null when every word it
 * consumed resolved; returns the first word that did NOT resolve where a
 * subcommand was still possible. Trailing words that are ARGUMENTS (the node has
 * no children left) are not verbs and are never reported -- that is what keeps
 * `pix config set services memory` from being read as four verbs.
 */
function unresolvedVerb(types, words) {
	let node = "rootCmd";
	for (const word of words) {
		const children = types[node];
		if (!children || Object.keys(children).length === 0) return null; // args from here on
        if (word.startsWith("-")) return null; // a flag ends the verb path
		if (!children[word]) return word;
		node = children[word];
	}
	return null;
}

// The surfaces a wrong verb actually hurts someone from.
//
// SKILLS AND docs/ ARE IN THIS LIST FOR A REASON. They were not, and that is
// precisely where the next round of this bug landed: `pix mcp register` and
// `pix mcp load` sat in four SKILL.md files and docs/gworkspace.md for months
// after both verbs were deleted. A skill is the WORST place for a dead verb —
// the agent reads it as instruction, runs it, gets "no command named", and has
// no way to know the documentation was wrong rather than its own reasoning.
//
// Enumerated dynamically, so a new skill or doc is covered the day it is
// written rather than the day someone remembers to add it here.
const listFiles = (dir, pred) =>
	fs.existsSync(path.join(repoRoot, dir))
		? fs.readdirSync(path.join(repoRoot, dir), { withFileTypes: true }).flatMap((e) => {
				const rel = `${dir}/${e.name}`;
				if (e.isDirectory()) return listFiles(rel, pred);
				return pred(rel) ? [rel] : [];
			})
		: [];

// A doc classified HISTORICAL by its own front matter, not by a per-file
// waiver. `docs/design/**` and `docs/legal/**` are excluded by directory
// because that is what those trees ARE (design reasoning and compliance
// history); everything else that is equally a record of a not-yet-migrated
// or already-retired surface carries the marker itself, one line near the
// top: `Status: PRE-V2 ...` or `Status: HISTORICAL ...` (a design doc already
// says `Status: **ACCEPTED FOR IMPLEMENTATION**`, the same shape, so this is
// one classification mechanism, not two). A live, current-surface doc never
// carries this marker, so adding one is a deliberate, reviewable act, not a
// silent opt-out — and it covers the WHOLE file, so no second list of
// individual line waivers has to be kept in sync with which doc is which.
const isHistoricalByMarker = (rel) => {
	let head;
	try {
		head = read(rel).split("\n", 15).join("\n");
	} catch {
		return false;
	}
	return /^Status:\s*\**\s*(PRE-V2|HISTORICAL|SUPERSEDED)\b/im.test(head);
};

const SURFACES = [
	"capabilities.json",
	"README.md",
	"AGENTS.md",
	// Every skill: the agent-facing surface, where a dead verb is acted on.
	...listFiles("skills", (f) => f.endsWith(".md")),
	// Every user-facing doc. Two subtrees stay out because they are RECORDS,
	// not instructions, and a record of a removal necessarily names the removed
	// thing: docs/design/** (what changed and why) and docs/legal/** (release
	// and compliance history, which also quotes prose like "pix is an
	// independent project" that is not an invocation at all). A doc OUTSIDE
	// those trees can be equally a record rather than live instruction —
	// typically a human-run UAT/ops doc paired one-to-one with a script that
	// has not been migrated to the current verb surface yet — and is excluded
	// the same principled way, by its own `Status: PRE-V2` marker
	// (isHistoricalByMarker), never by naming the file here.
	...listFiles(
		"docs",
		(f) => f.endsWith(".md") && !f.startsWith("docs/design/") && !f.startsWith("docs/legal/") && !isHistoricalByMarker(f),
	),
	...listFiles("extensions", (f) => f.endsWith(".ts")),
	...fs
		.readdirSync(path.join(repoRoot, "services/host/cmd/pix"))
		.filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"))
		.map((f) => `services/host/cmd/pix/${f}`),
	// Wave B carry-forward (E1.13): both packages narrate `pix env` verbs in
	// doc comments and refusal text (config/environment.go's `pix env add`/
	// `pix env use`, workflow/provision/config.go's config-key refusals
	// pointing at `pix env use`/`add`/`forget`) written BEFORE the verb
	// surface existed to resolve. All seven verbs are wired now (E1.9-E1.13),
	// so these two packages are added to the surfaces a wrong verb cannot
	// hide in, same as every other production Go file in this list.
	...listFiles("services/host/config", (f) => f.endsWith(".go") && !f.endsWith("_test.go")),
	...listFiles("services/host/workflow/provision", (f) => f.endsWith(".go") && !f.endsWith("_test.go")),
];

// An INVOCATION, not prose. "pix learns from what you do" is English about the
// tool; `pix models route` is an instruction. Only the second kind can send
// someone to a dead end, so only the second kind is matched: `pix <verb>` inside
// backticks or quotes, or at the start of a line (a code-block command).
//
// `pix-host` and `pix-recalled-context` are names, not invocations: the pattern
// requires a literal space after "pix", which excludes every hyphenated form.
const DELIMITED = /[`'"]pix ((?:[a-z][a-z0-9-]*|--[a-z-]+)(?:[ ]+(?:[a-z][a-z0-9-]*|--[a-z-]+))*)/g;
const FENCED_COMMAND = /^\s*(?:\$ )?pix ((?:[a-z][a-z0-9-]*|--[a-z-]+)(?:[ ]+(?:[a-z][a-z0-9-]*|--[a-z-]+))*)/;

// Words that can legitimately follow "pix " in an invocation without being a
// subcommand: a flag, or a positional the reader is meant to substitute.
const NOT_A_VERB = new Set(["--help", "--version", "--all", "-h", "dir", "name", "path"]);

// A reference that is HISTORY, quoting a removed verb in order to say it is
// removed. Each is checked to still be accurate prose, not silently allowed.
const RETIRED_ON_PURPOSE = [
	{ file: "docs/reference.md", verb: "monitor", says: /was removed/ },
	{ file: "docs/reference.md", verb: "load", says: /was removed/ },
	// `state` grouped backup/restore/reset. All three went; only `reset` came
	// back, and as a TOP-LEVEL verb, so the grouping noun stays dead.
	{ file: "docs/reference.md", verb: "state", says: /is one of those removals/ },
	{ file: "AGENTS.md", verb: "host", says: /DELETED|retired|PIX_RETIRED/ },
	{ file: "AGENTS.md", verb: "gworkspace", says: /old|deleted|retired/ },
	{ file: "AGENTS.md", verb: "loaded", says: /deleted it/ },
	// D2/D3: `pix env rm` is deliberately never a working verb (env_cmd.go's
	// envCmd has no Rm field at all) — its own doc comments say so, in the
	// same "naming a thing that does not exist in order to say it does not
	// exist" sense every other RETIRED_ON_PURPOSE entry covers.
	{ file: "services/host/cmd/pix/env_cmd.go", verb: "rm", says: /is never one|not owning its files/ },
];

// docs/design/** is excluded from SURFACES above because it is mostly HISTORY:
// a record of what changed and why, where naming a removed verb is correct
// prose. `docs/design/environments.md` is the one design doc that is not pure
// history: its "CLI contract" section (§8) and adjoining precedence/lifecycle
// prose are the LIVE spec for the not-yet-implemented `pix env` surface, and
// three drafting rounds left it internally inconsistent with itself — a
// `pix env rm` that both "unregisters" (§8.1) and is later described as
// upstream sandbox+credential removal (§4), a `--sbxenv` flag next to a spec
// that also wants an exact positional enum, and a recreate example that still
// carried a deleted `--name` flag. None of that is catchable by the kong-tree
// walk above (the verb does not exist in Go yet), so this checks the live
// sections against the reconciled design directly: presence of the corrected
// surface, and absence of the stale one.
test("docs/design/environments.md's live env-verb spec carries no stale references", () => {
	const doc = read("docs/design/environments.md");

	// Stale forms that a prior drafting round left behind. Each must be gone
	// from the live spec now that `forget` replaces the unregister half of `rm`
	// and `rm` itself is a pointer error, not a working verb.
	//
	// `--sbxenv` is checked with a lookbehind, not a bare substring match: the
	// corrected spec must still be able to SAY "there is no `--sbxenv` flag" in
	// prose (the same RETIRED_ON_PURPOSE pattern the rest of this file uses for
	// a removed verb) without that true sentence tripping the guard meant to
	// catch the flag being documented as if it worked.
	const stale = [
		[/pix env rm NAME \[--force\]/, "the old CLI-contract line still lists `rm` as a working verb"],
		[/(?<!no `)--sbxenv/, "the old `--sbxenv` flag is documented as usable, not just named while explaining it does not exist"],
		[/--name pix-repo-work/, "the recreate example still carries the deleted `--name` flag"],
		[/no registered environment\n/, "env-selection precedence still leaves the no-environment case unnamed instead of calling it `none`"],
		// Wave B closeout: `env forget` never had a real force override (D21's
		// refusals for "current default" and "live holder" are absolute), so a
		// `[--force]` escape hatch on `forget` is a stale, never-shipped surface.
		[/forget NAME \[--force\]/, "`env forget` must not document a `[--force]` escape; both its refusals (default, live holder) have no override"],
		[/unless the explicit force contract permits it/, "`forget`'s refusal prose must not describe a force override that does not exist"],
		// PRD §5.3's rejected alternative, named explicitly in the PRD's own D12
		// row: an unattributed refusal makes the recreate tax feel arbitrary.
		[/create-time environment state differs/, "the recreate refusal must name drifted facets by canonical key path (PRD §5.3), not the rejected vague form"],
		// "alias" is the wrong noun for a registered environment name: the design
		// doc used to call the name/registration an "alias" in several places,
		// which reads as though `pix env forget` or repointing a name could ever
		// carry or transfer trust the way an alias implies. "registration"/"name"
		// are what is actually true. The one exception is the CLI-verb sense
		// (`pix env` is a command ALIAS for `pix env ls`), which is a different,
		// correct use of the word and is excluded by requiring "the alias".
		[/\bthe alias\b/, "'alias' must not stand in for the environment name/registration; say 'registration' or 'name'"],
	];
	for (const [pattern, why] of stale) {
		assert.doesNotMatch(doc, pattern, `docs/design/environments.md: ${why}`);
	}

	// Corrected forms the reconciled design must state.
	const required = [
		[/pix env forget NAME(?! \[)/, "the CLI contract must list bare `forget NAME`, not `rm`, as the unregister verb, and with no bracketed flag"],
		[/pix env show \[NAME\] \[--json\] \[--path\] \[--effective\]/, "the CLI contract must add `--effective` to `show`, alongside `--json`/`--path`"],
		[/pix env edit NAME pix\|sbxenv/, "`edit` must take the exact `pix|sbxenv` positional enum"],
		[/pix rm \S+ && pix run --env \S+/, "the recreate command must be `pix rm NAME && pix run --env ENV`, with no `--name`"],
		[/`none`/, "the no-environment case must be named `none`"],
		[/pix env rm.{0,80}(sandbox|source|registration)/s, "`pix env rm` must be documented as a pointer error naming sandbox/source/registration"],
		[/pix help env/, "setup must point only to `pix help env`, not walk `pix env` commands inline"],
		[/at most 100 create-intent records/, "the create-intent list must be capped at 100 entries"],
		[/pix doctor --recreates/, "per-record recreate detail must be behind `pix doctor --recreates`"],
		[/pre-composition/, "sandbox identity must be attributed pre-composition, not injected as a post-parse runtime fact"],
		// PRD §5.2/§5.3 exact copy: both example refusals must attribute the
		// change to a canonical `changed:` key path, not just gesture at it.
		[/changed: host\.services\.warehouse-proxy\.command/, "the host-execution-change example (PRD §5.2) must show a `changed:` key-path line"],
		[/changed: mcp\.servers\[github\]\.url, env\.PIX_MEMORY_SCOPE/, "the recreate-required example (PRD §5.3) must show a `changed:` key-path line"],
		[/cannot be reused\. Its environment changed since it\s+was created\./, "the recreate-required example must use the PRD §5.3 wording, not the rejected 'create-time environment state differs'"],
		// PRD §5.5 exact wording/order: what-failed sentence, then forget, then
		// rm, then rm -rf, in that order.
		[/pix: `pix env rm` does not exist\. Registering a name is not owning the files\./, "the `env rm` pointer error must open with the PRD §5.5 sentence"],
		[/pix env forget home[\s\S]{0,80}pix rm pix-repo-home[\s\S]{0,80}rm -rf <path>/, "the `env rm` pointer error must list forget, then rm, then rm -rf, in PRD §5.5 order"],
		// D22/D24 (I4 recreate log) vs the failed-create `create intent`: two
		// distinct bounded records that a prior draft conflated by making
		// `pix doctor --recreates` describe the create-intent list instead of the
		// PRD §5.9 recreate log.
		[/### 9\.4 Recreate diagnostics \(I4\)/, "the recreate log (I4) needs its own section, separate from §9.3's create-intent bookkeeping"],
		[/is not the `I4` recreate log below, and `pix doctor` never reports\s+create-intent records/, "§9.3 must say create-intent records are never what `pix doctor`/`--recreates` reports"],
		[/environments {3}12 unplanned recreates recorded {3}pix doctor --recreates/, "the doctor one-line form must match PRD §5.9 exactly"],
	];
	for (const [pattern, why] of required) {
		assert.match(doc, pattern, `docs/design/environments.md: ${why}`);
	}

	// Exactly seven env verbs, matching the seven Pix jobs pattern this design
	// already uses elsewhere (§3.2): ls, add, use, show, edit, review, forget.
	// `rm` is deliberately excluded: it is a refusal, not a verb.
	const contract = doc.match(/```console\npix env\s+# alias for ls\n([\s\S]*?)```/);
	assert.ok(contract, "docs/design/environments.md: could not find the `pix env` CLI contract block");
	const verbs = [...contract[1].matchAll(/^pix env (\S+)/gm)].map((m) => m[1]);
	assert.deepEqual(
		verbs,
		["ls", "add", "use", "show", "edit", "review", "forget"],
		"docs/design/environments.md: the CLI contract must list exactly these seven env verbs, in order",
	);
});

test("every `pix <verb>` in a user- or agent-facing surface names a real verb", () => {
	const types = verbTree();
	// The harvest must be a real TREE, or a path check silently degrades into the
	// first-word check that missed both original bugs.
	assert.ok(types.rootCmd?.run && types.rootCmd?.doctor, `root harvest looks broken: ${Object.keys(types.rootCmd ?? {})}`);
	// Pix v2 cut the whole verb surface down to run/ls/rm/task/env/secret/
	// setup/doctor/reset/help/version (docs/design/pix-v2-architecture.md):
	// `mcp`, `models`, `config`, `serve`, `pack`, `memory`, and `agent` are not
	// merely missing a subverb, the top-level verb itself does not exist.
	assert.equal(unresolvedVerb(types, ["mcp", "add"]), "mcp", "the deleted `pix mcp` verb group must NOT resolve");
	assert.equal(unresolvedVerb(types, ["models", "route"]), "models", "the deleted `pix models` verb group must NOT resolve");
	assert.ok(unresolvedVerb(types, ["env", "trust", "NAME"]) === null, "trailing ARGS must not read as verbs");

	const bad = [];
	for (const file of SURFACES) {
		let src;
		try {
			src = read(file);
		} catch {
			continue; // a surface that does not exist is not this test's business
		}
		const lines = src.split("\n");
		let inFence = false;
		lines.forEach((line, i) => {
			if (/^\s*```/.test(line)) {
				inFence = !inFence;
				return;
			}
			const hits = [...line.matchAll(DELIMITED)];
			if (inFence) {
				const cmd = line.match(FENCED_COMMAND);
				if (cmd) hits.push(cmd);
			}
			for (const m of hits) {
				const words = m[1].toLowerCase().split(/\s+/).filter(Boolean);
				const word = unresolvedVerb(types, words);
				if (!word || NOT_A_VERB.has(word)) continue;
				const retired = RETIRED_ON_PURPOSE.find((r) => r.file === file && r.verb === word);
				if (retired) {
					assert.match(
						line,
						retired.says,
						`${file}:${i + 1} quotes the retired verb "${word}" but no longer says it is retired:\n  ${line.trim()}`,
					);
					continue;
				}
				bad.push(`${file}:${i + 1}: "pix ${word}" is not a verb — ${line.trim().slice(0, 110)}`);
			}
		});
	}
	assert.deepEqual(bad, [], `references to commands that do not exist:\n${bad.join("\n")}`);
});
