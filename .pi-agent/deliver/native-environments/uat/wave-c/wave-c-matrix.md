# Wave C QA/UAT Matrix — E1.7–E1.15 `pix env`, committed HEAD 8ab00d7e

Executed by an independent QA subagent standing in for the `qa-lead` preset,
which twice failed to start (local Ollama unavailable — an infrastructure
failure of the model-routing preset, not a product defect; see
"Preset infrastructure failure" below). Isolation: fresh `$PIX_CONFIG`,
`$XDG_CONFIG_HOME`, `$XDG_STATE_HOME`, `$XDG_DATA_HOME` scratch directories
per scenario, against a locally built candidate
(`services/host/cmd/pix` + `services/host` built at 8ab00d7e). No real user
config, no real sandbox, no host secret values. No sbx/host-launch claims are
made anywhere in this evidence: every row below is either a shipped Go/node
test executed directly, or a real `pix env`/`pix status`/`pix doctor` command
run against the candidate binary with isolated XDG/PIX_CONFIG — nothing here
depends on an available host-side `sbx`.

Since d15ca2c1 (prior Wave C round), 8 more commits landed
(`d232272e` env review TOCTOU (H1) + config lost-update (L1) fixes,
`4ac9f494` commitEnvRegistryMutation full-config sync (A3),
`7cd2f775` physical (EvalSymlinks) containment (A1),
`b8fa3594` planted-violation copy-lint self-tests (A2),
`e731714f` explicit review-state taxonomy + run hint (B4/B5),
`811dbde9` show flag conflict / review-gate retries / add PATH prevalidation
(C6-C8), `d0a2e6c0`+`853b4121` rm-pointer/closest/repoint/copy fixes
(C9-C13), `e1b15a97` shell-quoting of every dynamic token (security review
BLOCK), `8ab00d7e` terminal-safe display sanitization at the trust-bill
renderer (M1)). This round re-verifies the full E1.7–E1.15 surface plus every
one of those fixes, live, at 8ab00d7e.

## Preset infrastructure failure (non-product)

The `qa-lead` subagent preset resolves to a local Ollama model
(`agents/qa-lead.md`); Ollama was not reachable in this environment on two
consecutive attempts, so the preset could not start a session at all — no
prompt was sent, no tool ran. This is host/model-routing infrastructure, not
a defect in `pix env` or in the committed product code, and it blocks
nothing below: this QA pass was executed directly, running every command and
test itself rather than through that preset.

## User cleanup handling (out of scope, never touched)

