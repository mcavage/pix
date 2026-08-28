// Package env is E1.7's new L3 workflow: the native-environment
// registry/exact-name-resolution/location-refusal spine (docs/design/
// environments.md §5.3, §8.1; Wave C of the native-environments delivery,
// units.json E1.7). It composes config (L0), hosttrust (L1) and envinfo (L1)
// and owns nothing they already own.
//
// This unit is deliberately narrow — "the resolution spine only" per its
// units.json scope line. It has NO `pix env` verb, NO command dispatch, and
// NO host bill-of-materials rendering (that is E1.8's review.go/bom.go) or
// CLI wiring (E1.9-E1.13's cmd/pix/env_cmd.go). What it does own:
//
//   - registry.go — Register/Unregister/Root/Known, each a thin wrapper over
//     the config-owned helpers (config.AddEnvironment, config.RemoveEnvironment,
//     the Environments map) added in E1.5. This package never reimplements
//     canonicalization, name validation, or the sparse-Save contract — those
//     stay config's job, and this file's whole point is that a `pix env` verb
//     never has to reach into cfg.Environments directly.
//   - resolve.go — Resolve (exact-name -> canonical root, typed unknown
//     error, no prefix/fuzzy fallback: AC-10), RefuseContainment (AC-11),
//     RefuseSymlinkedRoot/RefuseSymlinkedReference (AC-12), and the
//     Subject/IsAccepted pair that proves repointing a name never inherits
//     acceptance (AC-16) — the hosttrust Subject is keyed by CANONICAL ROOT,
//     never by NAME, so a name that starts pointing somewhere else is, from
//     hosttrust's point of view, simply a different subject with no record
//     at all.
//
// # Why this is a workflow, not a capability
//
// It composes two L1 capabilities (hosttrust, envinfo) plus L0 config and
// cli — exactly what an L3 workflow is for (architecture.md). It imports no
// other workflow/* package (see arch_test.go's sibling-workflow rule) and,
// per this unit's own scope, contains no domain knowledge envinfo or
// hosttrust do not already own: it wires their answers together and returns
// the caller (E1.8, E1.9, a future `pix env` verb) a resolved root, a typed
// refusal, or a trust lookup.
//
// # No process globals
//
// Every function here takes its inputs explicitly: a *config.Config to read
// or mutate, a canonical root or list of workspace roots to check, a
// *hosttrust.AcceptanceStore to query. None of them call config.Load,
// os.Getwd, or any other process-global lookup on their own — a caller (a
// future command layer) decides which config or working directory is live
// and hands it in. This is what makes "resolves identically from any cwd"
// (AC-10) true by construction rather than by convention: nothing in this
// package's resolution path ever consults the process's current directory.
package env
