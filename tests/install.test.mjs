import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";

const src = fs.readFileSync(new URL("../install.sh", import.meta.url), "utf8");

test("installer never executes an existing unverified Pix binary", () => {
	assert.doesNotMatch(src, /\$\{PREFIX\}\/pix[^\n]*version/);
	assert.match(src, /Compare verified bytes, never execute an existing untrusted installation/);
	assert.match(src, /sha256_of "\$\{PREFIX\}\/\$\{b\}"/);
});

test("installer cleanup trap does not interpolate attacker-controlled TMPDIR as shell code", () => {
	assert.match(src, /trap 'rm -rf "\$tmp"' EXIT INT TERM/);
	assert.doesNotMatch(src, /trap "rm -rf/);
});

test("installer checks both public binaries for PATH collisions and shadowing", () => {
	assert.match(src, /for binary in pix pix-host/);
	assert.match(src, /assert_installed_resolution/);
	assert.match(src, /PIX_FORCE_INSTALL/);
});
