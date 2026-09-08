// The delivery skill is the production caller: these checks protect its
// required decisions and evidence, not a particular phase/table layout.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const skill = fs.readFileSync(path.join(repoRoot, "skills/deliver/SKILL.md"), "utf8");
const text = skill.replace(/\s+/g, " ");

function requires(...patterns) {
	for (const pattern of patterns) assert.match(text, pattern);
}

const retiredRequirements = [
	/minimum (?:of )?two reviews|at least two explicit|total (?:review )?rounds (?:MUST be )?>=\s*2/i,
	/one round is never enough|one-and-done review|hard-pinned per the table/i,
	/MUST NOT drop the PM|PM first, then applicable specialists|PM still frames the JTBD/i,
	/product-manager.{0,70}always for non-trivial|any non-trivial work \(always\).{0,30}product-manager/i,
	/security-lead MUST run before REVIEW #1|SECURITY \(mandatory before REVIEW #1 for shipping code\)/i,
	/qa-lead MUST run on every deliverable|qa-lead.{0,30}MUST build and review the UAT matrix for every/i,
	/no solo-coding, no exceptions|there are NO size-based exemptions/i,
	/(?:openai|anthropic|google|gemini|ollama)\/(?:gpt|o\d|claude|gemini|qwen|llama|deepseek)[\w.-]*/i,
	/(?:always|still) requires? a (?:clean )?second review|two (?:independent )?reviews are required/i,
	/confirmation (?:review|pass) (?:is )?(?:always|still) required|confirm every clean review/i,
	/the reviewer (?:is|acts as) the secret boundary|rely on the reviewer to (?:catch|detect) secrets/i,
];

function retiredMatches(value) {
	return retiredRequirements.filter((pattern) => pattern.test(value.replace(/\s+/g, " ")));
}

test("deliver stays concise, descriptive, and free of em dashes", () => {
	const lines = skill.trimEnd().split("\n").length;
	assert.ok(lines >= 180 && lines <= 280, `deliver is ${lines} lines; target is 180-280`);
	assert.match(skill, /^---\nname: deliver\ndescription: .*Adaptive.*\n---\n/);
	assert.match(skill, /cook and deliver/);
	assert.doesNotMatch(skill, /\u2014/);
});

test("classification follows scope, risk, and uncertainty, with automatic risky triggers", () => {
	requires(
		/scope, risk, and uncertainty/i,
		/bounded/i, /risky/i, /epic/i,
		/not line count/i,
		/small does not mean safe/i,
		/credentials\/auth\/trust/i,
		/data loss\/migration/i,
		/concurrency\/cancellation/i,
		/public API\/CLI compatibility/i,
		/external side effects/i,
		/recovery\/retry/i,
		/reclassify when evidence changes/i,
	);
});

test("the contract names the real user outcome and cannot be weakened by implementation", () => {
	requires(
		/user.*situation.*real entry point/i,
		/observable success and error/i,
		/unchanged behavior/i,
		/interruption.*retry.*data preservation.*trust/i,
		/executable checks/i,
		/unresolved questions/i,
		/implementer cannot weaken/i,
		/contract changes.*orchestrator.*invalidate/i,
	);
});

test("an independent test challenge precedes the bounded implementation unit", () => {
	requires(
		/independent pre-implementation test challenge/i,
		/challenger must not implement/i,
		/missing.*negative.*boundary/i,
		/one bounded `engineer` or `deep` implementation unit/i,
		/red\/green\/refactor/i,
		/characterization\/equivalence/i,
		/red-on-base.*behavioral regression.*practical/i,
		/meaningless red.*syntax.*import.*unconditional/i,
	);
	assert.ok(text.indexOf("## 3. Challenge") < text.indexOf("## 4. Implement"));
});

test("candidate identity includes dirty and untracked shipping content, not just HEAD", () => {
	requires(
		/base.*head.*digest/i,
		/SHA-256/i,
		/staged and unstaged.*binary diffs/i,
		/untracked shipping files/i,
		/paths, types, modes, symlink targets, and content/i,
		/ignored shipping files/i,
		/before and after.*command/i,
	);
});

test("verification binds the contract, inputs, environment, and full command result", () => {
	requires(
		/contract digest/i,
		/command.*cwd.*environment\/profile.*test inputs.*timestamps.*exit.*log/i,
		/candidate, contract, or input changes invalidate affected evidence/i,
		/reuse evidence for an identical candidate/i,
		/not rerun full suites between unchanged gates/i,
		/fresh evidence in the current turn.*`verify`/i,
		/digest check alone.*not.*test/i,
		/baseline-red/i,
		/no new failures.*affected.*pass/i,
	);
});

test("a local secret scan gates the prompt before any content reaches a model reviewer", () => {
	requires(
		/before any diff, file content, or log excerpt leaves for a model reviewer/i,
		/local secret scan over the exact bytes you intend to send/i,
		/repository's own scan or gate where one exists/i,
		/equivalent local high-entropy and credential-pattern check/i,
		/working-content scan if it covers only history/i,
		/`scripts\/check-secret-history\.sh` scans committed refs, not dirty or untracked bytes, so it is insufficient alone/i,
		/untracked shipping files/i,
		/unresolved detection blocks the prompt/i,
		/removing or rotating the credential.*reviewed allowlist/i,
		/redact matched values while preserving paths, line numbers, and surrounding context/i,
		/never the first secret boundary/i,
		/late backstop, not the gate/i,
		/verification -> local secret scan -> one independent review/i,
	);
	assert.ok(
		text.indexOf("local secret scan over the exact bytes") < text.indexOf("Clean means explicit LGTM"),
		"the scan requirement must precede the review-verdict rules",
	);
});