The working tree carried unstaged user changes at task start: deletion of
`.claude/settings.local.json`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`,
`NOTICE.md`, `SECURITY.md`, `THIRD_PARTY_NOTICES.md`, and a one-line edit to
`LICENSE`. These were never staged, committed, or modified by this QA pass.

1. **Gate as-is** (`gate-asis-with-user-cleanup.log`): run first, before
   touching anything, to record exact attribution. Result: `node-test`
   segment FAILS (all other 10 segments pass) — exclusively on the legal-doc
   suite (`tests/legal-inclusion-and-docker-base.test.mjs`,
   `tests/legal-third-party-notices.test.mjs`,
   `scripts/check-third-party-notices.sh`), which assert `NOTICE.md`,
   `CONTRIBUTING.md`, `THIRD_PARTY_NOTICES.md` exist and that `LICENSE`
   matches the recorded copyright holder. This is the user's own deletions
   breaking legal-doc tests, exactly as expected — not a product regression.
2. **Byte-exact capture**: `git diff > user-cleanup.patch`
   (sha256 `c6df92b5ac482047c92b7224c6e8427cb21ae829825c33f49d4057382dc0536d`,
   see `user-cleanup.patch.sha256`) plus `git status --porcelain` snapshot
   (`user-cleanup.status.before`).
3. **Stash** (`git stash push` naming only those 7 tracked files, no `-u`
   needed — no untracked files existed), leaving the tree byte-identical to
   committed HEAD `8ab00d7e` (`git status --porcelain` empty,
   `git rev-parse HEAD` unchanged).
4. **Gate at committed HEAD** (`gate-final-committed-head.log`): full clean
   PASS, all 11 segments `ok`, total 32.778s, verdict `pass`. This is the
   authoritative final gate for this evidence.
5. **Restore**: `git stash pop`. Verified byte-exact: `git status --porcelain`
   after (`user-cleanup.status.after`) diffs identical to before, and
   `git diff` after the pop hashes to the *same* sha256
   (`c6df92b5ac482047c92b7224c6e8427cb21ae829825c33f49d4057382dc0536d`) as the
   captured patch. Nothing of the user's was staged or committed.

## Matrix

| # | Item | Verdict | Evidence |
|---|------|---------|----------|
| 1 | All seven verbs present; `env rm` is a hidden pointer, not a verb | PASS | `happy-tier0.log` (`env --help` lists ls/add/use/show/edit/review/forget, no rm); `forget-default-holder-repoint.log` #55: `pix env rm home` → "`pix env rm` does not exist... pix env forget home" exit 2 |
| 2 | Tier0: `review: not-required`, `n/a` in `ls`, `review_state: "not_required"` in JSON | PASS | `happy-tier0.log` #4/#7/#5: show renders `not-required`; `ls` shows `n/a`; JSON `"review_state": "not_required"` |
| 3 | Tier1 accepted | PASS | `happy-tier1-retry-review.log` #20/#21/#23: `review: accepted (fingerprint …)`, JSON `"accepted": true, "review_state": "accepted"`, `ls` shows `accepted` |
| 4 | Tier1 unaccepted (first-ever host-exec, never reviewed) | PASS | `edit-targets-verdicts.log` #47: JSON `"accepted": false, "review_state": "unaccepted"` after edit introduces host-exec with no prior acceptance record |
| 5 | Tier1 changed (prior acceptance, footprint mutated) | PASS | `happy-tier1-retry-review.log` #24/#25: `use` refused "changed what it runs on your host"; JSON `"accepted": false, "review_state": "changed"` |
| 6 | JSON explicit `"accepted": false` (never a bare absent/null field) | PASS | `happy-tier0.log` #5 (`false`/`not_required`); `happy-tier1-retry-review.log` #25 (`false`/`changed`); `edit-targets-verdicts.log` #47 (`false`/`unaccepted`) |
| 7 | Review counts (host commands / credential targets) | PASS | `happy-tier1-retry-review.log` #19/#26: `1 host command  secret:db` / `1 credential target  command: … -> (unbound)` |
| 8 | TTY `y/N`: `n` refuses, records nothing | PASS | `tty-pty.log` #28/#29 (real pty via python, sends bare `n`): "not accepted (you said no)... recorded: nothing"; env absent afterward |
| 9 | TTY `y/N`: `y` accepts | PASS | `tty-pty.log` #30/#31 (real pty sends `y`): "recorded acceptance...", JSON `accepted: true` |
| 10 | Non-TTY without `--yes` fails closed | PASS | `happy-tier1-retry-review.log` #19 (`env add` under `< /dev/null`, no tty): "refusing to review it non-interactively (fail closed)"; env `review_test.go: TestReview_NonTTYFailsClosed` |
| 11 | Non-TTY `--yes` accepts | PASS | `happy-tier1-retry-review.log` #19 second half + #26: `--yes` → "consent supplied via --yes" → recorded |
| 12 | Emitted retry roundtrip: the exact suggested retry command works verbatim | PASS | `happy-tier1-retry-review.log` #14→#19: refusal at step 14 emits `retry: pix env add work <path> --yes`; step 19 runs that exact literal string and it succeeds (exit 0) |
| 13 | Shell injection: every dynamic token shell-quoted in a runnable command (e1b15a97 fix) | PASS | Go: `shellquote_injection_test.go` — 12/12 `TestShellInjection_*` PASS under `-race` (`shell-terminal-injection.log`, `race-registry-concurrent.log`) |
| 14 | Terminal control injection: no raw control/bidi bytes reach the terminal from untrusted doc content (8ab00d7e fix) | PASS | Go: `review_terminal_injection_test.go` — `TestTerminalControlViolations_SelfTest` incl. bidi RLO / isolate subtests PASS (`shell-terminal-injection.log`); `bom_e18block_test.go` hostile-content render tests PASS |
| 15 | Exact-name lookup, closest-match suggestion on typo (no fuzzy select) | PASS | `closest-containment-collision.log` #32/#33: `show hme` / `use homr` → "no environment named" + `closest: home` (suggestion only, never auto-selected) |
| 16 | Repoint default via `use` | PASS | `forget-default-holder-repoint.log` #56: `add other2` (same root as forgotten `other`) then `use other2` → "now the default" |
| 17 | Missing path collision | PASS | `closest-containment-collision.log` #36: nonexistent path → "does not exist... scaffold instead: pix env add missing" exit 2 |
| 18 | Non-dir path collision | PASS | `closest-containment-collision.log` #37: a plain file path → "is not a directory... scaffold instead" exit 2 |
| 19 | Scaffold collision (dir already exists at the default scaffold location) | PASS | `closest-containment-collision.log` #38: "already exists; refusing to overwrite... register it as-is: pix env add <name> <path>" exit 2 |
| 20 | Containment: relative / `~` paths refused (canonicalized, non-existent-after-canon refused) | PASS | `closest-containment-collision.log` #34/#35: relative path canonicalized against CWD then refused as nonexistent; `~/...` expands and is refused as nonexistent — neither is silently accepted as a raw string |
| 21 | Physical symlink containment (EvalSymlinks, A1 fix 7cd2f775) | PASS | `closest-containment-collision.log` #39: environment root itself a symlink → "is a symlink; refusing" exit 2; Go `resolve_test.go` symlink-escape + `TestShellInjection_NoncanonicalRoot` PASS |
| 22 | Concurrent registry / race: disjoint adds under `-race` (H1/L1/A3 fixes) | PASS | Go: `TestAdd_CommitPreservesConcurrentDisjointRegistration`, `TestAdd_ConcurrentRepointDuringPromptRefusesDeterministically`, `TestAdd_ConcurrentSameRootRegistrationIsIdempotent`, `TestUse_CommitPreservesConcurrentRegistration`, `TestUse_RefusesWhenNameForgottenConcurrently`, `TestForget_CommitPreservesConcurrentRegistration`, `TestForget_RefusesWhenConcurrentUseMadeItTheDefault`, `TestCommit_SyncsEntireConfigIncludingUnrelatedFields`, `TestEnvMutations_ParallelDisjointAddsAllSurvive` — 10/10 PASS under `go test -race` (`race-registry-concurrent.log`) |
| 23 | Live concurrent registration (5 real parallel `pix env add` processes, disjoint names) | PASS | `live-concurrent-registration.log` #57/#58: all 5 processes exit 0, all 5 appear in `env ls` afterward — no lost update |
| 24 | Edit targets: `edit NAME pix`, `edit NAME sbxenv`, exact enum, no flag, bogus/missing target refused | PASS | `edit-targets-verdicts.log` #41/#42 (valid targets, exit 0, "no host-execution footprint to review"); #43 (`bogus` → "unknown target" exit 2); #44 (no target, non-TTY → "needs a target file; no TTY to ask interactively" exit 2) |
| 25 | Edit verdicts: valid/no-op, valid-but-changed-footprint (never prompts inline) | PASS | `edit-targets-verdicts.log` #42 ("is valid; no host-execution footprint to review"); #45 (edit introduces host-exec → "is valid, but its host-execution footprint changed... next: pix env review home", exit 0, no inline prompt); #46 (`use` after refuses: "has not been reviewed") |
| 26 | Forget: unregisters, never deletes source | PASS | `forget-default-holder-repoint.log` #53: "unregistered. Source untouched: <path>"; file still present on disk after |
| 27 | Forget/default holder seam: forgetting the current default refuses | PASS | `forget-default-holder-repoint.log` #52: "is the current default; forget refuses to leave it dangling... pick a different default first" exit 2 |
| 28 | `pix reset`: never deletes an environment source (external or scaffolded) | PASS | Go: `TestReset_NeverDeletesAnEnvironmentSource`, `TestReset_ExternalEnvironmentSource_UntouchedByteIdenticalAcceptanceGone`, `TestReset_ScaffoldedEnvironmentSource_TravelsWithDataDirRename`, `TestReset_DataBlockedFailure_StillInvalidatesAcceptance_NeverMovesTheSource`, `TestReset_NeverDeletes` — 5/5 PASS (`reset-unit.log`) |
| 29 | `pix reset` invalidates every env trust acceptance | PASS | same suite above; env-package acceptance-record tests confirm the fingerprint store is what `reset` clears |
| 30 | doctor/status: no environment row at all when none registered (not even "none") | PASS | Live: `hint-status-doctor.log` #61 (`pix doctor \| grep -i env` → empty output); Go: `workflow/doctor/doctor_test.go` "empty `[environments]` registry ... names no environment row at all" assertion PASS |
| 31 | Run hint: single negative-first nudge, at-most-once per canonical workspace, never parses `.sbxenv.yaml`, fails open toward silence | PASS | Go: `TestRunHint_AbsentFile_NoHint`, `TestRunHint_PresentFile_NoRegistration_Hints`, `TestRunHint_ExactRegisterAndSelectCommand`, `TestRunHint_RegisteredEnvironment_Suppresses`, `TestRunHint_RepeatedRun_ShowsOnce`, `TestRunHint_DifferentWorkspaces_EachShowsOnce`, `TestRunHint_MarkerIsDurableOnDisk` — 7/7 PASS (`runhint-unit.log`). The hint fires inside `cmd/pix/run_cmd.go`'s real sandbox-launch path, which requires host `sbx` — genuinely out of this QA's reach; honestly reported as such rather than claimed via an unavailable host-sbx call. `RunHint()` itself is exercised directly and completely by the unit suite above, including the "shows once" and "durable marker on disk" cases. |
| 32 | Copy lint: no em-dash/filler/unearned verdict in env copy | PASS | Node: `tests/anti-em-dash.test.mjs` (part of `node-criteria-suite.log`, 20/20 including em-dash) + Go `env_copy_lint_test.go` (in full env-package run, `env-package-verbose.log`) |
| 33 | Decision-alignment / verb-reference docs reconciled | PASS | Node: `tests/environments-decision-alignment.test.mjs`, `tests/environments-security-docs.test.mjs`, `tests/verb-references.test.mjs` — all PASS (`node-criteria-suite.log`, 20/20) |
| 34 | Full `workflow/env` package, race-clean | PASS | `go test -race ./workflow/env/... ./workflow/reset/...` exit 0 (`race-env-reset.log`); full non-race verbose run: 206 PASS / 0 FAIL / 0 SKIP (`env-package-verbose.log`) |
| 35 | Focused package set (env, reset, doctor, provision, uat, cmd/pix, cmd/pix/corpus) | PASS | 848 PASS / 0 FAIL / 1 SKIP (unrelated `TestOAuthBrowserLockHeldDuringLifetime`), all 7 packages `ok` (`focused-tests.log`) |
| 36 | Full `make gate` at committed HEAD 8ab00d7e | PASS | 11/11 segments `ok` (go-build, go-vet, go-test, node-test, typecheck, open-core, recall-xport, secret-scan, arch-metrics, deadcode, rename-guard), total 32.778s, verdict `pass` (`gate-final-committed-head.log`) |

## Findings (none blocking)

1. INFO — the run-hint's live trigger point lives inside the real sandbox
   launch path (`cmd/pix/run_cmd.go`), which needs host `sbx`; that half is
   out of this QA's reach and is not claimed as tested. `RunHint()` itself
   (the actual logic: file present/absent, registered/not, shows-once,
   durable marker) is fully unit-tested and was re-verified directly.
2. INFO — `env show --effective` remains `ErrEffectiveNotAvailable` (exit 1)
   by design; the effective renderer is E2.1, not in Wave C scope.
3. INFO — sandbox drift in `env show`/`env ls` is reported as
   `unknown (live-launch drift lands with a later wave)`; live-holder
   probing is injectable and defaults to `NoLiveHolders` in the CLI —
   real sandbox-holder checks are a later wave / host-sbx scope.
4. NOTE — the `qa-lead` preset's Ollama dependency failing to start twice is
   recorded above as infrastructure, not product; this pass substituted
   direct command/test execution and lost no coverage as a result.

## VERDICT

**PASS** — every in-scope Wave C row (E1.7–E1.15, all seven verbs + hidden
`rm`, tier0/tier1 review states with explicit JSON booleans and counts,
TTY/non-TTY/`--yes` review and add, the emitted retry roundtrip run
literally, shell and terminal control injection hardening, closest-name
suggestion, repoint, missing/non-dir/scaffold-dir collisions, physical
symlink containment, concurrent registry/race (both `-race` unit tests and 5
live parallel processes), edit targets and verdicts, the forget/default
holder seam, `pix reset` external/scaffolded-source invariants, and the
doctor/run-hint-once surfaces) passes at committed HEAD `8ab00d7e`. Full
`make gate` is clean (11/11 segments) at that exact HEAD with the user's
unstaged legal-doc cleanup stashed out and restored byte-identical
afterward (verified by matching `git status` and matching patch SHA256).
No claim in this document depends on an unavailable host `sbx`.
