import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// AC-REL-01: generated THIRD_PARTY_NOTICES.md, with a fail-closed license-class
// gate over the live Go module set. Covers: MPL-2.0 go-plugin/yamux attribution,
// the live Suture entry, npm globals (incl. the patched pi-tui), and the
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

test("bakedTools ledger records ruff, fd, and go with license/source/version provenance", () => {
	const ruff = deps.bakedTools.find((t) => t.name === "ruff");
	const fd = deps.bakedTools.find((t) => t.name === "fd");
	const go = deps.bakedTools.find((t) => t.name === "go");
	assert.ok(ruff, "expected a bakedTools entry for ruff");
	assert.ok(fd, "expected a bakedTools entry for fd");
	assert.ok(go, "expected a bakedTools entry for go");
	for (const t of [ruff, fd, go]) {
		assert.equal(t.class, "permissive");
		assert.ok(t.license, `${t.name} missing license`);
		assert.ok(t.source, `${t.name} missing source`);
		assert.ok(t.version, `${t.name} missing version`);
		assert.ok(t.dockerfileArg, `${t.name} missing dockerfileArg cross-check`);
		assert.ok(t.licenseEvidence, `${t.name} missing licenseEvidence`);
	}
	assert.equal(ruff.license, "MIT");
	assert.equal(fd.license, "MIT OR Apache-2.0");
	assert.equal(go.license, "BSD-3-Clause");
});

test("validateBakedTools(): passes when the Dockerfile ARG pins match the ledger", () => {
	const dockerfileText = fs.readFileSync(path.join(repoRoot, "images/agent/Dockerfile"), "utf8");
	const { ok, findings } = gen.validateBakedTools(dockerfileText, deps.bakedTools);
	assert.equal(ok, true, JSON.stringify(findings));
});

test("validateBakedTools(): fails closed when the Dockerfile ARG drifts from the ledger version", () => {
	const drifted = "ARG RUFF_VERSION=99.99.99\n";
	const { ok, findings } = gen.validateBakedTools(drifted, [
		{ name: "ruff", version: "0.15.22", dockerfileArg: "RUFF_VERSION" },
	]);
	assert.equal(ok, false);
	assert.match(findings[0].reason, /ledger pins 0\.15\.22, Dockerfile ARG RUFF_VERSION pins 99\.99\.99/);
});

test("validateBakedTools(): fails closed when a ledger tool has no matching Dockerfile ARG", () => {
	const { ok, findings } = gen.validateBakedTools("FROM scratch\n", [
		{ name: "ruff", version: "0.15.22", dockerfileArg: "RUFF_VERSION" },
	]);
	assert.equal(ok, false);
	assert.match(findings[0].reason, /no longer baked|ARG renamed/);
});

test("CLI --check-baked-tools passes against the real Dockerfile", () => {
	const out = execFileSync(
		"node",
		[genScript, "--check-baked-tools", path.join(repoRoot, "images/agent/Dockerfile")],
		{ cwd: repoRoot, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }
	);
	void out;
});

