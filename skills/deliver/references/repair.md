# Continue a blocked review in a fresh context

Use this only for concrete review findings when the current implementation context
is large, repeatedly rereading, or nearing an explicit runtime/budget limit. A small
fix in a healthy context needs no extra handoff. Do not wait for admission failure:
a fresh child still requires an authorized tool invocation and available budget.
This is not automatic compaction or recovery of a dead parent session.

Keep the product/architecture contract fixed. Save the complete review (with observed
model identity), reviewed candidate inventory/diff, and existing check evidence as
immutable local artifacts. In `.pi-agent/deliver/<slug>/repair-spec.json`, write:

```json
{
  "task": "Fix F-1 through the real caller; preserve the contract and unrelated work. Run the affected checks. Return changed paths, finding dispositions, evidence paths and remaining blockers.",
  "findings": [{"id": "F-1", "text": "Concrete failure scenario and source location from the independent review."}],
  "references": {
    "contract": ".pi-agent/deliver/<slug>/contract.md",
    "review": ".pi-agent/deliver/<slug>/review-1.json",
    "candidate": ".pi-agent/deliver/<slug>/candidate-1.json",
    "evidence": ".pi-agent/deliver/<slug>/evidence-1.json"
  },
  "ignoredShipping": []
}
```

Use actual repository-relative paths. Explicitly list ignored shipping inputs;
tracked and ordinary untracked files are included automatically. Do not put shipping
files in `.pi-agent/`. The helper supports regular files, symlinks and deletions;
submodules or unsupported file types block instead of being silently omitted.

Resolve `scripts/repair-packet.mjs` relative to this deliver skill directory. From
the working repository, run `node <skill-dir>/scripts/repair-packet.mjs create
<spec> <new-packet>`. It writes at most 12 KiB, fingerprints the working candidate,
and hashes referenced artifacts. Large logs and transcripts stay behind paths;
never truncate findings to meet the bound. If findings do not fit, decompose by
independent scope while retaining the complete review and every finding disposition.

Scan exact outgoing packet and original referenced bytes with the existing local
secret boundary. The helper is not a secret scanner. Run `node <skill-dir>/scripts/repair-packet.mjs
dispatch <packet>` to obtain arguments for the existing `subagent` tool. Dispatch
that two-step chain: `engineer` repairs and verifies, then `review` independently
checks the delta in another fresh context. These role names use existing environment
bindings; no model is selected by this helper. If a different implementation role is
explicitly configured, substitute that role before dispatch and record its identity.
Ensure the review binding differs from every known author before starting.

The chain needs no parent model turn between repair and follow-up review. Each
child gets artifact paths rather than the parent's transcript or the other child's
full output. The repair worker records new checks at `<packet>.checks.json`, the
complete repair delta at `<packet>.repair.diff`, and runs the helper's `result`
command to create `<packet>.result.json`. It must locally secret-scan new outgoing
bytes and record a candidate-bound, redacted `secretScan` result before the review
step reads source. The reviewer checks that prerequisite first. Do not dispatch
without local scan capability or sufficient authorized capacity for both steps.
A failed child stops the existing chain; a successful process exit alone does not
establish a successful repair or review. Check the artifact state and final verdict.

An unchanged source returns `blocked-no-source-progress` (exit 2): do not dispatch
another identical repair. A supported refutation instead goes to independent
validation under review.md; unchanged source does not prove a refutation false.
A changed source returns `needs-verification-and-review`, **never accepted**.
Changes outside the finding scope require impact analysis, not automatic credit.
After two distinct failed hypotheses, use the existing specialist/blocker rule;
do not grow an unbounded repair chain. Preserve all attempt costs and unknowns.

On return, record every observed repair author from subagent response metadata.
Validate affected candidate-bound checks and the chain’s read-only follow-up
review of the repair delta, prior findings and current evidence. Keep
the original complete patch available; do not require re-review of unchanged scope.
The reviewer must cover the combined final candidate and all finding dispositions.
An error, stale evidence, unknown/same-vendor identity, or unresolved finding blocks
acceptance. A clean follow-up plus current verification completes the existing
workflow; no extra confirmation pass. Update the existing status.json, not a second
workflow registry. The packet does not confer permissions or enforce model budgets.
