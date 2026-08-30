// Fast static tests for the pix-v2 packaging artifact graph
// (docs/design/pix-v2-architecture.md §3): images/agent/Dockerfile is the
// canonical pix-agent build, pix-agent and pix-memory are independently
// tagged/build/publish targets, there is exactly ONE host binary (no
// pix-host), and the release manifest mechanism binds version + both image
// digests + the runtime digest + the kit revision. No Docker daemon is
// required: every assertion here is file-shape or dry-run (`make -n`).
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const dockerfile = fs.readFileSync(path.join(repoRoot, "images/agent/Dockerfile"), "utf8");
const makefile = fs.readFileSync(path.join(repoRoot, "Makefile"), "utf8");
const kitSpec = fs.readFileSync(path.join(repoRoot, "pi-kit/spec.yaml"), "utf8");
const pkg = JSON.parse(fs.readFileSync(path.join(repoRoot, "package.json"), "utf8"));

test("images/agent/Dockerfile is the canonical pix-agent build; there is no duplicate root Dockerfile", () => {
	assert.ok(fs.existsSync(path.join(repoRoot, "images/agent/Dockerfile")), "images/agent/Dockerfile must exist");
	assert.equal(
		fs.existsSync(path.join(repoRoot, "Dockerfile")),
		false,
		"a root Dockerfile duplicates images/agent/Dockerfile; pix has no user compatibility contract requiring one",
	);
});

test("PI_PACKAGE is pinned to the accepted 0.84.4 release", () => {
	assert.match(dockerfile, /ARG PI_PACKAGE=@earendil-works\/pi-coding-agent@0\.84\.4/);
});

test("services/memory/Dockerfile is the independent pix-memory build (no shared Dockerfile with pix-agent)", () => {
	assert.ok(fs.existsSync(path.join(repoRoot, "services/memory/Dockerfile")));
	const memoryDockerfile = fs.readFileSync(path.join(repoRoot, "services/memory/Dockerfile"), "utf8");
	// The two images share no FROM/base: pix-memory never installs Node/Pi.
	assert.doesNotMatch(memoryDockerfile, /npm install -g/);
	assert.doesNotMatch(memoryDockerfile, /PI_PACKAGE/);
});

test("Makefile defines independent build/publish targets for pix-agent and pix-memory", () => {
	for (const target of ["build-agent", "build-memory", "publish-agent", "publish-memory"]) {
		assert.match(makefile, new RegExp(`^${target}:`, "m"), `Makefile is missing the ${target} target`);
	}
	assert.match(makefile, /AGENT_IMAGE\s*\?=\s*docker\.io\/\$\(DOCKER_USER\)\/pix-agent:\$\(VERSION\)/);
	assert.match(makefile, /MEMORY_IMAGE\s*\?=\s*docker\.io\/\$\(DOCKER_USER\)\/pix-memory:\$\(VERSION\)/);
	assert.match(makefile, /AGENT_DOCKERFILE\s*\?=\s*images\/agent\/Dockerfile/);
	assert.match(makefile, /MEMORY_DOCKERFILE\s*\?=\s*services\/memory\/Dockerfile/);
});

test("Makefile builds and installs exactly ONE host binary (pix); pix-host is gone", () => {
	const launcherBlock = makefile.slice(makefile.indexOf("\nlauncher:"), makefile.indexOf("\nmcp-auth:"));
	assert.match(launcherBlock, /-o \$\(CURDIR\)\/out\/pix /);
	assert.doesNotMatch(launcherBlock, /out\/pix-host/);

	const installBlock = makefile.slice(makefile.indexOf("\ninstall:"), makefile.indexOf("\nclean:"));
	assert.match(installBlock, /ln -sf \$\(CURDIR\)\/out\/pix \$\(HOME\)\/\.local\/bin\/pix\n/);
	assert.doesNotMatch(installBlock, /pix-host/);
});

