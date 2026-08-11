# `sbx rm` prompts for confirmation and refuses outright with no TTY attached

**Version:** `sbx v0.38` (the version this repo currently pins/documents against)

## Summary

A bare `sbx rm <name>` (no `-f`/`--force`) now asks for interactive
confirmation before removing anything. On a terminal that is fine; from a
non-interactive caller — every subprocess `pix-host` itself execs, since none
of them attach a pty — the prompt has nobody to answer it, and the call
refuses outright rather than hanging. `-f`/`--force` skips the prompt and
removes immediately; there is no third mode.

This is a **transport** change, not a widened removal policy on sbx's side:
`-f` still only skips the confirmation question. It says nothing about
whether removing a given sandbox was ever a good idea — that judgment call is
entirely pix's own, made before `sbx rm` is ever invoked.

## Impact on pix

Every removal pix-host performs is non-interactive (it execs `sbx` from a Go
subprocess call, never with a pty attached), so **every one of them now needs
`-f` just to complete at all** — including the automatic last-shell teardown
and the orphan sweep, which used to plan a deliberately plain `rm` specifically
so a forced removal reaching sbx was itself a signal something had gone wrong
(see the pre-v0.38 history in `services/host/sandbox/remove.go` and
`services/host/workflow/launch/reap.go`).

That collapses a distinction that used to live on the wire (plain `rm` vs.
`rm -f`) into one that must now live entirely in pix's own code: **is this
removal already authorized?** Two proofs answer that, and only two:

1. A kernel-verified, zero-holder reference proof (`lease.TryReapProof`): no
   shell still references the sandbox, no lifecycle transition is in flight.
   This authorizes the automatic reaper and the orphan sweep.
2. An explicitly, individually named removal intent with no lease state left
   to prove a reference against at all (nothing to check). This never covers
   a wildcard (`--all`/`--orphans`).

Only `pix rm NAME --force`, typed by a human, skips proof (1) and (2)
entirely — that is pix's OWN forced seam, unrelated to sbx's `-f`.

## Fix

`services/host/sandbox/remove.go` gained `PlanForceRemove`, sharing the exact
same pix-* scope and name-safety validation as the existing `PlanRemove`
(`validateScopedName`). Every automated removal path
(`workflow/launch/reap.go`'s `decideTeardown`/`removeAndConfirm`, used by the
session teardown, the orphan sweep, and an explicit non-force `pix rm NAME`)
and the explicit `--force` seam (`workflow/launch/sandbox.go`'s
`RemovePixSandbox`) now route through `PlanForceRemove`, so the WIRE argv
always carries `-f`/`--force` while pix's own authorization logic — the two
proofs above — is completely unchanged. `PlanRemove` (no `-f`) is kept as the
pure, tested argv shape a hypothetical interactive caller could still use; no
production caller in this codebase reaches for it against a live `sbx`
anymore, because none of them have a TTY to answer sbx's own prompt.

A grep/AST guard (`services/host/forcerm_guard_test.go`,
`TestNoAdHocForcedSbxRmOutsidePlanner`) fails the build on any OTHER call site
that hand-composes the `"sbx", "rm", "-f"`/`"--force"` literal argv shape —
the only place that shape may appear is the planner itself.

## Notes

If a future sbx version restores a working non-interactive plain `rm` (e.g. a
flag that answers "no" to the prompt without also meaning "skip
confirmation AND remove"), `PlanRemove`'s non-force shape is already in place
to switch back to for the proof-gated paths; only the two call sites named
above would need to change, not the authorization logic itself.
