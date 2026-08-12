import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

// PR-gate patch test: exercises the real patch scripts against LOCAL fixtures
// vendored from the exact pinned versions (@earendil-works/pi-tui and
// pi-manage-todo-list dist files under tests/fixtures/pi-patches/**), so it
// runs with zero network calls and no `npm install`. The equivalent real
// npm-resolution smoke (install the actual published packages, then patch)
// lives in the release gate (.github/workflows/publish.yml, job
// `patch-smoke`) — see docs there for why a smoke nobody runs is worse than
// none.
//
// Each package also ships a deliberately-broken fixture (pi-tui-broken/,
// pi-manage-todo-list-broken/) whose anchor context no longer matches what
// the patch script expects — simulating upstream drift. This test asserts
// that mismatch is CAUGHT (a loud warning for the non-fatal tui patch, a
// thrown error for the fail-fast todo patch), not silently ignored.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const fixturesRoot = path.join(repoRoot, "tests/fixtures/pi-patches");

function run(command, args, env) {
	return execFileSync(command, args, {
		cwd: repoRoot,
		env,
		encoding: "utf8",
		stdio: ["ignore", "pipe", "pipe"],
		timeout: 10_000,
		killSignal: "SIGKILL",
	});
}

// Captures BOTH stdout and stderr (execFileSync's return value only carries
// stdout on success), for asserting on the patch scripts' console.warn/error
// output either way, without caring whether the script exits 0 or nonzero.
function runCapture(command, args, env) {
	return spawnSync(command, args, {
		cwd: repoRoot,
		env,
		encoding: "utf8",
		timeout: 10_000,
		killSignal: "SIGKILL",
	});
}

function markerCount(file, marker) {
	return fs.readFileSync(file, "utf8").split(marker).length - 1;
}

function copyDir(from, to) {
	fs.mkdirSync(to, { recursive: true });
	fs.cpSync(from, to, { recursive: true });
}

// The pi-tui fixture's single runtime dependency (get-east-asian-width, which
// tui.js's utils.js imports as a bare specifier) is vendored under
// tests/fixtures/pi-patches/pi-tui/vendor-modules/ rather than node_modules/,
// so the fixture stays self-contained in git (node_modules/ is gitignored
// repo-wide and would otherwise be silently dropped from any commit/checkout).
// Materializing it as a real node_modules/ here, only in the disposable temp
// fixture, preserves real Node module resolution for the patch + render
// harness without depending on an ignored path.
function copyTuiFixture(from, to) {
	copyDir(from, to);
	const vendorModules = path.join(to, "vendor-modules");
	if (fs.existsSync(vendorModules)) {
		fs.renameSync(vendorModules, path.join(to, "node_modules"));
	}
}

// The former "Dockerfile and host pin the same pi + curated packages" test
// lived here to catch drift between the Dockerfile's curated pi package loop
// (ARG PI_PACKAGE + the `pi install` list) and services/host/workflow/launch/
// hostrun.go's HostPiPackages, which `pix host setup` used to provision the
// SAME curated set on a bare-metal host. W2/U03B (commit cfd4522) deleted
// host mode entirely — hostrun.go, HostPiPackages, and every caller — so
// there is no longer a second pin for the Dockerfile's to drift against
// (`grep -r HostPiPackages services/` and `grep -r hostrun.go services/`
// both come up empty). The assertion has no current owner to point at; it is
// removed rather than left reading a deleted file. The Dockerfile's own
// PI_PACKAGE/todo-package pins are exercised implicitly by the patch tests
// below, which run the real patch scripts against fixtures vendored from
// those exact pinned versions.

