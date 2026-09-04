// Package env is the v2 native-environment L3 workflow (docs/design/
// pix-v2-surface.md §3.4, docs/design/pix-v2-architecture.md §6): an
// environment IS a directory under $PIX_HOME/envs/<name>/ declaring
// .sbxenv.yaml (native sbx grammar) and an optional pix.toml sidecar. There
// is no registration database — Pix does not maintain one — so this
// package has no add/edit/use/review/forget mutation API and no
// config-registry lookup. What it does own:
//
//   - home.go — ValidName/ResolveIn/List/SelectName/SelectIn: the whole v2
//     selection model. An environment stored elsewhere is a SYMLINK inside
//     envs/, resolved AT MOST one hop (a chain is refused, never chased) so
//     every later read, containment check, and fingerprint uses the same
//     resolved Root. refuseUnsafeMode refuses a group- or world-writable
//     root.
//   - resolve.go — RefuseContainment (an environment root must not resolve
//     inside any writable workspace it mounts), RefuseSymlinkedRoot (a
//     symlinked root is refused) / ResolveSymlinkedReference (a referenced
//     local executable's symlink chain is resolved to its physical target,
//     refusing only a broken, escaping, or wrong-shaped result — never
//     every symlink outright), RequiresSymlinkCheck (fail-closed local-vs-
//     remote classification), ResolveLocalCommand (bare/relative/absolute
//     command resolution that never touches the calling process's cwd),
//     and the Subject/IsAccepted pair a future trust rewrite may key
//     acceptance by (hosttrust.Subject is keyed by CANONICAL ROOT, never
//     NAME, so repointing a name never inherits acceptance).
//   - load.go / loadhome.go — the end-to-end pre-BOM composition: read the
//     required native `.sbxenv.yaml` and optional `pix.toml` sidecar
//     (envinfo.Parse/ParseSidecar), build the pre-composition Tree
//     (envinfo.Merge + envinfo.BuildTree), refuse an authored collision
//     with either reserved built-in MCP name (envinfo.RefuseReservedMCPNames),
//     refuse an undefined bare `${VAR}` interpolation
//     (envinfo.RefuseUndefinedInterpolations), validate every sidecar skill
//     path against the caller-supplied workspaces
//     (envinfo.ValidateSkillWorkspaces), and refuse every local referenced
//     kit/command/executable that is symlinked or ambiguously classified.
//     LoadHome (loadhome.go) is the v2 entry point, taking an already
//     pixhome-resolved Selected (home.go) instead of a registry lookup;
//     load.go supplies the shared helpers (AuthoredMounts,
//     AuthoredAdditionalMounts, workspacePaths, writableWorkspacePaths,
//     refuseLocalReferenceSymlinks) both LoadHome and bom.go's ComputeBoM
//     reuse.
//   - bom.go — the canonical, pure-function-of-the-document host bill of
//     materials (ComputeBoM) and its fingerprint (Fingerprint): every host
//     command/service, credential target, mount expansion, MCP server,
//     kit digest, and authored interpolation an environment would run on
//     or hand a credential to. `pix env trust` (cmd/pix/env_cmd.go) is the
//     one caller; this is the "complete canonical host BOM", never a
//     two-file hash.
//   - effective.go — ComputeEffective/RenderEffectiveDocument: the ONE
//     preview-side caller of envinfo.RenderEffective (`pix env [NAME]
//     --effective`), composing the SAME reserved pix-memory/pix-session
//     built-ins a real launch adds (cmd/pix/run_env.go's own
//     builtinMCPFacts), so a preview never shows a shape a real create
//     would then silently add to.
//   - runhint.go — the one-shot "you have an unregistered .sbxenv.yaml
//     right here" hint `pix run` prints for a project workspace that
//     carries a native file Pix was never asked to use (docs/design/
//     pix-v2-surface.md §3.4: "Pix never auto-selects a `.sbxenv.yaml`
//     found in a project workspace").
//
// # Why this is a workflow, not a capability
//
// It composes L1 capabilities (hosttrust, envinfo, sandbox) plus L0
// pixhome/config/cli — exactly what an L3 workflow is for
// (services/host/arch_test.go). It imports no other workflow/* package
// (the sibling-workflow rule) and contains no domain knowledge envinfo or
// hosttrust do not already own: it wires their answers together and
// returns the caller (cmd/pix/env_cmd.go, workflow/launch) a resolved
// root, a typed refusal, or the full composed *Environment.
//
// # No process globals
//
// Every function here takes its inputs explicitly: a pixhome.Paths to
// read, a canonical root or list of workspace roots to check, a
// *hosttrust.AcceptanceStore to query. None of them call config.Load,
// os.Getwd, or any other process-global lookup on their own (ComputeEffective
// is the one caller that reads os.Getwd, and it does so explicitly for the
// documented "current directory is the project workspace" contract, never
// implicitly) — a caller decides which home or working directory is live
// and hands it in. This is what makes exact-name resolution identical from
// any cwd true by construction rather than by convention.
package env
