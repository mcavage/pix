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

test("the OAuth pass authorizes each shipped catalog server individually, asserting its own exit code", () => {
	assert.match(script, /assert_exit 0 "pix mcp auth \$s completed" pix mcp auth "\$s"/);
	assert.match(script, /for s in notion atlassian granola; do\n\s*assert_exit 0 "pix mcp auth \$s completed"/);
});

test("the OAuth pass never sweeps in every registered server via --all (an unrelated 8th server must not fail this release check)", () => {
	// A rationale COMMENT is allowed to name the shape it avoids; only
	// executable lines must never actually invoke it.
	const executable = script
		.split("\n")
		.filter((l) => !/^\s*#/.test(l))
		.join("\n");
	assert.doesNotMatch(executable, /pix mcp auth --all/);
});

test("OAuth completion is certified by a machine-readable probe (pix doctor --json), not operator say-so", () => {
	assert.match(script, /DOCTOR_JSON="\$\(pix doctor --json/);
	assert.match(script, /registered, not authenticated/);
});

test("the optional human confirmation reads a bounded /dev/tty via a real fd open, never the script's own stdin", () => {
	assert.match(script, /exec 3<>\/dev\/tty 2>\/dev\/null/);
	assert.match(script, /read -r -t 30 ans <&3 2>\/dev\/null/);
});

test("the /dev/tty open suppresses device-open noise instead of relying on a -r/-w stat check that can lie", () => {
	// /dev/tty can fail to OPEN (ENXIO, no controlling terminal) even when its
	// stat permission bits pass -r/-w — a bare `[ -r /dev/tty ] && [ -w /dev/tty ]`
	// check is not the real test; the open attempt itself, with stderr
	// suppressed, is.
	assert.doesNotMatch(script, /if \[ -r \/dev\/tty \] && \[ -w \/dev\/tty \]/);
	const oauthSection = script.slice(script.indexOf("[9] External OAuth"));
	assert.match(oauthSection, />&3 2>\/dev\/null/);
});

test("optional operator confirmation cannot make machine-verified OAuth incomplete or failed", () => {
	const oauthSection = script.slice(script.indexOf("[9] External OAuth"));
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

test("only catalog registrations this run actually added are removed on cleanup", () => {
	assert.match(script, /MCP_ADDED_NAMES=\(\)/);
	assert.match(script, /MISSING_CATALOG=\(\)/);
	assert.match(script, /MCP_ADDED_NAMES\+=\("\$s"\)/);
	const cleanup = script.slice(script.indexOf("cleanup() {"), script.indexOf("verdict() {"));
	assert.match(cleanup, /for s in "\$\{MCP_ADDED_NAMES\[@\]:-\}"/);
	assert.match(cleanup, /sbx mcp rm "\$s"/);
	assert.doesNotMatch(cleanup, /pix mcp bundle rm/);
});

test("pre-existing catalog servers are verified individually and left unchanged", () => {
	assert.match(script, /pass "shipped catalog servers are already registered \(pre-existing host state; left unchanged\)"/);
	assert.match(script, /for s in notion atlassian granola/);
});

test("backgrounded pix run invocations read stdin from their OWN hold FIFO, never /dev/null or an inherited terminal", () => {
	// The prompt-based hold this replaces read from /dev/null (a `-p` prompt
	// needs no further input, so /dev/null was a safe sentinel then). A FIFO
	// hold's whole point is that stdin stays open with nothing queued, so
	// /dev/null (immediate EOF) or an inherited terminal (real keystrokes) are
	// both wrong here — each backgrounded session must read its own named FIFO.
	const multiShell = script.slice(script.indexOf("[6] Multi-shell"), script.indexOf("[7] --keep"));
	const backgrounded = multiShell.split("\n").filter((l) => l.trim().endsWith("&") && /pix run/.test(l));
	assert.ok(backgrounded.length >= 2, "expected at least the two backgrounded pix run lines in section 6");
	for (const line of backgrounded) {
		assert.match(line, /<"\$FIFO[12]"/, `backgrounded line does not read its own hold FIFO: ${line}`);
		assert.doesNotMatch(line, /<\/dev\/null/, `backgrounded line still reads /dev/null instead of a hold FIFO: ${line}`);
	}
});

test("each backgrounded hold closes both hold FDs for ITSELF first, so the second job cannot inherit the first job's open write end", () => {
	// The deadlock this closes: fd 5 (opened by THIS script before the second
	// job is backgrounded) would otherwise be inherited into that second job's
	// own subshell process, which never execs it away — a silent extra writer
	// that keeps FIFO1 from ever reaching EOF no matter how deterministically
	// this script later closes its OWN fd 5.
	const multiShell = script.slice(script.indexOf("[6] Multi-shell"), script.indexOf("[7] --keep"));
	const backgrounded = multiShell.split("\n").filter((l) => l.trim().endsWith("&") && /pix run/.test(l));
	assert.strictEqual(backgrounded.length, 2, `expected exactly two backgrounded pix run lines, got ${backgrounded.length}`);
	for (const line of backgrounded) {
		assert.match(line, /^\(exec 5>&- 6>&-;/, `backgrounded hold does not close fds 5/6 for itself first: ${line}`);
	}
});

test("every background wait is bounded, so a wedged pix run cannot hang the whole UAT run", () => {
	assert.match(script, /bounded_wait\(\) \{/);
	const multiShell = script.slice(script.indexOf("[6] Multi-shell"), script.indexOf("[7] --keep"));
	assert.doesNotMatch(multiShell, /\bwait \$SH1\b/);
	assert.doesNotMatch(multiShell, /\bwait \$SH2\b/);
	assert.match(multiShell, /bounded_wait "\$SH1" 60/);
	assert.match(multiShell, /bounded_wait "\$SH2" 60/);
});

test("the snapshot secret scan does not pass vacuously when units[] is empty in a service-enabled run", () => {
	const hostSection = script.slice(script.indexOf("[8] Host services"), script.indexOf("[9] External OAuth"));
	assert.match(hostSection, /UNIT_COUNT="\$\(printf '%s' "\$UNITS" \| grep -c '"identity"'\)"/);
	assert.match(hostSection, /if \[ "\$UNIT_COUNT" -eq 0 \]; then\n\s*fail "snapshot carries no secrets"/);
	// The old shape: the negative regex ran unconditionally, so an empty
	// units[] (nothing to have scanned) still printed a free "no secrets" PASS.
	assert.doesNotMatch(hostSection, /else pass "snapshot carries no secrets";/);
});

// extractSecretsCheck — the literal if/elif/else block that scores the
// snapshot secret scan, so the behavioral tests below run the ACTUAL shipped
// logic against a fabricated UNITS payload.
function extractSecretsCheck() {
	const startMarker = 'UNIT_COUNT="$(printf';
	const start = script.indexOf(startMarker);
	assert.ok(start !== -1, "could not find the UNIT_COUNT secrets-check block");
	const endMarker = 'else pass "snapshot carries no secrets ($UNIT_COUNT unit(s) scanned)"; fi';
	const end = script.indexOf(endMarker, start);
	assert.ok(end !== -1, "could not find the end of the secrets-check block");
	return script.slice(start, end + endMarker.length);
}

function runSecretsCheck(unitsJson) {
	const block = extractSecretsCheck();
	const harness = [
		"set -uo pipefail",
		'pass() { printf "PASS:%s\\n" "$1"; }',
		'fail() { printf "FAIL:%s\\n" "$1"; }',
		block,
	].join("\n");
	return execFileSync("bash", ["-c", harness], {
		encoding: "utf8",
		env: { ...process.env, UNITS: unitsJson },
	});
}

test("snapshot secret scan (behavioral): empty units[] FAILs, it does not pass vacuously", () => {
	const out = runSecretsCheck('{"units": []}');
	assert.match(out, /FAIL:snapshot carries no secrets/);
	assert.doesNotMatch(out, /PASS:snapshot carries no secrets/);
});

test("snapshot secret scan (behavioral): a real unit with no credential-shaped text PASSes", () => {
	const out = runSecretsCheck('{"units": [{"name": "memory", "identity": "abc123", "state": "running"}]}');
	assert.match(out, /PASS:snapshot carries no secrets/);
	assert.doesNotMatch(out, /FAIL:/);
});

test("snapshot secret scan (behavioral): a real unit WITH credential-shaped text still FAILs", () => {
	const out = runSecretsCheck('{"units": [{"name": "memory", "identity": "abc123", "note": "Bearer abcd1234efgh"}]}');
	assert.match(out, /FAIL:snapshot carries no secrets/);
	assert.doesNotMatch(out, /PASS:/);
});

// preflightSection — section [2], the launchd preflight + reversible install,
// which now runs BEFORE the first `pix run` (section [3]'s digest naming),
// not inside the host-services checks (section [8]) anymore.
function preflightSection() {
	return script.slice(script.indexOf('[2] Host services'), script.indexOf('[3] Digest-suffixed'));
}

test("host services preflight WHICH pix-host binary an already-running serve is before trusting it", () => {
	const preflight = preflightSection();
	assert.match(preflight, /current_bin_path pix-host/);
	assert.match(preflight, /running_bin_path "\$RUNNING_PID"/);
	// Any pre-existing daemon blocks a clean launchd lifecycle test, even if it is
	// the current binary; the rerun must install and own the managed service.
	assert.match(preflight, /clean UAT must install and exercise the launchd-managed service itself/);
	assert.match(preflight, /die "a serve is already running.*pix serve stop.*re-run this script/s);
	assert.match(preflight, /cannot prove which binary would be tested.*pix serve stop/s);
	assert.doesNotMatch(preflight, /skip "which binary the running serve is"/);
});

test("installing serve for this run is reported as reversible and names the binary under test", () => {
	const preflight = preflightSection();
	assert.match(preflight, /pass "installed and started serve from the current build \(\$CUR_HOSTBIN\).*reversible: uninstalled on exit"/);
});

test("the launchd preflight + reversible install runs BEFORE this script's first `pix run` (section [3]'s digest naming)", () => {
	// This is the ordering fix: install/preflight must land ahead of any
	// sandbox this run creates, so a later serve state change can never be
	// confused with "changed because the lifecycle checks changed it".
	const preflightIdx = script.indexOf('head1 "[2] Host services: launchd preflight + install"');
	const installIdx = script.indexOf('pix serve install');
	const firstRunIdx = script.indexOf("pix run . --keep -- -p 'digest a'");
	assert.ok(preflightIdx !== -1, "could not find the section [2] preflight header");
	assert.ok(installIdx !== -1, "could not find the 'pix serve install' call");
	assert.ok(firstRunIdx !== -1, "could not find the script's first `pix run` invocation (digest naming)");
	assert.ok(preflightIdx < firstRunIdx, "section [2]'s preflight header must appear before the first pix run");
	assert.ok(installIdx < firstRunIdx, "'pix serve install' must run before the first pix run");
});

test("section [8] host services CONSUMES the already-installed daemon; it does not re-preflight or reinstall it", () => {
	// Section7-was-[7]-is-now-[8]: after the reorder, the host-services check
	// section must read $CUR_HOSTBIN/$INSTALLED_SERVE set by section [2], never
	// call current_bin_path/running_bin_path or `pix serve install` itself —
	// that would be re-running the same lazy-start gate a second time, after
	// sandboxes it was meant to precede already exist.
	const hostSection = script.slice(script.indexOf("[8] Host services"), script.indexOf("[9] External OAuth"));
	assert.doesNotMatch(hostSection, /current_bin_path pix-host/);
	assert.doesNotMatch(hostSection, /running_bin_path "\$RUNNING_PID"/);
	assert.doesNotMatch(hostSection, /pix serve install/);
	assert.doesNotMatch(hostSection, /RUNNING_PID="\$\(pix serve status/);
	assert.match(hostSection, /CONSUMES that managed daemon/);
});

test("the install-if-down gate uses the machine-readable serve_is_running helper, not a text substring match", () => {
	const preflight = preflightSection();
	assert.match(preflight, /if ! serve_is_running; then/);
	const hostSection = script.slice(script.indexOf("[8] Host services"), script.indexOf("[9] External OAuth"));
	assert.match(hostSection, /if ! serve_is_running; then pass "'serve stop' is mode-aware/);
	// The exact bug this replaces: "serve: not running" CONTAINS the substring
	// "running", so a bare `grep -q running` matches both states and a `!`
	// negation is permanently false — it can never gate an install.
	assert.doesNotMatch(script, /pix serve status 2>\/dev\/null \| grep -q "running"/);
	assert.doesNotMatch(script, /pix serve status 2>\/dev\/null \| grep -q "not running"/);
});

// serve_is_running (behavioral): proves the ACTUAL extracted helper against a
// stub `pix` on PATH, for both the buggy substring shape ("serve: not
// running" contains "running") and the fixed JSON-boolean shape.
function runServeIsRunning(jsonRunning) {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-serverunning-"));
	try {
		const fakePix = path.join(work, "pix");
		fs.writeFileSync(
			fakePix,
			"#!/bin/sh\n" +
				'if [ "$1 $2" = "serve status" ] && [ "$3" = "--json" ]; then\n' +
				`  printf '{"running": ${jsonRunning}}\\n'\n` +
				"  exit 0\n" +
				"fi\n" +
				'if [ "$1 $2" = "serve status" ]; then\n' +
				`  if [ "${jsonRunning}" = "true" ]; then printf 'serve: running (pid 1)\\n'; else printf 'serve: not running\\n'; fi\n` +
				"  exit 0\n" +
				"fi\n" +
				"exit 1\n",
			{ mode: 0o755 },
		);
		const fn = extractFn("serve_is_running");
		const harness = `PATH="$1:$PATH"\n${fn}\nif serve_is_running; then echo RUNNING; else echo NOT_RUNNING; fi`;
		return execFileSync("bash", ["-c", harness, "bash", work], { encoding: "utf8" }).trim();
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
}

test("serve_is_running (behavioral): reports NOT_RUNNING when the JSON boolean is false, even though the human line contains the substring 'running'", () => {
	assert.strictEqual(runServeIsRunning("false"), "NOT_RUNNING");
});

test("serve_is_running (behavioral): reports RUNNING when the JSON boolean is true", () => {
	assert.strictEqual(runServeIsRunning("true"), "RUNNING");
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
	const oauthSection = script.slice(script.indexOf("[9] External OAuth"));
	assert.match(oauthSection, /grep -qF "\$s: registered \(host registration"/);
	assert.match(oauthSection, /pass "catalog server \$s authenticated \(pix doctor --json evidence\)"/);
});

test("OAuth classification explicitly FAILs on 'not registered', separately from 'not authenticated'", () => {
	const oauthSection = script.slice(script.indexOf("[9] External OAuth"));
	assert.match(oauthSection, /grep -qF "\$s: not registered"/);
	assert.match(oauthSection, /fail "catalog server \$s authenticated" "pix doctor --json reports it NOT REGISTERED/);
	assert.match(oauthSection, /grep -qF "\$s: registered, not authenticated"/);
	assert.match(oauthSection, /fail "catalog server \$s authenticated" "pix doctor --json still reports it unauthenticated/);
});

test("OAuth classification no longer matches on the bare '$s:' substring (the false-PASS this rewrite closes)", () => {
	const oauthSection = script.slice(script.indexOf("[9] External OAuth"));
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

// --- multi-shell FIFO holds: behavioral proof -----------------------------
// The false-hold this replaces: the old section [6] (then [5]) launched two
// REAL `pix run` sessions with `-p '<sleep text>'` prompts and separated
// their "up"/"released" timing with blind `sleep 5`/`sleep 3`/`sleep 8`
// guesses — every prompt was an actual MODEL CALL, and every timing decision
// was a guess, not a fact. This test runs the ACTUAL extracted section-[6]
// FIFO block (not a reimplementation) against a stub `pix` that blocks
// reading its stdin and reports exactly how many bytes it ever received, so
// the test proves both halves of the fix: (1) release is deterministic —
// closing one fd ends exactly that one held session, in order — and (2) zero
// bytes ever cross either pipe, so nothing sent could ever be mistaken for a
// submitted prompt (a model call).
test("multi-shell FIFO holds (behavioral): two real background sessions stay alive with zero bytes sent, released one then the other, deterministically", () => {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-fifo-"));
	try {
		const binDir = path.join(work, "bin");
		const markDir = path.join(work, "marks");
		const projDir = path.join(work, "b", "proj");
		fs.mkdirSync(binDir);
		fs.mkdirSync(markDir);
		fs.mkdirSync(projDir, { recursive: true });

		// A stub `pix`: `run . --name X` blocks reading stdin (exactly like the
		// real interactive RunSession would) and, on EOF, records the exact byte
		// count it ever saw; `ls` reports the box as up while any hold is live.
		const fakePix = path.join(binDir, "pix");
		fs.writeFileSync(
			fakePix,
			"#!/bin/sh\n" +
				'if [ "$1 $2" = "run ." ]; then\n' +
				"  token=\"$$\"\n" +
				'  : > "$MARK_DIR/live.$token"\n' +
				"  n=$(wc -c | tr -d ' ')\n" +
				'  echo "$n" > "$MARK_DIR/bytes.$token"\n' +
				'  rm -f "$MARK_DIR/live.$token"\n' +
				"  exit 0\n" +
				'elif [ "$1" = "ls" ]; then\n' +
				'  count=$(ls "$MARK_DIR"/live.* 2>/dev/null | wc -l | tr -d \' \')\n' +
				'  if [ "$count" -gt 0 ]; then printf \'NAME\\n%s\\n\' "$BOX2"; else printf \'NAME\\n\'; fi\n' +
				"  exit 0\n" +
				"fi\n" +
				"exit 1\n",
			{ mode: 0o755 },
		);

		const startMarker = 'FIFO_DIR="$WORK/holds"';
		const start = script.indexOf(startMarker);
		assert.ok(start !== -1, "could not find the FIFO_DIR setup in section [6]");
		const endMarker = 'else pass "teardown on last shell exit"; fi';
		const end = script.indexOf(endMarker, start);
		assert.ok(end !== -1, "could not find the end of the multi-shell FIFO block");
		const block = script.slice(start, end + endMarker.length);

		const fns = [extractFn("fifo_release"), extractFn("bounded_wait")].join("\n");
		const harness = [
			`PATH="${binDir}:$PATH"`,
			`export MARK_DIR="${markDir}"`,
			`WORK="${work}"`,
			'export BOX2="test-box2-multi"',
			"HOLD_PIDS=()",
			'die() { printf "DIE:%s\\n" "$1" >&2; exit 9; }',
			'pass() { printf "PASS:%s\\n" "$1"; }',
			'fail() { printf "FAIL:%s:%s\\n" "$1" "${2:-}"; }',
			"head1() { :; }",
			"newbox() { :; }",
			fns,
			block,
		].join("\n");

		const out = execFileSync("bash", ["-c", harness], { encoding: "utf8", timeout: 20000 });
		assert.match(out, /PASS:test-box2-multi is up with two shells attached/);
		assert.match(out, /PASS:sandbox survives the FIRST shell leaving/);
		assert.match(out, /PASS:teardown on last shell exit/);
		assert.doesNotMatch(out, /FAIL:/);

		const bytesFiles = fs.readdirSync(markDir).filter((f) => f.startsWith("bytes."));
		assert.ok(bytesFiles.length >= 2, `expected at least 2 byte-count files (one per held session), got ${bytesFiles.length}`);
		for (const f of bytesFiles) {
			const n = fs.readFileSync(path.join(markDir, f), "utf8").trim();
			assert.strictEqual(n, "0", `hold FIFO ${f} received ${n} byte(s), want 0 — nothing may ever cross a hold pipe`);
		}
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
});

test("multi-shell FIFO holds: no prompt (-p) or sleep-based timing gate remains in section [6]", () => {
	const multiShell = script.slice(script.indexOf("[6] Multi-shell"), script.indexOf("[7] --keep"));
	assert.doesNotMatch(multiShell, /-p 'sleep quietly'/);
	assert.doesNotMatch(multiShell, /-p 'second shell'/);
	assert.match(multiShell, /mkfifo "\$FIFO1" "\$FIFO2"/);
	assert.match(multiShell, /fifo_release 5/);
	assert.match(multiShell, /fifo_release 6/);
});

// --- verdict(): rc (die/abort) precedence over accumulated FAIL ----------------
// The bug this closes: `die` exits 2 directly, and the EXIT trap always calls
// cleanup -> verdict(rc). The old verdict() checked `[ "$FAIL" -gt 0 ]` FIRST,
// so a die/abort that fired AFTER earlier sections already racked up real
// FAILs got silently reported as plain "UAT FAILED" (exit 1) — the operator
// never learns the run was cut short, only that "some checks failed".

test("verdict() checks rc (die/abort) BEFORE the accumulated FAIL count, not after", () => {
	const fn = extractFn("verdict");
	const rcIdx = fn.indexOf('if [ "$rc" -ne 0 ]');
	const failIdx = fn.indexOf('if [ "$FAIL" -gt 0 ]');
	assert.ok(rcIdx !== -1, "verdict() has no rc-abort check");
	assert.ok(failIdx !== -1, "verdict() has no FAIL check");
	assert.ok(rcIdx < failIdx, "verdict() must check rc/abort before the accumulated FAIL count");
});

// runVerdict — runs the ACTUAL extracted verdict() against fabricated
// PASS/FAIL/SKIP counters and an rc, capturing its exit code the same way the
// exit trap would receive it.
function runVerdict({ pass = 0, fail = 0, skip = 0, rc = 0 } = {}) {
	const fn = extractFn("verdict");
	const harness = [
		"set -uo pipefail",
		'red() { printf "%s" "$*"; }',
		'grn() { printf "%s" "$*"; }',
		'ylw() { printf "%s" "$*"; }',
		"head1() { :; }",
		`PASS=${pass}`,
		`FAIL=${fail}`,
		`SKIP=${skip}`,
		fn,
		`verdict ${rc}`,
	].join("\n");
	try {
		const out = execFileSync("bash", ["-c", harness], { encoding: "utf8" });
		return { code: 0, out };
	} catch (e) {
		return { code: e.status, out: `${e.stdout || ""}${e.stderr || ""}` };
	}
}

test("verdict (behavioral): a die/abort (rc != 0) reports UAT ABORTED / exit 2, even with FAILs already accumulated", () => {
	const { code, out } = runVerdict({ pass: 3, fail: 2, skip: 0, rc: 2 });
	assert.strictEqual(code, 2);
	assert.match(out, /UAT ABORTED/);
	assert.doesNotMatch(out, /UAT FAILED/);
});

test("verdict (behavioral): rc=0 with accumulated FAIL still reports UAT FAILED / exit 1", () => {
	const { code, out } = runVerdict({ pass: 3, fail: 2, skip: 0, rc: 0 });
	assert.strictEqual(code, 1);
	assert.match(out, /UAT FAILED/);
});

test("verdict (behavioral): rc=0, no FAIL, PASS=0 reports the vacuous-run INCOMPLETE / exit 2", () => {
	const { code, out } = runVerdict({ pass: 0, fail: 0, skip: 0, rc: 0 });
	assert.strictEqual(code, 2);
	assert.match(out, /nothing was actually asserted/);
});

test("verdict (behavioral): rc=0, no FAIL, PASS>0, SKIP>0 reports INCOMPLETE / exit 2", () => {
	const { code, out } = runVerdict({ pass: 3, fail: 0, skip: 1, rc: 0 });
	assert.strictEqual(code, 2);
	assert.match(out, /check\(s\) could not run/);
});

test("verdict (behavioral): a fully clean run (rc=0, no FAIL, PASS>0, no SKIP) reports UAT PASSED / exit 0", () => {
	const { code, out } = runVerdict({ pass: 5, fail: 0, skip: 0, rc: 0 });
	assert.strictEqual(code, 0);
	assert.match(out, /UAT PASSED/);
});
