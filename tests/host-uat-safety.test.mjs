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

test("host-uat.sh refuses BEFORE any mutation when a pre-existing resource under its OWN scoped names would be touched", () => {
	// Slice between the two STEP banners, not between raw substrings: the
	// header comment also mentions `make bundle`, so an indexOf on that
	// alone would produce an empty (vacuously passing) region.
	const preflightStart = script.indexOf('step "pre-flight');
	const buildStart = script.indexOf('step "build the real installable bundle');
	assert.ok(preflightStart > 0 && buildStart > preflightStart, "the pre-flight step must precede the build step");
	const preflight = script.slice(preflightStart, buildStart);
	assert.match(preflight, /memory_container_exists/, "the pre-flight must probe for an existing memory container");
	assert.match(preflight, /mcp_registration_exists/, "the pre-flight must probe for an existing memory MCP registration");
	assert.match(preflight, /\$MEMORY_CONTAINER/, "the refusal must name THIS run's scoped container, not the bare legacy name");
	assert.match(preflight, /\$MEMORY_MCP/, "the refusal must name THIS run's scoped MCP registration");
	assert.equal((preflight.match(/fail "/g) || []).length, 2, "both probes must refuse, not warn");
	// The refusal has to come before the first thing that changes the host.
	assert.ok(preflightStart < script.indexOf('step "U1 setup'), "the pre-flight must run before pix setup");
});

test("host-uat.sh holds a sandbox open with --keep whenever it inspects one before removing it", () => {
	// `pix run -- <cmd>` tears the sandbox down when that last shell exits.
	// Any row that lists, greps or removes the sandbox afterwards must have
	// asked for it to stay: without --keep those rows assert against a box
	// that is already gone, which passes or fails for the wrong reason.
	// Command lines only: the header comment and the make-load check also
	// mention `pix run --dev`, and a comment is not a launch.
	const launches = [...script.matchAll(/^(?!#)[^\n]*\bpix run --dev[^\n]*$/gm)]
		.map((m) => m[0])
		.filter((line) => !line.includes("|| fail \"make load"));
	assert.ok(launches.length >= 2, "the UAT must launch at least the two sandboxes it inspects");
	for (const line of launches) {
		assert.match(line, /--keep/, `a launch whose sandbox is inspected afterwards must pass --keep: ${line}`);
		assert.match(line, /--name "\$SANDBOX(_B)?"/, `every launch must name an explicit, stack-scoped sandbox so cleanup can find it: ${line}`);
	}
});

test("host-uat.sh names every sandbox with its OWN stack's scoped form", () => {
	assert.match(script, /SANDBOX="pix-\$STACK_ID-uat"/, "the first stack's sandbox must carry its own stack id");
	assert.match(script, /SANDBOX_B="pix-\$STACK_ID_B-uat"/, "the second stack's sandbox must carry ITS own stack id");
	// A short logical --name would be scoped by the launcher into the same
	// namespace, but the script would not know the resulting name to clean up,
	// so no row may use one.
	assert.doesNotMatch(script, /--name "b-uat"/, "a bare short name leaves cleanup guessing at the scoped result");
});

test("host-uat.sh cleans up only resources it proved it created", () => {
	for (const flag of ["CREATED_MEMORY_CONTAINER", "CREATED_MCP_REGISTRATION", "CREATED_SANDBOX", "CREATED_SANDBOX_B"]) {
		assert.match(script, new RegExp(`${flag}=0`), `${flag} must default to "not mine"`);
		assert.match(script, new RegExp(`if \\[ "\\$${flag}" = "1" \\]`), `cleanup must gate on ${flag}`);
	}
	// The unconditional `docker rm -f pix-memory` of the old script is the
	// exact bug this pins: it deleted a global container the run may not
	// have created.
	const cleanup = script.slice(script.indexOf("cleanup() {"), script.indexOf("trap cleanup EXIT"));
	const dockerRm = cleanup.indexOf('docker rm -f "$MEMORY_CONTAINER"');
	assert.ok(dockerRm > 0, "cleanup should still remove a container this run created");
	assert.ok(
		cleanup.slice(0, dockerRm).includes('if [ "$CREATED_MEMORY_CONTAINER" = "1" ]'),
		"the pix-memory removal must be guarded by this run's own ownership flag",
	);
});

test("host-uat.sh removes an MCP registration it created, best-effort and honestly", () => {
	const cleanup = script.slice(script.indexOf("cleanup() {"), script.indexOf("trap cleanup EXIT"));
	assert.match(cleanup, /sbx mcp rm "\$MEMORY_MCP"|sbx mcp remove "\$MEMORY_MCP"/, "it must try the native removal verb against its OWN scoped name");
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

test("host-uat.sh derives every Pix-owned name from a stack id it computes INDEPENDENTLY of the launcher", () => {
	assert.match(script, /stack_id_for\(\)/, "the script must derive the stack id itself, not ask pix for it");
	assert.match(script, /cut -c1-16/, "the stack id is the first 16 hex characters of the sha256 of the canonical PIX_HOME path");
	assert.match(script, /MEMORY_CONTAINER="pix-memory-\$STACK_ID"/, "the container name must be scoped");
	assert.match(script, /MEMORY_MCP="pix-memory-\$STACK_ID"/, "the memory MCP name must be scoped");
	assert.match(script, /SESSION_MCP="pix-session-\$STACK_ID"/, "the session MCP name must be scoped");
	// Bare, unscoped names must never be what a probe or a removal targets.
	assert.doesNotMatch(script, /docker rm -f pix-memory\b(?!-)/, "cleanup must never remove the bare legacy container name");
	assert.doesNotMatch(script, /name=\^pix-memory\$/, "no probe may match the bare legacy container name");
});

test("host-uat.sh proves TWO concurrent PIX_HOMEs coexist: distinct ids, containers, ports, MCP names and sandbox names", () => {
	const u8 = script.slice(script.indexOf('step "U8 coexistence'), script.indexOf('step "U9 reset'));
	assert.ok(u8.length > 0, "the coexistence section must exist");
	assert.match(u8, /HOME_B="\$\(mktemp -d/, "the second PIX_HOME must be its own throwaway temp dir");
	assert.match(u8, /\[ "\$STACK_ID_B" != "\$STACK_ID" \]/, "the two homes must derive different stack ids");
	assert.match(u8, /\[ "\$PORT_A" != "\$PORT_B" \]/, "the two homes must allocate different loopback memory ports");
	assert.match(u8, /MEMORY_CONTAINER_B/, "the second home must get its own memory container");
	assert.match(u8, /sbx mcp ls \| grep -q "\$MEMORY_MCP"/, "the first stack's MCP registration must survive the second stack's setup");
	assert.match(u8, /pix-\$STACK_ID_B-/, "the second stack's sandbox names must carry its own stack id");
});

test("host-uat.sh proves resetting stack B leaves stack A intact, and never adopts B's resources", () => {
	const u9 = script.slice(script.indexOf('step "U9 reset'));
	assert.match(u9, /PIX_HOME="\$HOME_B" pix reset --yes/, "reset must run against the SECOND home");
	assert.match(u9, /grep -qx "\$MEMORY_CONTAINER"/, "stack A's container must still exist afterwards");
	assert.match(u9, /grep -q "\$MEMORY_MCP"/, "stack A's MCP registration must still exist afterwards");
	assert.match(u9, /\.state\/memory\/port/, "stack A's memory port state must be unchanged");
	// The second home's container is cleaned up only if this run created it.
	assert.match(script, /CREATED_MEMORY_CONTAINER_B=0/, "the second home's container flag must default to \"not mine\"");
	assert.match(script, /if \[ "\$CREATED_MEMORY_CONTAINER_B" = "1" \]/, "cleanup must gate on it");
	// And so is its sandbox: a failure between the launch and `pix rm --all`
	// must not leave the second stack's kept sandbox running on the host.
	const cleanup = script.slice(script.indexOf("cleanup() {"), script.indexOf("trap cleanup EXIT"));
	assert.match(cleanup, /PIX_HOME="\$HOME_B" pix rm "\$SANDBOX_B" --force/, "cleanup must remove the second stack's sandbox from ITS own home");
	assert.ok(
		cleanup.indexOf('if [ "$CREATED_SANDBOX_B" = "1" ]') < cleanup.indexOf('pix rm "$SANDBOX_B"'),
		"that removal must be gated by this run's own ownership flag",
	);
});

test("host-uat.sh reports the global-secret row honestly instead of claiming a negative it cannot prove", () => {
	const u7 = script.slice(script.indexOf('step "U7 no HOST-GLOBAL'), script.indexOf('step "U8 coexistence'));
	assert.match(u7, /sbx secret ls --global/, "it must actually look at the global scope");
	assert.match(u7, /cannot prove the negative/, "a host that already had global secrets must get a NOTE, not a pass");
	assert.doesNotMatch(u7, /sbx secret rm/, "the UAT must never remove a global secret it did not create");
});
