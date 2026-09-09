# Independent review details

## Review once, then follow findings

One focused independent cross-vendor review is the default. Dispatch a read-only
`review` invocation after verification. Supply the contract, full base-to-candidate
change (committed, staged, unstaged, and untracked shipping content), relevant
caller context, criterion/evidence mapping, risky triggers, findings, and limitations.
Ask for concrete failure scenarios with file/line evidence and a verdict.
The read-only review preset has no shell tool. Save the complete shipping diff as
a local artifact and pass its path, alongside the candidate files and any new
shipping content. Do not ask the reviewer to run `git diff` itself. A follow-up
review also needs the prior findings and the exact changes made to address them.

Before any diff, file content, or log excerpt leaves for a model reviewer, run a
local secret scan over the exact bytes you intend to send: use the repository's own
scan or gate where one exists, plus a working-content scan if it covers only history.
Here `scripts/check-secret-history.sh` scans committed refs, not dirty or untracked
bytes, so it is insufficient alone. Apply an equivalent local high-entropy and
credential-pattern check to outgoing content, including untracked shipping files. An unresolved detection blocks the prompt; clear
it by removing or rotating the credential, or by recording a reviewed allowlist
decision for a known fixture or example. Redact matched values while preserving
paths, line numbers, and surrounding context so the reviewer can still judge the
code. A model reviewer is never the first secret boundary: it sits behind the
local scan, and its own detection is a late backstop, not the gate.

For a main-agent implementation, the subagent result includes
`details.parentModels` (also printed as parent session models observed). These are
observed assistant provider/model pairs from the current session branch, not the
requested model or a claim about which files each model authored. Associate the
observations with the implementation history; include every possible author when
uncertain. Missing observations remain unknown: never substitute an environment
variable, requested configuration, or self-report as proof.

Prove identity from assistant response metadata in `details.results[]`:
`messages[]` entries with role=assistant carry `provider` and `model`. Compare the
reviewer's observed vendor to every implementation author's observed vendor, not
the orchestrator's vendor alone. Never accept self-report or requested model as
proof. A gateway or local runner is not itself the model vendor; resolve the
observed model's vendor from catalog evidence, or mark it unknown.
Environment bindings remain authoritative under existing model precedence.
There is no hardcoded model table or silent model fallback in this workflow.
Same-vendor or unknown identity is a review blocker: use an authorized configured
cross-vendor binding, or report the missing capability without calling it reviewed.

Clean means explicit LGTM or APPROVE, zero unresolved findings, and complete scope.
Timeout, error, or missing context is not a clean review. A clean first review
finishes the review stage. Do not require a clean second review of an unchanged
candidate, and do not add a confirmation pass to a clean one. Record every finding; fix it, substantiate a refutation, or
obtain explicit user acceptance of that specific risk. Never silently drop or
downgrade findings. A finding that needs a fix causes fix + impacted verification
+ another review because the candidate changed. New shipping edits invalidate its
review even if described as a nit.

When a finding is refuted and the candidate therefore stays unchanged, the review
is not accepted until a short independent validation invocation reads the finding,
the refutation, and its evidence, and explicitly accepts that disposition. Bound
it to the disposition; it is not a second review of the whole candidate, and a
refuted finding never becomes acceptance by the orchestrator's own assertion.
