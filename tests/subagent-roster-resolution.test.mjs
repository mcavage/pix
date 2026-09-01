// E3.2: extensions/subagents.ts resolves an agent's model from the additive
// `roster` field on the generated inference.json v1 manifest (docs/design/
// environments.md §6.4), never from routing.json. Precedence:
//
//   1. a custom (project) agent's own explicit `model:`
//   2. the selected environment's roster.agents[<agent name>]
//   3. roster.main
//   4. inherit the parent session's own model
//
// An agent name absent from the roster degrades straight to (4) — never a
// "scored" pick, because the deleted intent router is gone, not merely
// unreachable.
//
// BLOCK fix: legacy `fallback_intent:` frontmatter (pending outright deletion
// in E3.4) must never join this chain — it is not a roster.agents lookup and
// has zero effect on the resolved model, proven below with security-lead's
// shipped `fallback_intent: review` and an arbitrary `fallback_intent:
// breadth`. See tests/no-fallback-intent-resolution.test.mjs for the paired
// source/static sentinel.
//
// These tests read the resolved model off the `/subagents` listing (which
// prints `a.model`), never by actually spawning a child pi process: model
// RESOLUTION is pure per-agent-file logic and does not need a live model
// call to observe.
import assert from "node:assert/strict";
import * as fs from "node:fs";
import { register } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

let seq = 0;
async function loadSubagents(env = {}) {
	const saved = {};
	for (const [k, v] of Object.entries(env)) {
		saved[k] = process.env[k];
		process.env[k] = v;
	}
	try {
		const url = new URL(`../extensions/subagents.ts?roster=${seq++}`, import.meta.url);
		const mod = await import(url.href);
		const reg = { tool: null, command: null };
		mod.default({
			on() {},
			registerTool(t) {
				reg.tool = t;
			},
			registerCommand(_name, cfg) {
				reg.command = cfg;
			},
		});
		reg.mod = mod;
		return reg;
	} finally {
		for (const [k, v] of Object.entries(saved)) {
			if (v === undefined) delete process.env[k];
			else process.env[k] = v;
		}
	}
}

// Runs `/subagents both` and returns { line(name) -> full listing line }.
async function listAgents(reg, cwd) {
	const notes = [];
	await reg.command.handler("both", { cwd, ui: { notify: (msg) => notes.push(msg) } });
	assert.equal(notes.length, 1);
	return notes[0];
}
function lineFor(listing, name) {
	const m = listing.match(new RegExp(`^  ${name} \\([a-z]+\\).*$`, "m"));
	assert.ok(m, `expected a listing line for "${name}", got:\n${listing}`);
	return m[0];
}

function setup() {
	const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "roster-agentdir-"));
	fs.mkdirSync(path.join(agentDir, "agents"), { recursive: true });
	const projectRoot = fs.mkdtempSync(path.join(os.tmpdir(), "roster-project-"));
	fs.mkdirSync(path.join(projectRoot, ".pi", "agents"), { recursive: true });
	return { agentDir, projectRoot };
}
function writeInference(agentDir, manifest) {
	fs.writeFileSync(path.join(agentDir, "inference.json"), JSON.stringify(manifest));
}
function writeUserAgent(agentDir, name, frontmatter) {
	const fm = Object.entries(frontmatter)
		.map(([k, v]) => `${k}: ${v}`)
		.join("\n");
	fs.writeFileSync(
		path.join(agentDir, "agents", `${name}.md`),
		`---\n${fm}\n---\nYou are ${name}.\n`,
	);
}
function writeProjectAgent(projectRoot, name, frontmatter) {
	const fm = Object.entries(frontmatter)
		.map(([k, v]) => `${k}: ${v}`)
		.join("\n");
	fs.writeFileSync(
		path.join(projectRoot, ".pi", "agents", `${name}.md`),
		`---\n${fm}\n---\nYou are ${name}.\n`,
	);
}

