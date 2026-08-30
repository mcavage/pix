package env

import (
	"fmt"
	"path/filepath"

	"pix/host/envinfo"
	"pix/host/hosttrust"
)

// Environment is Load's fully composed result: the ONE typed aggregate a
// later Wave C consumer (E1.8's host bill-of-materials/`pix env review`,
// and every later `pix env` verb) reads instead of re-resolving, re-
// parsing, or re-validating any of it. Every field on it has already
// cleared Load's own refusals — a caller holding an *Environment never has
// to re-run RefuseContainment, RefuseSymlinkedRoot, or a strict parse to
// trust it.
type Environment struct {
	// Name is the registered name Load was asked to resolve.
	Name string
	// Root is the canonical environment root: exact-resolved (Resolve),
	// proven not itself a symlink (RefuseSymlinkedRoot), and proven not to
	// resolve inside any of the workspaces passed to Load
	// (RefuseContainment).
	Root string

	// SbxenvPath is the canonical path Document was parsed from — always
	// Root + "/.sbxenv.yaml", since the native document is required.
	SbxenvPath string
	// Document is the strict parse of the required native `.sbxenv.yaml`
	// (envinfo.Parse).
	Document *envinfo.Document
	// Tree is the pre-composition semantic tree envinfo.BuildTree derived
	// from Document (see envinfo/tree.go): every stable-identity node,
	// every authored interpolation, every host-execution facet.
	Tree *envinfo.Tree

	// SidecarPath is the canonical path the optional pix.toml sidecar was
	// parsed from, or "" when the environment has none at all.
	SidecarPath string
	// Sidecar is the strict parse of the optional pix.toml sidecar
	// (envinfo.ParseSidecar), or nil when SidecarPath == "". A missing
	// sidecar is not an error — pix.toml is optional (docs/design/
	// environments.md §5.2) — but an INVALID one Load finds IS.
	Sidecar *envinfo.Sidecar

	// Subject is the hosttrust.Subject this environment's host-exec
	// acceptance record (if any) is keyed by: Subject(Root), never Name.
	// See Subject's own doc comment for why repointing a name never
	// inherits a record under this field.
	Subject hosttrust.Subject
	// Accepted reports whether the store Load was given already held an
	// accepted record for Subject as of this call (IsAccepted(store,
	// Root)) — the review state a caller (a future `pix env use`, AC-14;
	// E1.8's review flow) needs before it decides anything further.
	Accepted bool
}

// MissingRequiredFileError is Load's refusal when a registered environment
// root carries no `.sbxenv.yaml` at all. docs/design/environments.md §5.1
// describes the native document as the environment's own declaration —
// there is no fallback default document Load may substitute for a missing
// one, unlike pix.toml (§5.2), which is genuinely optional.
type MissingRequiredFileError struct {
	Name string
	Root string
	File string
}

func (e *MissingRequiredFileError) Error() string {
	return fmt.Sprintf(
		"pix: environment %q has no required %s.\n     missing: %s\n     create it: author %s",
		e.Name, e.File, filepath.Join(e.Root, e.File), filepath.Join(e.Root, e.File))
}

// AuthoredMounts is the mount set the DOCUMENT ITSELF declares: its
// authored primary `workspace:` plus every `additionalWorkspaces[]` entry,
// each at the path envinfo.Parse already resolved against the environment
// file's own directory. Before the native schema modeled those two keys,
// this package could only ever check a root against mounts a CALLER
// supplied, so an environment whose own file mounted its own directory
// passed containment silently — the exact placement upstream's own
// reference warns against ("Keep the environment file outside the
// directories you mount into the sandbox. That includes the primary
// workspace and every additionalWorkspaces mount"), because the agent can
// then rewrite the file that controls the next `sbx env` command.
//
// The authored primary is reported READ-ONLY when it declares `clone:
// true`: clone mode bind-mounts the host repository read-only and gives
// the agent a private in-container clone instead, so restriction 4's
// writable-workspace rule does not bite there. Every other entry keeps its
// own authored readOnly bit.
//
// An OMITTED `workspace:` contributes NOTHING here. Upstream's documented
// default for it is "first file's directory" — which is the environment
// root itself — but envinfo deliberately does not materialize that default
// (envinfo.Document.Workspace), and a Pix launch always renders its own run
// workspace as the effective primary. Synthesizing it here would refuse
// every environment on earth for a mount Pix never makes.
func AuthoredMounts(doc *envinfo.Document) EffectiveMounts {
	if doc == nil {
		return nil
	}
	var out EffectiveMounts
	if ws := doc.Workspace; ws.Present {
		if path := authoredWorkspacePath(ws.Resolved, ws.Raw); path != "" {
			out = append(out, WorkspaceMount{Path: path, ReadOnly: ws.Clone})
		}
	}
	out = append(out, AuthoredAdditionalMounts(doc)...)
	return out
}

