// Unit tests for the extensions/subagents.ts host-mode kill switch (H4).
// Run: node --test tests/
//
// subagents.ts reads its env config at import time, so each scenario dynamic-
// imports a fresh module instance (cache-busting query) with the env set first.
// The pi runtime packages are stubbed via a module.register resolve hook.
import assert from "node:assert";
import * as fs from "node:fs";
import { register } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

register("./stub-loader.mjs", import.meta.url);

// An empty agent dir for the stubbed getAgentDir(): no agents, no routing.json.
const agentDir = fs.mkdtempSync(path.join(os.tmpdir(), "subagents-test-"));
fs.mkdirSync(path.join(agentDir, "agents"), { recursive: true });
fs.mkdirSync(path.join(agentDir, "skills", "challenge"), { recursive: true });
fs.writeFileSync(path.join(agentDir, "skills", "challenge", "SKILL.md"), "# challenge\n");
process.env.PI_TEST_AGENT_DIR = agentDir;

// PI_SUBAGENT_DEPTH is cleared too: when this test itself runs INSIDE a
// subagent, the inherited depth would shift the depth-limit assertions.
const ENV_KEYS = [
	"PI_SUBAGENT_DISABLED",
	"PI_SUBAGENT_MAX_DEPTH",
	"PI_SUBAGENT_DEPTH",
];
let seq = 0;

// Import a FRESH subagents.ts instance with the given env, capture what it
// registers on a fake pi API, and restore the env.
async function loadSubagents(env) {
	const saved = {};
	for (const k of ENV_KEYS) {
		saved[k] = process.env[k];
		delete process.env[k];
	}
	for (const [k, v] of Object.entries(env)) process.env[k] = v;
	try {
		const url = new URL(
			`../extensions/subagents.ts?case=${seq++}`,
			import.meta.url,
		);
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
		assert.ok(reg.tool, "subagent tool registered");
		assert.ok(reg.command, "/subagents command registered");
		reg.mod = mod;
		return reg;
	} finally {
		for (const k of ENV_KEYS) {
			if (saved[k] === undefined) delete process.env[k];
			else process.env[k] = saved[k];
		}
	}
}

test("curated children reload generated inference providers before subagents", async () => {
	const reg = await loadSubagents({});
	const self = path.join(agentDir, "extensions", "subagents.ts");
	const inference = path.join(agentDir, "extensions", "inference.ts");
	fs.mkdirSync(path.dirname(self), { recursive: true });
	fs.writeFileSync(self, "");
	fs.writeFileSync(inference, "");
	assert.deepEqual(reg.mod.coreChildExtensionArgs(self), ["-e", inference, "-e", self]);
});

test("an explicit parent Ollama model becomes the subagent availability boundary", async () => {
	const reg = await loadSubagents({});
	assert.equal(
		reg.mod.explicitParentOllamaModel([
			"pi",
			"--model",
			"ollama/glm-5.2:cloud",
		]),
		"ollama/glm-5.2:cloud",
	);
	assert.equal(
		reg.mod.explicitParentOllamaModel(["pi", "--model=ollama/qwen3.5:9b"]),
		"ollama/qwen3.5:9b",
	);
	assert.equal(
		reg.mod.explicitParentOllamaModel([
			"pi",
			"--model",
			"docker-openai/gpt-5.6-sol",
		]),
		"",
		"cloud parents retain normal roster resolution",
	);
});

test("policy-refusal detection is narrow and recognizes Anthropic cyber refusals", async () => {
	const reg = await loadSubagents({});
	assert.equal(
		reg.mod.isProviderPolicyRefusal({
			errorMessage:
				"This request triggered restrictions on violative cyber content and was blocked under Anthropic's Usage Policy.",
			stderr: "",
			messages: [],
		}),
		true,
	);
	for (const errorMessage of [
		"Model not found: docker-google/gemini",
		"401 invalid x-api-key",
		"request timed out",
	]) {
		assert.equal(
			reg.mod.isProviderPolicyRefusal({ errorMessage, stderr: "", messages: [] }),
			false,
			errorMessage,
		);
	}
});

test("invalid compiled routes become actionable diagnostics", async () => {
	const reg = await loadSubagents({});
	const result = {
		exitCode: 1,
		stopReason: "error",
		timedOut: null,
		errorMessage: 'Model "docker-google/gemini-bad" not found',
		stderr: "",
		messages: [],
	};
	reg.mod.clarifyRoutedModelFailure(result, {
		name: "review",
		model: "docker-google/gemini-bad",
	});
	assert.match(result.errorMessage, /resolved to model "docker-google\/gemini-bad"/);
	assert.match(result.errorMessage, /pix rm <box> && pix run/);
	assert.match(result.errorMessage, /Original: Model/);
});

const ctx = { cwd: process.cwd(), hasUI: false, ui: null };
const exec = (reg, params) =>
	reg.tool.execute("id", params, new AbortController().signal, undefined, ctx);
const text = (r) => r.content?.map((c) => c.text ?? "").join("\n") ?? "";

// ── (a) disabled refuses EVERY spawn path: single, parallel, chain, doctor ──
test("PI_SUBAGENT_DISABLED=1 refuses single, parallel, chain, and doctor", async () => {
	const reg = await loadSubagents({ PI_SUBAGENT_DISABLED: "1" });
	for (const params of [
		{ agent: "fanout", task: "t" },
		{ tasks: [{ agent: "fanout", task: "t" }] },
		{ chain: [{ agent: "fanout", task: "t" }] },
	]) {
		const r = await exec(reg, params);
		assert.equal(r.isError, true, JSON.stringify(params));
		assert.match(text(r), /disabled in host mode/i);
		assert.equal(r.details.results.length, 0, "nothing may have run");
	}
	// The doctor path used to call runSingle() directly, bypassing the check
	// that lived only in execute() — it spawned the canary even when disabled.
	const notes = [];
	await reg.command.handler("doctor", {
		cwd: process.cwd(),
		ui: { notify: (msg, level) => notes.push({ msg, level }) },
	});
	assert.equal(notes.length, 1);
	assert.match(notes[0].msg, /refusing to spawn the canary/i);
	assert.match(notes[0].msg, /disabled in host mode/i);
	assert.equal(notes[0].level, "error");
});

