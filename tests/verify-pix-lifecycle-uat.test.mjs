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
import test from "node:test";

const scriptPath = new URL("../scripts/macos/verify-pix-lifecycle.sh", import.meta.url);
const script = fs.readFileSync(scriptPath, "utf8");

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

test("a missing/closed TTY or a timed-out read is SKIP, never FAIL, for the operator confirmation", () => {
	const oauthSection = script.slice(script.indexOf("[8] External OAuth"));
	assert.match(oauthSection, /skip "operator confirmation" "no controlling TTY available/);
	assert.match(oauthSection, /skip "operator confirmation" "no answer on \/dev\/tty within 30s/);
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

test("a catalog bundle already present before this run is left alone, not claimed as ours to remove", () => {
	assert.match(script, /already registered before this run.*not ours to add or remove/);
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
	// A mismatch must die with the exact repair command, not merely warn.
	assert.match(hostSection, /die "a serve is already running.*pix serve stop.*re-run this script/s);
	// An unresolvable comparison is SKIP (unproven), never a silent match.
	assert.match(hostSection, /skip "which binary the running serve is"/);
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
