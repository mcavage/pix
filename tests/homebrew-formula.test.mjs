// The Homebrew formula and the tarball it installs from must agree.
//
// They did not, once, and it shipped: `pix.1` was retired along with `pix
// man`/`--man`, the publish workflow stopped staging it into the darwin
// tarball, and the formula kept running `man1.install "pix.1"`. `brew install`
// died with `Errno::ENOENT: No such file or directory - pix.1` on the released
// v0.1.27. Nothing caught it, because the tarball was described here and the
// formula lived in mcavage/homebrew-tap, and the bump automation only rewrites
// version/url/sha256 -- it never reads `def install`.
//
// packaging/homebrew/pix.rb is now the source of truth (the bump job copies it
// out), so both halves are readable from one checkout and this file compares
// them. These assertions are DERIVED, not transcribed: they fail only when the
// formula and the workflow genuinely disagree about what exists, never because
// someone reworded a comment.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const FORMULA = fs.readFileSync(path.join(REPO_ROOT, "packaging", "homebrew", "pix.rb"), "utf8");
const PUBLISH = fs.readFileSync(path.join(REPO_ROOT, ".github", "workflows", "publish.yml"), "utf8");
const INSTALL_SH = fs.readFileSync(path.join(REPO_ROOT, "install.sh"), "utf8");

/**
 * Every path staged into the darwin tarball, read off the `tar` invocation
 * that builds it. That command line IS the manifest: whatever is not named
 * there is not in the archive Homebrew unpacks.
 */
function tarballMembers() {
	const m = PUBLISH.match(/tar -C "\$stage" -czf "[^"]+"\s+(.+)$/m);
	assert.ok(m, "could not find the darwin tarball's `tar` command in publish.yml; if the packaging step moved, this parser must move with it");
	return m[1].trim().split(/\s+/).filter(Boolean);
}

/**
 * Every path the formula installs, across all install targets (bin, doc, man1,
 * pkgshare, ...), read out of `def install`. Deliberately target-agnostic: a
 * future `man1.install` is caught the same way the last one should have been.
 */
function formulaInstalledPaths() {
	const block = FORMULA.match(/def install\n([\s\S]*?)\n  end/);
	assert.ok(block, "packaging/homebrew/pix.rb has no `def install` block");
	const paths = [];
	for (const line of block[1].split("\n")) {
		const stripped = line.replace(/#.*$/, "");
		const call = stripped.match(/^\s*[\w.]+\.install\s+(.+)$/);
		if (!call) continue;
		for (const q of call[1].matchAll(/"([^"]+)"/g)) paths.push(q[1]);
	}
	assert.ok(paths.length > 0, "parsed no installed paths out of `def install`");
	return paths;
}

test("the formula only installs paths the tarball actually contains", () => {
	const members = new Set(tarballMembers());
	const missing = formulaInstalledPaths().filter((p) => !members.has(p) && !members.has(p.split("/")[0]));
	assert.deepEqual(
		missing,
		[],
		`packaging/homebrew/pix.rb installs ${JSON.stringify(missing)}, which the publish workflow does not stage into the tarball. ` +
			`brew install would fail with ENOENT on the next release. Tarball contains: ${[...members].join(", ")}`,
	);
});

test("the formula installs the pix binary, so a bad edit cannot ship a formula that installs nothing runnable", () => {
	const installed = formulaInstalledPaths();
	assert.ok(installed.includes("pix"), "the formula must install pix");
	// There is no pix-host any more (services/host/cmd/pix is the one build
	// target): a formula that reintroduces it would be installing a binary the
	// tarball no longer stages.
	assert.ok(!installed.includes("pix-host"), "the formula must not install pix-host (it no longer ships)");
});

test("Homebrew installs every notice install.sh installs, so brew is not the one channel that drops them", () => {
	// install.sh refuses to install unless the tarball carries all of these,
	// and places them next to the binaries. Homebrew silently dropped them for
	// several releases: the tarball carried them, `def install` ignored them,
	// so the brew-installed copy distributed the binaries with none of the
	// notices that MIT s2 / MPL-2.0 s3.1 require to travel with them.
	const m = INSTALL_SH.match(/^NOTICES="([^"]+)"/m);
	assert.ok(m, "could not read the NOTICES list from install.sh");
	const installed = formulaInstalledPaths();
	const dropped = m[1]
		.trim()
		.split(/\s+/)
		.filter((n) => !installed.includes(n) && !installed.includes(n.split("/")[0]));
	assert.deepEqual(
		dropped,
		[],
		`install.sh ships ${JSON.stringify(dropped)} alongside the binaries but the Homebrew formula does not install them`,
	);
});

test("the bump job copies this file over the tap's, so the vendored formula is genuinely the source of truth", () => {
	// Without the copy, this file is decorative: the tap's own `def install`
	// would keep shipping and every assertion above would be checking a
	// document nobody installs from.
	assert.match(
		PUBLISH,
		/cp "\$GITHUB_WORKSPACE\/packaging\/homebrew\/pix\.rb" tap\/Formula\/pix\.rb/,
		"publish.yml must copy packaging/homebrew/pix.rb over the tap's Formula/pix.rb before rewriting version/url/sha256",
	);
});