test("make serve / memory-serve / host-service targets are removed", () => {
	assert.doesNotMatch(makefile, /^serve:/m);
	assert.doesNotMatch(makefile, /^memory-serve:/m);
	assert.doesNotMatch(makefile, /^host-service:/m);
	assert.doesNotMatch(makefile, /\.PHONY:.*\bserve\b/);
	assert.doesNotMatch(makefile, /\.PHONY:.*\bmemory-serve\b/);
});

test("Makefile defines runtime-archive and release-manifest targets", () => {
	assert.match(makefile, /^runtime-archive:/m);
	assert.match(makefile, /^release-manifest:/m);
	assert.match(makefile, /scripts\/release\/build-runtime-archive\.sh/);
	assert.match(makefile, /scripts\/release\/emit-manifest\.mjs/);
});

test("`make -n` dry-runs the whole build/publish/runtime-archive graph without a Docker daemon", () => {
	for (const target of ["build", "build-agent", "build-memory", "load", "publish", "publish-agent", "publish-memory", "launcher", "install", "clean", "runtime-archive"]) {
		const res = spawnSync("make", ["-n", target], { cwd: repoRoot, encoding: "utf8" });
		assert.equal(res.status, 0, `make -n ${target} failed:\n${res.stdout}${res.stderr}`);
	}
});

test("the ONE pix binary actually builds standalone from services/host/cmd/pix (no pix-host needed)", (t) => {
	if (spawnSync("go", ["version"], { encoding: "utf8" }).status !== 0) {
		t.skip("go toolchain not on PATH in this environment");
		return;
	}
	const out = fs.mkdtempSync(path.join(os.tmpdir(), "pix-artifact-graph-build-"));
	t.after(() => fs.rmSync(out, { recursive: true, force: true }));
	execFileSync("go", ["build", "-o", path.join(out, "pix"), "./cmd/pix"], {
		cwd: path.join(repoRoot, "services/host"),
		encoding: "utf8",
	});
	assert.ok(fs.existsSync(path.join(out, "pix")));
});

test("pi-kit/spec.yaml points at the pix-agent artifact, consistently with the Makefile", () => {
	assert.match(kitSpec, /image:\s*"docker\.io\/mcavage\/pix-agent:[^"]+"/);
	assert.doesNotMatch(kitSpec, /image:\s*"docker\.io\/mcavage\/pix:[^"]+"/);
	const kitVersion = kitSpec.match(/image:\s*"docker\.io\/mcavage\/pix-agent:([^"]+)"/)[1];
	const makeVersion = makefile.match(/^VERSION\s*\?=\s*(\S+)/m)[1];
	assert.equal(kitVersion, makeVersion, "pi-kit/spec.yaml's pinned image tag must match the Makefile VERSION");
});

test("CI publish.yml builds pix-agent from images/agent/Dockerfile and pix-memory from services/memory/Dockerfile", () => {
	const workflow = fs.readFileSync(path.join(repoRoot, ".github/workflows/publish.yml"), "utf8");
	assert.match(workflow, /file:\s*images\/agent\/Dockerfile/);
	assert.match(workflow, /file:\s*services\/memory\/Dockerfile/);
	assert.match(workflow, /AGENT_IMAGE:\s*docker\.io\/.*pix-agent/);
	assert.match(workflow, /MEMORY_IMAGE:\s*docker\.io\/.*pix-memory/);
	assert.match(workflow, /release-manifest:/);
	assert.match(workflow, /emit-manifest\.mjs/);
});

