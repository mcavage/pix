// Unit tests for extensions/host-guard.ts (the host-mode accident guard).
// Run: node --test tests/
//
// The extension exports its pure, root-parameterized logic exactly so these
// tests can prove the containment/parse behavior without a pi process. The
// module's own WORKSPACE_ROOT binding is exercised via the factory test at the
// bottom (registered tool_call handler end-to-end).
import assert from "node:assert";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";

const guard = await import("../extensions/host-guard.ts");
const {
	checkWriteEditPath,
	computeWorkspaceRoot,
	extractRmTargets,
	isInsideRoot,
	matchIrreversibleFor,
	resolvePath,
	rmRfHitFor,
} = guard;

// A real (symlink-resolved) workspace + a real outside dir, with a symlink
// from inside the workspace pointing out — the C2 escape shape.
function makeWorkspace(t) {
	const ws = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "hg-ws-")));
	const outside = fs.realpathSync(
		fs.mkdtempSync(path.join(os.tmpdir(), "hg-out-")),
	);
	fs.symlinkSync(outside, path.join(ws, "outside"));
	t.after(() => {
		fs.rmSync(ws, { recursive: true, force: true });
		fs.rmSync(outside, { recursive: true, force: true });
	});
	return { ws, outside };
}

// ── C2: write path jail must survive a symlinked parent ─────────────────────
test("C2: write through a symlinked parent to a NEW file is blocked", (t) => {
	const { ws, outside } = makeWorkspace(t);
	// The file does not exist yet, so realpathSync(target) throws — the old
	// lexical fallback said ws/outside/new-file is "inside". The parent walk
	// must resolve it to <outside-real>/new-file.
	const r = checkWriteEditPath("outside/new-file.txt", ws, ws);
	assert.equal(r.block, true, `resolved to ${r.resolved}`);
	assert.ok(r.resolved.startsWith(outside + path.sep));
	// Deeper nonexistent suffix under the symlinked parent: still blocked.
	assert.equal(checkWriteEditPath("outside/a/b/new.txt", ws, ws).block, true);
});

test("C2: a genuinely-in-workspace NEW file is allowed", (t) => {
	const { ws } = makeWorkspace(t);
	assert.equal(checkWriteEditPath("brand-new.txt", ws, ws).block, false);
	// Nonexistent intermediate dirs, still lexically+really inside the root.
	assert.equal(
		checkWriteEditPath("newdir/sub/new-file.txt", ws, ws).block,
		false,
	);
	assert.equal(
		checkWriteEditPath(path.join(ws, "abs-new.txt"), ws, ws).block,
		false,
	);
	// And an existing outside path (realpath branch) is still blocked.
	assert.equal(checkWriteEditPath(os.tmpdir(), ws, ws).block, true);
	// `..` escape is blocked even when the target does not exist.
	assert.equal(checkWriteEditPath("../escape-new.txt", ws, ws).block, true);
});

test("C2: resolvePath walks up to the nearest existing ancestor", (t) => {
	const { ws, outside } = makeWorkspace(t);
	assert.equal(
		resolvePath("outside/x/y/z.txt", ws),
		path.join(outside, "x", "y", "z.txt"),
	);
	assert.equal(resolvePath("nope/z.txt", ws), path.join(ws, "nope", "z.txt"));
});

// ── H2: rm -rf target parsing fails closed on anything non-literal ──────────
test("H2: quoted / expanded / substituted / glob rm -rf targets fail closed", (t) => {
	const { ws } = makeWorkspace(t);
	// Each of the review's exact bypasses must produce a hit (confirm-or-block).
	for (const cmd of [
		'rm -rf "/"', // quoted root — stripped to a literal, resolves outside
		'rm -rf "$HOME"', // env expansion
		"rm -rf ../sibling", // relative escape
		"rm -rf $(find / -name x)", // command substitution
		"rm -rf `echo /`", // backticks
		"rm -rf /tmp/*", // glob
		"rm -rf build/*", // glob even inside the workspace: unknown expansion
		"rm -rf foo\\ bar", // backslash escape
		'rm -rf "unbalanced', // stray quote
	]) {
		assert.ok(rmRfHitFor(cmd, ws), `expected a hit for: ${cmd}`);
	}
});

