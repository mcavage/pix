# sbx 0.39 native environments: observed contract (Story 0, unit E0.7)

**Version:** `sbx version: v0.39.0 def8cb0523a77e757bdd6ef52b459fe374f3783e`

This is an observed-contract record, not a tutorial. It states what the final
candidate-owned host run actually proved against a real `sbx` binary, what
remains an implementation responsibility for later stories, and where the
evidence lives. It corrects two assumptions `docs/design/environments.md` and
`services/host/uatenvmatrix` code comments made before this run: created-sandbox
digest equality (unobservable) and the exact leaked-fixture cleanup debt from an
earlier run.

## 1. Prerequisites

- `sbx` 0.39.0 or later. `sbx --version` is rejected as an unknown flag on this
  host; the fallback `sbx version` works and reports the banner above.
- `docker` for local image inspection and `sbx template ls`.
- A candidate image already built, saved, and `sbx template load`ed under a
  run-unique tag before any check runs.
- Every check runs through an injected `Executor` seam in
  `services/host/uatenvmatrix`, and in production only inside the SUBMITTED
  candidate's own `pix-host uat-env-matrix` binary, invoked by
  `workflow/uat.Runner`'s `envMatrix` seam. No check is reachable through a
  generic shell/argv/env UAT action.

## 2. Candidate and run identity

- run: `run-20260824-110322-d24dac52`
- candidate: `33499a056a4390b5095d0b50d51475b3580cd2ec`
- scenario: `uat/scenarios/smoke.yaml`
- result: PASS at `2026-08-24T11:11:23.503499-07:00`
- `steps/env_matrix.log` listed exactly six candidate-owned checks and ended
  `RESULT: PASS`.

The scenario itself is unchanged: `uat/scenarios/smoke.yaml` runs
`candidate_smoke` with `expect: {verdict: pass}`. `candidate_smoke` already
fails the scenario if any named check fails; this run is the evidence that
contract holds, not a reason to add a second, decorative assertion.

## 3. The six exact checks

`services/host/uatenvmatrix.CheckNames()` reports exactly these six names, and
`candidate_smoke` executes only these, only through the candidate binary:

1. `environment_create_then_exec_invocation`
2. `environment_uses_local_candidate_image`
3. `environment_recreate_boundary`
4. `environment_failed_create_cleanup`
5. `environment_rm_scope_refusal`
6. `environment_custom_agent_ollama`

No generic `sbx_env_smoke` action exists and none was added. Each check owns
typed inputs internally; an MCP caller cannot supply host commands, argv,
environment variables, or arbitrary paths.

## 4. Custom agent and relative kits