test("a refuted finding needs an independent disposition validation, a clean review needs nothing more", () => {
	requires(
		/a clean first review finishes the review stage/i,
		/do not require a clean second review of an unchanged candidate/i,
		/do not add a confirmation pass to a clean one/i,
		/finding is refuted and the candidate therefore stays unchanged/i,
		/not accepted until a short independent validation invocation/i,
		/explicitly accepts that disposition/i,
		/not a second review of the whole candidate/i,
		/never becomes acceptance by the orchestrator's own assertion/i,
		/dispositionValidatedBy/,
	);
});

test("one independent cross-vendor review is sufficient until the candidate changes", () => {
	requires(
		/one focused independent cross-vendor review is the default/i,
		/another review because the candidate changed/i,
		/do not require a clean second review of an unchanged candidate/i,
		/LGTM.*APPROVE.*zero unresolved findings/i,
		/timeout.*error.*missing context.*not.*clean/i,
		/response metadata/i,
		/implementation author.*vendor/i,
		/environment bindings remain authoritative/i,
		/no hardcoded model table/i,
		/same-vendor.*unknown.*block/i,
	);
});

test("specialists answer explicit questions instead of attending every change", () => {
	requires(
		/product thinking is mandatory.*separate.*conditional/i,
		/every invocation.*explicit question and deliverable/i,
		/security-lead.*actual trust\/security triggers/i,
		/qa-lead.*broad acceptance risk.*beyond.*test challenge/i,
		/product-manager.*uncertain/i,
		/architect.*uncertainty/i,
		/DX.*designer.*surface is touched/i,
		/not shard a bounded one-unit change just to create handoffs/i,
		/epic.*product\/architecture decomposition/i,
		/each child runs the small loop, not the full epic process/i,
	);
});

test("the orchestrator owns acceptance and permits only tiny mechanical direct edits", () => {
	requires(
		/top-level orchestrator owns framing, contract, integration, evidence, and report/i,
		/tiny mechanical shipping edit when delegation costs more than the change/i,
		/behavior, security, concurrency, and recovery work.*implementation subagent/i,
		/not.*direct coding the default/i,
	);
});

test("the mechanical-edit exception overrides delegation-guide ownership and nothing else", () => {
	requires(
		/overrides `delegation-guide`'s general rule that the\s+orchestrator never writes the unit itself/i,
		/only while\s+`deliver` is active/i,
		/every other `delegation-guide` requirement still binds in\s+full/i,
		/context passing, prescriptive prompts, parallel dispatch, file discipline,\s+per-unit worktrees, and escalation limits/i,
		/owns team size, review count, and the mechanical-edit\s+exception while `deliver` is active/i,
		/`delegation-guide` still supplies context,\s+file, and worktree discipline/i,
	);
});

test("status is small, candidate-bound, and uses the extension's existing result metadata", () => {
	requires(/\.pi-agent\/deliver\/<slug>\/status\.json/, /details\.results\[\]/);
	const example = skill.match(/```json\n([\s\S]*?)\n```/);
	assert.ok(example, "status needs a usable JSON example");
	const state = JSON.parse(example[1]);
	assert.deepEqual(Object.keys(state).sort(), [
		"candidate", "classification", "contract", "decision", "evidence", "findings", "stages", "subagents",
	]);
	for (const key of ["base", "head", "digest"]) assert.ok(key in state.candidate, `candidate.${key}`);
	for (const key of ["candidate", "contract", "command", "cwd", "environment", "inputs", "startedAt", "endedAt", "exit", "log"]) {
		assert.ok(key in state.evidence[0], `evidence.${key}`);
	}
	for (const key of ["startedAt", "endedAt", "durationMs", "usage", "observedModels", "resultRef"]) {
		assert.ok(key in state.subagents[0], `subagents.${key}`);
	}
	requires(/messages\[\].*provider.*model/, /never.*self-report.*requested model/i);
});

test("paid evaluations have one explicit total cap, ordinary delivery has no invented cap", () => {
	requires(
		/paid evaluations require one explicit total cap before any run/i,
		/all child, retry, and cloud costs count/i,
		/ordinary delivery.*not invent a dollar cap unless the user supplied one/i,
		/no separate rate table/i,
		/evaluationBudget/,
		/unknown cost.*not zero/i,
	);
});

test("delivery ends with an accepted patch or an explicit blocker and respects no-commit requests", () => {
	requires(
		/accepted patch or explicit blocker/i,
		/no-commit.*no-push/i,
		/do not claim background continuation/i,
		/contract.*candidate.*checks.*review.*findings.*limitations/i,
	);
});

test("retired unconditional crew, review-count, and model-pin requirements stay deleted", () => {
	assert.deepEqual(retiredMatches(skill), [], "retired unconditional requirements were reintroduced");
});

test("the retirement guard rejects representative old requirements", () => {
	for (const legacy of [
		"Run at least two explicit top-level review rounds",
		"Total rounds MUST be >= 2",
		"MUST NOT drop the PM or any applicable specialist",
		"product-manager always for non-trivial work",
		"SECURITY (mandatory before REVIEW #1 for shipping code)",
		"security-lead MUST run before REVIEW #1 on every deliverable",
		"qa-lead MUST run on every deliverable that ships",
		"No solo-coding, no exceptions",
		"Review model openai/gpt-5.6-sol",
		"Review model anthropic/claude-opus-4-8",
		"Review model google/gemini-3.1-pro-preview",
		"A clean review still requires a second review by another vendor",
		"Two independent reviews are required for every shipping change",
		"A confirmation pass is always required after review",
		"Rely on the reviewer to catch secrets in the diff",
	]) {
		assert.ok(retiredMatches(legacy).length > 0, `retirement guard missed: ${legacy}`);
	}
});
