# Verification details

## Candidate and checks

Use the shipped `scripts/capture-evidence.mjs` beside this skill for local Git
checks instead of writing a new recorder. Run it from the candidate repository
with a small JSON spec and a fresh output directory under `.pi-agent/`:

```json
{"contract":".pi-agent/deliver/example/contract.md","criteria":["AC-1"],"command":["npm","test"],"environment":"declared Node/toolchain and profile identity, no secrets","inputs":["package-lock.json"],"ignoredShipping":[]}
```

```sh
node /path/to/deliver/scripts/capture-evidence.mjs check-spec.json .pi-agent/deliver/example/checks/test-1
```

The recorder uses the repair helper's SHA-256 candidate identity: HEAD, staged
binary diff, and a sorted manifest of tracked/untracked working bytes, deletions,
modes and symlink targets. Staged and unstaged binary diffs are thereby covered
(the unstaged state is represented by final content). It includes untracked shipping files
and explicitly listed ignored shipping files. The manifest records paths, types,
modes, symlink targets, and content bytes via their hashes. `.pi-agent/` is excluded.
Keep generated build outputs ignored unless they ship; list ignored shipping inputs
explicitly. Base/head SHAs and the complete release diff still belong in the review
packet; this command does not infer a release base, scan secrets or dispatch agents.

It fingerprints before and after each command, records the contract digest, input
hashes, command, cwd (root), declared environment/profile, test inputs, start/end timestamps,
exit code and separate stdout/stderr log paths. A changing candidate/input or nonzero
exit cannot pass. `commandPassed` only describes execution and stability: read the
actual result and criterion evidence. It never sets product acceptance. Environment
is an explicit redacted description, not proof of external service state.
Use existing runtime metadata for model identity/cost; never manufacture it here.
For non-Git/external checks record the same evidence through the actual tool caller.

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

Reference the existing tool result rather than retyping its metadata. Subagent timing (`startedAt`, `endedAt`, `durationMs`), usage, and model
metadata comes from `details.results[]`; retain result references for single, parallel,
and chain calls, including failures/retries. Do not infer duration from log order.
Use the existing usage/cost accounting; there is no separate rate table.

Paid evaluations require one explicit total cap before any run. Optional
`evaluationBudget` records currency, totalCap, spent, reserved, and user approval.
All child, retry, and cloud costs count, including orchestration and external
services. Reserve the bounded cost of in-flight work before launching more; stop
if the remaining cap cannot cover the run. Unknown cost is not zero: resolve it
or block the evaluation. Do not double-count nested usage already aggregated.
Ordinary delivery must not invent a dollar cap unless the user supplied one.
