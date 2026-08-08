import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// AC-REL-02/03: tarball/image inclusion of THIRD_PARTY_NOTICES.md, and the
// Docker base image's explicit digest/build-arg path with a documented public
// fallback that does not assert unresolved DHI rights.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const dockerfile = fs.readFileSync(path.join(repoRoot, "Dockerfile"), "utf8");
const publishWorkflow = fs.readFileSync(
	path.join(repoRoot, ".github/workflows/publish.yml"),
	"utf8"
);

test("scripts/check-third-party-notices.sh passes on the real tree", () => {
	const res = spawnSync("bash", ["scripts/check-third-party-notices.sh"], {
		cwd: repoRoot,
		encoding: "utf8",
	});
	assert.equal(res.status, 0, res.stdout + res.stderr);
});

test("scripts/check-third-party-notices.sh fails closed when the notices file is stale", () => {
	// The check script resolves its own repo ROOT from its own location
	// (BASH_SOURCE) and reads/writes only within it, so a full working copy of
	// the tree in a scratch temp dir is a fully isolated sandbox: the mutation
	// below never touches the real, tracked THIRD_PARTY_NOTICES.md. This
	// avoids a concurrency race against other test files that read the real
	// file (e.g. legal-third-party-notices.test.mjs) under
	// --test-concurrency>1.
	const scratchRoot = fs.mkdtempSync(path.join(os.tmpdir(), "legal-notices-stale-"));
	try {
		// A committed-tree snapshot (git archive of HEAD), not a cp of the live
		// working tree: immune to any uncommitted local edit in repoRoot too.
		const tarPath = path.join(scratchRoot, "tree.tar");
		execFileSync("bash", ["-c", `git -C "${repoRoot}" archive HEAD > "${tarPath}"`]);
		execFileSync("tar", ["-xf", tarPath, "-C", scratchRoot]);

		const scratchNoticesPath = path.join(scratchRoot, "THIRD_PARTY_NOTICES.md");
		const original = fs.readFileSync(scratchNoticesPath, "utf8");
		fs.writeFileSync(scratchNoticesPath, original + "\nstale drift line\n");

		const res = spawnSync("bash", ["scripts/check-third-party-notices.sh"], {
			cwd: scratchRoot,
			encoding: "utf8",
		});
		assert.notEqual(res.status, 0);
		assert.match(res.stdout + res.stderr, /stale/i);
	} finally {
		fs.rmSync(scratchRoot, { recursive: true, force: true });
	}
});

test("Dockerfile pins the base image behind a BASE_IMAGE build ARG, not a bare FROM", () => {
	assert.match(dockerfile, /ARG BASE_IMAGE=dhi\.io\/node:25-debian13-dev/);
	assert.match(dockerfile, /FROM \$\{BASE_IMAGE\}/);
	// No OTHER unparameterized `FROM dhi.io/...` line snuck back in.
	const bareFroms = dockerfile
		.split("\n")
		.filter((l) => /^FROM /.test(l.trim()) && !l.includes("${BASE_IMAGE}"));
	assert.deepEqual(bareFroms, []);
});

test("Dockerfile documents an immutable-digest path and a public fallback without claiming DHI rights", () => {
	assert.match(dockerfile, /--build-arg BASE_IMAGE=dhi\.io\/node:25-debian13-dev@sha256:/);
	assert.match(dockerfile, /scripts\/release\/resolve-base-digest\.sh/);
	assert.match(dockerfile, /Public fallback/);
	assert.match(dockerfile, /docker\.io\/library\/node:25-bookworm/);
	assert.match(dockerfile, /assert any right \(unresolved or otherwise\) to DHI/);
});

test("Dockerfile bakes THIRD_PARTY_NOTICES.md and NOTICE.md into the image", () => {
	assert.match(dockerfile, /COPY --chown=agent:agent THIRD_PARTY_NOTICES\.md/);
	assert.match(dockerfile, /COPY --chown=agent:agent NOTICE\.md/);
});

// B3: the image is a distribution of pix (MIT s2) and of the MPL-2.0 code
// linked into pix-host (MPL-2.0 s3.1) — both texts have to be IN it.
test("Dockerfile bakes LICENSE and licenses/ (MPL-2.0 text) into the image", () => {
	assert.match(dockerfile, /COPY --chown=agent:agent LICENSE\s+\/home\/agent\/\.pi\/agent\/LICENSE/);
	assert.match(dockerfile, /COPY --chown=agent:agent licenses\/\s+\/home\/agent\/\.pi\/agent\/licenses\//);
});

test("publish.yml bundles the notices, LICENSE and licenses/ into the Homebrew darwin tarball", () => {
	assert.match(publishWorkflow, /cp "\$GITHUB_WORKSPACE\/THIRD_PARTY_NOTICES\.md"/);
	assert.match(publishWorkflow, /cp "\$GITHUB_WORKSPACE\/LICENSE"/);
	assert.match(publishWorkflow, /cp "\$GITHUB_WORKSPACE\/licenses\/MPL-2\.0\.txt"/);
	assert.match(
		publishWorkflow,
		/tar -C "\$stage" -czf .* THIRD_PARTY_NOTICES\.md NOTICE\.md LICENSE licenses/
	);
});

test("licenses/MPL-2.0.txt is the full verbatim MPL-2.0 text, not a stub", () => {
	const mpl = fs.readFileSync(path.join(repoRoot, "licenses/MPL-2.0.txt"), "utf8");
	assert.match(mpl, /^Mozilla Public License Version 2\.0/);
	assert.match(mpl, /3\.2\. Distribution of Executable Form/);
	assert.match(mpl, /Exhibit B - "Incompatible With Secondary Licenses" Notice/);
	assert.ok(mpl.length > 15000, `MPL-2.0 text looks truncated (${mpl.length} bytes)`);
});

test("NOTICE.md states ownership, disclaims third-party affiliation, and claims no legal advice", () => {
	const notice = fs.readFileSync(path.join(repoRoot, "NOTICE.md"), "utf8");
	assert.match(notice, /Docker, Inc\. project/);
	assert.match(notice, /not affiliated with/i);
	assert.match(notice, /does not constitute legal\s+advice/i);
	// The pre-B2 wording said pix was NOT affiliated with Docker, Inc., which
	// contradicts the recorded ownership. It must not come back.
	assert.doesNotMatch(notice, /not affiliated with[\s\S]{0,120}Docker, Inc\./i);
});
