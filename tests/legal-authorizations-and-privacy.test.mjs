import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { validateCopyleftDisclosure } from "../scripts/legal/generate-third-party-notices.mjs";

// Phase10 legal blockers B1/B2/B4:
//   B1 — MPL-2.0 disclosure: pinned Source Code Form URLs, a license text that
//        actually ships, and no self-contradiction about whether it does.
//   B2 — the durable authorization record behind the ownership/DHI claims.
//   B4 — provenance + SBOM wired to the PUBLISHED digest, no silently-passing
//        job, and a privacy doc that is honest about its limits.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const read = (p) => fs.readFileSync(path.join(repoRoot, p), "utf8");

const deps = JSON.parse(read("scripts/legal/dependencies.json"));
const notices = read("THIRD_PARTY_NOTICES.md");
const publishWorkflow = read(".github/workflows/publish.yml");
const legalWorkflow = read(".github/workflows/legal.yml");

// --- B1 -----------------------------------------------------------------------

test("every MPL-2.0 ledger entry pins a Source Code Form URL at the linked version", () => {
	const mpl = deps.goModules.filter((m) => m.class === "weak-copyleft");
	assert.ok(mpl.length >= 2, "expected go-plugin + yamux");
	for (const m of mpl) {
		assert.match(m.sourceUrl, /^https:\/\//, `${m.module} has no https source URL`);
		assert.ok(
			m.sourceUrl.includes(m.version),
			`${m.module} source URL ${m.sourceUrl} does not pin ${m.version}`
		);
		assert.equal(m.licenseTextFile, "licenses/MPL-2.0.txt");
	}
});

test("validateCopyleftDisclosure fails closed on a missing URL, an unpinned URL, or an absent license text", () => {
	const always = () => true;
	const base = {
		module: "example.com/x",
		version: "v1.0.0",
		class: "weak-copyleft",
		sourceUrl: "https://example.com/x/tree/v1.0.0",
		licenseTextFile: "licenses/MPL-2.0.txt",
	};

	assert.equal(validateCopyleftDisclosure({ goModules: [base] }, always).ok, true);

	const noUrl = validateCopyleftDisclosure({ goModules: [{ ...base, sourceUrl: undefined }] }, always);
	assert.equal(noUrl.ok, false);
	assert.match(noUrl.findings[0].reason, /Source Code Form URL/);

	const unpinned = validateCopyleftDisclosure(
		{ goModules: [{ ...base, sourceUrl: "https://example.com/x" }] },
		always
	);
	assert.equal(unpinned.ok, false);
	assert.match(unpinned.findings[0].reason, /does not pin the ledger version/);

	const missingText = validateCopyleftDisclosure({ goModules: [base] }, () => false);
	assert.equal(missingText.ok, false);
	assert.match(missingText.findings[0].reason, /not present in the tree/);

	// A permissive entry is out of scope for this gate.
	assert.equal(
		validateCopyleftDisclosure(
			{ goModules: [{ module: "example.com/p", version: "v1.0.0", class: "permissive" }] },
			() => false
		).ok,
		true
	);
});

test("the real ledger passes the copyleft-disclosure gate against the real tree", () => {
	const res = validateCopyleftDisclosure(deps, (f) => fs.existsSync(path.join(repoRoot, f)));
	assert.equal(res.ok, true, JSON.stringify(res.findings, null, 2));
});

test("notices disclose the MPL source URLs and the shipped text, with no contradiction", () => {
	assert.match(notices, /https:\/\/github\.com\/hashicorp\/go-plugin\/tree\/v1\.8\.0/);
	assert.match(notices, /https:\/\/github\.com\/hashicorp\/yamux\/tree\/v0\.1\.2/);
	assert.match(notices, /licenses\/MPL-2\.0\.txt/);
	// The old text claimed BOTH "reproduced below" and "not reproduced here".
	assert.doesNotMatch(notices, /reproduced below/);
	assert.doesNotMatch(notices, /Full upstream license texts are not reproduced verbatim here/);
});

// --- B2 -----------------------------------------------------------------------

test("LICENSE names Docker, Inc. and stays MIT", () => {
	const license = read("LICENSE");
	assert.match(license, /^MIT License/);
	assert.match(license, /Copyright \(c\) \d{4} Docker, Inc\./);
	assert.doesNotMatch(license, /Copyright \(c\) \d{4} Mark Cavage/);
});

test("AUTHORIZATIONS.md records the DHI and employer-IP basis, with explicit non-coverage", () => {
	const auth = read("docs/legal/AUTHORIZATIONS.md");
	assert.match(auth, /A-1/);
	assert.match(auth, /A-2/);
	assert.match(auth, /President, Docker, Inc\./);
	assert.match(auth, /Explicitly not covered/);
	// The record must not overreach into a trademark grant.
	assert.match(auth, /No trademark or brand license/i);
	assert.match(auth, /not legal advice/i);
});

test("NOTICE.md points at the authorization record instead of re-deriving DHI rights per build", () => {
	const notice = read("NOTICE.md");
	assert.match(notice, /docs\/legal\/AUTHORIZATIONS\.md/);
	assert.match(notice, /docs\/legal\/PRIVACY\.md/);
	assert.match(notice, /inbound/i);
	assert.match(notice, /MIT license/);
});

test("CONTRIBUTING.md states inbound = outbound MIT with no CLA/assignment", () => {
	const contributing = read("CONTRIBUTING.md");
	assert.match(contributing, /license your contribution under the MIT license/i);
	assert.match(contributing, /inbound license equals outbound license/i);
	assert.match(contributing, /no copyright assignment/i);
});

// --- B4 -----------------------------------------------------------------------

test("publish.yml exports the published manifest digest and records provenance against it", () => {
	assert.match(publishWorkflow, /outputs:\s*\n\s*digest: \$\{\{ steps\.manifest\.outputs\.digest \}\}/);
	assert.match(publishWorkflow, /bash scripts\/release\/verify-provenance\.sh "\$V" "\$DIGEST"/);
	assert.match(publishWorkflow, /needs: \[version, merge\]/);
	// The version bump (and therefore the release) waits on provenance.
	assert.match(publishWorkflow, /needs: \[version, merge, provenance\]/);
});

test("publish.yml generates the SBOM against the published image digest, not a rebuild", () => {
	assert.match(publishWorkflow, /image: \$\{\{ env\.IMAGE \}\}@\$\{\{ needs\.merge\.outputs\.digest \}\}/);
	assert.match(publishWorkflow, /SBOM does not reference the published digest/);
});

test("no legal/publish job passes silently (continue-on-error is gone)", () => {
	assert.doesNotMatch(legalWorkflow, /continue-on-error:\s*true/);
	assert.doesNotMatch(publishWorkflow, /continue-on-error:\s*true/);
	// SBOM generation is blocking and asserted non-empty in both places.
	assert.match(legalWorkflow, /packages \| length > 0/);
	assert.match(publishWorkflow, /packages \| length > 0/);
});

test("docs stay honest about what is NOT gated (SBOM diffing) and record provenance scope", () => {
	const findings = read("docs/legal/FINDINGS.md");
	assert.match(findings, /SBOM \*diffing\*|SBOM \*diff/);
	assert.match(findings, /RESOLVED/);
	assert.match(findings, /Provenance record durability/);
	const safeguards = read("docs/legal/RELEASE-SAFEGUARDS.md");
	assert.match(safeguards, /blocking/i);
	assert.doesNotMatch(safeguards, /Wired as a \*\*non-blocking\*\* job/);
});

test("PRIVACY.md states the data flows and does not overclaim compliance", () => {
	const privacy = read("docs/legal/PRIVACY.md");
	assert.match(privacy, /no telemetry|no analytics|no pix backend/i);
	assert.match(privacy, /GDPR/);
	assert.match(privacy, /CCPA/);
	assert.match(privacy, /loopback/);
	assert.match(privacy, /op:\/\//);
	assert.match(privacy, /not legal advice/i);
	// No blanket "pix is GDPR compliant" style claim.
	assert.doesNotMatch(privacy, /is (fully )?(GDPR|CCPA)[- ]compliant/i);
});