// AuthoredAdditionalMounts is the subset of AuthoredMounts that actually
// becomes a mount in the effective document: the authored
// `additionalWorkspaces[]`. The authored primary is excluded on purpose —
// a Pix launch overrides it with the run's own project workspace
// (envinfo/render.go's effectiveWorkspaces), so listing it as a mount in a
// reviewed bill of materials would ask a reviewer to consent to host
// access Pix never grants.
func AuthoredAdditionalMounts(doc *envinfo.Document) EffectiveMounts {
	if doc == nil {
		return nil
	}
	var out EffectiveMounts
	for _, ws := range doc.AdditionalWorkspaces {
		if path := authoredWorkspacePath(ws.Resolved, ws.Path); path != "" {
			out = append(out, WorkspaceMount{Path: path, ReadOnly: ws.ReadOnly})
		}
	}
	return out
}

// authoredWorkspacePath prefers Parse's resolved path and falls back to
// the authored text, which is what a path still carrying an unresolved
// `${VAR}` keeps (parse.go resolves no interpolation). A containment check
// against an unexpanded expression cannot match a real directory, and that
// is the honest outcome: this package never resolves a host variable to
// decide a refusal.
func authoredWorkspacePath(resolved, raw string) string {
	if resolved != "" {
		return resolved
	}
	return raw
}

// workspacePaths returns every Path in mounts, regardless of its ReadOnly
// bit — the full set envinfo.ValidateSkillWorkspaces checks a LOCAL
// sidecar skill against: a skill may legitimately live under a read-only
// mount exactly as well as a writable one, and Load always adds the
// environment's own root to this set (see Load's own doc comment) so a
// skill declared right under the environment's own directory has
// somewhere to resolve against even when no other workspace was ever
// supplied.
func workspacePaths(mounts EffectiveMounts) []string {
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, m.Path)
	}
	return out
}

// writableWorkspacePaths returns only the WRITABLE entries of mounts —
// RefuseContainment's sole input. docs/design/environments.md §5.1
// restriction 4 is explicitly scoped to a WRITABLE workspace ("must not
// resolve inside any writable workspace it mounts"); a read-only entry can
// never trigger it. This is what makes it safe for Load to unconditionally
// add the environment's own canonical root, read-only, to the workspace
// set it validates sidecar skills against (skillWorkspaces in Load, below)
// without that addition ever self-refusing: every environment's root
// trivially "resolves inside" itself, which would otherwise turn Load's
// own intrinsic bookkeeping into a universal refusal.
func writableWorkspacePaths(mounts EffectiveMounts) []string {
	var out []string
	for _, m := range mounts {
		if !m.ReadOnly {
			out = append(out, m.Path)
		}
	}
	return out
}

// refuseLocalReferenceSymlinks refuses a symlink on every referenced local
// filesystem path Load's own parse produced: each local `kits:` entry
// (doc.Kits[i].Local), each native `mcp.servers[]` local command
// (tree.MCPServers[i].Command), and each pix.toml `[[host.services]]`
// command. A kit's own local-vs-remote classification was already decided,
// ambiguity refused, by envinfo.Parse (classifyKit / ErrAmbiguousKitReference)
// before this function ever runs; RequiresSymlinkCheck gives the MCP-server
// and host-service commands that SAME fail-closed treatment here, since
// envinfo's sidecar/native schemas do not classify those two fields
// themselves.
//
// srv.Command and svc.Command are BARE-OR-PATH command references, unlike a
// kit's already-resolved Resolved field: neither envinfo's native nor
// sidecar schema resolves them, so this function does what
// ResolveLocalCommand documents — a bare name through lookPath, a relative
// path against root, an absolute path unchanged — rather than handing the
// raw string straight to a symlink check that would Lstat it relative to
// this PROCESS's cwd (never the environment's).
func refuseLocalReferenceSymlinks(doc *envinfo.Document, sidecar *envinfo.Sidecar, tree *envinfo.Tree, root string, lookPath func(string) (string, error)) error {
	for i, k := range doc.Kits {
		if !k.Local {
			continue
		}
		if err := RefuseSymlinkedReference(fmt.Sprintf("kit path kits[%d]", i), k.Resolved); err != nil {
			return err
		}
	}
	for _, srv := range tree.MCPServers {
		if srv.Command == "" || !RequiresSymlinkCheck(srv.Command) {
			continue
		}
		resolved, ok := ResolveLocalCommand(root, srv.Command, lookPath)
		if !ok {
			continue
		}
		if err := RefuseSymlinkedReference(fmt.Sprintf("MCP server command %s", srv.KeyPath), resolved); err != nil {
			return err
		}
	}
	if sidecar != nil {
		for _, svc := range sidecar.Host.Services {
			if svc.Command == "" || !RequiresSymlinkCheck(svc.Command) {
				continue
			}
			resolved, ok := ResolveLocalCommand(root, svc.Command, lookPath)
			if !ok {
				continue
			}
			if err := RefuseSymlinkedReference(fmt.Sprintf("host service command %s", svc.Name), resolved); err != nil {
				return err
			}
		}
	}
	return nil
}
