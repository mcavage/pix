// Static safety tests for scripts/host-uat.sh — the HOST-ONLY acceptance
// run. The script itself cannot be executed here (it needs a real Docker
// daemon and the sbx CLI), so what is pinned is the shape that makes it
// safe to run against a developer's real machine, plus `bash -n` so a
// broken edit is caught by CI rather than by the human running it.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const scriptPath = path.join(repoRoot, "scripts/host-uat.sh");
const script = fs.readFileSync(scriptPath, "utf8");

test("host-uat.sh parses (bash -n)", () => {
	const r = spawnSync("bash", ["-n", scriptPath], { encoding: "utf8" });
	assert.equal(r.status, 0, r.stderr);
});

test("host-uat.sh runs the REAL adjacent bundle and the REAL agent image, not a bare temp binary", () => {
	assert.match(script, /make bundle/, "it must build the installable bundle (out/pix + manifest + runtime archive)");
	assert.match(script, /make load/, "it must build and load the pix-agent image into sbx");
	assert.match(script, /PIX="\$REPO\/out\/pix"/, "it must exercise repo out/pix, whose adjacent release manifest setup discovers");
	assert.doesNotMatch(
		script,
		/go build -o "\$PIX_HOME/,
		"a bare temp binary has no release-manifest.json beside it, so `pix setup` refuses; that shape is not what any user installs",
	);
});

test("host-uat.sh launches with --dev so it consumes out/.local-image-tag", () => {
	assert.match(script, /pix run --dev/, "the launch row must use --dev to pin the image make load just wrote");
	assert.match(script, /out\/\.local-image-tag/, "it must verify the local image tag exists before launching");
});

test("host-uat.sh refuses BEFORE any mutation when a pre-existing pix-memory container or MCP registration would be touched", () => {
	// Slice between the two STEP banners, not between raw substrings: the
	// header comment also mentions `make bundle`, so an indexOf on that
	// alone would produce an empty (vacuously passing) region.
	const preflightStart = script.indexOf('step "pre-flight');
	const buildStart = script.indexOf('step "build the real installable bundle');
	assert.ok(preflightStart > 0 && buildStart > preflightStart, "the pre-flight step must precede the build step");
	const preflight = script.slice(preflightStart, buildStart);
	assert.match(preflight, /memory_container_exists/, "the pre-flight must probe for an existing pix-memory container");
	assert.match(preflight, /mcp_registration_exists/, "the pre-flight must probe for an existing pix-memory MCP registration");
	assert.equal((preflight.match(/fail "/g) || []).length, 2, "both probes must refuse, not warn");
	// The refusal has to come before the first thing that changes the host.
	assert.ok(preflightStart < script.indexOf('step "U1 setup'), "the pre-flight must run before pix setup");
});

test("host-uat.sh cleans up only resources it proved it created", () => {
	for (const flag of ["CREATED_MEMORY_CONTAINER", "CREATED_MCP_REGISTRATION", "CREATED_SANDBOX"]) {
		assert.match(script, new RegExp(`${flag}=0`), `${flag} must default to "not mine"`);
		assert.match(script, new RegExp(`if \\[ "\\$${flag}" = "1" \\]`), `cleanup must gate on ${flag}`);
	}
	// The unconditional `docker rm -f pix-memory` of the old script is the
	// exact bug this pins: it deleted a global container the run may not
	// have created.
	const cleanup = script.slice(script.indexOf("cleanup() {"), script.indexOf("trap cleanup EXIT"));
	const dockerRm = cleanup.indexOf("docker rm -f pix-memory");
	assert.ok(dockerRm > 0, "cleanup should still remove a container this run created");
	assert.ok(
		cleanup.slice(0, dockerRm).includes('if [ "$CREATED_MEMORY_CONTAINER" = "1" ]'),
		"the pix-memory removal must be guarded by this run's own ownership flag",
	);
});

test("host-uat.sh removes an MCP registration it created, best-effort and honestly", () => {
	const cleanup = script.slice(script.indexOf("cleanup() {"), script.indexOf("trap cleanup EXIT"));
	assert.match(cleanup, /sbx mcp rm pix-memory|sbx mcp remove pix-memory/, "it must try the native removal verb");
	assert.match(
		cleanup,
		/no MCP removal verb/,
		"when this sbx cannot remove a registration, the script must say the host is not clean rather than claiming it is",
	);
});

test("host-uat.sh keeps a throwaway PIX_HOME and never touches ~/.pix", () => {
	assert.match(script, /PIX_HOME="\$\(mktemp -d/, "PIX_HOME must be a temp directory");
	assert.match(script, /rm -rf "\$PIX_HOME"/, "the temp PIX_HOME must be removed on exit");
	assert.doesNotMatch(script, /rm -rf "?\$HOME\/\.pix/, "it must never delete the user's real PIX_HOME");
});

test("host-uat.sh authors valid native environment fixtures outside their writable roots", () => {
	const fixtures = [...script.matchAll(/cat > "\$PIX_HOME\/envs\/[^\n]+\/\.sbxenv\.yaml" <<'YAML'\n([\s\S]*?)\nYAML/g)];
	assert.equal(fixtures.length, 2, "U4 and U4b must each author one native environment fixture");
	for (const [, yaml] of fixtures) {
		assert.match(yaml, /^schemaVersion: "1"$/m, "native sbx environments use schemaVersion, not version");
		assert.match(yaml, /^agent: pix$/m, "the effective agent must be Pix");
		assert.doesNotMatch(yaml, /^workspace:/m, "the env root must not mount itself writable; launch supplies the project workspace");
	}
});

test("host-uat.sh exercises the [[setup]] hook path, the v2 replacement for pack install hooks", () => {
	assert.match(script, /\[\[setup\]\]/, "the UAT must cover environment setup hooks");
	assert.match(script, /pix setup --env hooked/, "hooks run only through an explicit pix setup --env");
	assert.ok(
		script.indexOf("pix env trust hooked") < script.indexOf("pix setup --env hooked"),
		"the hook environment must be trusted before its hooks are allowed to run",
	);
});