test("release-manifest binds ONE version to both image digests, the runtime digest, and the kit revision", async () => {
	const genScript = path.join(repoRoot, "scripts/release/emit-manifest.mjs");
	const gen = await import(`${genScript.replace(/\\/g, "/")}?t=${Date.now()}`).catch(() => import(genScript));
	const validDigest = `sha256:${"a".repeat(64)}`;
	const manifest = gen.buildManifest({
		version: "1.2.3",
		agentDigest: validDigest,
		memoryDigest: validDigest,
		runtimeDigest: validDigest,
		kitRevision: "0123456789abcdef0123456789abcdef01234567",
	});
	assert.equal(manifest.version, "1.2.3");
	assert.equal(manifest.artifacts["pix-agent"].digest, validDigest);
	assert.equal(manifest.artifacts["pix-memory"].digest, validDigest);
	assert.equal(manifest.artifacts.runtime.digest, validDigest);
	assert.equal(manifest.kitRevision, "0123456789abcdef0123456789abcdef01234567");

	assert.throws(() => gen.buildManifest({ version: "1.2.3", agentDigest: "not-a-digest", memoryDigest: validDigest, runtimeDigest: validDigest, kitRevision: "abc1234" }));
	assert.throws(() => gen.buildManifest({ version: "not-semver", agentDigest: validDigest, memoryDigest: validDigest, runtimeDigest: validDigest, kitRevision: "abc1234" }));
	assert.throws(() => gen.buildManifest({ version: "1.2.3", agentDigest: validDigest, memoryDigest: validDigest, runtimeDigest: validDigest, kitRevision: "" }));
});

test("the runtime archive stages skills/agents/settings/keybindings/themes into the canonical runtime/<version>/ layout, without touching the live repo tree", (t) => {
	const out = fs.mkdtempSync(path.join(os.tmpdir(), "pix-runtime-archive-"));
	t.after(() => fs.rmSync(out, { recursive: true, force: true }));
	const archive = path.join(out, "pix-runtime-9.9.9.tar.gz");

	execFileSync("bash", [path.join(repoRoot, "scripts/release/build-runtime-archive.sh"), "9.9.9", archive], {
		cwd: repoRoot,
		encoding: "utf8",
	});
	assert.ok(fs.existsSync(archive));

	const listing = execFileSync("tar", ["-tzf", archive], { encoding: "utf8" });
	for (const member of [
		"runtime/9.9.9/skills/",
		"runtime/9.9.9/agents/",
		"runtime/9.9.9/pi/settings.json",
		"runtime/9.9.9/pi/keybindings.json",
		"runtime/9.9.9/pi/themes/",
		"runtime/9.9.9/manifest.json",
	]) {
		assert.ok(listing.includes(member), `runtime archive is missing ${member}`);
	}

	// Building the archive must never move or rewrite the LIVE repo-root
	// directories `make run`'s dev-mode --skill flag reads (Mode B live
	// skills): the archive step only COPIES into a disposable temp stage.
	assert.ok(fs.existsSync(path.join(repoRoot, "skills")), "repo-root skills/ must remain in place after building the runtime archive");
	assert.ok(fs.existsSync(path.join(repoRoot, "agents")), "repo-root agents/ must remain in place after building the runtime archive");
});

test("Makefile dev-mode live-skill loading is untouched by the runtime-archive step", () => {
	assert.match(makefile, /DEV_SKILLS = --no-skills --skill \$\(CURDIR\)\/skills/);
});

test("the pix-agent Dockerfile keeps the vendored patch toolchain (scripts/patches/) and required tooling", () => {
	for (const pattern of [
		/COPY scripts\/patches\/ \/usr\/local\/share\/pix\/patches\//,
		/apply-pix-resume-command\.mjs/,
		/apply-hide-host-state\.mjs/,
		/apply-tui-bottom-pin\.mjs/,
		/apply-mcp-problems-status\.mjs/,
		/apply-todo-durable-clear\.mjs/,
		/apply-web-access-gateway\.mjs/,
	]) {
		assert.match(dockerfile, pattern, `images/agent/Dockerfile no longer applies ${pattern}`);
	}
});

test("install.sh and the Homebrew formula install exactly the pix binary, no pix-host", () => {
	const installSh = fs.readFileSync(path.join(repoRoot, "install.sh"), "utf8");
	const formula = fs.readFileSync(path.join(repoRoot, "packaging/homebrew/pix.rb"), "utf8");
	assert.match(installSh, /BINARIES="pix"/);
	assert.doesNotMatch(installSh, /pix-host/);
	assert.match(formula, /bin\.install "pix"/);
	assert.doesNotMatch(formula, /pix-host/);
});
