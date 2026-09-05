// Unit tests for lib/inference-roster.ts's parseRoster: the ONE shape every
// reader of the additive `roster` field (docs/design/environments.md §7)
// shares. Absent and malformed input must both degrade to "no roster" —
// never throw, never partially trust a bad shape.
import assert from "node:assert/strict";
import { test } from "node:test";

const { parseRoster } = await import("../lib/inference-roster.ts");

test("a well-formed roster round-trips exactly", () => {
	const roster = parseRoster({
		version: 1,
		backends: {},
		models: [],
		roster: {
			main: "zai/glm-5",
			agents: { engineer: "zai/glm-5", review: "google/gemini-3.1-pro-preview" },
		},
	});
	assert.deepEqual(roster, {
		main: "zai/glm-5",
		agents: { engineer: "zai/glm-5", review: "google/gemini-3.1-pro-preview" },
	});
});

test("a roster with no [agents] table still yields a usable main-only roster", () => {
	const roster = parseRoster({ roster: { main: "zai/glm-5" } });
	assert.deepEqual(roster, { main: "zai/glm-5", agents: {} });
});

test("an ABSENT roster key is not an error — it is the pre-E3.1 manifest shape", () => {
	assert.equal(parseRoster({ version: 1, backends: {}, models: [] }), undefined);
});

for (const junk of [
	null,
	undefined,
	42,
	"nope",
	{},
	{ roster: null },
	{ roster: "nope" },
	{ roster: 7 },
	{ roster: {} }, // no `main`
	{ roster: { main: "" } }, // blank main
	{ roster: { main: "   " } }, // whitespace-only main
	{ roster: { main: 42 } }, // wrong type
]) {
	test(`malformed input degrades to undefined, never throws: ${JSON.stringify(junk)}`, () => {
		assert.doesNotThrow(() => parseRoster(junk));
		assert.equal(parseRoster(junk), undefined);
	});
}

test("a wrong-type [agents] table degrades to no overrides, but a valid main still resolves", () => {
	assert.deepEqual(parseRoster({ roster: { main: "zai/glm-5", agents: "nope" } }), {
		main: "zai/glm-5",
		agents: {},
	});
});

test("a non-string agent model id is dropped, not the whole roster", () => {
	const roster = parseRoster({
		roster: {
			main: "zai/glm-5",
			agents: { engineer: "zai/glm-5", broken: 42, blank: "   " },
		},
	});
	assert.deepEqual(roster, { main: "zai/glm-5", agents: { engineer: "zai/glm-5" } });
});

test("main is trimmed", () => {
	assert.deepEqual(parseRoster({ roster: { main: "  zai/glm-5  " } }), {
		main: "zai/glm-5",
		agents: {},
	});
});
