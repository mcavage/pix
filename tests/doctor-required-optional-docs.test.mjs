// DX finding 7: docs/reference.md's #9 (Status and doctor) used to say exit 1
// happens "only when a core requirement (a model provider key, or the config
// file itself)" verified a gap -- omitting sbx, whose Required() is
// unconditionally true (services/host/health/probes.go). A missing sbx alone
// is ALSO enough to fail doctor (pinned by
// workflow/doctor/doctor_test.go's TestDoctor_SbxAloneIsARequiredGap), so the
// doc must name it among the required checks, and must not claim launchd/pack
// are required (they never are).
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
	assert.match(probes, /func \(LaunchdProbe\) Required\(\) bool\s*\{\s*return false/);
	assert.match(probes, /func \(PackProbe\) Required\(\) bool\s*\{\s*return false/);
	assert.match(probes, /func \(ProviderKeyProbe\) Required\(\) bool\s*\{\s*return true/);
});

test("docs/reference.md's doctor exit-code section names sbx among the required checks", () => {
	const section = reference.slice(reference.indexOf("## 9. Status and doctor"));
	assert.match(section, /Required, always:.*sbx CLI/is);
	assert.match(section, /provider key/i);
	assert.doesNotMatch(section, /only when a core requirement\s*\n?\(a model provider key, or the config file itself\)/i);
});

test("docs/reference.md does not claim launchd or pack are required", () => {
	const section = reference.slice(reference.indexOf("## 9. Status and doctor"));
	assert.match(section, /Never required:\s*\n?\*\*launchd\*\* and \*\*pack\*\*/i);
});
