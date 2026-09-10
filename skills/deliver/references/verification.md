# Verification details

## Candidate and checks

Record base and head SHAs plus a candidate digest. Compute SHA-256 over
length-delimited base/head records, staged and unstaged binary diffs, and a sorted
manifest of untracked shipping files including paths, types, modes, symlink targets,
and content bytes. Include ignored shipping files explicitly; never omit a new
shipping test because Git has not tracked it. Exclude logs/status under `.pi-agent/`,
not shipping inputs. Keep the shipping inventory with the evidence.

Every check binds candidate digest + contract digest to criterion IDs, exact
command, cwd, relevant environment/profile, test inputs, start/end timestamps,
exit code, actual result, and log path. Record toolchain/lockfile/fixture identities
and external-state versions or probes; redact credentials, not the useful result.
Fingerprint before and after each command; a mutation during a check invalidates
that check until rerun on a stable candidate. Do not validate a moving worktree.

Candidate, contract, or input changes invalidate affected evidence. Record impact
analysis and rerun affected checks; when impact is uncertain, widen verification.
Reuse evidence for an identical candidate, contract, inputs, and environment;
do not rerun full suites between unchanged gates. Run required repository gates
at the appropriate integrated scope, not a full repository build after every edit.

After resuming, confirm the candidate, contract, inputs, and environment still
match the recorded evidence before reusing it. A new turn alone does not invalidate
a completed check. Report the original run time and result; do not describe a
digest check as a test rerun. Rerun when those identities cannot be established,
a relevant input changed, or a repository rule or user explicitly requires it.
This evidence-reuse rule governs delivery across turns as well as between gates.
Keep baseline-red failures explicit: no new failures, affected/new checks pass,
and the known failure set is unchanged or reduced. Never report that as all green.


## State and cost

README and final completion claims must be derived from recorded check results
and reviewer disposition for the current candidate. Never prewrite a passing
verification claim while scaffolding a product. A missing, interrupted or truncated
result is pending/blocked; retain partial work without declaring release complete.

At framing, write `.pi-agent/deliver/<slug>/status.json`; update after each stage.
Keep contracts and logs nearby, referenced rather than copied into every record.
This is a minimal schema; arrays hold real records, not invented zero-cost results:

```json
{
  "classification": {"kind": "bounded|risky|epic", "reason": "...", "triggers": []},
  "contract": {"path": "contract.md", "digest": "sha256:...", "unresolved": []},
  "stages": {"frame": "done", "contract": "done", "implement": "pending", "verify": "pending", "review": "pending"},
  "candidate": {"base": "sha", "head": "sha", "digest": "sha256:...", "shippingManifest": "shipping.json"},
  "evidence": [{
    "candidate": "sha256:...", "contract": "sha256:...", "criteria": ["AC-1"],
    "command": "...", "cwd": "...", "environment": "redacted profile/toolchain identity",
    "inputs": {"manifest": "inputs.json", "digest": "sha256:..."},
    "startedAt": "ISO-8601", "endedAt": "ISO-8601", "exit": 0,
    "result": "pass|fail|baseline-red", "log": "checks/1.log"
  }],
  "subagents": [{
    "id": "...", "agent": "...", "question": "...", "deliverable": "...",
    "resultRef": "tool-call-id/details.results[0]", "startedAt": 0, "endedAt": 0,
    "durationMs": 0, "usage": {}, "observedModels": [], "exitCode": 0
  }],
  "findings": [{"id": "F-1", "source": "...", "text": "...", "status": "open|fixed|refuted|user-accepted", "evidence": "...", "dispositionValidatedBy": null}],
  "decision": {"status": "pending|accepted|blocked", "reason": "...", "next": null}
}
```

Copy subagent timing (`startedAt`, `endedAt`, `durationMs`), usage, and model
metadata from `details.results[]`; retain result references for single, parallel,
and chain calls, including failures/retries. Do not infer duration from log order.
Use the existing usage/cost accounting; there is no separate rate table.

Paid evaluations require one explicit total cap before any run. Optional
`evaluationBudget` records currency, totalCap, spent, reserved, and user approval.
All child, retry, and cloud costs count, including orchestration and external
services. Reserve the bounded cost of in-flight work before launching more; stop
if the remaining cap cannot cover the run. Unknown cost is not zero: resolve it
or block the evaluation. Do not double-count nested usage already aggregated.
Ordinary delivery must not invent a dollar cap unless the user supplied one.
