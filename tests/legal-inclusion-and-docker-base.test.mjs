import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
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
	const noticesPath = path.join(repoRoot, "THIRD_PARTY_NOTICES.md");
	const original = fs.readFileSync(noticesPath, "utf8");
	fs.writeFileSync(noticesPath, original + "\nstale drift line\n");
	try {
		const res = spawnSync("bash", ["scripts/check-third-party-notices.sh"], {
			cwd: repoRoot,
			encoding: "utf8",
		});
		assert.notEqual(res.status, 0);
		assert.match(res.stdout + res.stderr, /stale/i);
	} finally {
		fs.writeFileSync(noticesPath, original);
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

test("publish.yml bundles THIRD_PARTY_NOTICES.md and NOTICE.md into the Homebrew darwin tarball", () => {
	assert.match(publishWorkflow, /cp "\$GITHUB_WORKSPACE\/THIRD_PARTY_NOTICES\.md"/);
	assert.match(publishWorkflow, /tar -C "\$stage" -czf .* THIRD_PARTY_NOTICES\.md NOTICE\.md/);
});

test("NOTICE.md carries a non-affiliation disclaimer and does not claim legal confirmation", () => {
	const notice = fs.readFileSync(path.join(repoRoot, "NOTICE.md"), "utf8");
	assert.match(notice, /not affiliated with/i);
	assert.match(notice, /does not constitute legal\s+advice/i);
});