test("tui bottom-pin patch applies to a fixture pi-tui and passes the render harness", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-tui-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const npmPrefix = path.join(temp, "npm-global");
	const tuiRoot = path.join(
		npmPrefix,
		"lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui",
	);
	copyTuiFixture(path.join(fixturesRoot, "pi-tui"), tuiRoot);
	const tuiPath = path.join(tuiRoot, "dist/tui.js");
	const env = {
		...process.env,
		TUI_JS: tuiPath,
		NPM_CONFIG_PREFIX: path.join(temp, "deliberately-wrong-prefix"),
	};

	const tuiPatch = path.join(repoRoot, "scripts/patches/apply-tui-bottom-pin.mjs");
	assert.match(run(process.execPath, [tuiPatch], env), /\[apply-tui-bottom-pin\] patched/);
	assert.match(run(process.execPath, [tuiPatch], env), /already patched/);
	assert.equal(markerCount(tuiPath, "Bottom-block pin"), 1);
	run(process.execPath, ["--check", tuiPath], env);

	for (const [script, result] of [
		["test.mjs", "RESULT: PASS"],
		["edge.mjs", "EDGE RESULT: PASS"],
		["integrity.mjs", "INTEGRITY RESULT: PASS"],
	]) {
		const output = run(
			process.execPath,
			[path.join(repoRoot, "docs/upstream/tui-bottom-pin", script)],
			{ ...env, PI_TUI_INDEX: path.join(tuiRoot, "dist/index.js") },
		);
		assert.match(output, new RegExp(result));
	}
});

test("tui bottom-pin patch catches upstream drift instead of silently applying", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-tui-broken-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const npmPrefix = path.join(temp, "npm-global");
	const tuiRoot = path.join(
		npmPrefix,
		"lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-tui",
	);
	// Start from the good fixture (for the runtime siblings tui.js imports), then
	// swap in the deliberately-broken tui.js whose anchor context no longer
	// matches what the patch script expects.
	copyTuiFixture(path.join(fixturesRoot, "pi-tui"), tuiRoot);
	fs.copyFileSync(
		path.join(fixturesRoot, "pi-tui-broken/dist/tui.js"),
		path.join(tuiRoot, "dist/tui.js"),
	);
	const tuiPath = path.join(tuiRoot, "dist/tui.js");
	const before = fs.readFileSync(tuiPath, "utf8");
	const env = {
		...process.env,
		TUI_JS: tuiPath,
		NPM_CONFIG_PREFIX: path.join(temp, "deliberately-wrong-prefix"),
	};

	const tuiPatch = path.join(repoRoot, "scripts/patches/apply-tui-bottom-pin.mjs");
	// Non-fatal by design (image build must still succeed), but LOUD: the
	// mismatch is reported, and the file is left untouched rather than
	// half-patched. The warning goes to stderr (console.warn), so capture both
	// streams instead of run()'s stdout-only return.
	const { status, stdout, stderr } = runCapture(process.execPath, [tuiPatch], env);
	assert.equal(status, 0, "non-fatal patch mismatch must still exit 0");
	const output = stdout + stderr;
	assert.match(output, /expected renderer context around .* not found/);
	assert.match(output, /pi's renderer changed; refresh scripts\/patches/);
	assert.equal(fs.readFileSync(tuiPath, "utf8"), before, "broken fixture must be left unpatched");
	assert.equal(markerCount(tuiPath, "Bottom-block pin"), 0);
});

