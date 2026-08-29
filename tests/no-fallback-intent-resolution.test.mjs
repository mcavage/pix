// E3.2 BLOCK fix: `fallback_intent:` frontmatter is LEGACY, pending outright
// deletion in E3.4 (docs/design/routing.md; architecture.md's E3.4 row). It
// must never be a `roster.agents` name lookup and must never affect which
// model a subagent runs — not at load time, and not via the old cross-vendor
// retry-on-policy-refusal feature, which has been removed entirely (it was
// the only thing that ever resolved `fallback_intent` to a model).
//
// This is a source/static sentinel, not a behavior test (see
// tests/subagent-roster-resolution.test.mjs for the behavioral proof with
// security-lead's shipped `fallback_intent: review` and an arbitrary made-up
// value). It fails the moment resolution code re-adds a roster lookup keyed
// by `fallbackIntent`, in code OR in a stale comment that would mislead the
// next reader.
//
// Scope note: the shipped `fallback_intent:` frontmatter key itself (in
// agents/*.md, docs prose describing it as legacy) is NOT deleted here —
// that finding is explicitly assigned to E3.4. Parsing the field is allowed
// to remain (for display/audit visibility during the migration window); only
// its runtime EFFECT is banned.
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const subagentsSrc = fs.readFileSync(
	path.join(repoRoot, "extensions", "subagents.ts"),
	"utf8",
);

test("subagents.ts still parses fallback_intent frontmatter (display-only, not deleted yet)", () => {
	assert.match(
		subagentsSrc,
		/frontmatter\.fallback_intent/,
		"the frontmatter parse itself is allowed to remain until E3.4",
	);
});

test("subagents.ts never resolves fallbackIntent through the roster (no roster.agents[fallbackIntent] lookup)", () => {
	assert.doesNotMatch(
		subagentsSrc,
		/ROSTER\??\.agents\[\s*fallbackIntent\s*\]/,
		"fallback_intent must never be used as a roster.agents name lookup",
	);
});

test("subagents.ts has no `fallbackModel` — the field that used to carry fallback_intent's resolved model is gone entirely", () => {
	assert.doesNotMatch(
		subagentsSrc,
		/fallbackModel/,
		"fallbackModel was the only mechanism through which fallback_intent ever affected model choice; it must be fully removed, not merely unused",
	);
});

test("subagents.ts has no cross-vendor retry keyed off a policy refusal + fallback route", () => {
	// The retired feature re-invoked runSingle with a swapped model whenever a
	// provider policy refusal fired. Any resurrection of that pairing — a
	// second runSingle call gated on isProviderPolicyRefusal — is exactly the
	// hidden model-choice effect this fix removes.
	assert.doesNotMatch(
		subagentsSrc,
		/isProviderPolicyRefusal\(result\)[\s\S]{0,200}runSingle\(/,
		"a provider policy refusal must not trigger a retry that changes the model",
	);
});

test("the AgentConfig.fallbackIntent comment documents it as semantically inert, not as a routing knob", () => {
	assert.match(
		subagentsSrc,
		/fallbackIntent\?: string;/,
		"the field itself remains (parsed for display) pending E3.4",
	);
	assert.match(
		subagentsSrc,
		/NEVER a\s*\n?\s*\/\/ roster\.agents lookup/,
		"the comment above fallbackIntent must say it is never a roster lookup",
	);
});

test("finding: agents/security-lead.md still ships fallback_intent: review — that deletion is E3.4's job, not falsely closed here", () => {
	const securityLead = fs.readFileSync(
		path.join(repoRoot, "agents", "security-lead.md"),
		"utf8",
	);
	assert.match(
		securityLead,
		/^fallback_intent:\s*review\s*$/m,
		"shipped legacy frontmatter is left in place; E3.4 removes it from agents/*.md, not this fix",
	);
});
