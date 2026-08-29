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

test("declaring `intent:` in frontmatter has NO effect on the resolved model (no vestigial intent routing)", async () => {
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
	// would — the declared intent is display-only.
	assert.match(lineFor(listing, "engineer"), /· zai\/glm-5/);
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
