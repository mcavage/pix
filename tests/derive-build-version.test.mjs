// TDD coverage for scripts/release/derive-build-version.sh: the LOCAL build
// version pix's launcher stamps so a release stack (clean X.Y.Z, committed in
// package.json / Makefile VERSION / pi-kit/spec.yaml) and a dev stack (this
// derived NEXT-PATCH prerelease) coexist without either resolving to, or
// being mistaken for, the other's identity (see AGENTS.md "Build, load, run"
// and services/host/launcher/released.go's IsReleased).
//
// Every case here drives the REAL script against a temp git repo — no
// mocking of git or the base version — so a change to the script's actual
// git/hash plumbing is what these tests exercise.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const scriptPath = path.join(repoRoot, "scripts/release/derive-build-version.sh");

// A prerelease per SemVer 2.0.0: dot-separated [0-9A-Za-z-]+ identifiers, no
// leading zero on a purely-numeric one.
const SEMVER_RE = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)$/;
// OCI Distribution Spec tag grammar.
const OCI_TAG_RE = /^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$/;

function isLegalSemverPrerelease(version) {
	const m = SEMVER_RE.exec(version);
	if (!m) return false;
	for (const id of m[4].split(".")) {
		if (/^[0-9]+$/.test(id) && id !== "0" && id.startsWith("0")) return false;
	}
	return true;
}

function makeTempRepo() {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-derive-build-version-"));
	run("git", ["init", "-q", "-b", "main"], dir);
	run("git", ["config", "user.email", "test@example.com"], dir);
	run("git", ["config", "user.name", "Test"], dir);
	return dir;
}

function run(cmd, args, cwd) {
	const res = spawnSync(cmd, args, { cwd, encoding: "utf8" });
	if (res.status !== 0) {
		throw new Error(`${cmd} ${args.join(" ")} failed in ${cwd}:\n${res.stdout}\n${res.stderr}`);
	}
	return res.stdout;
}

function writePkg(dir, version) {
	fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify({ name: "pix", version }, null, 2) + "\n");
}

function commitAll(dir, message) {
	run("git", ["add", "-A"], dir);
	run("git", ["commit", "-q", "-m", message], dir);
}

function derive(dir) {
	return execFileSync("bash", [scriptPath, dir], { encoding: "utf8" }).trim();
}

function deriveExpectFailure(dir) {
	const res = spawnSync("bash", [scriptPath, dir], { encoding: "utf8" });
	assert.notEqual(res.status, 0, `expected derive-build-version.sh to fail in ${dir}, got: ${res.stdout}`);
	assert.equal(res.stdout.trim(), "", "a failing run must print nothing to stdout that a caller could mistake for a version");
	return res.stderr;
}

test("a clean tree derives X.Y.(Z+1)-beta.g<sha7> with no dirty suffix", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "1.2.3");
		commitAll(dir, "init");
		const sha = run("git", ["rev-parse", "--short=7", "HEAD"], dir).trim();

		const version = derive(dir);
		assert.equal(version, `1.2.4-beta.g${sha}`);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("an UNSTAGED change to a tracked file derives a .dirty.<12hex> suffix", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "1.2.3");
		fs.writeFileSync(path.join(dir, "README.md"), "hello\n");
		commitAll(dir, "init");

		fs.writeFileSync(path.join(dir, "README.md"), "hello, world\n");
		const version = derive(dir);
		assert.match(version, /^1\.2\.4-beta\.g[0-9a-f]{7}\.dirty\.[0-9a-f]{12}$/);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("a STAGED change to a tracked file also derives a .dirty.<12hex> suffix", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "1.2.3");
		fs.writeFileSync(path.join(dir, "README.md"), "hello\n");
		commitAll(dir, "init");

		fs.writeFileSync(path.join(dir, "README.md"), "hello, staged\n");
		run("git", ["add", "README.md"], dir);
		const version = derive(dir);
		assert.match(version, /^1\.2\.4-beta\.g[0-9a-f]{7}\.dirty\.[0-9a-f]{12}$/);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("staged and unstaged dirt against the SAME diff bytes derive the SAME dirty hash", () => {
	// Two DIFFERENT repos (so different HEAD shas, and thus a different
	// -beta.g<sha7>) but the SAME diff bytes must still agree on the
	// .dirty.<12hex> suffix: only the DIFF is hashed, nothing about the repo
	// identity itself.
	const dirA = makeTempRepo();
	const dirB = makeTempRepo();
	try {
		for (const dir of [dirA, dirB]) {
			writePkg(dir, "1.2.3");
			fs.writeFileSync(path.join(dir, "README.md"), "hello\n");
			commitAll(dir, "init");
		}
		fs.writeFileSync(path.join(dirA, "README.md"), "hello, changed\n"); // unstaged
		fs.writeFileSync(path.join(dirB, "README.md"), "hello, changed\n");
		run("git", ["add", "README.md"], dirB); // staged, identical bytes

		const dirtyA = derive(dirA).split(".dirty.")[1];
		const dirtyB = derive(dirB).split(".dirty.")[1];
		assert.ok(dirtyA && dirtyB, "both derived versions must carry a .dirty.<hex> suffix");
		assert.equal(dirtyA, dirtyB);
	} finally {
		fs.rmSync(dirA, { recursive: true, force: true });
		fs.rmSync(dirB, { recursive: true, force: true });
	}
});

