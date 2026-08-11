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
const SURFACES = [
	"capabilities.json",
	"README.md",
	"AGENTS.md",
	"docs/getting-started.md",
	"docs/reference.md",
	...fs.readdirSync(path.join(repoRoot, "extensions")).filter((f) => f.endsWith(".ts")).map((f) => `extensions/${f}`),
	...fs
		.readdirSync(path.join(repoRoot, "services/host/cmd/pix"))
		.filter((f) => f.endsWith(".go") && !f.endsWith("_test.go"))
		.map((f) => `services/host/cmd/pix/${f}`),
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
];

test("every `pix <verb>` in a user- or agent-facing surface names a real verb", () => {
	const types = verbTree();
	// The harvest must be a real TREE, or a path check silently degrades into the
	// first-word check that missed both original bugs.
	assert.ok(types.rootCmd?.run && types.rootCmd?.doctor, `root harvest looks broken: ${Object.keys(types.rootCmd ?? {})}`);
	assert.ok(unresolvedVerb(types, ["mcp", "add"]) === null, "`pix mcp add` must resolve");
	assert.equal(unresolvedVerb(types, ["models", "route"]), "route", "the deleted `pix models route` must NOT resolve");
	assert.equal(unresolvedVerb(types, ["mcp", "load"]), "load", "the deleted `pix mcp load` must NOT resolve");
	assert.ok(unresolvedVerb(types, ["config", "set", "services", "memory"]) === null, "trailing ARGS must not read as verbs");

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
