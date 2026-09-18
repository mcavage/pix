import assert from "node:assert";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const dockerfile = fs.readFileSync(path.join(repoRoot, "images/agent/Dockerfile"), "utf8");
const workflow = fs.readFileSync(path.join(repoRoot, ".github/workflows/patch-smoke.yml"), "utf8");
const dependencies = JSON.parse(fs.readFileSync(path.join(repoRoot, "scripts/legal/dependencies.json"), "utf8"));

test("the Pix runtime pins /btw and its license ledger to the same exact package", () => {
	const entry = dependencies.npmGlobal.find((item) => item.name === "pi-mono-btw");
	assert.ok(entry, "pi-mono-btw must be recorded in the runtime license ledger");
	assert.equal(entry.license, "MIT");
	assert.match(dockerfile, new RegExp(`\\bpi-mono-btw@${entry.version.replaceAll(".", "\\.")}\\b`));
});

test("real-registry CI installs and starts Pi with the pinned /btw package", () => {
	assert.match(workflow, /BTW_PACKAGE="\$\(grep -oP 'pi-mono-btw@\\S\+'/);
	assert.match(workflow, /pi install "npm:\$BTW_PACKAGE"/);
	assert.match(workflow, /pi --mode rpc --no-session/);
	assert.match(workflow, /"command":"get_state","success":true/);
});
