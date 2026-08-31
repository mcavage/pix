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

test("install packages ONE binary plus its adjacent release bundle, in the right order", () => {
	// `pix setup` discovers release-manifest.json and pix-runtime-<version>.tar.gz
	// NEXT TO the resolved pix binary, so `make install` must produce all three in
	// out/ before symlinking — a symlinked binary with no bundle beside its target
	// is exactly the broken installation setup now refuses.
	assert.match(makefile, /^bundle: launcher release-manifest/m);
	assert.match(makefile, /^install: bundle/m);
	assert.match(makefile, /^release-manifest: build-agent build-memory runtime-archive/m);
	const bundleBlock = makefile.slice(makefile.indexOf("\nbundle:"), makefile.indexOf("\ninstall:"));
	for (const artifact of ["out/pix", "out/release-manifest.json", "out/pix-runtime-$(VERSION).tar.gz"]) {
		assert.ok(bundleBlock.includes(artifact), `make bundle must produce ${artifact}`);
	}
});

test("the Makefile no longer references removed commands or removed targets", () => {
	for (const gone of [/\bpix config get\b/, /\bpix mcp \w/, /\bpix models\b/, /pix-host/, /^serve:/m, /^pack:/m, /^mcp-register:/m, /^mcp-auth:/m, /^pull-models:/m, /^doctor:/m]) {
		assert.doesNotMatch(makefile.replace(/`pix config get` does not exist in v2/g, "").replace(/There is no `pix config get`/g, ""), gone, `Makefile still offers removed behavior: ${gone}`);
	}
});

test("the launcher's bundle file names match what the release targets actually write", () => {
	const bundleGo = fs.readFileSync(path.join(repoRoot, "services/host/release/bundle.go"), "utf8");
	assert.match(bundleGo, /BundleManifestFile = "release-manifest\.json"/);
	assert.match(bundleGo, /return "pix-runtime-" \+ version \+ "\.tar\.gz"/);
	assert.match(makefile, /out\/release-manifest\.json/);
	assert.match(makefile, /out\/pix-runtime-\$\(VERSION\)\.tar\.gz/);
	const archiveScript = fs.readFileSync(path.join(repoRoot, "scripts/release/build-runtime-archive.sh"), "utf8");
	assert.match(archiveScript, /pix-runtime-\$\{VERSION\}\.tar\.gz/);
});

test("install.sh and the Homebrew formula install exactly the pix binary, no pix-host", () => {
	const installSh = fs.readFileSync(path.join(repoRoot, "install.sh"), "utf8");
	const formula = fs.readFileSync(path.join(repoRoot, "packaging/homebrew/pix.rb"), "utf8");
	assert.match(installSh, /BINARIES="pix"/);
	assert.doesNotMatch(installSh, /pix-host/);
	assert.match(formula, /bin\.install "pix"/);
	assert.doesNotMatch(formula, /pix-host/);
});

// ── DHI base tags are DATED, never floating ───────────────────────────────
// DHI publishes dated distribution tags (`20250419-debian13`), and its
// static image has no `latest` at all. A floating `:latest` default is not
// just unpinned, it is UNRESOLVABLE: `make install` / `make load` fail at
// build time, which is how the host UAT run died before it started. Pin the
// dated tag; a release still overrides it with an immutable digest via the
// documented --build-arg path.
test("no Dockerfile base image defaults to a floating :latest tag", () => {
	for (const rel of ["images/agent/Dockerfile", "services/memory/Dockerfile"]) {
		const text = fs.readFileSync(path.join(repoRoot, rel), "utf8");
		for (const line of text.split("\n")) {
			const m = /^ARG\s+(\w*IMAGE\w*)=(\S+)/.exec(line.trim());
			if (!m) continue;
			assert.doesNotMatch(
				m[2],
				/:latest(@|$)/,
				`${rel}: ${m[1]} defaults to a floating tag (${m[2]}); DHI publishes dated tags and its static image has no :latest`,
			);
			assert.match(m[2], /:[^:@\s]+/, `${rel}: ${m[1]} (${m[2]}) must carry an explicit tag, never an implicit :latest`);
		}
		assert.doesNotMatch(text, /^FROM\s+\S+:latest/m, `${rel}: a FROM must not pin :latest`);
	}
});

test("services/memory/Dockerfile pins the dated DHI static runtime tag", () => {
	const memoryDockerfile = fs.readFileSync(path.join(repoRoot, "services/memory/Dockerfile"), "utf8");
	assert.match(
		memoryDockerfile,
		/ARG RUNTIME_IMAGE=dhi\.io\/static:20250419-debian13/,
		"the pix-memory runtime base must stay on the dated DHI static tag this repo verified",
	);
	// The digest-override path documented in the header must name the same
	// dated tag, so a release pin and the default build agree.
	assert.match(memoryDockerfile, /--build-arg RUNTIME_IMAGE=dhi\.io\/static:20250419-debian13@sha256:/);
});
