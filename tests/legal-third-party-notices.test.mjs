import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// AC-REL-01: generated THIRD_PARTY_NOTICES.md, with a fail-closed license-class
// gate over the live Go module set. Covers: MPL-2.0 go-plugin/yamux attribution,
// the Suture "planned" entry, npm globals (incl. the patched pi-tui), and the
// committed file staying in sync with the ledger.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const genScript = path.join(repoRoot, "scripts/legal/generate-third-party-notices.mjs");
const depsPath = path.join(repoRoot, "scripts/legal/dependencies.json");
const policyPath = path.join(repoRoot, "scripts/legal/notices-policy.json");
const noticesPath = path.join(repoRoot, "THIRD_PARTY_NOTICES.md");

const gen = await import(`${genScript.replace(/\\/g, "/")}?t=${Date.now()}`).catch(() =>
	import(genScript)
);

const deps = JSON.parse(fs.readFileSync(depsPath, "utf8"));
const policy = JSON.parse(fs.readFileSync(policyPath, "utf8"));

test("ledger declares MPL-2.0 for go-plugin and yamux, allowlisted by policy", () => {
	const goPlugin = deps.goModules.find((m) => m.module === "github.com/hashicorp/go-plugin");
	const yamux = deps.goModules.find((m) => m.module === "github.com/hashicorp/yamux");
	assert.equal(goPlugin.license, "MPL-2.0");
	assert.equal(goPlugin.class, "weak-copyleft");
	assert.equal(yamux.license, "MPL-2.0");
	assert.equal(yamux.class, "weak-copyleft");
	assert.ok(policy.weakCopyleftAllowlist.includes(goPlugin.module));
	assert.ok(policy.weakCopyleftAllowlist.includes(yamux.module));
});

test("Suture is recorded as a planned entry, not a live dependency", () => {
	const planned = deps.goModulesPlanned.find((m) => m.module === "github.com/thejerf/suture");
	assert.ok(planned, "expected a planned ledger entry for thejerf/suture");
	assert.equal(planned.status, "planned");
	assert.ok(!deps.goModules.some((m) => m.module === "github.com/thejerf/suture"));
});

test("npm globals include the patched pi-tui with attribution", () => {
	const tui = deps.npmGlobal.find((p) => p.name === "@earendil-works/pi-tui");
	assert.ok(tui, "expected @earendil-works/pi-tui in npmGlobal");
	assert.match(tui.role, /PATCH/);
	assert.equal(tui.license, "MIT");
});

test("classify(): permissive licenses pass, unlisted weak-copyleft fails, unknown module fails closed", () => {
	assert.equal(gen.classify({ class: "permissive" }, policy).ok, true);
	assert.equal(
		gen.classify({ module: "example.com/not-allowlisted", class: "weak-copyleft" }, policy).ok,
		false
	);
	assert.equal(gen.classify({ class: "copyleft" }, policy).ok, false);
	assert.equal(gen.classify(undefined, policy).ok, false);
});

test("validateLiveModules(): fails closed on a live dependency absent from the ledger", () => {
	const live = ["github.com/hashicorp/go-plugin@v1.8.0", "example.com/unreviewed@v1.0.0"];
	const { ok, findings } = gen.validateLiveModules(live, deps, policy);
	assert.equal(ok, false);
	assert.ok(findings.some((f) => f.module === "example.com/unreviewed"));
});

test("validateLiveModules(): fails closed on a version drift from the pinned ledger entry", () => {
	const live = ["github.com/hashicorp/go-plugin@v99.0.0"];
	const { ok, findings } = gen.validateLiveModules(live, deps, policy);
	assert.equal(ok, false);
	assert.match(findings[0].reason, /re-verify the license/);
});

test("validateLiveModules(): passes when every live module matches the ledger", () => {
	const live = deps.goModules.map((m) => `${m.module}@${m.version}`);
	const { ok } = gen.validateLiveModules(live, deps, policy);
	assert.equal(ok, true);
});

test("renderNotices() output contains required sections", () => {
	const rendered = gen.renderNotices(deps);
	assert.match(rendered, /THIRD.PARTY|Third-Party Notices/i);
	assert.match(rendered, /github\.com\/hashicorp\/go-plugin/);
	assert.match(rendered, /MPL-2\.0/);
	assert.match(rendered, /thejerf\/suture/);
	assert.match(rendered, /planned/i);
	assert.match(rendered, /@earendil-works\/pi-tui/);
	assert.match(rendered, /not affiliated/i);
});

test("committed THIRD_PARTY_NOTICES.md is in sync with the ledger (no drift)", () => {
	assert.ok(fs.existsSync(noticesPath), "THIRD_PARTY_NOTICES.md must exist and be committed");
	const committed = fs.readFileSync(noticesPath, "utf8");
	const regenerated = gen.renderNotices(deps);
	assert.equal(committed, regenerated, "THIRD_PARTY_NOTICES.md is stale — regenerate it");
});

test("CLI --check-live fails closed on an undeclared module", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "notices-live-"));
	const liveFile = path.join(tmp, "live.txt");
	fs.writeFileSync(liveFile, "example.com/unreviewed@v1.0.0\n");
	try {
		execFileSync("node", [genScript, "--check-live", liveFile], {
			cwd: repoRoot,
			stdio: ["ignore", "pipe", "pipe"],
		});
		assert.fail("expected --check-live to exit non-zero");
	} catch (err) {
		assert.notEqual(err.status, 0);
		assert.match(err.stderr.toString(), /fail-closed/i);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

test("CLI --check-live passes on the real, live services/host module set", () => {
	const listScript = path.join(repoRoot, "scripts/legal/list-go-modules.sh");
	const live = execFileSync("bash", [listScript], { cwd: repoRoot, encoding: "utf8" });
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "notices-live-"));
	const liveFile = path.join(tmp, "live.txt");
	fs.writeFileSync(liveFile, live);
	try {
		const out = execFileSync("node", [genScript, "--check-live", liveFile], {
			cwd: repoRoot,
			encoding: "utf8",
			stdio: ["ignore", "pipe", "pipe"],
		});
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});
