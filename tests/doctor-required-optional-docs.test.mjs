// DX finding 7: docs/reference.md's #9 (Status and doctor) used to say exit 1
// happens "only when a core requirement (a model provider key, or the config
// file itself)" verified a gap -- omitting sbx, whose Required() is
// unconditionally true (services/host/health/probes.go). A missing sbx alone
// is ALSO enough to fail doctor (pinned by
// workflow/doctor/doctor_test.go's TestDoctor_SbxAloneIsARequiredGap), so the
// doc must name it among the required checks.
//
// LaunchdProbe and PackProbe (both asserted never-required here originally)
// were deleted along with `pix-host serve`'s Suture supervision and the pack
// system in the Pix v2 deletion sweep (docs/design/pix-v2-architecture.md
// §14, AC-16): doctor's probe set is now sbx, providers and github only
// (workflow/doctor/probes.go), so there is nothing left to assert never-
// required about either.
//
// The Pix v2 doc rewrite moved this content from the old "## 9. Status and
// doctor" section (a status/doctor combined narrative) to "## 7. Doctor" in
// the rewritten docs/reference.md (docs/design/pix-v2-surface.md §3.7); there
// is no top-level `pix status` verb left to combine it with.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const reference = fs.readFileSync(path.join(repoRoot, "docs", "reference.md"), "utf8");
const probes = fs.readFileSync(
	path.join(repoRoot, "services", "host", "health", "probes.go"),
	"utf8",
);

test("probes.go's Required() set matches what this test expects to guard (fitness function pins itself against drift)", () => {
	assert.match(probes, /func \(SbxProbe\) Required\(\) bool\s*\{\s*return true/);
	assert.match(probes, /func \(ProviderKeyProbe\) Required\(\) bool\s*\{\s*return true/);
});

test("docs/reference.md's doctor exit-code section names sbx among the required checks", () => {
	const section = reference.slice(reference.indexOf("## 7. Doctor"), reference.indexOf("## 8. Tasks"));
	assert.match(section, /Required, always:.*sbx CLI/is);
	assert.match(section, /provider key/i);
	assert.doesNotMatch(section, /only when a core requirement\s*\n?\(a model provider key, or the config file itself\)/i);
});

test("docs/reference.md does not claim launchd or pack are required", () => {
	const section = reference.slice(reference.indexOf("## 7. Doctor"), reference.indexOf("## 8. Tasks"));
	assert.doesNotMatch(section, /\*\*launchd\*\*/i);
	assert.doesNotMatch(section, /\*\*pack\*\*/i);
});
