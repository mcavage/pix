// U08f retired `pack new`/`pack add` (the pack authoring verbs): a pack.toml
// and its skills/*/SKILL.md, bin/<name>, and [[integrations]]/[[proxy]]
// stanzas are now created and edited by hand, then picked up on `pack use`.
//
// Docs drift silently once a CLI surface is cut, because nothing fails to
// build when a doc still teaches a retired verb. This is the grep/doc parity
// net: every LIVE doc/help surface must never cite `pack new`/`pack add` as a
// thing you can run, and every HISTORICAL design doc that still describes
// them (because that's the record of why the schema looks the way it does)
// must carry a visible U08f retirement marker so a reader doesn't mistake
// history for current behavior.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const read = (rel) => fs.readFileSync(path.join(repoRoot, rel), "utf8");

// A retired authoring verb cited as something you can run: "pack new" or
// "pack add" as a whole word (does not match prose like "pack adds" or
// "pack.toml's `[[integrations]]` stanza add[s]").
const RETIRED_VERB = /\bpack\s+(new|add)\b/;

// A line naming the retired verb ONLY to say it's gone ("there is no `pack
// new`", "pack new and pack add are gone/retired") is documentation of the
// retirement, not a live citation of it as runnable.
const NEGATED_ON_SAME_LINE =
	/\b(no|not|gone|retired|removed)\b|is\s+retired|isn'?t\s+(a|the)|there'?s?\s+no/i;

function retiredVerbLinesNotNegated(text) {
	return text
		.split("\n")
		.filter((line) => RETIRED_VERB.test(line) && !NEGATED_ON_SAME_LINE.test(line));
}

// Live surfaces: read by users/agents *today* to learn what pix can do. None
// of these may cite the retired verbs.
const LIVE_DOCS = [
	"docs/reference.md",
	"AGENTS.md",
	"skills/onboarding/SKILL.md",
	"services/host/cmd/pix/pack_cmd.go",
	"services/host/cmd/pix/root.go",
];

// Historical design docs: allowed to still narrate `pack new`/`pack add`
// (that's the historical record), but only if clearly marked retired.
const HISTORICAL_DOCS = ["docs/design/packs.md", "docs/design/packs-v2.md", "docs/design/packs-v2-impl.md"];

const RETIREMENT_MARKER = /U08f/;

test("live docs/help never cite the retired `pack new`/`pack add` verbs as runnable", () => {
	for (const rel of LIVE_DOCS) {
		const offending = retiredVerbLinesNotNegated(read(rel));
		assert.deepEqual(offending, [], `${rel} still cites a retired pack authoring verb as runnable`);
	}
});

test("docs/reference.md's pack row has no authoring verb and points at hand-editing", () => {
	const text = read("docs/reference.md");
	assert.match(text, /`pack`.*ls\/show\/use\/rm/);
	assert.match(text, /no authoring verb/);
});

test("historical pack design docs still mentioning the retired verbs carry a U08f marker", () => {
	for (const rel of HISTORICAL_DOCS) {
		const text = read(rel);
		if (RETIRED_VERB.test(text)) {
			assert.match(text, RETIREMENT_MARKER, `${rel} cites a retired pack verb with no U08f retirement marker`);
		}
	}
});

test("pack_cmd.go help text explains direct pack.toml/skills editing, not a scaffolding verb", () => {
	const text = read("services/host/cmd/pix/pack_cmd.go");
	assert.match(text, /no authoring verb/i);
	assert.match(text, /pack\.toml and skills\/\*\/SKILL\.md/);
});
