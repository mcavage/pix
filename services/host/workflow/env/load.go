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
// and RefuseContainment (AC-11, the root must not resolve inside any of
// workspaces). It is exported in its own right — not merely a Load
// implementation detail — because a caller that only needs a trustworthy
// root (no file parsing at all) should never have to duplicate this
// ordering to get one.
//
// Order is deliberate: an unknown name is reported before either location
// refusal runs at all, since neither has a root to inspect otherwise; the
// symlink check runs before containment so a symlinked root is always
// named as exactly that, never mischaracterized as a containment problem
// merely because its resolved target happens to sit somewhere unexpected.
func ResolveEnvironment(cfg *config.Config, name string, workspaces []string) (string, error) {
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
	if err := RefuseSymlinkedRoot(root); err != nil {
		return "", err
	}
	if err := RefuseContainment(root, workspaces); err != nil {
		return "", err
	}
	return root, nil
}

// Load is the end-to-end E1.7 pre-spine composition: resolve name to a
// trustworthy canonical root (ResolveEnvironment), read the required
// native `.sbxenv.yaml` and the optional `pix.toml` sidecar (envinfo.Parse
// / envinfo.ParseSidecar), build the pre-composition Tree
// (envinfo.Merge + envinfo.BuildTree), validate every sidecar skill path
// against workspaces (envinfo.ValidateSkillWorkspaces), and refuse every
// local referenced kit/command/executable Load can name that is either
// symlinked or whose local-vs-remote classification is ambiguous
// (RefuseSymlinkedReference + RequiresSymlinkCheck, the same fail-closed
// rule AC-12 already established). workspaces is the caller-supplied list
// of writable workspace roots this environment would mount — the same
// input RefuseContainment and ValidateSkillWorkspaces both need, and
// neither this package nor envinfo derives it on its own (see resolve.go's
// RefuseContainment doc comment for why).
//
// Load returns a structured error, never a bare string: a cli.UsageError
// (cli.ExitCode == 2) for anything the user can fix by editing what they
// authored or registered — an unknown name, a location refusal, a strict
// parse/validation failure, a missing required file — and a plain error
// (cli.ExitCode == 1) for an operational failure Load cannot itself
// resolve, such as a file that exists but could not be read for a reason
// unrelated to its content (a permissions error, for example).
func Load(cfg *config.Config, store *hosttrust.AcceptanceStore, name string, workspaces []string) (*Environment, error) {
	root, err := ResolveEnvironment(cfg, name, workspaces)
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
		if err := envinfo.ValidateSkillWorkspaces(sidecarPath, sidecar, workspaces); err != nil {
			return nil, cli.UsageError{Err: err}
		}
	}

	if err := refuseLocalReferenceSymlinks(doc, sidecar, tree); err != nil {
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
func refuseLocalReferenceSymlinks(doc *envinfo.Document, sidecar *envinfo.Sidecar, tree *envinfo.Tree) error {
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
		if err := RefuseSymlinkedReference(fmt.Sprintf("MCP server command %s", srv.KeyPath), srv.Command); err != nil {
			return err
		}
	}
	if sidecar != nil {
		for _, svc := range sidecar.Host.Services {
			if svc.Command == "" || !RequiresSymlinkCheck(svc.Command) {
				continue
			}
			if err := RefuseSymlinkedReference(fmt.Sprintf("host service command %s", svc.Name), svc.Command); err != nil {
				return err
			}
		}
	}
	return nil
}
