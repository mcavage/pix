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
	// The loop iterates the GAP set (AUTH_GAPS), not the fixed three names
	// unconditionally — a server doctor already certifies is never re-run
	// through `pix mcp auth`.
	assert.match(script, /for s in "\$\{AUTH_GAPS\[@\]\}"; do\n\s*assert_exit 0 "pix mcp auth \$s completed"/);
});

test("OAuth auth is gated on current doctor evidence BEFORE forcing any browser flow: an already registered+authenticated server is never re-authorized", () => {
	const oauthSection = script.slice(script.indexOf("[9] External OAuth"));
	assert.match(oauthSection, /PRE_AUTH_DOCTOR="\$\(pix doctor --json/);
	assert.match(oauthSection, /AUTH_GAPS=\(\)/);
	assert.match(oauthSection, /AUTH_GAPS\+=\("\$s"\)/);
	assert.match(oauthSection, /if \[ "\$\{#AUTH_GAPS\[@\]\}" -eq 0 \]; then/);
	assert.match(oauthSection, /pass "no OAuth gaps: notion\/atlassian\/granola are already registered\+authenticated \(pix doctor --json evidence\) — no browser flow invoked"/);
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

test("multi-shell readiness windows are bounded appropriately for a COLD post-load image pull (180s create / 90s attach by default), not the old flat 30s", () => {
	assert.match(script, /UAT_CREATE_WAIT_SECS="\$\{UAT_CREATE_WAIT_SECS:-180\}"/);
	assert.match(script, /UAT_ATTACH_WAIT_SECS="\$\{UAT_ATTACH_WAIT_SECS:-90\}"/);
	// Both windows are overridable via env (never a hardcoded magic number that
	// could regress below what a real cold pull needs, and never an untestable
	// constant a test harness cannot shrink).
	assert.match(script, /UAT_POLL_INTERVAL="\$\{UAT_POLL_INTERVAL:-1\}"/);
});

test("the assert_box_listed/assert_box_not_listed wrappers use the same cold-pull CREATE window, not a smaller magic number that could expire before creation finishes", () => {
	const fnListed = extractFn("assert_box_listed");
	const fnNotListed = extractFn("assert_box_not_listed");
	assert.match(fnListed, /bounded_wait_listed "\$name" "\$UAT_CREATE_WAIT_SECS"/);
	assert.match(fnNotListed, /bounded_wait_absent "\$name" "\$UAT_CREATE_WAIT_SECS"/);
	assert.doesNotMatch(fnListed, /bounded_wait_listed "\$name" 15/);
	assert.doesNotMatch(fnNotListed, /bounded_wait_absent "\$name" 15/);
});

test("every bounded_wait_* poll loop sleeps an injectable interval (UAT_POLL_INTERVAL), not a hardcoded 'sleep 1', so a test harness can shrink real wall time without editing the script", () => {
	for (const name of ["bounded_wait", "bounded_wait_listed", "bounded_wait_attach_log", "bounded_wait_absent"]) {
		const fn = extractFn(name);
		assert.match(fn, /sleep "\$UAT_POLL_INTERVAL"/, `${name} does not sleep the injectable poll interval`);
		assert.doesNotMatch(fn, /sleep 1\n/, `${name} still has a hardcoded 'sleep 1'`);
	}
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

// --- section [4]: exact lease-record path, no more `find -newer $WORK` -------
// The heuristic this replaces (`find "$STATE_DIR" -name record.json -newer
// "$WORK" | head -1`) was unsound on its own terms even ignoring style: on a
// host running ANY other concurrent pix activity, an unrelated pix run
// elsewhere could write a record.json newer than $WORK under the SAME
// STATE_DIR, and `head -1` would happily pick up THAT one instead of this
// run's. Lease state is keyed by the FINAL resolved sandbox name (services/
// host/cmd/pix/run_cmd.go's sessionKeyFor), and --name travels verbatim, so
// with an explicit --name the record path is exact and deterministic.

test("section [4] no longer uses `find ... -newer` to locate the lease record", () => {
	const executable = script
		.split("\n")
		.filter((l) => !/^\s*#/.test(l))
		.join("\n");
	assert.doesNotMatch(executable, /-newer\s*"?\$WORK/);
	assert.doesNotMatch(executable, /find\s+"\$STATE_DIR"/);
});

test("section [4] asserts the EXACT lease record path, keyed by the final sandbox name ($BOX1)", () => {
	const section4 = script.slice(script.indexOf('head1 "[4]'), script.indexOf('head1 "[5]'));
	assert.match(section4, /REC="\$STATE_DIR\/sandboxes\/\$BOX1\/record\.json"/);
	assert.match(section4, /if \[ -f "\$REC" \]; then/);
});

test("section [4] captures pix run's stderr separately and surfaces it on BOTH a launch failure and a missing-record failure", () => {
	const section4 = script.slice(script.indexOf('head1 "[4]'), script.indexOf('head1 "[5]'));
	// stdout and stderr land in separate files (never merged into one
	// combined log) so a later failure message can quote stderr specifically.
	assert.match(section4, />"\$WORK\/run1\.out" 2>"\$WORK\/run1\.err"/);
	assert.match(section4, /fail "pix run launched \$BOX1" "stderr: \$\(tail -5 "\$WORK\/run1\.err"/);
	assert.match(section4, /fail "lease instance record" "no record\.json at exact path \$REC; launch stderr: \$\(tail -5 "\$WORK\/run1\.err"/);
});

test("section [4] skips (never double-fails) the record check when the launch itself already failed", () => {
	const section4 = script.slice(script.indexOf('head1 "[4]'), script.indexOf('head1 "[5]'));
	assert.match(section4, /LAUNCH1_OK=0/);
	assert.match(section4, /if \[ "\$LAUNCH1_OK" = 1 \]; then/);
	assert.match(section4, /skip "lease instance record" "pix run launched \$BOX1 failed above/);
});

// section4RecordBlock — the literal BOX1/LAUNCH1_OK/REC block from section
// [4], so the behavioral tests below run the ACTUAL shipped logic against a
// fabricated STATE_DIR/pix run outcome, not a reimplementation of it.
function section4RecordBlock() {
	const start = script.indexOf('BOX1="${BOX_PREFIX}-one"');
	assert.ok(start !== -1, "could not find the BOX1 launch in section [4]");
	const endMarker = 'skip "lease instance record" "pix run launched $BOX1 failed above; no record is expected — see its captured stderr"\nfi';
	const end = script.indexOf(endMarker, start);
	assert.ok(end !== -1, "could not find the end of the section [4] record block");
	return script.slice(start, end + endMarker.length);
}

// runSection4RecordBlock — runs the ACTUAL extracted block under a stub `pix`
// and a real STATE_DIR/sandboxes/<name>/record.json fixture, so these tests
// exercise the shipped bash, not a description of it.
function runSection4RecordBlock({ launchOk, writeRecord, instanceId = "abc-123" }) {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-rec-"));
	try {
		const binDir = path.join(work, "bin");
		const projDir = path.join(work, "a", "proj");
		const stateDir = path.join(work, "state");
		fs.mkdirSync(binDir);
		fs.mkdirSync(projDir, { recursive: true });
		fs.mkdirSync(stateDir, { recursive: true });

		const boxName = "pix-uat-rec-test-one";
		if (writeRecord) {
			// $STATE_DIR = $XDG_STATE_HOME/pix (the script appends /pix itself).
			const recDir = path.join(stateDir, "pix", "sandboxes", boxName);
			fs.mkdirSync(recDir, { recursive: true });
			fs.writeFileSync(path.join(recDir, "record.json"), JSON.stringify({ instance_id: instanceId, created_pid: 1 }));
		}

		// A stub `pix`: `run . --name NAME` exits with the requested outcome,
		// writing a distinguishing message to stderr either way. `ls` reports
		// the box as present (the record write above already stands in for a
		// real create having happened, or not, exactly as `launchOk` says).
		const fakePix = path.join(binDir, "pix");
		fs.writeFileSync(
			fakePix,
			[
				"#!/bin/sh",
				'if [ "$1" = "run" ] && [ "$2" = "." ]; then',
				launchOk
					? '  echo "ready"; exit 0'
					: '  echo "boom: launch exploded" >&2; exit 1',
				'elif [ "$1" = "ls" ]; then',
				`  printf 'NAME\\n%s\\n' "${boxName}"`,
				"  exit 0",
				"fi",
				"exit 1",
				"",
			].join("\n"),
			{ mode: 0o755 },
		);

		const fns = [extractFn("runbox"), extractFn("newbox")].join("\n");
		const harness = [
			`PATH="${binDir}:$PATH"`,
			`WORK="${work}"`,
			`XDG_STATE_HOME="${stateDir}"`,
			"CREATED_BOXES=()",
			'pass() { printf "PASS:%s\\n" "$1"; }',
			'fail() { printf "FAIL:%s:%s\\n" "$1" "${2:-}"; }',
			'skip() { printf "SKIP:%s:%s\\n" "$1" "${2:-}"; }',
			'assert_contains() { :; }', // section [4] also calls this; not under test here
			fns,
			section4RecordBlock().replace('BOX1="${BOX_PREFIX}-one"', `BOX1="${boxName}"`),
		].join("\n");
		return execFileSync("bash", ["-c", harness], { encoding: "utf8", timeout: 15000 });
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
}

test("section [4] record check (behavioral): launch OK + record present at the exact path PASSes and reports the instance id", () => {
	const out = runSection4RecordBlock({ launchOk: true, writeRecord: true, instanceId: "iid-42" });
	assert.match(out, /PASS:pix run launched pix-uat-rec-test-one non-interactively/);
	assert.match(out, /PASS:lease record carries an instance id \(iid-42\)/);
	assert.doesNotMatch(out, /FAIL:/);
	assert.doesNotMatch(out, /SKIP:/);
});

test("section [4] record check (behavioral): launch OK but no record at the exact path FAILs and surfaces the launch stderr, it never SKIPs a launch that reported success", () => {
	const out = runSection4RecordBlock({ launchOk: true, writeRecord: false });
	assert.match(out, /PASS:pix run launched pix-uat-rec-test-one non-interactively/);
	assert.match(out, /FAIL:lease instance record:no record\.json at exact path .*sandboxes\/pix-uat-rec-test-one\/record\.json; launch stderr:/);
	assert.doesNotMatch(out, /SKIP:/);
});

test("section [4] record check (behavioral): a failed launch SKIPs the record check (never asserts a record that could not exist) and captures the launch's stderr in its own FAIL", () => {
	const out = runSection4RecordBlock({ launchOk: false, writeRecord: false });
	assert.match(out, /FAIL:pix run launched pix-uat-rec-test-one:stderr: boom: launch exploded/);
	assert.match(out, /SKIP:lease instance record:pix run launched pix-uat-rec-test-one failed above/);
	assert.doesNotMatch(out, /FAIL:lease instance record/);
});

// --- section [4]/[5]: BOX1 is kept so it can be INSPECTED before it is
// explicitly removed, and never leaks into section [6] --------------------
// The false failure this replaces: section [4] launched BOX1 with a plain
// non-interactive `pix run` (no --keep). That launch tears the sandbox down
// the instant its own `-p` command exits — the defining behavior of a
// non-interactive launch — so `pix ls lists $BOX1`, the exact record.json
// read, its instance id, and the changed-MCP attach-fingerprint refusal all
// raced (and often lost to) a teardown that had nothing to do with any of
// those checks. Keeping BOX1 fixes the race; these tests prove keeping it
// does not itself introduce a NEW false pass: the record/fingerprint checks
// must still run against the live box (not be skipped or trivially true),
// the fingerprint refusal must still be a real refusal --keep cannot cause
// or hide, and BOX1 must be explicitly removed — leaving no trace — before
// section [6] ever creates $BOX2.

test("section [4] launches BOX1 with --keep, so it survives its own non-interactive exit for the checks that follow", () => {
	const section4 = script.slice(script.indexOf('head1 "[4]'), script.indexOf('head1 "[5]'));
	assert.match(section4, /runbox "\$BOX1" "\$WORK\/a\/proj" --keep -- -p 'print the single word ready'/);
});

test("section [4]'s changed-MCP fingerprint-refusal attach does NOT itself pass --keep, so the refusal it asserts can never be attributed to (or masked by) the --keep BOX1 was created with", () => {
	const section4 = script.slice(script.indexOf('head1 "[4]'), script.indexOf('head1 "[5]'));
	const attachLine = section4.match(/if OUT="\$\( \(cd "\$WORK\/a\/proj" && pix run \. --name "\$BOX1"[^)]*\) 2>&1 \)"; then/);
	assert.ok(attachLine, "could not find the fingerprint-refusal attach line in section [4]");
	assert.doesNotMatch(attachLine[0], /--keep/);
	assert.match(attachLine[0], /--mcp definitely-not-registered/);
});

test("section [5] runs its exit-propagation check against a FRESH $BOX_EXIT create, then explicitly removes the kept $BOX1 sandbox AFTER that coverage and BEFORE section [6] begins", () => {
	const section5 = script.slice(script.indexOf('head1 "[5]'), script.indexOf('head1 "[6]'));
	// Ordering within [5]: exit-propagation coverage against $BOX_EXIT first,
	// BOX1's explicit rm last.
	const propagationIdx = section5.indexOf('runbox "$BOX_EXIT" "$WORK/exit/proj" -- --definitely-not-a-pi-flag');
	const rmIdx = section5.indexOf('pix rm "$BOX1"');
	assert.ok(propagationIdx !== -1, "exit-propagation check against a fresh $BOX_EXIT is missing from section [5]");
	assert.ok(rmIdx !== -1, "explicit 'pix rm \"$BOX1\"' is missing from section [5]");
	assert.ok(propagationIdx < rmIdx, "BOX1 must be removed AFTER exit-propagation coverage, not before");
	assert.match(section5, /if pix rm "\$BOX1" >\/dev\/null 2>&1 && ! pix ls 2>\/dev\/null \| grep -q "\$BOX1"; then/);
	assert.match(section5, /pass "explicit 'pix rm' removes the kept \$BOX1 sandbox before section \[6\]"/);
});

// --- section [5]: exit-propagation must hit a CREATE, never an ATTACH ---------
// The false test this replaces: section [5] used to run
// `pix run . --name "$BOX1" -- --definitely-not-a-pi-flag` against the
// ALREADY-CREATED, kept $BOX1 — an attach, not a create. pix intentionally
// REPLAYS the stored create-time invocation on attach (services/host/
// workflow/launch/attach_argv_test.go), so that new, invalid passthrough flag
// was silently ignored by design: the check could only ever observe BOX1's
// ORIGINAL (valid) create invocation's exit code, never the invalid one it
// claimed to be testing. These tests pin that section [5] now targets a
// brand-new, uniquely named $BOX_EXIT that pix has never seen before, so the
// invalid flag is unavoidably part of the CREATE invocation pix executes.

test("section [5] never reuses an already-created box name for exit-propagation: it creates a brand-new $BOX_EXIT via runbox, the same helper used for every other genuinely-fresh box in this script", () => {
	const section5 = script.slice(script.indexOf('head1 "[5]'), script.indexOf('head1 "[6]'));
	assert.match(section5, /BOX_EXIT="\$\{BOX_PREFIX\}-exit"/);
	// The suffix is distinct from every other box this script names (-one,
	// -multi, -keep), so it can never collide with (or be mistaken for) BOX1,
	// BOX2, or BOX3.
	assert.doesNotMatch(section5, /BOX_EXIT="\$\{BOX_PREFIX\}-(one|multi|keep)"/);
	assert.match(section5, /runbox "\$BOX_EXIT" "\$WORK\/exit\/proj" -- --definitely-not-a-pi-flag/);
	// The old attach-shaped invocation — a bare `pix run` naming the
	// already-created $BOX1 — must be entirely gone from section [5].
	assert.doesNotMatch(section5, /pix run \. --name "\$BOX1" -- --definitely-not-a-pi-flag/);
});

test("section [5] documents WHY it cannot reuse $BOX1: attach replays the stored create-time invocation, ignoring new passthrough flags by design", () => {
	const section5WithComment = script.slice(script.indexOf('# --- 5. exit-code propagation'), script.indexOf('head1 "[6]'));
	assert.match(section5WithComment, /REPLAYS the stored create-time invocation on attach/);
	assert.match(section5WithComment, /attach_argv_test\.go/);
	assert.match(section5WithComment, /silently ignored BY DESIGN/);
});

test("section [5] asserts $BOX_EXIT is absent after the failing create, or removes it explicitly — it never just assumes teardown happened", () => {
	const section5 = script.slice(script.indexOf('head1 "[5]'), script.indexOf('head1 "[6]'));
	assert.match(section5, /if pix ls 2>\/dev\/null \| grep -q "\$BOX_EXIT"; then/);
	assert.match(section5, /if pix rm "\$BOX_EXIT" >\/dev\/null 2>&1 && ! pix ls 2>\/dev\/null \| grep -q "\$BOX_EXIT"; then/);
	assert.match(section5, /pass "explicit 'pix rm' removed \$BOX_EXIT left behind by the failing create"/);
	assert.match(section5, /pass "\$BOX_EXIT was not left behind by its own failed non-interactive create"/);
	assert.match(section5, /fail "cleanup of \$BOX_EXIT" "\$BOX_EXIT is still listed/);
});

test("section [4] does not reappear inside section [6]: BOX1's explicit removal happens strictly before the [6] header, never after", () => {
	const idx4 = script.indexOf('head1 "[4]');
	const idx5 = script.indexOf('head1 "[5]');
	const idx6 = script.indexOf('head1 "[6]');
	const rmIdx = script.indexOf('pix rm "$BOX1"');
	assert.ok(idx4 < idx5 && idx5 < idx6, "section headers are out of order");
	assert.ok(rmIdx > idx5 && rmIdx < idx6, "BOX1's explicit removal must sit inside section [5], before section [6] begins");
});

// section4Through5Block — the literal, executable text from BOX1's launch in
// section [4] through its explicit removal at the end of section [5], so the
// behavioral test below runs the ACTUAL shipped bash end to end, not a
// reimplementation of the ordering it claims.
function section4Through5Block() {
	const start = script.indexOf('BOX1="${BOX_PREFIX}-one"');
	assert.ok(start !== -1, "could not find the BOX1 launch in section [4]");
	const endMarker = 'fail "explicit rm of kept $BOX1" "$BOX1 is still listed after \'pix rm $BOX1\'"\nfi';
	const end = script.indexOf(endMarker, start);
	assert.ok(end !== -1, "could not find the end of the section [5] explicit-removal block");
	return script.slice(start, end + endMarker.length);
}

// runSection4Through5Block — runs the ACTUAL extracted section [4]->[5] block
// against a stub `pix` that records every invocation (command + full argv) to
// a trace file and tracks BOX1's existence in a tiny state file, so this
// proves the REAL execution order (record/fingerprint inspected before the
// explicit rm), the REAL end state (BOX1 gone, nothing leaked into [6]), and
// that the exit-propagation invocation actually names a BRAND-NEW box
// ($BOX_EXIT) rather than replaying an attach against BOX1 — not a
// description of any of that. BOX_PREFIX is set for real (not text-replaced
// after the fact), so `${BOX_PREFIX}-one` and `${BOX_PREFIX}-exit` in the
// extracted script both expand exactly the way they do in production.
// leftBehind controls whether the stub simulates $BOX_EXIT surviving its own
// failed create (still listed on `pix ls` afterward) so both halves of
// section [5]'s "assert absent, or explicitly remove it" branch run for
// real, against the ACTUAL shipped bash, not just the common no-op case.
function runSection4Through5Block({ leftBehind = false } = {}) {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-rec2-"));
	try {
		const binDir = path.join(work, "bin");
		const projDir = path.join(work, "a", "proj");
		const stateDir = path.join(work, "state");
		const trace = path.join(work, "trace.log");
		const boxFlag = path.join(work, "box-exists");
		const exitBoxFlag = path.join(work, "exit-box-exists");
		fs.mkdirSync(binDir);
		fs.mkdirSync(projDir, { recursive: true });
		fs.mkdirSync(stateDir, { recursive: true });

		const boxPrefix = "pix-uat-rec2-test";
		const boxName = `${boxPrefix}-one`;
		const exitBoxName = `${boxPrefix}-exit`;
		const recDir = path.join(stateDir, "pix", "sandboxes", boxName);

		// A stub `pix` that:
		//  - `run . --name $BOX1 --keep -- -p '...'`         -> CREATE: writes BOX1's
		//    record.json, marks it existing, exit 0 (BOX1's section-[4] launch).
		//  - `run . --name $BOX1 --mcp ... -- -p hi`          -> the fingerprint-mismatch
		//    attach: REFUSED (exit 1), BOX1's state untouched — the real gate this test
		//    proves is exercised, independent of --keep (which never appears on this
		//    invocation at all).
		//  - `run . --name $BOX_EXIT -- --definitely-not-a-pi-flag` -> the
		//    exit-propagation CREATE of a name pix has never seen before: fails
		//    (exit 3) and, unless leftBehind, never marks itself existing at all —
		//    modeling the real self-teardown a non-interactive, non-kept create
		//    does the instant its failing inner command exits.
		//  - `ls`                                             -> lists BOX1 while
		//    boxFlag exists, and $BOX_EXIT while exitBoxFlag exists.
		//  - `rm NAME`                                        -> removes the matching flag,
		//    exit 0.
		// Every invocation is appended to trace.log as one line: "CMD|arg|arg|...".
		const fakePix = path.join(binDir, "pix");
		fs.writeFileSync(
			fakePix,
			[
				"#!/bin/sh",
				`printf '%s\\n' "$*" >> "${trace}"`,
				'if [ "$1" = "run" ] && [ "$2" = "." ]; then',
				'  case " $* " in',
				'    *" --keep "*)',
				`      mkdir -p "${recDir}"`,
				`      printf '{"instance_id":"iid-99","created_pid":1}' > "${recDir}/record.json"`,
				`      touch "${boxFlag}"`,
				'      echo "ready"; exit 0 ;;',
				'    *" --mcp "*)',
				'      echo "pix run: attach fingerprint diverged: static_mcp changed" >&2; exit 1 ;;',
				'    *" --definitely-not-a-pi-flag "*)',
				leftBehind ? `      touch "${exitBoxFlag}"` : "      :",
				'      echo "unknown flag" >&2; exit 3 ;;',
				'    *) echo "ready"; exit 0 ;;',
				'  esac',
				'elif [ "$1" = "ls" ]; then',
				`  [ -f "${boxFlag}" ] && printf 'NAME\\n%s\\n' "${boxName}"`,
				`  [ -f "${exitBoxFlag}" ] && printf '%s\\n' "${exitBoxName}"`,
				'  exit 0',
				'elif [ "$1" = "rm" ]; then',
				`  [ "$2" = "${boxName}" ] && rm -f "${boxFlag}"`,
				`  [ "$2" = "${exitBoxName}" ] && rm -f "${exitBoxFlag}"`,
				'  exit 0',
				'fi',
				'exit 2',
				"",
			].join("\n"),
			{ mode: 0o755 },
		);

		const fns = [extractFn("runbox"), extractFn("newbox"), extractFn("assert_exit")].join("\n");
		const harness = [
			`PATH="${binDir}:$PATH"`,
			`WORK="${work}"`,
			`XDG_STATE_HOME="${stateDir}"`,
			`RUN_ID="rec2"`,
			`BOX_PREFIX="${boxPrefix}"`,
			"CREATED_BOXES=()",
			'PASS=0; FAIL=0; SKIP=0',
			'pass() { PASS=$((PASS+1)); printf "PASS:%s\\n" "$1"; }',
			'fail() { FAIL=$((FAIL+1)); printf "FAIL:%s:%s\\n" "$1" "${2:-}"; }',
			'skip() { SKIP=$((SKIP+1)); printf "SKIP:%s:%s\\n" "$1" "${2:-}"; }',
			'head1() { :; }', // section headers between BOX1's launch and its explicit rm; not under test here
			'die() { printf "DIE:%s\\n" "$1" >&2; exit 9; }',
			'assert_contains() { local needle="$1" name="$2"; shift 2; local out; out="$("$@" 2>&1)"; if printf "%s" "$out" | grep -qF -- "$needle"; then pass "$name"; else fail "$name" "missing $needle"; fi; }',
			fns,
			section4Through5Block(),
			`echo "===FINAL_LS==="`,
			`pix ls`,
		].join("\n");
		const out = execFileSync("bash", ["-c", harness], { encoding: "utf8", timeout: 15000 });
		return { out, trace: fs.readFileSync(trace, "utf8").trim().split("\n"), boxName, exitBoxName };
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
}

test("section [4]->[5] (behavioral): the record and fingerprint-refusal checks run, in order, BEFORE BOX1's explicit removal, against the ACTUAL shipped bash", () => {
	const { out, trace, boxName } = runSection4Through5Block();
	// The record was there to inspect: instance id surfaced, real refusal seen.
	assert.match(out, /PASS:pix run launched pix-uat-rec2-test-one non-interactively/);
	assert.match(out, /PASS:lease record carries an instance id \(iid-99\)/);
	assert.match(out, /PASS:attach fingerprint gate refused a changed MCP set/);
	assert.match(out, /PASS:last-exit propagation \(inner failure surfaced as exit 3\)/);
	assert.match(out, /PASS:pix-uat-rec2-test-exit was not left behind by its own failed non-interactive create/);
	assert.match(out, /PASS:explicit 'pix rm' removes the kept pix-uat-rec2-test-one sandbox before section \[6\]/);
	assert.doesNotMatch(out, /FAIL:/);

	// Order of REAL invocations pix actually received: BOX1's create, then ls,
	// then the fingerprint-mismatch attach (refused), then the exit-propagation
	// CREATE against $BOX_EXIT, then — and only then — BOX1's explicit rm. The
	// record/fingerprint inspection happens strictly before removal, never
	// interleaved or reversed.
	const createIdx = trace.findIndex((l) => l.includes("--keep") && l.includes("print the single word ready"));
	const fingerprintIdx = trace.findIndex((l) => l.includes("--mcp"));
	const propagationIdx = trace.findIndex((l) => l.includes("--definitely-not-a-pi-flag"));
	const rmIdx = trace.findIndex((l) => l.startsWith("rm " + boxName));
	assert.ok(createIdx !== -1 && fingerprintIdx !== -1 && propagationIdx !== -1 && rmIdx !== -1, `missing an expected invocation in trace: ${trace.join(" | ")}`);
	assert.ok(createIdx < fingerprintIdx, "the box must be created before the fingerprint-mismatch attach is even attempted");
	assert.ok(fingerprintIdx < rmIdx, "the fingerprint check must run BEFORE the explicit rm, not after");
	assert.ok(propagationIdx < rmIdx, "exit-propagation coverage must run BEFORE the explicit rm, not after");

	// The fingerprint-mismatch attach itself never carries --keep: the refusal
	// this proves cannot be attributed to (or accidentally bypassed by) the
	// --keep flag BOX1 was originally created with.
	assert.doesNotMatch(trace[fingerprintIdx], /--keep/);

	// No leak: pix ls after BOX1's explicit rm reports it gone, same as [6]
	// will see it when it starts. $BOX_EXIT never appears at all — it was never
	// left behind by its own failing create.
	const finalLsSection = out.slice(out.indexOf("===FINAL_LS==="));
	assert.doesNotMatch(finalLsSection, /pix-uat-rec2-test-one/);
	assert.doesNotMatch(finalLsSection, /pix-uat-rec2-test-exit/);
});

test("section [4]->[5] (behavioral): the exit-propagation invocation names a box pix has NEVER SEEN BEFORE in this run — proof no attach path is used, since an attach requires a name pix already created", () => {
	const { trace, boxName, exitBoxName } = runSection4Through5Block();
	const propagationLine = trace.find((l) => l.includes("--definitely-not-a-pi-flag"));
	assert.ok(propagationLine, `no exit-propagation invocation found in trace: ${trace.join(" | ")}`);
	// It targets $BOX_EXIT, never the already-created $BOX1.
	assert.match(propagationLine, new RegExp(`--name ${exitBoxName}\\b`));
	assert.doesNotMatch(propagationLine, new RegExp(`--name ${boxName}\\b`));
	// And that name never appeared in ANY earlier invocation this run made —
	// the structural guarantee that this call is necessarily pix's first-ever
	// encounter with it, so pix's own create/attach decision (based on sbx
	// state, not on argv content) can only resolve it as a CREATE.
	const propagationIdx = trace.indexOf(propagationLine);
	const priorLines = trace.slice(0, propagationIdx);
	assert.ok(
		!priorLines.some((l) => l.includes(exitBoxName)),
		`$BOX_EXIT's name (${exitBoxName}) appeared in an earlier invocation, which would make this an attach, not a create: ${priorLines.join(" | ")}`,
	);
});

test("section [4]->[5] (behavioral): when the failing create somehow leaves $BOX_EXIT listed, section [5] explicitly removes it rather than assuming teardown happened", () => {
	const { out, trace, exitBoxName } = runSection4Through5Block({ leftBehind: true });
	assert.match(out, /PASS:last-exit propagation \(inner failure surfaced as exit 3\)/);
	assert.match(out, new RegExp(`PASS:explicit 'pix rm' removed ${exitBoxName} left behind by the failing create`));
	assert.doesNotMatch(out, /FAIL:/);
	// The fallback rm was actually invoked against $BOX_EXIT specifically.
	assert.ok(trace.some((l) => l === `rm ${exitBoxName}`), `expected an 'rm ${exitBoxName}' invocation in trace: ${trace.join(" | ")}`);
	// And it is gone from the final listing, same as the common no-op case.
	const finalLsSection = out.slice(out.indexOf("===FINAL_LS==="));
	assert.doesNotMatch(finalLsSection, new RegExp(exitBoxName));
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
// fakeMultiShellPix — a stub `pix` covering both commands section [6]'s
// FIFO block calls: `run . --name X` (blocks reading stdin exactly like the
// real interactive RunSession would, and records the exact byte count it
// ever saw on EOF) and `ls`/`ls --json` (reports $BOX2 once — and only once —
// a `created` marker exists). The FIRST `run` call simulates a CREATE: after
// an optional CREATE_DELAY it drops the `created` marker (what makes it
// listed) with no "attaching" line, exactly like a fresh create. Every
// SUBSEQUENT call simulates an ATTACH: after an optional ATTACH_DELAY it
// prints pix's own real attach line (run_cmd.go's literal wording) to its
// OWN stdout before blocking on stdin. Both delays default to 0; setting
// them lets a test PROVE the bounded waits actually wait, rather than merely
// happening to pass because the marker/line was already there the instant
// they first checked.
function writeFakeMultiShellPix(binDir) {
	const fakePix = path.join(binDir, "pix");
	fs.writeFileSync(
		fakePix,
		[
			"#!/bin/sh",
			'if [ "$1" = "run" ] && [ "$2" = "." ]; then',
			'  if [ -e "$MARK_DIR/created" ]; then',
			'    sleep "${ATTACH_DELAY:-0}"',
			'    printf \'pix run: attaching to running sandbox "%s"\\n\' "$BOX2"',
			"  else",
			'    sleep "${CREATE_DELAY:-0}"',
			'    : > "$MARK_DIR/created"',
			"  fi",
			'  token="$$"',
			'  : > "$MARK_DIR/started.$token"',
			'  : > "$MARK_DIR/live.$token"',
			"  n=$(wc -c | tr -d ' ')",
			'  echo "$n" > "$MARK_DIR/bytes.$token"',
			'  rm -f "$MARK_DIR/live.$token"',
			// Simulate real teardown: the box stays "created" as long as ANY
			// reference (started.*) remains, and disappears the instant the
			// LAST one exits — never on the first of two references leaving.
			'  rm -f "$MARK_DIR/started.$token"',
			'  remaining=$(ls "$MARK_DIR"/started.* 2>/dev/null | wc -l | tr -d \' \')',
			'  [ "$remaining" -eq 0 ] && rm -f "$MARK_DIR/created"',
			"  exit 0",
			'elif [ "$1" = "ls" ] && [ "$2" = "--json" ]; then',
			'  if [ "${LS_FAIL:-0}" = "1" ]; then',
			'    echo "stub pix ls --json: simulated failure" >&2',
			"    exit 1",
			"  fi",
			'  if [ -e "$MARK_DIR/created" ]; then',
			'    printf \'[{"name": "%s", "state": "running"}]\\n\' "$BOX2"',
			"  else",
			"    printf '[]\\n'",
			"  fi",
			"  exit 0",
			'elif [ "$1" = "ls" ]; then',
			'  if [ -e "$MARK_DIR/created" ]; then',
			'    printf \'NAME\\n%s\\n\' "$BOX2"',
			"  else",
			"    printf 'NAME\\n'",
			"  fi",
			"  exit 0",
			"fi",
			"exit 1",
			"",
		].join("\n"),
		{ mode: 0o755 },
	);
	return fakePix;
}

// multiShellSectionBlock — the literal section [6] FIFO body, from the FIFO
// directory setup through the final teardown assertion, so every behavioral
// test below runs the ACTUAL shipped logic.
function multiShellSectionBlock() {
	const startMarker = 'FIFO_DIR="$WORK/holds"';
	const start = script.indexOf(startMarker);
	assert.ok(start !== -1, "could not find the FIFO_DIR setup in section [6]");
	const endMarker = 'assert_box_not_listed "$BOX2" "teardown on last shell exit"';
	const end = script.indexOf(endMarker, start);
	assert.ok(end !== -1, "could not find the end of the multi-shell FIFO block");
	return script.slice(start, end + endMarker.length);
}

const multiShellHelperFns = [
	"fifo_release",
	"bounded_wait",
	"ls_json_lists",
	"bounded_wait_listed",
	"bounded_wait_absent",
	"bounded_wait_attach_log",
	"assert_box_listed",
	"assert_box_not_listed",
].map(extractFn).join("\n");

test("multi-shell FIFO holds (behavioral): two real background sessions stay alive with zero bytes sent, released one then the other, deterministically — even with a delayed create/attach, proving the waits actually wait rather than winning a timing race", () => {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-fifo-"));
	try {
		const binDir = path.join(work, "bin");
		const markDir = path.join(work, "marks");
		const projDir = path.join(work, "b", "proj");
		fs.mkdirSync(binDir);
		fs.mkdirSync(markDir);
		fs.mkdirSync(projDir, { recursive: true });
		writeFakeMultiShellPix(binDir);

		const harness = [
			`PATH="${binDir}:$PATH"`,
			`export MARK_DIR="${markDir}"`,
			`WORK="${work}"`,
			'export BOX2="test-box2-multi"',
			// A DELAYED create and a DELAYED attach: the marker/line the bounded
			// waits poll for is deliberately NOT there on their first check. If
			// either wait were a single immediate probe instead of an actual
			// bounded poll, this would FAIL every run, not flake occasionally.
			'export CREATE_DELAY="1"',
			'export ATTACH_DELAY="1"',
			// Same real values (1s poll, 30s bound) the OLD hardcoded call sites
			// used, now injected explicitly so this test's semantics do not
			// silently change now that the script reads them from the env.
			'export UAT_CREATE_WAIT_SECS="30"',
			'export UAT_ATTACH_WAIT_SECS="30"',
			'export UAT_POLL_INTERVAL="1"',
			"HOLD_PIDS=()",
			'die() { printf "DIE:%s\\n" "$1" >&2; exit 9; }',
			'pass() { printf "PASS:%s\\n" "$1"; }',
			'fail() { printf "FAIL:%s:%s\\n" "$1" "${2:-}"; }',
			"head1() { :; }",
			"newbox() { :; }",
			multiShellHelperFns,
			multiShellSectionBlock(),
		].join("\n");

		const out = execFileSync("bash", ["-c", harness], { encoding: "utf8", timeout: 30000 });
		assert.match(out, /PASS:test-box2-multi observed via pix ls --json after the first shell's FIFO connected/);
		assert.match(out, /PASS:test-box2-multi's second shell observably attached \(log evidence \+ live process\)/);
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

test("multi-shell FIFO holds (behavioral): readiness needing MORE than 30 polls still succeeds under the cold-pull windows (180s create / 90s attach), proven via shrunk injectable poll-interval/window constants rather than real minutes of sleep", () => {
	// The OLD script hardcoded a flat 30 at both call sites (30 polls at a real
	// 1s interval = 30s), which a cold post-`make load` image pull can exceed.
	// This proves the FIX (injectable UAT_CREATE_WAIT_SECS/UAT_ATTACH_WAIT_SECS,
	// each polling at an injectable UAT_POLL_INTERVAL) genuinely survives
	// readiness that needs well over 30 polling iterations, without the Node
	// suite actually spending 30+ real seconds per assertion: shrink the poll
	// interval so >30 iterations cost a fraction of a real second, while the
	// iteration COUNT still exceeds the old bound.
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-fifo-coldpull-"));
	try {
		const binDir = path.join(work, "bin");
		const markDir = path.join(work, "marks");
		const projDir = path.join(work, "b", "proj");
		fs.mkdirSync(binDir);
		fs.mkdirSync(markDir);
		fs.mkdirSync(projDir, { recursive: true });
		writeFakeMultiShellPix(binDir);

		const harness = [
			`PATH="${binDir}:$PATH"`,
			`export MARK_DIR="${markDir}"`,
			`WORK="${work}"`,
			'export BOX2="test-box2-coldpull"',
			// Real per-poll delay before each readiness marker appears, tuned
			// against the shrunk UAT_POLL_INTERVAL below so BOTH waits need
			// roughly 45 polling iterations to succeed — comfortably more than
			// the OLD hardcoded 30-iteration bound would ever have tolerated.
			'export CREATE_DELAY="0.9"',
			'export ATTACH_DELAY="0.9"',
			// Shrunk injectable constants: the same knobs production's default
			// 180s-create/90s-attach windows (at a real 1s poll interval) use,
			// scaled down together so the >30-iteration proof above takes a
			// fraction of a real second instead of real minutes.
			'export UAT_POLL_INTERVAL="0.02"',
			'export UAT_CREATE_WAIT_SECS="60"',
			'export UAT_ATTACH_WAIT_SECS="60"',
			"HOLD_PIDS=()",
			'die() { printf "DIE:%s\\n" "$1" >&2; exit 9; }',
			'pass() { printf "PASS:%s\\n" "$1"; }',
			'fail() { printf "FAIL:%s:%s\\n" "$1" "${2:-}"; }',
			"head1() { :; }",
			"newbox() { :; }",
			multiShellHelperFns,
			multiShellSectionBlock(),
		].join("\n");

		const startedAt = Date.now();
		const out = execFileSync("bash", ["-c", harness], { encoding: "utf8", timeout: 45000 });
		const elapsedMs = Date.now() - startedAt;

		assert.match(out, /PASS:test-box2-coldpull observed via pix ls --json after the first shell's FIFO connected/);
		assert.match(out, /PASS:test-box2-coldpull's second shell observably attached \(log evidence \+ live process\)/);
		assert.match(out, /PASS:test-box2-coldpull is up with two shells attached/);
		assert.match(out, /PASS:sandbox survives the FIRST shell leaving/);
		assert.match(out, /PASS:teardown on last shell exit/);
		assert.doesNotMatch(out, /FAIL:/);
		// The whole point: readiness that needed far more than 30 polls still
		// finishes in well under a real minute (in practice under a couple of
		// real seconds), proving the shrunk constants — not a real 30-180s wait —
		// are what keeps this test fast.
		assert.ok(elapsedMs < 20000, `expected the shrunk-constant run to stay fast, took ${elapsedMs}ms`);
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
});

test("multi-shell FIFO holds (behavioral): a create that never gets listed within the bound FAILs with the log tail, it does not hang or silently pass", () => {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-fifo-neverlisted-"));
	try {
		const binDir = path.join(work, "bin");
		const markDir = path.join(work, "marks");
		fs.mkdirSync(binDir);
		fs.mkdirSync(markDir);
		// A `pix` that blocks on `run` forever accepting stdin but NEVER marks
		// itself created, and whose `ls --json` therefore always answers `[]`.
		const fakePix = path.join(binDir, "pix");
		fs.writeFileSync(
			fakePix,
			[
				"#!/bin/sh",
				'if [ "$1" = "run" ] && [ "$2" = "." ]; then',
				"  cat >/dev/null",
				"  exit 0",
				'elif [ "$1" = "ls" ]; then',
				"  printf '[]\\n'",
				"  exit 0",
				"fi",
				"exit 1",
				"",
			].join("\n"),
			{ mode: 0o755 },
		);
		const fns = [extractFn("ls_json_lists"), extractFn("bounded_wait_listed")].join("\n");
		// bounded_wait_listed's own default is 30s; a dedicated short-bound test
		// keeps this fast without editing the real function.
		const harness = [`PATH="${binDir}:$PATH"`, `TMPDIR="${work}"`, `UAT_POLL_INTERVAL="0.01"`, fns, 'bounded_wait_listed "never-created-box" 2; echo "RC=$?"'].join("\n");
		const out = execFileSync("bash", ["-c", harness], { encoding: "utf8", timeout: 15000 });
		assert.match(out, /RC=1/, "a genuinely never-listed box must time out (rc 1), not hang and not report an ls failure (rc 2)");
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
});

// --- OAuth: doctor evidence gates auth BEFORE forcing a browser flow --------
// extractAuthGapsBlock — the literal PRE_AUTH_DOCTOR/AUTH_GAPS block plus the
// if/else that either PASSes with zero `pix mcp auth` invocations or
// authorizes only the gap set, so the behavioral tests below run the ACTUAL
// shipped logic, not a reimplementation of it.
function extractAuthGapsBlock() {
	const startMarker = 'PRE_AUTH_DOCTOR="$(pix doctor --json';
	const start = script.indexOf(startMarker);
	assert.ok(start !== -1, "could not find the PRE_AUTH_DOCTOR/AUTH_GAPS block");
	const endMarker = 'assert_exit 0 "pix mcp auth $s completed" pix mcp auth "$s"\n    done\n  fi';
	const end = script.indexOf(endMarker, start);
	assert.ok(end !== -1, "could not find the end of the PRE_AUTH_DOCTOR/AUTH_GAPS block");
	return script.slice(start, end + endMarker.length);
}

// runAuthGapsBlock DOCTOR_JSON — runs the extracted block under a stub `pix`
// that records every `mcp auth <name>` invocation it receives (one file per
// call, in order), so the test can assert exactly which servers were (and
// were not) sent through a browser flow.
function runAuthGapsBlock(doctorJson) {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-authgaps-"));
	try {
		const binDir = path.join(work, "bin");
		fs.mkdirSync(binDir);
		const callLog = path.join(work, "auth-calls.log");
		const fakePix = path.join(binDir, "pix");
		fs.writeFileSync(
			fakePix,
			[
				"#!/bin/sh",
				'if [ "$1 $2" = "doctor --json" ]; then',
				'  printf "%s" "$DOCTOR_JSON"',
				"  exit 0",
				'elif [ "$1 $2" = "mcp auth" ]; then',
				`  printf '%s\\n' "$3" >> "${callLog}"`,
				"  exit 0",
				"fi",
				"exit 1",
				"",
			].join("\n"),
			{ mode: 0o755 },
		);
		const block = extractAuthGapsBlock();
		const harness = [
			`PATH="${binDir}:$PATH"`,
			"set -uo pipefail",
			'pass() { printf "PASS:%s\\n" "$1"; }',
			'fail() { printf "FAIL:%s:%s\\n" "$1" "${2:-}"; }',
			'assert_exit() { local want="$1" name="$2"; shift 2; local out; out="$("$@" 2>&1)"; local got=$?; if [ "$got" = "$want" ]; then pass "$name (exit $got)"; else fail "$name" "wanted exit $want, got $got"; fi; }',
			block,
		].join("\n");
		const out = execFileSync("bash", ["-c", harness], {
			encoding: "utf8",
			env: { ...process.env, DOCTOR_JSON: doctorJson },
		});
		const calls = fs.existsSync(callLog)
			? fs.readFileSync(callLog, "utf8").trim().split("\n").filter(Boolean)
			: [];
		return { out, calls };
	} finally {
		fs.rmSync(work, { recursive: true, force: true });
	}
}

test("OAuth auth gate (behavioral): all three already registered+authenticated per doctor evidence PASSes with ZERO `pix mcp auth` invocations", () => {
	const doctorJson =
		"notion: registered (host registration; attachment to a live session is not checkable from here); " +
		"atlassian: registered (host registration; attachment to a live session is not checkable from here); " +
		"granola: registered (host registration; attachment to a live session is not checkable from here)";
	const { out, calls } = runAuthGapsBlock(doctorJson);
	assert.match(out, /PASS:no OAuth gaps: notion\/atlassian\/granola are already registered\+authenticated/);
	assert.doesNotMatch(out, /FAIL:/);
	assert.deepStrictEqual(calls, [], "pix mcp auth must never be invoked when every server is already registered+authenticated");
});

test("OAuth auth gate (behavioral): only the servers with a real gap are authorized, individually — an already-authenticated server is skipped", () => {
	const doctorJson =
		"notion: registered (host registration; attachment to a live session is not checkable from here); " +
		"atlassian: not registered; " +
		"granola: registered, not authenticated";
	const { out, calls } = runAuthGapsBlock(doctorJson);
	assert.doesNotMatch(out, /PASS:no OAuth gaps/);
	assert.match(out, /PASS:pix mcp auth atlassian completed/);
	assert.match(out, /PASS:pix mcp auth granola completed/);
	assert.doesNotMatch(out, /pix mcp auth notion completed/);
	assert.doesNotMatch(out, /FAIL:/);
	assert.deepStrictEqual(calls.sort(), ["atlassian", "granola"], "only the gap servers should ever be sent through pix mcp auth, and notion (already authenticated) must never be one of them");
});

test("OAuth auth gate (behavioral): a totally empty doctor payload treats every server as a gap and authorizes all three (never silently skips an unclassifiable server)", () => {
	const { out, calls } = runAuthGapsBlock("");
	assert.doesNotMatch(out, /PASS:no OAuth gaps/);
	assert.doesNotMatch(out, /FAIL:/);
	assert.deepStrictEqual(calls.sort(), ["atlassian", "granola", "notion"]);
});

// --- ls_json_lists / bounded_wait_listed: a pix-ls FAILURE is never an ABSENCE ---
// The exact false-negative this closes: `pix ls 2>/dev/null | grep -q NAME`
// answers "not found" identically whether the sandbox is truly absent OR
// `pix ls` itself crashed/errored — a broken `pix` binary would silently read
// as "box not up yet" instead of the real, actionable fact that ls itself is
// broken.
test("ls_json_lists (behavioral): reports LISTED / ABSENT / ERROR as three distinct outcomes, never collapsing a failure into absence", () => {
	const work = fs.mkdtempSync(path.join(os.tmpdir(), "pix-uat-lsjson-"));
	try {
		const binDir = path.join(work, "bin");
		fs.mkdirSync(binDir);
		const fakePix = path.join(binDir, "pix");
		fs.writeFileSync(
			fakePix,
			[
				"#!/bin/sh",
				'if [ "$1 $2" = "ls --json" ]; then',
				'  if [ "${LS_FAIL:-0}" = "1" ]; then echo "boom: sbx ls failed" >&2; exit 1; fi',
				'  if [ "${LS_HAS_BOX:-0}" = "1" ]; then printf \'[{"name": "box-a", "state": "running"}]\\n\'; else printf \'[]\\n\'; fi',
				"  exit 0",
				"fi",
				"exit 1",
				"",
			].join("\n"),
			{ mode: 0o755 },
		);
		const fn = extractFn("ls_json_lists");
		const run = (env) =>
			execFileSync("bash", ["-c", `PATH="${binDir}:$PATH"\nTMPDIR="${work}"\n${fn}\nls_json_lists box-a`], {
				encoding: "utf8",
				env: { ...process.env, ...env },
			}).trim();

		assert.strictEqual(run({ LS_HAS_BOX: "1" }), "LISTED");
		assert.strictEqual(run({ LS_HAS_BOX: "0" }), "ABSENT");
		assert.match(run({ LS_FAIL: "1" }), /^ERROR:.*boom: sbx ls failed/, "a failed pix ls must report ERROR:<detail>, never ABSENT");
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

test("multi-shell FIFO holds: presence/absence of $BOX2 is asserted through pix ls --json, distinguishing an ls failure from a genuine absence, not a bare grep on possibly-failed output", () => {
	const multiShell = script.slice(script.indexOf("[6] Multi-shell"), script.indexOf("[7] --keep"));
	assert.match(multiShell, /bounded_wait_listed "\$BOX2" "\$UAT_CREATE_WAIT_SECS"/);
	assert.match(multiShell, /bounded_wait_attach_log "\$LOG2" "\$SH2" "\$UAT_ATTACH_WAIT_SECS"/);
	assert.match(multiShell, /assert_box_listed "\$BOX2"/);
	assert.match(multiShell, /assert_box_not_listed "\$BOX2" "teardown on last shell exit"/);
	// Each backgrounded pix run now logs to its OWN file, never /dev/null, so
	// this script can look for pix's real evidence instead of merely inferring
	// readiness from the FIFO connect.
	assert.match(multiShell, /LOG1="\$FIFO_DIR\/shell1\.log"; LOG2="\$FIFO_DIR\/shell2\.log"/);
	assert.doesNotMatch(multiShell, />\/dev\/null 2>&1\) &/);
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
