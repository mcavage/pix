// verify-pix-lifecycle-uat.test.mjs — the shell/static half of proving
// scripts/macos/verify-pix-lifecycle.sh (docs/HOST-UAT.md's machine-checkable
// artifact) does not lie to itself. The script needs a real host/sbx/docker
// to run end to end, so this file cannot exercise it live; it instead proves
// the SHAPE of the fixes that closed known false-failure modes, the same way
// tests/ci-gate.test.mjs asserts shapes of scripts/gate.sh without running a
// full CI matrix.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const scriptPath = new URL("../scripts/macos/verify-pix-lifecycle.sh", import.meta.url);
const script = fs.readFileSync(scriptPath, "utf8");

// extractFn NAME — the literal `NAME() { ... }` block from the real script,
// so helper-behavior tests below exercise the ACTUAL function text (not a
// reimplementation that could quietly drift from it).
function extractFn(name) {
	const re = new RegExp(`\\n${name}\\(\\) \\{[\\s\\S]*?\\n\\}\\n`);
	const m = script.match(re);
	assert.ok(m, `could not find function ${name}() in the script`);
	return m[0];
}

test("the script is syntactically valid bash (bash -n)", () => {
	// A real, cheap static check every environment can run: no host, no sbx,
	// no docker, just the shell's own parser.
	execFileSync("bash", ["-n", scriptPath.pathname], { stdio: "pipe" });
});

test("shellcheck reports no ERROR-severity findings, when shellcheck is available", (t) => {
	let out;
	try {
		out = execFileSync("shellcheck", ["--version"], { stdio: "pipe" });
	} catch {
		t.skip("shellcheck is not on PATH in this environment — not asserted here, run it in CI");
		return;
	}
	assert.ok(out.length > 0);
	// -S error: this script deliberately embeds long human-facing prose in
	// printf/fail() strings that legitimately trip shellcheck's STYLE/INFO
	// heuristics (e.g. quoting inside a message). Only an ERROR is a real bug.
	execFileSync("shellcheck", ["-S", "error", scriptPath.pathname], { stdio: "pipe" });
});

test("the OAuth pass asserts pix mcp auth --all's own exit code, not just fires it", () => {
	assert.match(script, /assert_exit 0 "pix mcp auth --all completed" pix mcp auth --all/);
});