test("pi resume patch emits an exact host-runnable pix command", async (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-resume-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const target = path.join(temp, "interactive-mode.js");
	fs.copyFileSync(path.join(fixturesRoot, "pi-resume/interactive-mode.js"), target);
	const env = { ...process.env, PI_INTERACTIVE_MODE: target };
	const patch = path.join(repoRoot, "scripts/patches/apply-pix-resume-command.mjs");

	assert.match(run(process.execPath, [patch], env), /patched/);
	assert.match(run(process.execPath, [patch], env), /already patched/);
	const source = fs.readFileSync(target, "utf8");
	assert.match(source, /PIX_RESUME_COMMAND/);
	assert.match(source, /hostResumeCommand && !sessionManager\.usesDefaultSessionDir\(\)/);
	assert.match(source, /process\.cwd\(\)/);
	assert.equal(markerCount(target, "pix host resume command"), 1);

	const oldCommand = process.env.PIX_RESUME_COMMAND;
	const tty = Object.getOwnPropertyDescriptor(process.stdout, "isTTY");
	process.env.PIX_RESUME_COMMAND = "pix resume";
	Object.defineProperty(process.stdout, "isTTY", { configurable: true, value: true });
	t.after(() => {
		if (oldCommand === undefined) delete process.env.PIX_RESUME_COMMAND;
		else process.env.PIX_RESUME_COMMAND = oldCommand;
		if (tty) Object.defineProperty(process.stdout, "isTTY", tty);
		else delete process.stdout.isTTY;
	});
	const { formatResumeCommand } = await import(`${pathToFileURL(target).href}?patched=1`);
	const sessionManager = {
		isPersisted: () => true,
		getSessionFile: () => target,
		getSessionId: () => "019fd77b-0295-79d5-8411-ef30ac524994",
		usesDefaultSessionDir: () => false,
	};
	assert.equal(
		formatResumeCommand(sessionManager),
		`pix resume 019fd77b-0295-79d5-8411-ef30ac524994 ${process.cwd()}`,
	);
	assert.equal(
		formatResumeCommand({ ...sessionManager, usesDefaultSessionDir: () => true }),
		"pi --session 019fd77b-0295-79d5-8411-ef30ac524994",
		"a manually launched pi with sandbox-local sessions must keep pi's own hint",
	);
});

test("pi resume patch fails when upstream command formatting drifts", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-resume-broken-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const target = path.join(temp, "interactive-mode.js");
	fs.copyFileSync(path.join(fixturesRoot, "pi-resume-broken/interactive-mode.js"), target);
	const env = { ...process.env, PI_INTERACTIVE_MODE: target };
	const patch = path.join(repoRoot, "scripts/patches/apply-pix-resume-command.mjs");
	const { status, stderr } = runCapture(process.execPath, [patch], env);

	assert.notEqual(status, 0);
	assert.match(stderr, /anchor not found/);
});

test("MCP status patch adds a problems-only mode", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-mcp-status-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));
	const target = path.join(temp, "pi-mcp-adapter");
	copyDir(path.join(fixturesRoot, "pi-mcp-status"), target);
	const env = { ...process.env, PI_MCP_ADAPTER_DIR: target };
	const patch = path.join(repoRoot, "scripts/patches/apply-mcp-problems-status.mjs");

	assert.match(run(process.execPath, [patch], env), /patched/);
	assert.match(run(process.execPath, [patch], env), /already patched/);
	assert.match(fs.readFileSync(path.join(target, "types.ts"), "utf8"), /"problems"/);
	const source = fs.readFileSync(path.join(target, "init.ts"), "utf8");
	assert.match(source, /footerStatus === "problems" && problemCount === 0/);
	assert.match(source, /getFailureAgeSeconds\(state, name\) !== null/);
	assert.match(source, /connection\?\.status === "needs-auth"/);
	assert.match(source, /problemCount.*server.*problem/, "the footer must report actual failures, not idle lazy servers");
	assert.doesNotMatch(
		source,
		/footerStatus === "problems" && connectedCount === enabledCount/,
		"a normal lazy or idle connection must not be treated as disconnected",
	);
});

test("MCP status patch upgrades the old disconnected-count behavior", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-mcp-status-v1-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));
	const target = path.join(temp, "pi-mcp-adapter");
	copyDir(path.join(fixturesRoot, "pi-mcp-status-v1"), target);
	const env = { ...process.env, PI_MCP_ADAPTER_DIR: target };
	const patch = path.join(repoRoot, "scripts/patches/apply-mcp-problems-status.mjs");

	assert.match(run(process.execPath, [patch], env), /patched/);
	assert.match(run(process.execPath, [patch], env), /already patched/);
	const source = fs.readFileSync(path.join(target, "init.ts"), "utf8");
	assert.match(source, /footerStatus === "problems" && problemCount === 0/);
	assert.doesNotMatch(source, /servers? disconnected/);
});

