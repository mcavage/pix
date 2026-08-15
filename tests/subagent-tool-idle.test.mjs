// Regression test for the subagent IDLE WATCHDOG measuring the wrong thing.
// Run: node --test tests/
//
// The idle watchdog kills a child that has emitted nothing for IDLE_MS. That is
// a sound liveness proxy only while the MODEL is working. It is NOT sound while
// a TOOL is working: a child running one long bash command emits
// tool_execution_start and then NOTHING until tool_execution_end. Measured
// against a real child pi, a 45s `sleep` produced a 45s gap in the event stream
// (tool_execution_update fires at the boundaries, it does not stream progress).
//
// So an engineer subagent running this repo's own required verification step,
// `go test ./... -race`, goes silent for minutes and was killed at 5 for doing
// exactly what it was told. That is not a hypothetical: it is how the ollama
// fix's engineer died mid-task, leaving uncommitted work and no report.
//
// The fix is to ask the right question. While a tool is in flight the child has
// PROVEN it is alive and said what it is doing, so silence is expected and the
// budget is TOOL_IDLE_MS. When no tool is running, silence is still evidence of
// a dead stream and the short budget applies. The wall cap is untouched, so a
// tool that hangs forever is still bounded.
import assert from "node:assert";
import * as fs from "node:fs";
import { register } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

register("./stub-loader.mjs", import.meta.url);

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "subagents-toolidle-"));
fs.mkdirSync(path.join(tmp, "agents"), { recursive: true });
fs.writeFileSync(
	path.join(tmp, "agents", "slowpoke.md"),
	"---\ndescription: emits a long silent tool call\ntools: read\n---\nYou are slowpoke.\n",
);
process.env.PI_TEST_AGENT_DIR = tmp;

// A fake `pi` that reproduces the exact stream shape a real child produces for
// one long tool call: announce the tool, go completely silent, then finish.
// SILENT_MS is how long the gap is; the test picks budgets around it.
const fakePi = path.join(tmp, "fake-pi.mjs");
fs.writeFileSync(
	fakePi,
	`
const emit = (o) => process.stdout.write(JSON.stringify(o) + "\\n");
const silentMs = Number(process.env.FAKE_SILENT_MS || 0);
const withTool = process.env.FAKE_WITH_TOOL === "1";
emit({ type: "agent_start" });
emit({ type: "turn_start" });
if (withTool) emit({ type: "tool_execution_start", toolName: "bash" });
setTimeout(() => {
  if (withTool) emit({ type: "tool_execution_end", toolName: "bash" });
  emit({
    type: "message_end",
    message: { role: "assistant", content: [{ type: "text", text: "done" }], stopReason: "endTurn" },
  });
  emit({ type: "turn_end" });
  emit({ type: "agent_settled" });
  process.exit(0);
}, silentMs);
`,
);

let seq = 0;
async function loadSubagents() {
	const url = new URL(
		`../extensions/subagents.ts?toolidle=${seq++}`,
		import.meta.url,
	);
	const mod = await import(url.href);
	let tool = null;
	mod.default({
		on() {},
		registerTool(t) {
			tool = t;
		},
		registerCommand() {},
	});
	return tool;
}

async function runSlowpoke({ silentMs, withTool, idleMs, toolIdleMs }) {
	process.env.FAKE_SILENT_MS = String(silentMs);
	process.env.FAKE_WITH_TOOL = withTool ? "1" : "0";
	process.env.PI_SUBAGENT_PI_COMMAND = `${process.execPath} ${fakePi}`;
	process.env.PI_SUBAGENT_IDLE_MS = String(idleMs);
	process.env.PI_SUBAGENT_TOOL_IDLE_MS = String(toolIdleMs);
	const tool = await loadSubagents();
	assert.ok(tool, "the extension must register a subagent tool");
	const res = await tool.execute(
		"t1",
		{ agent: "slowpoke", task: "wait quietly" },
		new AbortController().signal,
		() => {},
		{ cwd: tmp },
	);
	return JSON.stringify(res);
}

test("a silent TOOL call outliving the short idle budget is not killed", async () => {
	const out = await runSlowpoke({
		silentMs: 1200,
		withTool: true,
		idleMs: 300, // model-silence budget: far shorter than the gap
		toolIdleMs: 30_000, // tool-silence budget: comfortably longer
	});
	assert.ok(
		!/Timed out: no output/.test(out),
		`a child that ANNOUNCED a tool and then went silent must be judged by the tool budget, got: ${out}`,
	);
	assert.ok(
		/done/.test(out),
		`the run must complete and return its output, got: ${out}`,
	);
});

test("the same silence with NO tool in flight is still killed as idle", async () => {
	const out = await runSlowpoke({
		silentMs: 1200,
		withTool: false,
		idleMs: 300,
		toolIdleMs: 30_000,
	});
	assert.ok(
		/Timed out: no output/.test(out),
		`silence with no tool announced is still a dead stream and must be killed, got: ${out}`,
	);
});

test("the tool budget never demotes an explicit longer per-agent idle_ms", async () => {
	// An agent that raised idle_ms above the tool budget meant it. Taking the
	// tool budget verbatim here would SHORTEN that agent's protection the moment
	// it ran a tool, which is the opposite of what the override asked for.
	fs.writeFileSync(
		path.join(tmp, "agents", "patient.md"),
		"---\ndescription: explicitly patient\ntools: read\nidle_ms: 30000\n---\nYou are patient.\n",
	);
	process.env.FAKE_SILENT_MS = "1200";
	process.env.FAKE_WITH_TOOL = "1";
	process.env.PI_SUBAGENT_PI_COMMAND = `${process.execPath} ${fakePi}`;
	process.env.PI_SUBAGENT_IDLE_MS = "300";
	process.env.PI_SUBAGENT_TOOL_IDLE_MS = "400"; // shorter than the agent's idle_ms
	const tool = await loadSubagents();
	const res = await tool.execute(
		"t2",
		{ agent: "patient", task: "wait quietly" },
		new AbortController().signal,
		() => {},
		{ cwd: tmp },
	);
	const out = JSON.stringify(res);
	assert.ok(
		!/Timed out: no output/.test(out),
		`idle_ms 30000 must win over a shorter tool budget, got: ${out}`,
	);
});