// U07 landed the supervision tree, so Suture stopped being a plan and became a
// linked dependency: it must now be a LIVE ledger entry (license verified at
// the exact version in go.mod), and it must no longer sit in the planned list —
// a shipped dependency disclosed as "planned" is the failure this guards.
test("Suture is a live, license-verified ledger entry", () => {
	const live = deps.goModules.find((m) => m.module === "github.com/thejerf/suture/v4");
	assert.ok(live, "expected a live ledger entry for thejerf/suture/v4");
	assert.equal(live.license, "MIT");
	assert.equal(live.class, "permissive");
	assert.ok(live.version.startsWith("v4."), `unexpected version ${live.version}`);
	assert.ok(!(deps.goModulesPlanned || []).some((m) => m.module.includes("suture")));
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
	assert.match(rendered, /@earendil-works\/pi-tui/);
	assert.match(rendered, /not affiliated/i);
	assert.match(rendered, /astral-sh\/ruff/);
	assert.match(rendered, /sharkdp\/fd/);
	assert.match(rendered, /go\.dev\/dl/);
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

// --- reverse direction: the ledger must not over-declare -----------------------
// Only the forward check existed (a live module with no ledger row fails), so
// staleness could only grow: 24 rows for the charmbracelet/glamour tree
// survived the monitor TUI's deletion and were still published as attribution
// for code pix does not ship, while the docs quoted a module count nobody could
// reproduce from `go list`.

test("validateLedgerLiveness(): fails closed on a ledger row that is no longer in the live build graph", () => {
	const live = deps.goModules
		.filter((m) => m.module !== "github.com/hashicorp/yamux")
		.map((m) => `${m.module}@${m.version}`);
	const { ok, findings } = gen.validateLedgerLiveness(live, deps);
	assert.equal(ok, false);
	assert.equal(findings.length, 1);
	assert.equal(findings[0].module, "github.com/hashicorp/yamux");
	assert.match(findings[0].reason, /NOT in the live build graph/);
});

test("validateLedgerLiveness(): refuses to judge the ledger against an EMPTY live list", () => {
	// A broken `go list` must read as undecidable, not as "every row is stale".
	const { ok, findings } = gen.validateLedgerLiveness([], deps);
	assert.equal(ok, false);
	assert.match(findings[0].reason, /refusing to judge/);
});

test("validateLedgerLiveness(): passes against the REAL live module set", () => {
	const listScript = path.join(repoRoot, "scripts/legal/list-go-modules.sh");
	const live = execFileSync("bash", [listScript], { cwd: repoRoot, encoding: "utf8" }).split("\n");
	const { ok, findings } = gen.validateLedgerLiveness(live, deps);
	assert.equal(ok, true, JSON.stringify(findings, null, 2));
	// And the same set, both directions, is exactly the ledger.
	const liveModules = new Set(live.filter(Boolean).map((l) => gen.splitModule(l.trim()).module));
	assert.equal(liveModules.size, deps.goModules.length);
});

test("CLI --check-live fails closed on a STALE ledger row (reverse direction)", () => {
	const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "notices-stale-"));
	const liveFile = path.join(tmp, "live.txt");
	// A live list that is a strict subset of the ledger.
	fs.writeFileSync(
		liveFile,
		deps.goModules
			.slice(0, 3)
			.map((m) => `${m.module}@${m.version}`)
			.join("\n") + "\n"
	);
	try {
		execFileSync("node", [genScript, "--check-live", liveFile], {
			cwd: repoRoot,
			stdio: ["ignore", "pipe", "pipe"],
		});
		assert.fail("expected --check-live to exit non-zero on a stale ledger");
	} catch (err) {
		assert.notEqual(err.status, 0);
		assert.match(err.stderr.toString(), /ledger-liveness gate FAILED/);
	} finally {
		fs.rmSync(tmp, { recursive: true, force: true });
	}
});

test("RELEASE-SAFEGUARDS.md's stated Go module count matches the ledger (no unreproducible number)", () => {
	const doc = fs.readFileSync(path.join(repoRoot, "docs/legal/RELEASE-SAFEGUARDS.md"), "utf8");
	const m = doc.match(/build graph \(\*\*(\d+)\*\*,/);
	assert.ok(m, "RELEASE-SAFEGUARDS.md no longer states the Go module count");
	assert.equal(Number(m[1]), deps.goModules.length);
});

// --- global npm pins ----------------------------------------------------------
// `npm install -g typescript` with no version resolves to whatever the registry
// serves that build, which makes the ledger's recorded version and license a
// claim about a build nobody can reproduce.

test("the ledger's typescript row is ARG-pinned and matches the Dockerfile and package.json", () => {
	const ts = deps.npmGlobal.find((p) => p.name === "typescript");
	assert.ok(ts, "expected a typescript npmGlobal row");
	assert.equal(ts.dockerfileArg, "TYPESCRIPT_VERSION");
	const dockerfile = fs.readFileSync(path.join(repoRoot, "images/agent/Dockerfile"), "utf8");
	assert.match(dockerfile, new RegExp(`ARG TYPESCRIPT_VERSION=${ts.version.replace(/\./g, "\\.")}`));
	assert.match(dockerfile, /npm install -g --ignore-scripts "typescript@\$\{TYPESCRIPT_VERSION\}"/);
	const pkg = JSON.parse(fs.readFileSync(path.join(repoRoot, "package.json"), "utf8"));
	assert.equal(pkg.devDependencies.typescript, ts.version);
});

test("validateNpmGlobalPins(): fails closed on ARG drift, and on an unpinned install", () => {
	const drifted = gen.validateNpmGlobalPins("ARG TYPESCRIPT_VERSION=4.0.0\n", deps.npmGlobal);
	assert.equal(drifted.ok, false);
	assert.match(drifted.findings[0].reason, /ledger pins .*Dockerfile ARG TYPESCRIPT_VERSION pins 4\.0\.0/);

	const missing = gen.validateNpmGlobalPins("FROM scratch\n", deps.npmGlobal);
	assert.equal(missing.ok, false);
	assert.match(missing.findings[0].reason, /no longer baked|ARG renamed/);

	// Rows pinned some other way (ARG PI_PACKAGE, inline pkg@version) are out of
	// scope for this specific check and must not produce noise.
	assert.equal(gen.validateNpmGlobalPins("FROM scratch\n", [{ name: "pi-plan", version: "0.1.1" }]).ok, true);
});

test("CLI --check-npm-pins passes against the real Dockerfile", () => {
	execFileSync("node", [genScript, "--check-npm-pins", path.join(repoRoot, "images/agent/Dockerfile")], {
		cwd: repoRoot,
		stdio: ["ignore", "pipe", "pipe"],
	});
});
