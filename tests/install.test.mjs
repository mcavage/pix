import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const installPath = path.join(repoRoot, "install.sh");
const src = fs.readFileSync(installPath, "utf8");
const publishWorkflow = fs.readFileSync(
	path.join(repoRoot, ".github/workflows/publish.yml"),
	"utf8"
);

const NOTICES = ["THIRD_PARTY_NOTICES.md", "NOTICE.md", "LICENSE", "licenses/MPL-2.0.txt"];
const VERSION = "9.9.9";
const ARCH = process.arch === "x64" ? "amd64" : "arm64";
const TARBALL = `pix_${VERSION}_darwin_${ARCH}.tar.gz`;

test("installer never executes an existing unverified Pix binary", () => {
	assert.doesNotMatch(src, /\$\{PREFIX\}\/pix[^\n]*version/);
	assert.match(src, /Compare verified bytes, never execute an existing untrusted installation/);
	assert.match(src, /sha256_of "\$\{PREFIX\}\/\$\{b\}"/);
});

test("installer cleanup trap does not interpolate attacker-controlled TMPDIR as shell code", () => {
	assert.match(src, /trap 'rm -rf "\$tmp"' EXIT INT TERM/);
	assert.doesNotMatch(src, /trap "rm -rf/);
});

test("installer checks pix for PATH collisions and shadowing", () => {
	assert.match(src, /for binary in \$BINARIES/);
	assert.match(src, /assert_installed_resolution/);
	assert.match(src, /PIX_FORCE_INSTALL/);
	assert.match(src, /guard_homebrew_prefix/);
	assert.match(src, /brew install mcavage\/tap\/pix/);
	assert.match(src, /Nothing was written\./);
});

// AC-REL-02: the loose pix-<os>-<arch> asset shipped the pix binary — MIT
// s2 — with none of the required notices attached, and install.sh consumed
// exactly that. Both halves are gated: the release must not publish it, and
// the installer must not want it.

test("installer fetches the notice-bearing tarball, not the loose binaries", () => {
	assert.match(src, /tarball="pix_\$\{ver\}_\$\{os\}_\$\{arch\}\.tar\.gz"/);
	assert.match(src, /verify "\$\{tmp\}\/\$\{tarball\}" "\$tarball" "\$\{tmp\}\/SHA256SUMS"/);
	// The old loose-asset URL shape must not come back.
	assert.doesNotMatch(src, /asset="\$\{b\}-\$\{os\}-\$\{arch\}"/);
	assert.match(src, /NOTICES="THIRD_PARTY_NOTICES\.md NOTICE\.md LICENSE licenses\/MPL-2\.0\.txt"/);
	assert.match(src, /install_notices/);
});

test("publish.yml publishes only notice-bearing tarballs + SHA256SUMS, and proves the notices are inside them", () => {
	const releaseJob = publishWorkflow.slice(
		publishWorkflow.indexOf("\n  release-binaries:"),
		publishWorkflow.indexOf("\n  bump-tap:")
	);
	const files = releaseJob.slice(releaseJob.indexOf("files: |"));
	assert.match(files, /dist\/pix_\*\.tar\.gz/);
	assert.match(files, /dist\/SHA256SUMS/);
	assert.doesNotMatch(files, /dist\/pix-\*/);
	// The asset gate inspects the produced bytes, not the workflow text.
	assert.match(releaseJob, /Assert every published tarball carries the notices/);
	assert.match(releaseJob, /tar -tzf "\$t"/);
	assert.match(releaseJob, /sha256sum pix_\*\.tar\.gz > SHA256SUMS/);
});

// --- end-to-end against a synthetic local release -----------------------------
// The installer is exercised for real (download -> checksum -> unpack -> notice
// check -> install), with exactly two surgical substitutions to the copy under
// test: the release base URL becomes a file:// path, and the darwin-only
// platform gate is taught that this Linux CI box is the darwin target. Every
// other line — verify(), the notice assertions, the staging discipline — is the
// shipped code.

function harness({ withNotices = true, corrupt = false } = {}) {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-install-e2e-"));
	const assets = path.join(dir, "releases/download", `v${VERSION}`);
	fs.mkdirSync(assets, { recursive: true });

	// Build the release tarball the same way publish.yml does.
	const stage = path.join(dir, "stage");
	fs.mkdirSync(path.join(stage, "licenses"), { recursive: true });
	fs.writeFileSync(path.join(stage, "pix"), "#!/bin/sh\necho pix stub\n", { mode: 0o755 });
	const members = ["pix"];
	if (withNotices) {
		for (const n of NOTICES) fs.writeFileSync(path.join(stage, n), `${n} contents\n`);
		members.push("THIRD_PARTY_NOTICES.md", "NOTICE.md", "LICENSE", "licenses");
	}
	execFileSync("tar", ["-C", stage, "-czf", path.join(assets, TARBALL), ...members]);

	const sums = execFileSync("sha256sum", [TARBALL], { cwd: assets, encoding: "utf8" });
	fs.writeFileSync(path.join(assets, "SHA256SUMS"), sums);
	if (corrupt) fs.appendFileSync(path.join(assets, TARBALL), "tampered");

	// The installer under test: same bytes, two substitutions.
	let patched = src.replace('GH="https://github.com/${REPO}"', `GH="file://${dir}"`);
	assert.notEqual(patched, src, "install.sh's GH base URL line moved — update this harness");
	const platformGate = patched.match(/\t\tLinux\)  die [^\n]*\n/);
	assert.ok(platformGate, "install.sh's Linux platform gate moved — update this harness");
	patched = patched.replace(platformGate[0], '\t\tLinux)  os="darwin" ;;\n');
	const script = path.join(dir, "install-under-test.sh");
	fs.writeFileSync(script, patched);

	// `op` and `sbx` are hard prerequisites; stub them so the prereq check is
	// not what this test measures.
	const stubBin = path.join(dir, "stubbin");
	fs.mkdirSync(stubBin);
	for (const cmd of ["op", "sbx"]) {
		fs.writeFileSync(path.join(stubBin, cmd), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
	}

	const prefix = path.join(dir, "bin");
	const dataHome = path.join(dir, "share");
	const res = spawnSync("sh", [script], {
		encoding: "utf8",
		env: {
			PATH: `${stubBin}:/usr/bin:/bin`,
			HOME: dir,
			PIX_VERSION: VERSION,
			PIX_PREFIX: prefix,
			PIX_FORCE_INSTALL: "1",
			XDG_DATA_HOME: dataHome,
			XDG_CONFIG_HOME: path.join(dir, "config"),
		},
	});
	return { dir, res, prefix, docDir: path.join(dataHome, "pix") };
}

test("e2e: a good tarball installs the pix binary AND the notices beside it", () => {
	const { dir, res, prefix, docDir } = harness();
	try {
		assert.equal(res.status, 0, res.stdout + res.stderr);
		for (const b of ["pix"]) {
			const p = path.join(prefix, b);
			assert.ok(fs.existsSync(p), `${b} was not installed`);
			assert.ok(fs.statSync(p).mode & 0o111, `${b} is not executable`);
		}
		// MIT s2 / MPL-2.0 s3.1 are about the copy the USER ends up with.
		for (const n of NOTICES) {
			assert.ok(fs.existsSync(path.join(docDir, n)), `${n} was not installed to ${docDir}`);
		}
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("e2e: a checksum mismatch installs NOTHING", () => {
	const { dir, res, prefix, docDir } = harness({ corrupt: true });
	try {
		assert.notEqual(res.status, 0);
		assert.match(res.stdout + res.stderr, /checksum MISMATCH/);
		assert.equal(fs.existsSync(path.join(prefix, "pix")), false);
		assert.equal(fs.existsSync(docDir), false);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test("e2e: a tarball with a valid checksum but NO notices is refused, and installs nothing", () => {
	const { dir, res, prefix } = harness({ withNotices: false });
	try {
		assert.notEqual(res.status, 0);
		assert.match(res.stdout + res.stderr, /refusing to install a distribution with no notices/);
		assert.equal(fs.existsSync(path.join(prefix, "pix")), false);
	} finally {
		fs.rmSync(dir, { recursive: true, force: true });
	}
});
