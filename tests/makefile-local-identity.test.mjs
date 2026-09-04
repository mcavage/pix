// Static tests for the Makefile's LOCAL build identity and its worktree
// scoping. These are the two properties that only bite on a machine running
// more than one Pix stack, which is exactly the machine nobody tests on:
// a `make load` that prunes another worktree's templates, and a `make run`
// that pins every checkout to one hardcoded sandbox name.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const makefile = fs.readFileSync(path.join(repoRoot, "Makefile"), "utf8");

// The recipe as make itself expands it, not as it is written: a variable
// that silently expanded to empty would pass a source-text grep and then
// prune every template on the host.
function dryRun(target) {
	return execFileSync("make", ["-n", target], { cwd: repoRoot, encoding: "utf8" });
}

test("local builds tag BOTH images at the derived LAUNCHER_VERSION, and build-memory's VERSION arg matches", () => {
	assert.match(makefile, /LOCAL_AGENT_IMAGE\s+\?= docker\.io\/\$\(DOCKER_USER\)\/pix-agent:\$\(LAUNCHER_VERSION\)/);
	assert.match(makefile, /LOCAL_MEMORY_IMAGE\s+\?= docker\.io\/\$\(DOCKER_USER\)\/pix-memory:\$\(LAUNCHER_VERSION\)/);
	assert.match(
		makefile,
		/build-memory:.*\n\tdocker build -f \$\(MEMORY_DOCKERFILE\) --build-arg VERSION=\$\(LAUNCHER_VERSION\) -t \$\(LOCAL_MEMORY_IMAGE\)/,
		"the image tag and the VERSION build arg must be the SAME identity",
	);
});

test("publish targets stay on the clean VERSION and never push a local build", () => {
	assert.match(makefile, /^publish-agent: ##/m, "publish must not depend on the local-tagging build-agent");
	assert.match(makefile, /^publish-memory: ##/m, "publish must not depend on the local-tagging build-memory");
	const publish = makefile.slice(makefile.indexOf("publish-agent:"), makefile.indexOf("# The SAME gate CI runs"));
	assert.doesNotMatch(publish, /LAUNCHER_VERSION/, "a publish recipe must never name the derived local identity");
	assert.match(publish, /docker push \$\(AGENT_IMAGE\)/);
	assert.match(publish, /--build-arg VERSION=\$\(VERSION\)/, "a published pix-memory is built at the clean VERSION");
});

test("the release bundle binds ONE identity: binary, runtime archive, manifest and both image digests", () => {
	assert.match(makefile, /go build -ldflags "-X main\.version=\$\(LAUNCHER_VERSION\)"/);
	assert.match(makefile, /build-runtime-archive\.sh \$\(LAUNCHER_VERSION\)/);
	assert.match(makefile, /--version \$\(LAUNCHER_VERSION\)/);
	assert.match(makefile, /docker image inspect \$\(LOCAL_AGENT_IMAGE\)/, "the manifest must read the LOCAL agent digest");
	assert.match(makefile, /docker image inspect \$\(LOCAL_MEMORY_IMAGE\)/, "the manifest must read the LOCAL memory digest");
});

test("make load scopes its unique tag AND its prune to this worktree", () => {
	const load = dryRun("load");
	const hash = execFileSync("bash", ["-c", `printf '%s' "$(cd ${repoRoot} && pwd -P)" | { command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256; } | cut -c1-12`], {
		encoding: "utf8",
	}).trim();
	assert.match(hash, /^[0-9a-f]{12}$/, "the worktree hash must be 12 hex characters");
	assert.ok(load.includes(`TS="local-${hash}-`), `the loaded tag must carry this worktree's hash: ${load.split("\n")[1]}`);
	assert.ok(load.includes(`$2 ~ /^local-${hash}-/`), "the prune must match ONLY this worktree's tags");
	assert.ok(!/\$2=="[0-9]/.test(load), "the prune must not also match a published VERSION tag");
	assert.ok(!/\$2 ~ \/\^local-\/\{/.test(load), "an unscoped /^local-/ prune deletes other worktrees' templates");
});

test("make run pins no fixed sandbox name and passes --name only when one was asked for", () => {
	assert.match(makefile, /^NAME \?=\s*$/m, "NAME must default to empty so the launcher derives a stack-scoped name");
	assert.doesNotMatch(makefile, /^NAME \?= pix-pix/m, "the hardcoded pix-pix default is what broke coexistence");
	const run = dryRun("run");
	assert.ok(run.includes('${N:+--name "$N"}'), "--name must be conditional on an explicit override");
	assert.ok(!run.includes("--name pix-pix"), "no fixed name may reach the launcher");
});

test("make run WITH an explicit NAME still passes it through safely", () => {
	const run = execFileSync("make", ["-n", "run", "NAME=pix-explicit"], { cwd: repoRoot, encoding: "utf8" });
	assert.ok(run.includes('N="pix-explicit"'), "an explicit NAME must still reach the recipe");
	assert.ok(run.includes('${N:+--name "$N"}'), "and still be passed through the same quoted expansion");
});