test("OAuth completion is certified by a machine-readable probe (pix doctor --json), not operator say-so", () => {
	assert.match(script, /DOCTOR_JSON="\$\(pix doctor --json/);
	assert.match(script, /registered, not authenticated/);
});

test("the optional human confirmation reads a bounded /dev/tty, never the script's own stdin", () => {
	assert.match(script, /-r \/dev\/tty.*-w \/dev\/tty/);
	assert.match(script, /read -r -t 30 ans <\/dev\/tty/);
});

test("optional operator confirmation cannot make machine-verified OAuth incomplete or failed", () => {
	const oauthSection = script.slice(script.indexOf("[8] External OAuth"));
	assert.match(oauthSection, /no optional operator answer within 30s/);
	assert.match(oauthSection, /no controlling TTY for optional confirmation/);
	assert.doesNotMatch(oauthSection, /skip "operator confirmation"/);
	assert.doesNotMatch(oauthSection, /fail "operator confirmation"/);
	assert.doesNotMatch(oauthSection, /fail "remote OAuth flows"/);
});

test("the bare 'attached' substring check is gone (it always tripped on mcp ls's own honest disclaimer)", () => {
	assert.doesNotMatch(script, /assert_not_contains "attached"/);
	assert.doesNotMatch(script, /\bassert_not_contains\(\)/, "the bare-substring helper itself should be retired, not just its one flawed call site");
});

test("mcp ls is checked for the honest disclaimer (positive claim) and a precise negative attachment-claim regex", () => {
	assert.match(script, /assert_contains "not what's attached to" "mcp ls prints the honest host-registration disclaimer" pix mcp ls/);
	assert.match(script, /grep -qE "\[Ii\]s \(now \|currently \)\?attached to \(your\|this\|the running\) sandbox\|\[Ss\]ession \(is \)\?attached"/);
});

test("a registered MCP catalog bundle this run added is tracked and restored on exit", () => {
	assert.match(script, /MCP_BUNDLE_ADDED=0/);
	assert.match(script, /MCP_CATALOG_BUNDLE="pix-catalog"/);
	assert.match(script, /MCP_BUNDLE_ADDED=1/);
	const cleanup = script.slice(script.indexOf("cleanup() {"), script.indexOf("verdict() {"));
	assert.match(cleanup, /pix mcp bundle rm "\$MCP_CATALOG_BUNDLE"/);
});

test("a pre-existing catalog bundle is verified as a pass and left unchanged", () => {
	assert.match(script, /pass "mcp bundle is already registered \(pre-existing host state; left unchanged\)"/);
});

test("backgrounded pix run invocations redirect stdin from \\/dev\\/null", () => {
	const multiShell = script.slice(script.indexOf("[5] Multi-shell"), script.indexOf("[6]"));
	const backgrounded = multiShell.split("\n").filter((l) => l.trim().endsWith("&"));
	assert.ok(backgrounded.length >= 2, "expected at least the two backgrounded pix run lines in section 5");
	for (const line of backgrounded) {
		assert.match(line, /pix run/);
		assert.match(line, /<\/dev\/null/, `backgrounded line does not redirect stdin from /dev/null: ${line}`);
	}
});

test("every background wait is bounded, so a wedged pix run cannot hang the whole UAT run", () => {
	assert.match(script, /bounded_wait\(\) \{/);
	const multiShell = script.slice(script.indexOf("[5] Multi-shell"), script.indexOf("[6]"));
	assert.doesNotMatch(multiShell, /\bwait \$SH1\b/);
	assert.doesNotMatch(multiShell, /\bwait \$SH2\b/);
	assert.match(multiShell, /bounded_wait "\$SH1" 60/);
	assert.match(multiShell, /bounded_wait "\$SH2" 60/);
});

test("host services preflight WHICH pix-host binary an already-running serve is before trusting it", () => {
	const hostSection = script.slice(script.indexOf("[7] Host services"), script.indexOf("[8] External OAuth"));
	assert.match(hostSection, /current_bin_path pix-host/);
	assert.match(hostSection, /running_bin_path "\$RUNNING_PID"/);
	// Any pre-existing daemon blocks a clean launchd lifecycle test, even if it is
	// the current binary; the rerun must install and own the managed service.
	assert.match(hostSection, /clean UAT must install and exercise the launchd-managed service itself/);
	assert.match(hostSection, /die "a serve is already running.*pix serve stop.*re-run this script/s);
	assert.match(hostSection, /cannot prove which binary would be tested.*pix serve stop/s);
	assert.doesNotMatch(hostSection, /skip "which binary the running serve is"/);
});

test("installing serve for this run is reported as reversible and names the binary under test", () => {
	const hostSection = script.slice(script.indexOf("[7] Host services"), script.indexOf("[8] External OAuth"));
	assert.match(hostSection, /pass "installed and started serve from the current build \(\$CUR_HOSTBIN\).*reversible: uninstalled on exit"/);
});

test("the destructive-flag self-check still finds nothing in the rewritten script", () => {
	// Exercises the exact SELFCHECK regex the script runs on itself, so a
	// future edit that reintroduces a blast-radius flag is caught here too,
	// not only at UAT runtime.
	const executable = script
		.split("\n")
		.filter((l) => !/^\s*#/.test(l) && !/SELFCHECK/.test(l))
		.join("\n");
	assert.doesNotMatch(executable, /(^|[^-])pix rm ([^|]*)(--all|--force|[ \t]-f([ \t]|$))/m);
});

test("docs/HOST-UAT.md documents the machine-probe OAuth verdict and the binary preflight", () => {
	const doc = fs.readFileSync(new URL("../docs/HOST-UAT.md", import.meta.url), "utf8");
	assert.match(doc, /CERTIFIED against a machine-readable probe/);
	assert.match(doc, /preflight WHICH `pix-host` binary/);
	assert.match(doc, /bounded `read -t` from `\/dev\/tty`/);
});

// --- OAuth evidence classification: static shape + behavioral proof ------------
// The false-PASS this closes: the old code matched the bare substring "$s:",
// which is present in EVERY health/mcp.go note ("not registered", "registration
// unknown", "auth not checkable from here", ...), so anything but the literal
// "not authenticated" string silently PASSED. These tests pin both the static
// shape of the fix and its actual runtime behavior against fabricated evidence.

test("OAuth classification only PASSes on the exact registered+attachmentCaveat evidence line", () => {
	const oauthSection = script.slice(script.indexOf("[8] External OAuth"));
	assert.match(oauthSection, /grep -qF "\$s: registered \(host registration"/);
	assert.match(oauthSection, /pass "catalog server \$s authenticated \(pix doctor --json evidence\)"/);
});

test("OAuth classification explicitly FAILs on 'not registered', separately from 'not authenticated'", () => {
	const oauthSection = script.slice(script.indexOf("[8] External OAuth"));
	assert.match(oauthSection, /grep -qF "\$s: not registered"/);
	assert.match(oauthSection, /fail "catalog server \$s authenticated" "pix doctor --json reports it NOT REGISTERED/);
	assert.match(oauthSection, /grep -qF "\$s: registered, not authenticated"/);
	assert.match(oauthSection, /fail "catalog server \$s authenticated" "pix doctor --json still reports it unauthenticated/);
});

test("OAuth classification no longer matches on the bare '$s:' substring (the false-PASS this rewrite closes)", () => {
	const oauthSection = script.slice(script.indexOf("[8] External OAuth"));
	// The old flawed elif: `grep -qF "$s:"` with no further qualifier.
	assert.doesNotMatch(oauthSection, /grep -qF "\$s:"[^a-zA-Z(]/);
});

// extractOAuthClassifyLoop — the literal `for s in ...; do ... done` block that
// classifies each server's doctor evidence, so the behavioral tests below run
// the ACTUAL shipped logic, not a reimplementation of it.
function extractOAuthClassifyLoop() {
	const forIdx = script.lastIndexOf("for s in notion atlassian granola; do");
	assert.ok(forIdx !== -1, "could not find the OAuth classify loop's 'for s in ...' line");
	const doneMarker = "\n  done\n";
	const doneIdx = script.indexOf(doneMarker, forIdx);
	assert.ok(doneIdx !== -1, "could not find the OAuth classify loop's closing 'done'");
	return script.slice(forIdx, doneIdx + doneMarker.length);
}

// runOAuthClassification DOCTOR_JSON — runs the extracted loop under stub
// pass/fail/skip functions that just print their verdict, so the test reads
// the loop's real decision without depending on the rest of the script.
function runOAuthClassification(doctorJson) {
	const loop = extractOAuthClassifyLoop();
	const harness = [
		"set -uo pipefail",
		'pass() { printf "PASS:%s\\n" "$1"; }',
		'fail() { printf "FAIL:%s\\n" "$1"; }',
		'skip() { printf "SKIP:%s\\n" "$1"; }',
		loop,
	].join("\n");
	return execFileSync("bash", ["-c", harness], {
		encoding: "utf8",
		env: { ...process.env, DOCTOR_JSON: doctorJson },
	});
}

test("OAuth classification (behavioral): explicit ready/authenticated evidence is the only PASS", () => {
	const doctorJson =
		"notion: registered (host registration; attachment to a live session is not checkable from here); " +
		"atlassian: not registered; " +
		"granola: registered, not authenticated";
	const out = runOAuthClassification(doctorJson);
	assert.match(out, /PASS:catalog server notion authenticated/);
	assert.match(out, /FAIL:catalog server atlassian authenticated/);
	assert.match(out, /FAIL:catalog server granola authenticated/);
	assert.doesNotMatch(out, /PASS:catalog server atlassian/);
	assert.doesNotMatch(out, /PASS:catalog server granola/);
});

test("OAuth classification (behavioral): unknown or missing evidence is SKIP, never a silent PASS or a FAIL", () => {
	const doctorJson =
		"notion: registration unknown; " +
		"atlassian: registered, auth not checkable from here";
	// granola has no evidence line at all in this payload.
	const out = runOAuthClassification(doctorJson);
	assert.match(out, /SKIP:catalog server notion auth/);
	assert.match(out, /SKIP:catalog server atlassian auth/);
	assert.match(out, /SKIP:catalog server granola auth/);
	assert.doesNotMatch(out, /PASS:/);
	assert.doesNotMatch(out, /FAIL:/);
});

test("OAuth classification (behavioral): a totally empty doctor payload is SKIP for every server, not a free PASS", () => {
	const out = runOAuthClassification("");
	assert.strictEqual((out.match(/SKIP:/g) || []).length, 3);
	assert.doesNotMatch(out, /PASS:/);
	assert.doesNotMatch(out, /FAIL:/);
});

// --- current_bin_path / running_bin_path: portable symlink resolution ----------
// AGENTS.md invariant: a make-install symlink and the real executable lsof
// reports for an already-running process must compare EQUAL, on macOS, where
// neither `readlink -f` nor `realpath` can be assumed to exist.

test("resolve_symlink_final follows a relative multi-hop symlink chain through directories containing spaces", () => {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-symlink-"));
	try {
		const realDir = path.join(work, "real dir");
		const midDir = path.join(work, "mid link dir");
		const binDir = path.join(work, "bin");
		fs.mkdirSync(realDir);
		fs.mkdirSync(midDir);
		fs.mkdirSync(binDir);
		const realFile = path.join(realDir, "pix-host");
		fs.writeFileSync(realFile, "#!/bin/sh\necho real\n", { mode: 0o755 });
		// Two relative hops: bin/pix-host -> ../mid link dir/mid-link -> ../real dir/pix-host
		fs.symlinkSync(path.join("..", "real dir", "pix-host"), path.join(midDir, "mid-link"));
		fs.symlinkSync(path.join("..", "mid link dir", "mid-link"), path.join(binDir, "pix-host"));

		const fns = [extractFn("resolve_symlink_final"), extractFn("abs_path")].join("\n");
		const run = (p) =>
			execFileSync("bash", ["-c", `${fns}\nabs_path "$1"`, "bash", p], { encoding: "utf8" }).trim();

		const chainResult = run(path.join(binDir, "pix-host"));
		const directResult = run(realFile);
		const expected = fs.realpathSync(realFile);
		assert.strictEqual(chainResult, expected);
		assert.strictEqual(directResult, expected);
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
});

test("current_bin_path resolves a make-install-style PATH symlink to the real executable's path", () => {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-current-"));
	try {
		const realDir = path.join(work, "Cellar-1.2.3");
		const binDir = path.join(work, "bin");
		fs.mkdirSync(realDir);
		fs.mkdirSync(binDir);
		const realFile = path.join(realDir, "pix-host");
		fs.writeFileSync(realFile, "#!/bin/sh\necho real\n", { mode: 0o755 });
		fs.symlinkSync(path.join("..", "Cellar-1.2.3", "pix-host"), path.join(binDir, "pix-host"));

		const fns = [extractFn("resolve_symlink_final"), extractFn("abs_path"), extractFn("current_bin_path")].join(
			"\n",
		);
		const out = execFileSync("bash", ["-c", `PATH="$1:$PATH"\n${fns}\ncurrent_bin_path pix-host`, "bash", binDir], {
			encoding: "utf8",
		}).trim();
		assert.strictEqual(out, fs.realpathSync(realFile));
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
});

test("current_bin_path (PATH symlink) and running_bin_path (lsof-reported real file) compare equal", () => {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-lsof-"));
	try {
		const realDir = path.join(work, "real");
		const binDir = path.join(work, "bin");
		const toolsDir = path.join(work, "tools");
		fs.mkdirSync(realDir);
		fs.mkdirSync(binDir);
		fs.mkdirSync(toolsDir);
		const realFile = path.join(realDir, "pix-host");
		fs.writeFileSync(realFile, "#!/bin/sh\necho real\n", { mode: 0o755 });
		fs.symlinkSync(path.join("..", "real", "pix-host"), path.join(binDir, "pix-host"));
		const resolvedReal = fs.realpathSync(realFile);

		// A stub `lsof` that answers exactly the one line running_bin_path's awk
		// cares about ($4 == "txt", NAME == $NF): the fully-resolved real path, the
		// same shape a live kernel reports for an already-running process (never a
		// symlink), regardless of what pid is asked for.
		const fakeLsof = path.join(toolsDir, "lsof");
		fs.writeFileSync(
			fakeLsof,
			"#!/bin/sh\n" +
				"printf 'COMMAND   PID USER   FD   TYPE DEVICE SIZE/OFF   NODE NAME\\n'\n" +
				`printf 'pix-host  999 user   txt   REG    1,4    12345 678901 %s\\n' '${resolvedReal}'\n`,
			{ mode: 0o755 },
		);

		const fns = [
			extractFn("resolve_symlink_final"),
			extractFn("abs_path"),
			extractFn("current_bin_path"),
			extractFn("running_bin_path"),
		].join("\n");
		const harness = `PATH="$1:$2:$PATH"\n${fns}\nprintf 'CUR=%s\\nRUN=%s\\n' "$(current_bin_path pix-host)" "$(running_bin_path 999)"`;
		const out = execFileSync("bash", ["-c", harness, "bash", binDir, toolsDir], { encoding: "utf8" });
		const cur = /CUR=(.*)/.exec(out)?.[1];
		const run = /RUN=(.*)/.exec(out)?.[1];
		assert.ok(cur, `current_bin_path produced no output: ${out}`);
		assert.ok(run, `running_bin_path produced no output: ${out}`);
		assert.strictEqual(cur, resolvedReal);
		assert.strictEqual(run, resolvedReal);
		assert.strictEqual(
			cur,
			run,
			"a make-install symlink (current_bin_path) and lsof's reported real executable (running_bin_path) must compare equal",
		);
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
});