test("a different diff derives a DIFFERENT dirty hash (the hash is over the diff bytes, not just a dirty flag)", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "1.2.3");
		fs.writeFileSync(path.join(dir, "README.md"), "hello\n");
		commitAll(dir, "init");

		fs.writeFileSync(path.join(dir, "README.md"), "change one\n");
		const v1 = derive(dir);

		run("git", ["checkout", "--", "README.md"], dir);
		fs.writeFileSync(path.join(dir, "README.md"), "change two, a totally different edit\n");
		const v2 = derive(dir);

		assert.notEqual(v1, v2);
		const dirty1 = v1.split(".dirty.")[1];
		const dirty2 = v2.split(".dirty.")[1];
		assert.notEqual(dirty1, dirty2);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("the dirty hash is exactly the first 12 hex chars of sha256(git diff HEAD)", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "1.2.3");
		fs.writeFileSync(path.join(dir, "README.md"), "hello\n");
		commitAll(dir, "init");
		fs.writeFileSync(path.join(dir, "README.md"), "hello, changed\n");

		// Mirror the script's `$(git diff HEAD)` capture exactly: bash command
		// substitution strips ALL trailing newlines, which changes the hashed bytes.
		const diffBytes = run("git", ["diff", "HEAD"], dir).replace(/\n+$/, "");
		const expected = createHash("sha256").update(diffBytes).digest("hex").slice(0, 12);
		const version = derive(dir);
		assert.ok(version.endsWith(`.dirty.${expected}`), `expected .dirty.${expected}, got ${version}`);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("an untracked file (never staged, no diff) does NOT make the derived version dirty", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "1.2.3");
		commitAll(dir, "init");
		fs.writeFileSync(path.join(dir, "scratch.log"), "not tracked, not diffable\n");

		const version = derive(dir);
		assert.doesNotMatch(version, /dirty/);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("a malformed package.json base version fails loud instead of inventing an identity", () => {
	for (const bad of ["1.2", "1.2.3.4", "1.2.3-rc1", "v1.2.3", "not-a-version", ""]) {
		const dir = makeTempRepo();
		try {
			writePkg(dir, bad);
			commitAll(dir, "init");
			const stderr = deriveExpectFailure(dir);
			assert.match(stderr, /not plain X\.Y\.Z semver/, `base=${JSON.stringify(bad)}`);
		} finally {
			fs.rmSync(dir, { recursive: true, force: true });
		}
	}
});

