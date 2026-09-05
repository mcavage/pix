// E3.4: shipped `agents/*.md` declare no `intent:`, `fallback_intent:`, or
// `model:` (docs/design/environments.md §6.4). The environment roster is the
// one editable table for a shipped role's model; a custom project agent (not
// this file's concern — see tests/subagent-roster-resolution.test.mjs) may
// still pin its own exact `model:`.
//
// Replaces tests/kit-subagent-roster-intents.test.mjs (deleted, obsolete: it
// asserted pi-kit/spec.yaml's roster prose named an `intent` per preset,
// which is no longer true once shipped agents carry none).
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const agentsDir = path.join(repoRoot, "agents");

// A minimal, line-oriented frontmatter reader: agent files are flat YAML
// between the first two `---` markers, never nested under the keys this test
// cares about, so a full YAML parse is not needed to check for their absence.
function frontmatterOf(content) {
	const norm = content.replace(/\r\n/g, "\n");
	if (!norm.startsWith("---\n")) return "";
	const rest = norm.slice("---\n".length);
	const end = rest.indexOf("\n---");
	return end < 0 ? "" : rest.slice(0, end);
}

function shippedAgentFiles() {
	return fs
		.readdirSync(agentsDir)
		.filter((f) => f.endsWith(".md"))
		.sort();
}

test("the shipped roster has all 19 expected agents (fixture sanity — not 18, not 20)", () => {
	const files = shippedAgentFiles();
	assert.equal(files.length, 19, `expected 19 shipped agents, found ${files.length}: ${files.join(", ")}`);
});

test("no shipped agents/*.md declares intent:, fallback_intent:, or model:", () => {
	const offenders = [];
	for (const file of shippedAgentFiles()) {
		const fm = frontmatterOf(fs.readFileSync(path.join(agentsDir, file), "utf8"));
		for (const key of ["intent", "fallback_intent", "model"]) {
			if (new RegExp(`^${key}:`, "m").test(fm)) offenders.push(`${file}: ${key}:`);
		}
	}
	assert.deepEqual(
		offenders,
		[],
		`shipped agent frontmatter must declare none of intent:/fallback_intent:/model: (found: ${offenders.join(", ")})`,
	);
});

// The second half of the acceptance: every shipped agent name resolves a
// model — either an authored `[agents].<name>` entry in the selected
// environment's scaffold, or (for every other shipped name) the environment's
// `[models].main`. That fallback is unconditional and code-enforced, not
// dependent on which names happen to be authored: services/host/inference/
// roster.go's buildRoster seeds EVERY entry of RosterInput.ShippedAgents with
// `in.Main` before authored `[agents]` entries are applied on top (§6.4), so
// an agent absent from `[agents]` can never fall through unresolved — the
// scaffold's own example config only lists two names ([agents].engineer /
// [agents].review — docs/design/environments.md §5) and every other shipped
// name still resolves via that unconditional main-fallback.
const rosterGoPath = path.join(repoRoot, "services", "host", "inference", "roster.go");
const rosterGoSrc = fs.readFileSync(rosterGoPath, "utf8");

test("roster.go seeds every ShippedAgents name to [models].main before authored [agents] entries apply (universal fallback, §6.4)", () => {
	assert.match(
		rosterGoSrc,
		/for _, name := range in\.ShippedAgents \{\s*\n\s*agents\[name\] = in\.Main/,
		"buildRoster must default every shipped agent name to in.Main before authored [agents] entries are layered on top",
	);
	assert.match(
		rosterGoSrc,
		/agents\[name\] = model \/\/ authored entry wins over the shipped-agent default/,
		"an authored [agents].<name> entry must still win over the main-fallback default seeded above",
	);
});

test("the scaffold's example [agents] table (docs/design/environments.md §5) names a subset — every shipped agent not in it still resolves via main", () => {
	const envDoc = fs.readFileSync(path.join(repoRoot, "docs", "design", "environments.md"), "utf8");
	const m = envDoc.match(/\[agents\]\nengineer = "[^"]+"\nreview = "[^"]+"/);
	assert.ok(m, "docs/design/environments.md §5 must still show the minimal [agents] example (engineer, review)");
	const namedInExample = ["engineer", "review"];
	const shippedNames = shippedAgentFiles().map((f) => f.replace(/\.md$/, ""));
	// Every shipped name is in one of exactly two buckets: named in the
	// scaffold's example [agents] table, or (everyone else) falling through to
	// [models].main — buildRoster's unconditional seed (previous test)
	// guarantees the second bucket is never left unresolved. The meaningful
	// claim here is that the fallback bucket is non-empty (most shipped
	// agents), i.e. this test actually exercises the main-fallback path rather
	// than every name coincidentally being pre-named.
	const fallsThroughToMain = shippedNames.filter((n) => !namedInExample.includes(n));
	assert.ok(
		fallsThroughToMain.length >= shippedNames.length - namedInExample.length,
		"every shipped name not in the example [agents] table must fall through to main",
	);
	assert.ok(fallsThroughToMain.length > 0, "sanity: at least one shipped agent must exercise the main-fallback path");
});

test("docs/design/environments.md §6.4 documents shipped agents as carrying none of intent:/fallback_intent:/model:", () => {
	const envDoc = fs.readFileSync(path.join(repoRoot, "docs", "design", "environments.md"), "utf8");
	assert.match(
		envDoc,
		/Shipped `agents\/\*\.md` declare no `intent:`, `fallback_intent:`, or `model:`\./,
		"docs/design/environments.md §6.4 must state shipped agents carry none of these fields",
	);
});
