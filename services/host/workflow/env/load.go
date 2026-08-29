package env

import (
	"fmt"
	"os"
	"path/filepath"

	"pix/host/cli"
	"pix/host/config"
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
	Root string
	File string
}

func (e *MissingRequiredFileError) Error() string {
	return fmt.Sprintf("pix: environment root %s has no required %s", e.Root, e.File)
}

// ResolveEnvironment composes the three checks every later stage of Load
// shares into the ONE root-resolution step: Resolve (AC-10, exact name ->
// canonical root, no fuzzy fallback), RefuseSymlinkedRoot (AC-12, half),
// and RefuseContainment (AC-11, the root must not resolve inside any
// WRITABLE entry of effective). It is exported in its own right — not
// merely a Load implementation detail — because a caller that only needs a
// trustworthy root (no file parsing at all) should never have to duplicate
// this ordering to get one.
//
// effective is the ONE typed effective workspace set (bom.go's
// EffectiveMounts — {Path, ReadOnly}) that flows end-to-end through Load,
// Review and ComputeShow: there is no separate, independently-suppliable
// `workspaces []string` a caller could pass out of step with what a
// review/BoM computation sees (that was E1.9's BLOCK finding — two
// unrelated lists reaching the same environment, free to diverge). Only
// the WRITABLE entries of effective are ever checked here
// (writableWorkspacePaths): restriction 4 (docs/design/environments.md
// §5.1) is explicitly a WRITABLE-workspace rule, and a read-only entry —
// including the intrinsic environment-root source workspace Load itself
// adds below — must never self-refuse merely by existing.
//
// Order is deliberate: an unknown name is reported before either location
// refusal runs at all, since neither has a root to inspect otherwise; the
// symlink check runs before containment so a symlinked root is always
// named as exactly that, never mischaracterized as a containment problem
// merely because its resolved target happens to sit somewhere unexpected.
func ResolveEnvironment(cfg *config.Config, name string, effective EffectiveMounts) (string, error) {
	root, err := Resolve(cfg, name)
	if err != nil {
		// Resolve itself returns a bare *config.UnknownEnvironmentError (it
		// predates this file and has its own tests asserting that exact
		// type via errors.As); ResolveEnvironment is the composition layer
		// responsible for the usage/operational classification Load
		// promises, so it wraps here rather than asking Resolve to change
		// its own established return type. cli.UsageError's Unwrap still
		// makes errors.As(err, &unknownEnvErr) find the original type.
		return "", cli.UsageError{Err: err}
	}
	// Canonicalize/validate root IMMEDIATELY, before RefuseSymlinkedRoot's own
	// os.Lstat (a filesystem read) ever runs on it: config.AddEnvironment
	// (E1.5) never persists anything but an already-canonical value, and
	// config.Load's dropNoncanonicalEnvironments drops a hand-edited one on
	// disk load — but a *config.Config a caller assembles directly never runs
	// that pass, so this package cannot trust cfg.Environments
	// unconditionally. See NoncanonicalRootError's doc comment (resolve.go).
	if !config.IsCanonicalEnvironmentPath(root) {
		return "", cli.UsageError{Err: &NoncanonicalRootError{Name: name, Root: root}}
	}
	if err := RefuseSymlinkedRoot(root); err != nil {
		return "", err
	}
	if err := RefuseContainment(root, writableWorkspacePaths(effective)); err != nil {
		return "", err
	}
	return root, nil
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

// Load is the end-to-end E1.7 pre-spine composition: resolve name to a
// trustworthy canonical root (ResolveEnvironment), read the required
// native `.sbxenv.yaml` and the optional `pix.toml` sidecar (envinfo.Parse
// / envinfo.ParseSidecar), build the pre-composition Tree
// (envinfo.Merge + envinfo.BuildTree), validate every sidecar skill path
// against every resolved workspace (envinfo.ValidateSkillWorkspaces), and
// refuse every local referenced kit/command/executable Load can name that
// is either symlinked or whose local-vs-remote classification is ambiguous
// (RefuseSymlinkedReference + RequiresSymlinkCheck, the same fail-closed
// rule AC-12 already established).
//
// effective is the ONE caller-supplied, typed effective workspace set
// (EffectiveMounts) this call composes against — the same value
// ResolveEnvironment's containment refusal and this function's own skill
// validation both derive their inputs from, so there is no second,
// independently-suppliable `workspaces []string` a caller could pass out
// of step with it (E1.9's BLOCK finding). Neither this package nor
// envinfo derives effective's caller-declared entries on its own — that
// composition belongs to whichever later unit builds the effective launch
// declaration (E1.8's bill of materials, a future E2 renderer); see
// EffectiveMounts's own doc comment for the compile-time seam that forces
// E2 to supply real writable mounts here rather than reviving a bare
// []string.
//
// Load ALWAYS additionally validates sidecar skills against the
// environment's own canonical root, read-only — an intrinsic source
// workspace no caller ever has to supply and no caller can omit: a LOCAL
// `[pi].skills` entry legitimately lives right under the environment's own
// directory (docs/design/environments.md §5.2), and until a future E2
// launch composition supplies any writable mount at all, root is the ONLY
// workspace such a skill could possibly resolve inside. This addition is
// purely internal to skill validation — it is never returned on
// *Environment, never reaches ComputeBoM/Review's bill, and (being
// read-only) can never trigger RefuseContainment's self-refusal.
//
// Load returns a structured error, never a bare string: a cli.UsageError
// (cli.ExitCode == 2) for anything the user can fix by editing what they
// authored or registered — an unknown name, a location refusal, a strict
// parse/validation failure, a missing required file — and a plain error
// (cli.ExitCode == 1) for an operational failure Load cannot itself
// resolve, such as a file that exists but could not be read for a reason
// unrelated to its content (a permissions error, for example).
func Load(cfg *config.Config, store *hosttrust.AcceptanceStore, name string, effective EffectiveMounts, lookPath func(string) (string, error)) (*Environment, error) {
	root, err := ResolveEnvironment(cfg, name, effective)
	if err != nil {
		return nil, err
	}

	sbxenvPath := filepath.Join(root, ".sbxenv.yaml")
	if _, statErr := os.Stat(sbxenvPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, cli.UsageError{Err: &MissingRequiredFileError{Root: root, File: ".sbxenv.yaml"}}
		}
		return nil, fmt.Errorf("pix: environment %s: %w", name, statErr)
	}
	doc, err := envinfo.Parse(sbxenvPath)
	if err != nil {
		return nil, cli.UsageError{Err: err}
	}

	sidecarPath := filepath.Join(root, "pix.toml")
	var sidecar *envinfo.Sidecar
	switch _, statErr := os.Stat(sidecarPath); {
	case statErr == nil:
		sidecar, err = envinfo.ParseSidecar(sidecarPath)
		if err != nil {
			return nil, cli.UsageError{Err: err}
		}
	case os.IsNotExist(statErr):
		sidecarPath = "" // optional and absent: not an error (docs/design/environments.md §5.2)
	default:
		return nil, fmt.Errorf("pix: environment %s: %w", name, statErr)
	}

	merged, err := envinfo.Merge(doc)
	if err != nil {
		return nil, cli.UsageError{Err: err}
	}
	tree, err := envinfo.BuildTree(merged)
	if err != nil {
		return nil, cli.UsageError{Err: err}
	}

	if sidecar != nil {
		// skillWorkspaces is every caller-declared effective mount PLUS the
		// environment's own root — see Load's doc comment above for why the
		// root is always implicitly present here and nowhere else.
		skillWorkspaces := append(workspacePaths(effective), root)
		if err := envinfo.ValidateSkillWorkspaces(sidecarPath, sidecar, skillWorkspaces); err != nil {
			return nil, cli.UsageError{Err: err}
		}
	}

	if err := refuseLocalReferenceSymlinks(doc, sidecar, tree, root, lookPath); err != nil {
		return nil, err
	}

	root = hosttrust.CanonicalRoot(root)
	return &Environment{
		Name:        name,
		Root:        root,
		SbxenvPath:  sbxenvPath,
		Document:    doc,
		Tree:        tree,
		SidecarPath: sidecarPath,
		Sidecar:     sidecar,
		Subject:     Subject(root),
		Accepted:    IsAccepted(store, root),
	}, nil
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
