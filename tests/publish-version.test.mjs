import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const workflow = fs.readFileSync(new URL("../.github/workflows/publish.yml", import.meta.url), "utf8");
const pkg = JSON.parse(fs.readFileSync(new URL("../package.json", import.meta.url), "utf8"));

// The point of this test is the SOURCE of the version, not its value: CI must
// start from what package.json commits and must not go back to deriving it from
// github.run_number. Pinning the literal "0.1.0" here asserted the opposite —
// the publish job's own bump job commits the new version back to main, so the
// assertion started failing the moment the first release landed (it did: 0.1.1).
test("the publish version comes from committed package.json, not the run number", () => {
	assert.match(pkg.version, /^\d+\.\d+\.\d+$/, `package.json version ${pkg.version} is not plain semver`);
	assert.match(workflow, /require\('\.\/package\.json'\)\.version/);
	assert.doesNotMatch(workflow, /version=0\.0\.\$\{\{\s*github\.run_number/);
});

test("later publishes select an unused patch tag without overwriting a release", () => {
	assert.match(workflow, /fetch-depth:\s*0/);
	assert.match(workflow, /refs\/tags\/v\$\{version\}/);
	assert.match(workflow, /patch=\$\(\(patch \+ 1\)\)/);
	// The git tag alone is NOT enough: it is created by `bump`, after `merge`
	// already pushed the image, so a partial release leaves the tag free and
	// the registry tag taken. The registry is asked too (see
	// tests/legal-provenance.test.mjs for the classification and fail-closed
	// behavior of scripts/release/tag-availability.sh).
	assert.match(workflow, /tag-availability\.sh/);
});

// The manpage (pix.1) was retired along with `pix man`/`--man` (`pix help
// --all` is the one verb map now), so the Homebrew archive stopped bundling
// it — the tarball carries just the pix binary plus the third-party notices.
// pix-host is gone too: services/host/cmd/pix is the only build target.
test("Homebrew archives are additive and contain the pix binary, no pix-host, no manpage", () => {
	assert.match(workflow, /pix_\$\{V\}_darwin_\$\{arch\}\.tar\.gz/);
	assert.match(workflow, /tar -C "\$stage" -czf .* pix THIRD_PARTY_NOTICES\.md NOTICE\.md/);
	assert.doesNotMatch(workflow, /tar -C "\$stage" -czf .*pix\.1/);
	assert.doesNotMatch(workflow, /tar -C "\$stage" -czf .*pix-host/);
	// Only the notice-bearing tarballs are hashed and published now: the loose
	// pix-<os>-<arch> assets were a notice-less distribution of the same
	// binaries (see tests/install.test.mjs, AC-REL-02). They still get BUILT as
	// intermediates that get staged into the tarball.
	assert.match(workflow, /sha256sum pix_\*\.tar\.gz > SHA256SUMS/);
	assert.doesNotMatch(workflow, /sha256sum pix-\* pix_\*\.tar\.gz/);
	assert.match(workflow, /dist\/pix_\*\.tar\.gz/);
	assert.match(workflow, /-o "\$GITHUB_WORKSPACE\/dist\/pix-\$os-\$arch"/);
	assert.doesNotMatch(workflow, /host binary has\s+no such symbol/);
});

test("the tap bump reads release hashes and opens a gated PR", () => {
	assert.match(workflow, /bump-tap:/);
	assert.match(workflow, /needs: \[version, release-binaries\]/);
	assert.match(workflow, /pix_\$\{V\}_darwin_arm64\.tar\.gz/);
	assert.match(workflow, /pix_\$\{V\}_darwin_amd64\.tar\.gz/);
	assert.match(workflow, /secrets\.TAP_PUSH_TOKEN/);
	assert.match(workflow, /r'\(\^\\s\*version "/);
	assert.match(workflow, /expected one explicit version/);
	assert.match(workflow, /git checkout -b "\$branch"/);
	assert.match(workflow, /gh pr create --repo mcavage\/homebrew-tap/);
	assert.match(workflow, /gh pr merge "\$pr_url" --repo mcavage\/homebrew-tap --auto --squash/);
	assert.doesNotMatch(workflow, /sha256sum.*tap\/Formula\/pix\.rb/);
});
