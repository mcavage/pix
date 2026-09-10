// These are packaging/contract guards. They do not establish model adherence;
// that requires an independently graded live comparison.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import {fileURLToPath} from 'node:url';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..','skills','deliver');
const entry=fs.readFileSync(path.join(root,'SKILL.md'),'utf8');
const refs=['verification.md','review.md','complex-work.md','product-agreement.md'].map(name=>fs.readFileSync(path.join(root,'references',name),'utf8'));
const all=[entry,...refs].join('\n').replace(/\s+/g,' ');
function requires(...patterns){for(const p of patterns)assert.match(all,p);}

test('entry has a bounded byte budget and references resolve without extra discovered skills',()=>{
 assert.ok(Buffer.byteLength(entry)<=6000,'keep startup instructions under 6 KB');
 assert.match(entry,/^---\nname: deliver\ndescription: .*Adaptive.*\n---/);
 assert.match(entry,/cook and deliver/);
 const links=[...entry.matchAll(/\]\((references\/[^)]+)\)/g)].map(m=>m[1]);
 assert.deepEqual(links.sort(),['references/complex-work.md','references/product-agreement.md','references/review.md','references/verification.md']);
 for(const link of links){assert.ok(fs.statSync(path.join(root,link)).isFile());assert.notEqual(path.basename(link),'SKILL.md');}
 assert.match(entry,/Do not preload/);
 assert.match(entry,/Bound search output/);
 assert.ok(entry.indexOf('When a candidate exists, load')>entry.indexOf('## Implement'));
 assert.doesNotMatch(entry,/\u2014/);
});
test('default is one implementer and independent review; early challenge is conditional',()=>{
 assert.match(entry,/one implementer \(you\) and one independent reviewer/);
 assert.match(entry,/If a concrete uncertainty prevents a credible acceptance check/);
 assert.match(entry,/Otherwise let the final reviewer challenge both tests and implementation/);
 assert.match(entry,/overrides their unconditional crew, authorship, and review-count rules/);
 assert.doesNotMatch(all,/require an independent pre-implementation test challenge|behavior, security, concurrency, and recovery work goes to an implementation subagent/i);
});
test('compact framing keeps risky caller behavior and non-weakenable criteria',()=>{
 requires(/scope, risk, and uncertainty, not line count/,/bounded/,/risky/,/epic/,
 /credentials\/auth\/trust/,/data loss\/migration/,/concurrency\/cancellation/,
 /public API\/CLI compatibility/,/external side effects/,/recovery\/retry/,
 /User, situation, and real entry point/,/observable success and error/,
 /unchanged behavior/,/executable checks/,/implementer cannot weaken/,
 /Contract changes require explicit rationale, invalidate affected evidence/,
 /Reclassify when evidence changes/);
});
test('behavioral proof cannot be replaced by meaningless red or green exit alone',()=>{
 requires(/red-on-base where practical/,/real caller/,/Syntax\/import errors and unconditional failures are meaningless red/,
 /characterization\/equivalence/,/never revert someone else's work/,
 /Never infer correctness or completion from exit 0 alone/,
 /partial patch correctness separately from workflow completion/);
});
test('deferred verification covers the whole candidate and invalidation',()=>{
 requires(/SHA-256/,/staged and unstaged binary diffs/,/untracked shipping files/,
 /paths, types, modes, symlink targets, and content bytes/,/ignored shipping files/,
 /before and after each command/,/contract digest/,/command, cwd/,
 /environment\/profile, test inputs, start\/end timestamps/,/exit code/,/log path/,
 /Candidate, contract, or input changes invalidate affected evidence/,
 /Reuse evidence for an identical candidate/,/not rerun full suites between unchanged gates/,
 /baseline-red/,/never report that as all green/i);
});
test('review has an independent identity and secret boundary before dispatch',()=>{
 requires(/local secret scan over the exact bytes you intend to send/,
 /working-content scan if it covers only history/,/unresolved detection blocks the prompt/,
 /Same-vendor or unknown identity is a review blocker/,
 /response metadata/,/every implementation author's observed vendor/,
 /Environment bindings remain authoritative/,/no hardcoded model table/,
 /A clean first review finishes the review stage/,
 /do not require a clean second review of an unchanged candidate/i,
 /zero unresolved findings/,/Timeout, error, or missing context is not a clean review/,
 /short independent validation invocation/,/explicitly accepts that disposition/);
});
test('specialists and decomposition answer questions instead of manufacturing roles',()=>{
 requires(/separate PM and specialist invocations are conditional/,
 /not shard a bounded one-unit change just to create handoffs/,
 /explicit question and deliverable/,/security-lead.*actual trust\/security triggers/,
 /product-manager.*uncertain user outcome/,/architect.*architecture uncertainty/,
 /Each child implements and checks its assigned unit/,/isolated (?:git )?worktree/);
});
test('evidence example remains parseable and records observed costs and completion',()=>{
 const match=refs[0].match(/```json\n([\s\S]*?)\n```/);assert.ok(match);
 const state=JSON.parse(match[1]);
 for(const field of ['base','head','digest'])assert.ok(field in state.candidate);
 for(const field of ['candidate','contract','command','cwd','environment','inputs','startedAt','endedAt','exit','log'])assert.ok(field in state.evidence[0]);
 for(const field of ['resultRef','usage','observedModels','exitCode'])assert.ok(field in state.subagents[0]);
 requires(/unknown cost is not zero/i,/explicit total cap/,/All child, retry, and cloud costs count/,
 /no separate rate table/,/Ordinary delivery must not invent a dollar cap unless the user supplied one/,
 /no-commit and no-push/,/accepted patch or explicit blocker/);
});

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

test("delivery does not restore retired role, model-pin, or review-count requirements",()=>{
 for(const pattern of retiredRequirements)assert.doesNotMatch(all,pattern);
});


test('product agreement survives plan/build/deliver boundaries without mandatory crews',()=>{
 const agreement=refs[3];
 assert.match(agreement,/PR\/FAQ/);assert.match(agreement,/PRD and architecture/);
 assert.match(agreement,/authorizing|authorized/);assert.match(agreement,/Existing approval persists/);
 assert.match(agreement,/plan-only request ends/);assert.match(agreement,/Autonomy is explicit/);
 for(const name of ['plan','build']) {
  const skill=fs.readFileSync(path.join(root,'..',name,'SKILL.md'),'utf8');
  assert.ok(Buffer.byteLength(skill)<4000,`${name} must remain a small entry point`);
  assert.match(skill,/product-agreement\.md/);
  assert.doesNotMatch(skill,/AUTO-GATED|crew still runs in full|Full crew is the default/);
 }
 requires(/must read actual desktop\/mobile screenshots/,/Planned checks are not completed checks/,
 /Tests must fail if a required production module is missing/,
 /Delegation is optional/,/truncated or empty final output/);
});
