// Explicit Inference Setup, item 1: the shipped model catalog must be a
// materialized, versioned release artifact under runtime/<version>/models.json
// (PRD R4/R13), not only something baked into the pix binary. This test
// actually RUNS scripts/release/build-runtime-archive.sh (a cheap copy+tar
// step, no Docker/network involved) and inspects the produced archive, so a
// future edit that stops staging models.json fails here instead of only
// being caught in the launcher's own inspection-surface tests.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const scriptPath = path.join(repoRoot, "scripts/release/build-runtime-archive.sh");
const catalogPath = path.join(repoRoot, "services/host/inference/catalog/models.json");

function buildArchive(version) {
	const outDir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-runtime-archive-"));
	const outTar = path.join(outDir, `pix-runtime-${version}.tar.gz`);
	execFileSync("bash", [scriptPath, version, outTar], { cwd: repoRoot });
	const stageDir = fs.mkdtempSync(path.join(os.tmpdir(), "pix-runtime-stage-"));
	execFileSync("tar", ["-C", stageDir, "-xzf", outTar]);
	return { outDir, stageDir, runtimeDir: path.join(stageDir, "runtime", version) };
}

test("build-runtime-archive.sh stages the shipped catalog at runtime/<version>/models.json", () => {
	const version = "9.9.9-test-catalog";
	const { runtimeDir } = buildArchive(version);
	const staged = path.join(runtimeDir, "models.json");
	assert.ok(fs.existsSync(staged), `expected ${staged} to exist in the runtime archive`);

	const wantContent = fs.readFileSync(catalogPath, "utf8");
	const gotContent = fs.readFileSync(staged, "utf8");
	assert.equal(gotContent, wantContent, "staged models.json must be byte-identical to the embedded catalog source");

	// It must actually parse as the catalog shape, not an empty placeholder.
	const parsed = JSON.parse(gotContent);
	assert.ok(Array.isArray(parsed.models) && parsed.models.length > 0, "staged models.json has no models");
});

test("build-runtime-archive.sh's manifest.json declares models.json among its contents", () => {
	const version = "9.9.9-test-manifest";
	const { runtimeDir } = buildArchive(version);
	const manifest = JSON.parse(fs.readFileSync(path.join(runtimeDir, "manifest.json"), "utf8"));
	assert.ok(
		Array.isArray(manifest.contents) && manifest.contents.includes("models.json"),
		`manifest.json contents = ${JSON.stringify(manifest.contents)}, want it to include "models.json"`,
	);
});