test("missing git metadata fails loud instead of inventing an identity", () => {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-derive-build-version-nogit-"));
	try {
		writePkg(dir, "1.2.3");
		const stderr = deriveExpectFailure(dir);
		assert.match(stderr, /not inside a git checkout/);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("a git repo with no commits (unborn HEAD) fails loud instead of inventing an identity", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "1.2.3");
		// Deliberately no commit: HEAD does not resolve yet.
		const stderr = deriveExpectFailure(dir);
		assert.match(stderr, /could not resolve HEAD/);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("a missing package.json fails loud instead of inventing an identity", () => {
	// The package.json check runs before any git interaction, so an empty
	// (uncommitted) git repo is enough to exercise it.
	const dir = makeTempRepo();
	try {
		const stderr = deriveExpectFailure(dir);
		assert.match(stderr, /no package\.json/);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("clean and dirty derived versions are legal SemVer prereleases, legal OCI tags, and safe path segments", () => {
	const dir = makeTempRepo();
	try {
		writePkg(dir, "9.9.9");
		fs.writeFileSync(path.join(dir, "README.md"), "hello\n");
		commitAll(dir, "init");

		const clean = derive(dir);
		fs.writeFileSync(path.join(dir, "README.md"), "hello, dirty\n");
		const dirty = derive(dir);

		for (const version of [clean, dirty]) {
			assert.ok(isLegalSemverPrerelease(version), `${version} is not a legal SemVer prerelease`);
			assert.match(version, OCI_TAG_RE, `${version} is not a legal OCI tag`);
			assert.doesNotMatch(version, /[/\\]/, `${version} is not a safe path segment`);
			assert.doesNotMatch(version, /\.\./, `${version} must not contain a ".." dot-segment`);
		}
		assert.notEqual(clean, dirty);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("Makefile's LAUNCHER_VERSION default derives from this script, and CI still overrides it with a clean semver", () => {
	const makefile = fs.readFileSync(path.join(repoRoot, "Makefile"), "utf8");
	assert.match(makefile, /DERIVE_VERSION_SH\s*\?=\s*scripts\/release\/derive-build-version\.sh/);
	assert.match(makefile, /LAUNCHER_VERSION\s*\?=\s*\$\(call launcher-version-or-die,\$\(shell \$\(DERIVE_VERSION_SH\)\)\)/);
	// `make launcher LAUNCHER_VERSION=<clean>` must actually win over the
	// derived default — proven with a real `make -n` dry run, not just a
	// Makefile-text assertion, so a syntax mistake in the override plumbing
	// would fail this test.
	const dryRun = execFileSync("make", ["-n", "launcher", "LAUNCHER_VERSION=1.2.3"], { cwd: repoRoot, encoding: "utf8" });
	assert.match(dryRun, /-X main\.version=1\.2\.3/);
});

test("Make ABORTS when the derivation fails, instead of stamping an empty version", () => {
	// /bin/false stands in for a derivation that refuses (a malformed
	// package.json version, no git metadata, an illegal candidate): it exits
	// nonzero and prints nothing on stdout, exactly like the real script's
	// failure contract.
	const r = spawnSync("make", ["-n", "launcher", "DERIVE_VERSION_SH=/bin/false"], { cwd: repoRoot, encoding: "utf8" });
	assert.notEqual(r.status, 0, `make must fail; stdout was:\n${r.stdout}`);
	assert.match(r.stderr, /could not derive/i, `the failure must say what happened, got:\n${r.stderr}`);
	assert.match(r.stderr, /LAUNCHER_VERSION=/, "the failure must name the explicit override that bypasses the derivation");
	// The critical half: no target may run with an empty stamp.
	assert.doesNotMatch(r.stdout, /-X main\.version=\s*(-o|$)/m, "make continued with an empty version stamp");
	assert.doesNotMatch(r.stdout, /go build/, "no build command may be emitted once the version is unknown");
});

test("an explicit LAUNCHER_VERSION never invokes the derivation script at all", () => {
	// The stub would exit nonzero AND write a marker if it ran; `?=` must not
	// expand its default when the variable already has a command-line value,
	// so a release build (LAUNCHER_VERSION=$(VERSION)) never shells out.
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-derive-override-"));
	try {
		const marker = path.join(dir, "ran");
		const stub = path.join(dir, "derive-stub.sh");
		fs.writeFileSync(stub, `#!/bin/sh\ntouch "${marker}"\nexit 1\n`);
		fs.chmodSync(stub, 0o755);
		const r = spawnSync("make", ["-n", "launcher", "LAUNCHER_VERSION=9.9.9", `DERIVE_VERSION_SH=${stub}`], {
			cwd: repoRoot,
			encoding: "utf8",
		});
		assert.equal(r.status, 0, `make must succeed with an explicit version; stderr was:\n${r.stderr}`);
		assert.match(r.stdout, /-X main\.version=9\.9\.9/);
		assert.equal(fs.existsSync(marker), false, "the derivation script ran even though LAUNCHER_VERSION was given explicitly");
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});
