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
});

test("Homebrew archives are additive and contain both binaries plus the manpage", () => {
	assert.match(workflow, /pix_\$\{V\}_darwin_\$\{arch\}\.tar\.gz/);
	assert.match(workflow, /tar -C "\$stage" -czf .* pix pix-host pix\.1/);
	assert.match(workflow, /sha256sum pix-\* pix_\*\.tar\.gz/);
	assert.match(workflow, /dist\/pix_\*\.tar\.gz/);
	assert.match(workflow, /-o "\$GITHUB_WORKSPACE\/dist\/pix-\$os-\$arch"/);
	assert.doesNotMatch(workflow, /host binary has\s+no such symbol/);
});