test("roster.agents[name] wins over roster.main for a shipped-style user agent", async () => {
	const { agentDir, projectRoot } = setup();
	writeInference(agentDir, {
		version: 1,
		backends: {},
		models: [],
		roster: {
			main: "zai/glm-5",
			agents: { engineer: "zai/glm-5", review: "google/gemini-3.1-pro-preview" },
		},
	});
	writeUserAgent(agentDir, "engineer", { description: "eng" });
	writeUserAgent(agentDir, "review", { description: "rev" });
	writeUserAgent(agentDir, "designer", { description: "no roster entry" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	assert.match(lineFor(listing, "engineer"), /· zai\/glm-5/);
	assert.match(lineFor(listing, "review"), /· google\/gemini-3\.1-pro-preview/);
	// "designer" is absent from roster.agents: falls through to roster.main —
	// never left unresolved, and never a scored guess.
	assert.match(lineFor(listing, "designer"), /· zai\/glm-5/);
});

test("an agent name absent from BOTH roster.agents and any roster at all inherits the parent — never a scored pick", async () => {
	const { agentDir, projectRoot } = setup();
	// No inference.json at all: the pre-E3.1 world.
	writeUserAgent(agentDir, "designer", { description: "no manifest" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	assert.match(lineFor(listing, "designer"), /· model:inherit/);
	assert.doesNotMatch(listing, /designer.*·\s*(zai|google|anthropic|openai)\//);
});

test("a native cloud parent becomes the runtime model for an otherwise inheriting agent", async () => {
	const { agentDir } = setup();
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const agents = [{ name: "review", model: undefined }];
	const resolved = reg.mod.inheritActiveParentModel(agents, {
		provider: "openai",
		id: "gpt-5.6-sol",
	});
	assert.equal(resolved[0].model, "openai/gpt-5.6-sol");
	const source = fs.readFileSync(
		new URL("../extensions/subagents.ts", import.meta.url),
		"utf8",
	);
	assert.match(source, /inheritActiveParentModel\(discovered\.agents, ctx\.model\)/);
});

test("declaring `intent:` in frontmatter has NO effect on the resolved model and is never shown (E3.4 review fix: not merely inert, entirely unparsed/displayless)", async () => {
	const { agentDir, projectRoot } = setup();
	writeInference(agentDir, {
		version: 1,
		backends: {},
		models: [],
		roster: { main: "zai/glm-5", agents: {} },
	});
	// "code" used to be a live intent name under the deleted router.
	writeUserAgent(agentDir, "engineer", { description: "eng", intent: "code" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	// Falls through to roster.main, exactly as an agent with no intent at all
	// would — the declared intent has no effect on resolution.
	assert.match(lineFor(listing, "engineer"), /· zai\/glm-5/);
	// And, unlike the first E3.4 commit's "kept for display" treatment, it must
	// never even be SHOWN: an `intent:` frontmatter key is now an ordinary
	// unknown field, indistinguishable from a typo.
	assert.doesNotMatch(lineFor(listing, "engineer"), /intent/i);
	assert.doesNotMatch(listing, /intent:code|intent:"code"/);
});

test("declaring `fallback_intent: review` (security-lead's shipped value) has NO effect on the resolved model — it is never a roster.agents lookup", async () => {
	const { agentDir, projectRoot } = setup();
	writeInference(agentDir, {
		version: 1,
		backends: {},
		models: [],
		// "review" IS a live roster.agents key here — if fallback_intent were ever
		// resolved as a roster.agents name lookup, security-lead would wrongly
		// pick up google/gemini-3.1-pro-preview instead of falling through to
		// roster.main, since "security-lead" itself has no roster entry.
		roster: {
			main: "zai/glm-5",
			agents: { review: "google/gemini-3.1-pro-preview" },
		},
	});
	writeUserAgent(agentDir, "security-lead", {
		description: "sec",
		intent: "red-team",
		fallback_intent: "review",
	});
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	// Falls through to roster.main, exactly as if fallback_intent were absent.
	assert.match(lineFor(listing, "security-lead"), /· zai\/glm-5/);
	assert.doesNotMatch(lineFor(listing, "security-lead"), /gemini-3\.1-pro-preview/);
	// No warning is emitted about fallback_intent resolution either — it is
	// never resolved in the first place, so there is nothing to warn about.
	assert.doesNotMatch(listing, /fallback_intent/);
});

test("declaring an arbitrary, never-a-real-intent `fallback_intent: breadth` also has NO effect on the resolved model", async () => {
	const { agentDir, projectRoot } = setup();
	writeInference(agentDir, {
		version: 1,
		backends: {},
		models: [],
		roster: {
			main: "zai/glm-5",
			agents: { engineer: "anthropic/claude-sonnet-5", breadth: "google/gemini-3.1-flash-lite" },
		},
	});
	writeUserAgent(agentDir, "engineer", { description: "eng", fallback_intent: "breadth" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	// Resolves through the normal chain — roster.agents["engineer"] — exactly
	// as if fallback_intent were never declared; the "breadth" roster entry it
	// names is never consulted.
	assert.match(lineFor(listing, "engineer"), /· anthropic\/claude-sonnet-5/);
	assert.doesNotMatch(lineFor(listing, "engineer"), /gemini-3\.1-flash-lite/);
});

test("a custom PROJECT agent's explicit model: wins over BOTH roster.agents[name] and roster.main", async () => {
	const { agentDir, projectRoot } = setup();
	writeInference(agentDir, {
		version: 1,
		backends: {},
		models: [],
		roster: { main: "zai/glm-5", agents: { pinned: "google/gemini-3.1-pro-preview" } },
	});
	writeProjectAgent(projectRoot, "pinned", {
		description: "custom project agent with its own pin",
		model: "anthropic/claude-opus-5",
	});
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	assert.match(lineFor(listing, "pinned"), /· anthropic\/claude-opus-5/);
});

test("an explicit parent Ollama model overrides even a resolved roster entry", async () => {
	const { agentDir, projectRoot } = setup();
	writeInference(agentDir, {
		version: 1,
		backends: {},
		models: [],
		roster: { main: "zai/glm-5", agents: { engineer: "google/gemini-3.1-pro-preview" } },
	});
	writeUserAgent(agentDir, "engineer", { description: "eng" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const savedArgv = process.argv;
	process.argv = ["node", "pi", "--model", "ollama/qwen3.5:9b"];
	try {
		const reg = await loadSubagents();
		const listing = await listAgents(reg, projectRoot);
		assert.match(lineFor(listing, "engineer"), /· ollama\/qwen3\.5:9b/);
	} finally {
		process.argv = savedArgv;
	}
});

test("absent inference.json degrades to the pre-roster world for every agent (compatibility)", async () => {
	const { agentDir, projectRoot } = setup();
	writeUserAgent(agentDir, "engineer", { description: "eng" });
	writeUserAgent(agentDir, "pinned", { description: "pinned", model: "anthropic/claude-opus-5" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	assert.match(lineFor(listing, "engineer"), /· model:inherit/);
	assert.match(lineFor(listing, "pinned"), /· anthropic\/claude-opus-5/);
	assert.match(listing, /roster: off \(no roster in inference\.json; agents use model:\/inherit\)/);
});

test("a present but MALFORMED inference.json roster degrades cleanly, same as absent", async () => {
	const { agentDir, projectRoot } = setup();
	fs.writeFileSync(
		path.join(agentDir, "inference.json"),
		JSON.stringify({ version: 1, backends: {}, models: [], roster: { agents: { engineer: "zai/glm-5" } } }), // no `main`
	);
	writeUserAgent(agentDir, "engineer", { description: "eng" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	assert.match(lineFor(listing, "engineer"), /· model:inherit/);
	assert.match(listing, /roster: off/);
});

test("unreadable/unparseable inference.json degrades to no roster without throwing", async () => {
	const { agentDir, projectRoot } = setup();
	fs.writeFileSync(path.join(agentDir, "inference.json"), "{ not json");
	writeUserAgent(agentDir, "engineer", { description: "eng" });
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	assert.match(lineFor(listing, "engineer"), /· model:inherit/);
});

test("the roster status line reports main + agent-override count", async () => {
	const { agentDir, projectRoot } = setup();
	writeInference(agentDir, {
		version: 1,
		backends: {},
		models: [],
		roster: { main: "zai/glm-5", agents: { engineer: "zai/glm-5", review: "google/gemini-3.1-pro-preview" } },
	});
	process.env.PI_TEST_AGENT_DIR = agentDir;
	const reg = await loadSubagents();
	const listing = await listAgents(reg, projectRoot);
	assert.match(listing, /roster: on \(main=zai\/glm-5, 2 agent overrides, inference\.json\)/);
});
