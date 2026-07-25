import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const workflow = fs.readFileSync(new URL("../.github/workflows/publish.yml", import.meta.url), "utf8");
const pkg = JSON.parse(fs.readFileSync(new URL("../package.json", import.meta.url), "utf8"));

test("the first Pix publish starts from the committed 0.1.0 version", () => {
	assert.equal(pkg.version, "0.1.0");
	assert.match(workflow, /require\('\.\/package\.json'\)\.version/);
	assert.doesNotMatch(workflow, /version=0\.0\.\$\{\{\s*github\.run_number/);
});

test("later publishes select an unused patch tag without overwriting a release", () => {
	assert.match(workflow, /fetch-depth:\s*0/);
	assert.match(workflow, /refs\/tags\/v\$\{version\}/);
	assert.match(workflow, /patch=\$\(\(patch \+ 1\)\)/);
});