test("H2: plain literal in-workspace rm -rf targets still pass", (t) => {
	const { ws } = makeWorkspace(t);
	assert.equal(rmRfHitFor("rm -rf build", ws), null);
	assert.equal(rmRfHitFor("rm -rf ./node_modules dist", ws), null);
	// Matched quotes around a plain literal are stripped, not failed.
	assert.equal(rmRfHitFor('rm -rf "build"', ws), null);
	assert.equal(rmRfHitFor("rm -rf 'dist'", ws), null);
	// …and the same stripping still catches an out-of-workspace literal.
	assert.ok(rmRfHitFor('rm -rf "/etc"', ws));
	// A bare rm -rf with no target is unknown scope.
	assert.ok(rmRfHitFor("rm -rf", ws));
	// extractRmTargets itself reports the split.
	assert.deepEqual(extractRmTargets('rm -rf "build" dist'), {
		targets: ["build", "dist"],
		unsafe: false,
	});
	assert.equal(extractRmTargets('rm -rf "$HOME"').unsafe, true);
});

// ── H3: rc-file rule catches cp/mv/sed, not just redirection ────────────────
test("H3: any command naming an rc file / sandbox-persistent.sh is a hit", (t) => {
	const { ws } = makeWorkspace(t);
	for (const cmd of [
		"cp payload ~/.bashrc",
		"mv payload ~/.zshrc",
		"sed -i 's/a/b/' ~/.profile",
		"echo hi >> ~/.bashrc", // the old redirection case still hits
		"tee -a ~/.bash_profile < payload",
		"cp x /etc/sandbox-persistent.sh",
		"install -m 644 payload /etc/profile",
	]) {
		const hit = matchIrreversibleFor(cmd, ws);
		assert.ok(hit, `expected a hit for: ${cmd}`);
	}
	assert.equal(matchIrreversibleFor("echo hello", ws), null);
	assert.equal(matchIrreversibleFor("git status", ws), null);
});

// ── M4: unavailable cwd fails closed to a blocking sentinel ─────────────────
test("M4: missing cwd yields a sentinel root that blocks every write", () => {
	const sentinel = computeWorkspaceRoot("");
	assert.ok(sentinel.includes("\0"), "sentinel must be an impossible path");
	assert.equal(isInsideRoot("/tmp/anything", sentinel), false);
	assert.equal(checkWriteEditPath("/tmp/x", "/", sentinel).block, true);
	assert.equal(checkWriteEditPath("relative.txt", "/", sentinel).block, true);
	// A real cwd resolves normally.
	assert.equal(computeWorkspaceRoot(os.tmpdir()), fs.realpathSync(os.tmpdir()));
});

// ── M1: a failed guard registration must not be swallowed ───────────────────
test("M1: factory rethrows when pi.on fails (never silently unguarded)", () => {
	assert.throws(() =>
		guard.default({
			on() {
				throw new Error("registration boom");
			},
		}),
	);
});

// ── end-to-end: the registered tool_call handler enforces the jail ──────────
test("registered handler blocks outside writes and headless irreversible bash", async () => {
	let handler = null;
	guard.default({
		on(event, fn) {
			if (event === "tool_call") handler = fn;
		},
	});
	assert.ok(handler, "tool_call handler registered");
	const ctx = { cwd: process.cwd(), hasUI: false };
	// Outside write: blocked without a prompt.
	const w = await handler(
		{ toolName: "write", input: { path: "/etc/host-guard-test" } },
		ctx,
	);
	assert.equal(w?.block, true);
	// Inside write: allowed.
	assert.equal(
		await handler(
			{ toolName: "write", input: { path: "README.md" } },
			ctx,
		),
		undefined,
	);
	// Irreversible bash with no UI: fail closed.
	const b = await handler(
		{ toolName: "bash", input: { command: "sudo rm -rf /" } },
		ctx,
	);
	assert.equal(b?.block, true);
	// Reads always pass.
	assert.equal(
		await handler({ toolName: "read", input: { path: "/etc/passwd" } }, ctx),
		undefined,
	);
});