test("MCP status patch fails when adapter status rendering drifts", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-mcp-status-broken-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));
	const target = path.join(temp, "pi-mcp-adapter");
	copyDir(path.join(fixturesRoot, "pi-mcp-status-broken"), target);
	const env = { ...process.env, PI_MCP_ADAPTER_DIR: target };
	const patch = path.join(repoRoot, "scripts/patches/apply-mcp-problems-status.mjs");
	const { status, stderr } = runCapture(process.execPath, [patch], env);

	assert.notEqual(status, 0);
	assert.match(stderr, /anchor not found/);
});

test("baked and gateway MCP configs show only connection problems", () => {
	const config = JSON.parse(fs.readFileSync(path.join(repoRoot, "mcp.json"), "utf8"));
	assert.equal(config.settings?.mcpFooterStatus, "problems");
	const kit = fs.readFileSync(path.join(repoRoot, "pi-kit/spec.yaml"), "utf8");
	assert.match(kit, /\{"settings":\{"mcpFooterStatus":"problems"\}/);
	assert.match(
		kit,
		/"--session-dir",\s*"\.pi-sessions"/,
		"the printed workspace must own pi's relative session directory",
	);
});

test("todo controls patch applies to a fixture pi-manage-todo-list", async (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-todo-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const home = path.join(temp, "home");
	const todoRoot = path.join(home, ".pi/agent/npm/node_modules/pi-manage-todo-list");
	copyDir(path.join(fixturesRoot, "pi-manage-todo-list"), todoRoot);
	const env = {
		...process.env,
		HOME: path.join(temp, "deliberately-wrong-home"),
		TODO_DIST: path.join(todoRoot, "dist"),
	};

	const todoPatch = path.join(repoRoot, "scripts/patches/apply-todo-durable-clear.mjs");
	assert.match(run(process.execPath, [todoPatch], env), /patched/);
	assert.match(run(process.execPath, [todoPatch], env), /already patched/);
	const indexPath = path.join(todoRoot, "dist/index.js");
	const stateManagerPath = path.join(todoRoot, "dist/state-manager.js");
	assert.equal(markerCount(indexPath, "pi-stack-todo-cleared"), 1);
	assert.equal(markerCount(stateManagerPath, "pi-stack-todo-cleared"), 1);
	assert.equal(markerCount(indexPath, "pix-todo-cleared"), 0);
	assert.equal(markerCount(stateManagerPath, "pix-todo-cleared"), 1);
	assert.equal(markerCount(indexPath, "pix-todo-widget-visibility"), 1);
	run(process.execPath, ["--check", indexPath], env);
	run(process.execPath, ["--check", stateManagerPath], env);

	const { TodoStateManager } = await import(pathToFileURL(stateManagerPath).href);
	const todo = (title) => ({ id: 1, title, description: title, status: "in-progress" });
	const result = (title) => ({
		type: "message",
		message: {
			role: "toolResult",
			toolName: "manage_todo_list",
			details: { todos: [todo(title)] },
		},
	});
	const clear = { type: "custom", customType: "pi-stack-todo-cleared" };
	const legacyClear = { type: "custom", customType: "pix-todo-cleared" };
	const manager = new TodoStateManager();
	manager.loadFromSession({ sessionManager: { getBranch: () => [result("stale"), clear] } });
	assert.deepEqual(manager.read(), []);
	manager.loadFromSession({ sessionManager: { getBranch: () => [result("stale"), legacyClear] } });
	assert.deepEqual(manager.read(), [], "sessions written by the old patch remain cleared");
	manager.loadFromSession({
		sessionManager: { getBranch: () => [result("stale"), clear, result("new")] },
	});
	assert.equal(manager.read()[0]?.title, "new");

	const createRuntime = async (branch = [result("saved")]) => {
		const handlers = new Map();
		const commands = new Map();
		const shortcuts = new Map();
		const widgets = new Map();
		const notifications = [];
		const entries = [];
		let tool;
		const pi = {
			on(event, handler) {
				const list = handlers.get(event) ?? [];
				list.push(handler);
				handlers.set(event, list);
			},
			registerTool(value) { tool = value; },
			registerCommand(name, value) { commands.set(name, value); },
			registerShortcut(key, value) { shortcuts.set(key, value); },
			appendEntry(customType, data) { entries.push({ type: "custom", customType, data }); },
		};
		const ctx = {
			ui: {
				setWidget(id, value) { value === undefined ? widgets.delete(id) : widgets.set(id, value); },
				notify(message, level) { notifications.push({ message, level }); },
			},
			sessionManager: { getBranch: () => branch },
		};
		const { default: createExtension } = await import(`${pathToFileURL(indexPath).href}?runtime=${Math.random()}`);
		createExtension(pi);
		const fire = async (event) => {
			for (const handler of handlers.get(event) ?? []) await handler({}, ctx);
		};
		return { commands, shortcuts, widgets, notifications, entries, ctx, fire, tool };
	};

	const runtime = await createRuntime();
	await runtime.fire("session_start");
	assert.ok(runtime.widgets.has("todo-list"));
	assert.equal(runtime.shortcuts.has("alt+t"), true);

	await runtime.commands.get("todos").handler("hide", runtime.ctx);
	assert.equal(runtime.widgets.has("todo-list"), false);
	assert.equal(runtime.entries.at(-1)?.customType, "pix-todo-widget-visibility");
	assert.deepEqual(runtime.tool.read(), [todo("saved")]);
	await runtime.fire("turn_end");
	assert.equal(runtime.widgets.has("todo-list"), false, "turn_end must not resurrect a hidden widget");

	await runtime.commands.get("todos").handler("show", runtime.ctx);
	assert.ok(runtime.widgets.has("todo-list"));
	await runtime.commands.get("todos").handler("", runtime.ctx);
	assert.equal(runtime.widgets.has("todo-list"), false, "no arguments toggle visibility");
	await runtime.shortcuts.get("alt+t").handler(runtime.ctx);
	assert.ok(runtime.widgets.has("todo-list"), "Alt+T toggles visibility");

	const restored = await createRuntime([
		result("saved"),
		{ type: "custom", customType: "pix-todo-widget-visibility", data: { visible: false } },
	]);
	await restored.fire("session_start");
	assert.equal(restored.widgets.has("todo-list"), false, "hidden visibility survives reload");
	assert.deepEqual(restored.tool.read(), [todo("saved")]);

	await runtime.commands.get("todos").handler("clear", runtime.ctx);
	assert.equal(runtime.entries.at(-1)?.customType, "pi-stack-todo-cleared");
});

test("todo durable-clear patch catches upstream drift instead of silently applying", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-todo-broken-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));

	const home = path.join(temp, "home");
	const todoRoot = path.join(home, ".pi/agent/npm/node_modules/pi-manage-todo-list");
	copyDir(path.join(fixturesRoot, "pi-manage-todo-list-broken"), todoRoot);
	const env = {
		...process.env,
		HOME: path.join(temp, "deliberately-wrong-home"),
		TODO_DIST: path.join(todoRoot, "dist"),
	};

	const todoPatch = path.join(repoRoot, "scripts/patches/apply-todo-durable-clear.mjs");
	// This patch has no non-fatal fallback — a context mismatch throws, so a
	// deliberately broken fixture must fail the process rather than corrupt
	// the file or silently skip.
	const { status, stderr } = runCapture(process.execPath, [todoPatch], env);
	assert.notEqual(status, 0, "a context mismatch must fail the process, not silently skip");
	assert.match(stderr, /anchor not found in .*state-manager\.js/);
});