// ── (c) "0"/"false" are NOT disabled (Boolean(env) was truthy for both) ─────
test('PI_SUBAGENT_DISABLED="0" / "false" do not disable', async () => {
	for (const v of ["0", "false", ""]) {
		const reg = await loadSubagents({ PI_SUBAGENT_DISABLED: v });
		const r = await exec(reg, { agent: "no-such-agent", task: "t" });
		assert.equal(r.isError, true);
		// Reaches the normal path (unknown agent), NOT the host-mode refusal.
		assert.match(text(r), /Unknown agent/i, `env value ${JSON.stringify(v)}`);
		assert.doesNotMatch(text(r), /disabled in host mode/i);
	}
});

test("a skill name gets a skill-specific correction, not only an agent dump", async () => {
	const reg = await loadSubagents({});
	const r = await exec(reg, { agent: "challenge", task: "stress test this" });
	assert.equal(r.isError, true);
	assert.match(text(r), /"challenge" is a skill, not a subagent preset/i);
	assert.match(text(r), /\/skill:challenge/);
});

// ── (b) an explicit MAX_DEPTH=0 is honored (num() used to reject zero) ──────
test("PI_SUBAGENT_MAX_DEPTH=0 refuses at depth 0/0", async () => {
	const reg = await loadSubagents({ PI_SUBAGENT_MAX_DEPTH: "0" });
	const r = await exec(reg, { agent: "no-such-agent", task: "t" });
	assert.equal(r.isError, true);
	assert.match(text(r), /depth limit reached \(0\/0\)/i);
});

test("unset / invalid MAX_DEPTH still defaults to 3", async () => {
	for (const env of [{}, { PI_SUBAGENT_MAX_DEPTH: "banana" }]) {
		const reg = await loadSubagents(env);
		const r = await exec(reg, { agent: "no-such-agent", task: "t" });
		// Not a depth refusal — falls through to the unknown-agent path.
		assert.match(text(r), /Unknown agent/i);
	}
});

// ── retired cross-vendor retry-on-policy-refusal: permanent regression guard ──
// The retired feature re-invoked runSingle with a swapped model (once resolved
// via the since-deleted `fallback_intent:` frontmatter, E3.4) whenever a
// provider policy refusal fired. Folded in from the now-deleted
// tests/no-fallback-intent-resolution.test.mjs, whose fallback_intent-specific
// assertions this file's E3.4 removal made moot; this guard against the
// retry mechanism itself is not.
const subagentsSrc = fs.readFileSync(
	path.join(
		path.dirname(fileURLToPath(import.meta.url)),
		"..",
		"extensions",
		"subagents.ts",
	),
	"utf8",
);

test("subagents.ts has no `fallbackModel` — the field that used to carry a policy-refusal retry's resolved model is gone", () => {
	assert.doesNotMatch(
		subagentsSrc,
		/fallbackModel/,
		"fallbackModel was the only mechanism through which a cross-vendor retry ever affected model choice; it must be fully removed, not merely unused",
	);
});

test("subagents.ts has no cross-vendor retry keyed off a policy refusal + fallback route", () => {
	// Any resurrection of a second runSingle call gated on
	// isProviderPolicyRefusal is exactly the hidden model-choice effect that
	// was removed.
	assert.doesNotMatch(
		subagentsSrc,
		/isProviderPolicyRefusal\(result\)[\s\S]{0,200}runSingle\(/,
		"a provider policy refusal must not trigger a retry that changes the model",
	);
});

test("subagents.ts has no fallbackIntent field or fallback_intent frontmatter parsing (E3.4: field removed entirely)", () => {
	assert.doesNotMatch(subagentsSrc, /fallbackIntent/, "fallbackIntent must be gone from AgentConfig, not merely unused");
	assert.doesNotMatch(subagentsSrc, /frontmatter\.fallback_intent/, "fallback_intent frontmatter must no longer be parsed");
});

// ── E3.4 review fix: `intent` follows `fallbackIntent` out entirely ──────────
// The first E3.4 commit deleted `fallback_intent:` but left `intent` parsed
// and displayed "for display only". Review flagged that as inconsistent: a
// field with zero effect on behavior has no business being parsed, typed, or
// shown either — it is exactly the same kind of dead frontmatter the
// `fallbackIntent` deletion above already treats as intolerable. A custom
// agent's `intent:` must now be indistinguishable from any other unknown
// frontmatter key: never extracted, never on AgentConfig, never rendered.
test("subagents.ts has no `intent` field on AgentConfig or frontmatter.intent parsing (E3.4 review fix: field removed entirely, not merely unused)", () => {
	assert.doesNotMatch(
		subagentsSrc,
		/intent\?:\s*string/,
		"AgentConfig must not declare an `intent` field",
	);
	assert.doesNotMatch(
		subagentsSrc,
		/frontmatter\.intent\b/,
		"`intent:` frontmatter must no longer be parsed",
	);
	assert.doesNotMatch(
		subagentsSrc,
		/\bagent\.intent\b|\ba\.intent\b/,
		"no call site may read an AgentConfig's `.intent` — the field is gone",
	);
});