The authored fixture (`fixtures.go`'s `customAgentFixture`) declares:

```yaml
schemaVersion: "1"
agent: pix
name: pix-uatenv-fixture-0

kits:
  - ./kit

sandboxOptions:
  memory: 16g

env:
  PIX_MEMORY_SCOPE: personal
```

`./kit` resolves relative to the authored file's own directory, not the
process working directory. The materialized kit-spec at that path declares
strict kit-spec v2: `name: pix`, image `docker.io/mcavage/pix:0.1.71`,
entrypoint `[pi]`.

`agent: pix` is refused unless a referenced kit resolves to a materialized
kit-spec whose own declared `name` is exactly `pix`. An earlier fixture that
named its kit directory `kit` with a kit-spec `name` other than `pix` was
refused for exactly this reason: agent and kit identity must match. This is a
real upstream refusal, not an assumption: agent-identity mismatch fails
`sbx env create` outright before anything is created.

## 5. Create and exec

`sbx env create <authored>` created exactly `pix-uatenv-fixture-0`. `sbx ls
--json` exposes only `name`, `id`, `agent`, `status`, and `workspaces` per row,
nothing else, and showed exactly that name running.

The daemon-safe transport proof used non-TTY, name-based exec:

```console
$ sbx exec -i pix-uatenv-fixture-0 -- sh -c 'for a in "$@"; do printf '"'"'%s\n'"'"' "$a"; done' sh \
    pi --skill /opt/pix/kit/skills --skill /home/uat/personal-context/skills \
    --model anthropic/claude-sonnet-5 --session session-fixture-1
```

The sandbox echoed back the exact intended argv unchanged, one element per
line:

```
pi
--skill
/opt/pix/kit/skills
--skill
/home/uat/personal-context/skills
--model
anthropic/claude-sonnet-5
--session
session-fixture-1
```

This is why the check never starts Pi's real TUI: `sbx exec -it` running the
actual `pi` binary launches a non-terminating, interactive TUI under a
pty-less, long-lived host daemon process. A daemon-driven UAT run has no
terminal to attach and no way to signal "done" to that process; the earlier
version of this check treated a bare exit as transport proof and instead
observed `ERROR: inspect exec: context deadline exceeded` because the TUI
never exits. The terminating, non-TTY argv-echo probe above proves the same
fact, that sbx delivers the exact intended Pi invocation into the sandbox
unchanged, without ever starting a process that cannot terminate on its own.

Production's interactive shape is unchanged by this: a real launch still uses
`sbx exec -it NAME -- pi <exact invocation>`. Only this check's own transport
proof substitutes a terminating shell for the non-terminating TUI.

## 6. Interpolation

Three fixtures, one create call each, observed against a real `sbx env
create`:

| form | fixture value | observed sandbox-side result |
| --- | --- | --- |
| defined `${VAR}` | `${PIX_UAT_STORY0_DEFINED}` (host set to `pix-uat-story0-defined-value`) | `pix-uat-story0-defined-value` |
| missing with default `${VAR:-default}` | `${PIX_UAT_STORY0_MISSING:-fallback-value}` (host var stripped) | `fallback-value` |
| bare missing `${VAR}` | `${PIX_UAT_STORY0_MISSING}` (host var stripped) | create succeeded; the reference resolved to a sandbox-side environment variable set to the empty string |

The bare-missing case had two legitimate outcomes going in: a loader/create
refusal, or a create success with some classifiable sandbox-side value
(unset, empty, or the literal unexpanded `${VAR}` text). This run observed
create success with an empty string, not a refusal and not literal passthrough.
Section 5 of `docs/design/environments.md` should be read with this exact
result, not as an open question.

Every positively receipted interpolation fixture (all three) was
fresh-probe-gated before removal and removed by the same shared
`cleanupCreatedFixture` path every other check in this package uses: no
receipt, no removal attempt; a receipt with a failed fresh reconfirmation,
no removal attempt.

## 7. Candidate image evidence (AC-2 correction)

- local candidate image ID (evidence only, via `docker image inspect
  --format {{.Id}}`): `sha256:5e86bfdea88f617ab271e011554a7c27022a0b91f9a9a3b89819f9850a71b270`
- `sbx template ls` contained repository `docker.io/mcavage/pix`, tag
  `uat-run-20260824-110322-d24dac52`, image ID prefix `5e86bfdea88f`
- the fixture pinned `sandboxOptions.template: docker.io/mcavage/pix:uat-run-20260824-110322-d24dac52`
  with `pullPolicy: missing`
- the create receipt's `PREPARE IMAGE` section printed `→ check
  docker.io/mcavage/pix:uat-run-20260824-110322-d24dac52`, matching the pinned
  tag exactly, with no other tag referenced anywhere in the receipt for that
  repository
- no registry pull/download marker (`Pulling from`, `Pull complete`,
  `Download complete`, `Downloading`, `pulling image`) appeared in stdout or
  stderr
- a fresh `sbx ls --json` poll observed that exact instance running

**Correction:** sbx 0.39 does not expose a created-sandbox digest field.
`sbx ls --json` rows carry only `name`, `id`, `agent`, `status`, and
`workspaces`. A sandbox is also not a host-Docker container addressable by
its sandbox name: `docker inspect <sandbox-name>` returns "no such object".
An earlier version of this check compared the local candidate image ID
against `docker inspect --format {{.Image}} <sandbox name>`, an invented
observable that does not exist on this host.

AC-2 in committed design docs is revised accordingly: the proof is exact-tag
registration before create, an exact-tag match in the create receipt with no
mixed reference, an absent registry-pull marker, and a fresh running-instance
probe. This is never digest equality against a created sandbox. Do not claim
digest equality anywhere in this codebase; it is not observable with this sbx
release.

## 8. Recreate boundary

Baseline `pix-uatenv-fixture-recreate` created with `sandboxOptions.memory:
6g`. The same declared identity (same file path, same fixture name) with only
`sandboxOptions.memory: 60g` changed was refused:

```
sandbox 'pix-uatenv-fixture-recreate' already exists
```

The check names the recreate command a caller must use instead:

```console
pix rm pix-uatenv-fixture-recreate && pix run --env recreate-fixture
```

This proves one narrow fact: `sbx env create` does not silently reuse or
attach to a sandbox under a changed declaration at the same identity. It
refuses outright. It does not prove that Pix computes or enforces a semantic
fingerprint over every effective facet: that attribution work, and deciding
which facets require recreation versus in-place reconciliation, remains
Story 2's job in full.

## 9. Failed-create behavior

A create attempted before any positive receipt reported the failure and the
check logged possible residue (scoped secrets, bindings, or MCP
registrations sbx resolves before creating the sandbox) without issuing
either `sbx env rm` or bare `sbx rm`. The check enforces this with its own
`noCleanupExecutor` wrapper, which refuses to forward a removal command at
all in this code path.

This proves Pix's own fail-closed policy implementation inside the check: no
receipt, no removal authority, because another creator could race the same
identity. It does not prove that upstream `sbx` itself automatically cleans
up a failed create. Nothing in this run claims that.

## 10. Scope refusal

Two cases, both refused before any removal argv was issued:

- a non-`pix-*` effective name (`not-pix-scoped-env`)
- a `pix-*`-scoped effective name that does not match the recorded instance
  (`pix-uatenv-fixture-rm-scope-mismatch` against recorded
  `pix-uatenv-fixture-rm-scope`)

Story 0 has no production `sandbox.PlanEnvRemove` yet (Story 2 wires launch
and removal through it). This check exercises Pix's own typed safety-policy
function against two literal fixtures; it is not a claim that upstream `sbx`
enforces Pix's naming scope itself.

## 11. Ollama transport

Probe: `sbx exec -it NAME --model gemma4 --provider ollama -- pi --kit
/opt/pix/kit` against a custom `agent: pix` environment.

Observed failure: an OCI runtime exec error naming `--model` as the missing
executable:

```
executable file `--model` not found in $PATH
```

sbx forwarded the literal `--model` flag to the container runtime as the
command to execute rather than recognizing it as a pre-command transport
flag. This is a real, concrete refusal signature, not an inference: the check
matches the exact executable-not-found shape and records the capability as
`unsupported`.

`extensions/ollama-bridge.ts` remains required. Delete it only after a future
host UAT run proves sbx exposes a stable local-model transport to the Pix
custom agent. This run did not observe one.

## 12. Successful receipt-gated removal

Every fixture this matrix created was fresh-probed by exact name before
removal, then removed with `sbx env rm -f <fixture-path>`. Observed output
reported the sandbox removed and scoped secrets removed.

This run makes no claim about host-global binding or MCP-registration
preservation across removal. `docs/design/environments.md` §4/§9.2 documents
that credential bindings and MCP registrations are host-global and preserved
by default, but this run's fixtures declared no bindings and no MCP servers,
so removal never exercised that path. Do not cite this run as evidence for
binding/MCP preservation; it is unobserved here.

## 13. Closed check vocabulary

No generic shell/argv/env UAT action exists. The six checks in section 3 are
the only environment-matrix surface `candidate_smoke` executes, and they run
only inside the candidate's own `pix-host uat-env-matrix` binary through the
injected `Executor` seam. Unit tests in `services/host/uatenvmatrix` never
require a real `sbx` binary; production always does, through this one path.

## 14. Cleanup debt (known pre-fix residue)

An earlier run, `run-20260824-092338-d4c384f5`, leaked a
`pix-uatenv-fixture-image` sandbox on the host. The cause was an
instrumentation bug in `environment_uses_local_candidate_image`'s prior
version: it reused the same `err` variable for both the create call and a
later, now-removed `docker inspect` call, so the deferred cleanup closure
(which captures its create-error argument by reference) observed the LATER
call's outcome instead of the create's own, and misclassified a successful
create as receiptless. Receiptless means `cleanupCreatedFixture` never issues
a removal by design (section 9 above), so the fixture that create actually
succeeded was silently left running.

The final code fixes both the bug class and the naming hazard:

- `createErr` (and every sibling check's own create-error variable) is now a
  single, never-reassigned identifier per function, statically enforced by
  `immutable_create_err_test.go`.
- the candidate-image fixture's sandbox name is now derived from the
  run-unique image tag itself (`candidateImageFixtureName`), never a fixed
  literal, so two different runs' fixtures can never collide and a stray
  prior name can never block a later run's own attempt at this check.

**This does not make Story 0 leak-free.** The old `pix-uatenv-fixture-image`
sandbox from `run-20260824-092338-d4c384f5` is external cleanup debt on
whatever host ran it. It was not removed by this fix, and it is not removed
by this document. A safe, read-only way to check whether it or anything
similar is still present is:

```console
sbx ls --json
```

Actually removing it requires an explicit operator decision and command on
the host where it was created; no such command is embedded here as already
run, and none should be inferred from this document.

## 15. What this run proves and what it does not

Proven, with host evidence, on sbx v0.39.0:

- a custom `agent: pix` environment with a relative `./kit` reference can be
  created and positively identified (A1)
- name-based `sbx exec` after create delivers the exact intended Pi
  invocation unchanged, without needing an interactive TUI as the transport
  proof (A2)
- a pinned local candidate template creates with no registry pull, matched
  by exact tag in the create receipt (never by an unobservable digest)
- three concrete interpolation outcomes: defined, missing-with-default, and
  bare-missing-resolves-to-empty-string
- recreate at an unchanged identity with a changed effective facet is
  refused, not silently reused
- a before-receipt failed create leaves no Pix-issued removal call
- both non-`pix-*` and instance-mismatched removal are refused before any
  removal argv
- the custom-agent Ollama transport is unsupported today, with a concrete
  observed failure signature (A3 does not require Ollama support; it
  requires the bridge decision to be evidence-based, which it now is)
- safe removal (`sbx env rm -f`) reports the sandbox and scoped secrets
  removed

Left as later-story responsibility, not proven here:

- computing and enforcing a semantic creation/host-trust fingerprint over
  every effective facet (Story 2)
- host-global credential binding and MCP-registration preservation across
  removal, for an environment that actually declares them (unobserved by
  this run's fixtures)
- any production `sandbox.PlanEnvRemove` implementation (Story 2)
- cleanup of the specific pre-fix leaked sandbox named in section 14

## 16. Evidence paths

- `steps/env_matrix.log` (candidate stdout/stderr, ends `RESULT: PASS`)
- `services/host/uatenvmatrix/*_test.go` (unit-level pinned fixtures and
  assertions for every fact in this document)
- `services/host/workflow/uat/env_matrix.go` (the seam that runs the
  candidate's own `pix-host uat-env-matrix` binary)
- `uat/scenarios/smoke.yaml` (the unchanged scenario this run executed)