// --- hide the trusted host-state block ---------------------------------------
//
// The [pix-trusted-host-state] block has to be IN the generated onboarding
// prompt — that is the entire mechanism by which host facts reach the fenced
// agent — but it must not be the first thing a new user reads. Mark reported the
// raw JSON twice, the second time as "again this goofy json".
//
// These tests run the PATCHED code rather than grepping it, because the whole
// risk of a display-only strip is that it also eats something it should not.

test("host-state patch hides the block from display but keeps it for the model", async (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-hoststate-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));
	const target = path.join(temp, "interactive-mode.js");
	fs.copyFileSync(
		path.join(fixturesRoot, "pi-hoststate/interactive-mode.js"),
		target,
	);
	const env = { ...process.env, PI_INTERACTIVE_MODE: target };
	const patch = path.join(repoRoot, "scripts/patches/apply-hide-host-state.mjs");

	assert.match(run(process.execPath, [patch], env), /patched/);
	assert.match(run(process.execPath, [patch], env), /already patched/);

	const { InteractiveMode } = await import(`file://${target}`);
	const state = '{"provisioned":true,"keys":{"anthropic":false,"openai":false}}';
	const generated =
		"[pix-generated:onboarding] I just ran pix setup. Give me the upfront guide.\n\n" +
		`[pix-trusted-host-state]\n${state}\n[/pix-trusted-host-state]`;

	const m = new InteractiveMode(generated);
	m.addMessageToChat({ role: "user" }, { populateHistory: true });
	assert.equal(
		m.rendered,
		"[pix-generated:onboarding] I just ran pix setup. Give me the upfront guide.",
		"the JSON block must not reach the chat component",
	);
	assert.ok(
		!m.history.some((h) => h.includes("provisioned")),
		"and it must not land in the editor history either — nobody wants to arrow up into a JSON blob",
	);

	// An ordinary prompt is untouched.
	const plain = new InteractiveMode("what does snow do?");
	plain.addMessageToChat({ role: "user" });
	assert.equal(plain.rendered, "what does snow do?");

	// A user who TYPES the tag while asking about it keeps their whole message.
	// Stripping on the tag alone silently truncated this to "why does".
	const asking = new InteractiveMode(
		"why does [pix-trusted-host-state] show up? [/pix-trusted-host-state]",
	);
	asking.addMessageToChat({ role: "user" });
	assert.equal(
		asking.rendered,
		"why does [pix-trusted-host-state] show up? [/pix-trusted-host-state]",
		"only pix's own generated prompt may be stripped",
	);

	// A block with no closing tag stays VISIBLE: truncating there would hide a
	// malformed payload instead of surfacing it.
	const broken = new InteractiveMode(
		'[pix-generated:onboarding] hi\n\n[pix-trusted-host-state]\n{"a":1',
	);
	broken.addMessageToChat({ role: "user" });
	assert.ok(
		broken.rendered.includes('{"a":1'),
		"an unterminated block must not be silently swallowed",
	);
});

test("host-state patch catches upstream drift instead of silently applying", (t) => {
	const temp = fs.mkdtempSync(path.join(os.tmpdir(), "pix-patches-hoststate-broken-"));
	t.after(() => fs.rmSync(temp, { recursive: true, force: true }));
	const target = path.join(temp, "interactive-mode.js");
	fs.copyFileSync(
		path.join(fixturesRoot, "pi-hoststate-broken/interactive-mode.js"),
		target,
	);
	const env = { ...process.env, PI_INTERACTIVE_MODE: target };
	const patch = path.join(repoRoot, "scripts/patches/apply-hide-host-state.mjs");

	// FATAL, not a warning. The failure mode of a silent skip is the blob coming
	// back on a pi upgrade, which is the exact thing this patch exists to stop.
	assert.throws(() => run(process.execPath, [patch], env), /anchor not found/);
});
