// loadhome.go — the v2, pixhome-based half of E1.7's composition (see
// load.go's own doc comment for the full contract this mirrors). Load
// resolves a name through cfg.Environments, the v1 registry docs/design/
// environments.md §5.3 describes; that registry has no place in the v2
// surface (docs/design/pix-v2-surface.md §3.4: "Pix does not maintain an
// environment registration database"), so a v2 caller resolves a name to a
// Selected value with ResolveIn/SelectIn (home.go) FIRST, and hands the
// already-resolved, already-symlink-and-mode-checked result to LoadHome
// here.
//
// LoadHome does not re-derive anything ResolveIn already proved (the exact
// name lookup, the one-hop symlink resolution, the unsafe-mode refusal): it
// picks up from a trustworthy Root and does exactly what Load does from
// that point on — required-native-file parse, containment (twice, for the
// same reason Load's own doc comment gives), sidecar parse, tree
// composition, sidecar skill-workspace validation, and local-reference
// symlink refusal — plus ONE v2-only check Load's caller never needed: an
// authored server may not collide with either of Pix's reserved built-in
// MCP names (envinfo.RefuseReservedMCPNames). Load's caller never composes
// the built-ins Pix itself adds after parsing (they are added later, by the
// effective-document composer), so nothing before this point could ever
// see them; nothing on this path may accept an authored name that would
// otherwise silently shadow one.
package env

import (
	"fmt"
	"os"

	"pix/host/cli"
	"pix/host/envinfo"
	"pix/host/pixhome"
)

// pixManagedVars resolves the Pix-defined interpolation variables for THIS
// host's home. A home Pix cannot resolve yields no variables at all, which
// leaves `${PIX_HOME}` exactly as undefined as any other unset name rather
// than defining it as the empty string — the fail-closed direction, and the
// one that produces the refusal a person can act on instead of a container
// mounting the filesystem root.
func pixManagedVars() map[string]string {
	home, err := pixhome.Dir()
	if err != nil {
		return nil
	}
	return envinfo.PixManagedVars(home)
}

// LoadHome composes an *Environment from an already-resolved pixhome
// Selected value (ResolveIn/SelectIn). effective is the SAME typed
// caller-supplied writable-mount set Load's own effective parameter is
// (bom.go's EffectiveMounts): only the launch composing a real effective
// document ever has non-nil entries to pass here today. lookPath is the
// same PATH-lookup seam refuseLocalReferenceSymlinks already takes; nil
// defaults to exec.LookPath (ResolveLocalCommand's own contract).
func LoadHome(sel Selected, effective EffectiveMounts, lookPath func(string) (string, error)) (*Environment, error) {
	root := sel.Root

	sbxenvPath := sel.SbxEnvPath()
	if _, statErr := os.Stat(sbxenvPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, cli.UsageError{Err: &MissingRequiredFileError{Name: sel.Name, Root: root, File: ".sbxenv.yaml"}}
		}
		return nil, fmt.Errorf("pix: environment %s: %w", sel.Name, statErr)
	}
	doc, err := envinfo.Parse(sbxenvPath)
	if err != nil {
		return nil, cli.UsageError{Err: err}
	}

	// Refuse an authored collision with either reserved built-in name
	// BEFORE anything else composes further: the effective compiler's own
	// pix-memory/pix-session facts have not been added yet at this point
	// (they never are, this early), so any hit here is unambiguously
	// something the AUTHOR wrote.
	if err := envinfo.RefuseReservedMCPNames(doc); err != nil {
		return nil, cli.UsageError{Err: err}
	}

	// Containment runs twice for the reason load.go's Load documents: once
	// against the caller-supplied effective mounts (a launch's real runtime
	// set), once against what the document itself declares.
	if err := RefuseContainment(root, writableWorkspacePaths(effective)); err != nil {
		return nil, err
	}
	authored := AuthoredMounts(doc)
	if err := RefuseContainment(root, writableWorkspacePaths(authored)); err != nil {
		return nil, err
	}

	sidecarPath := sel.SidecarPath()
	var sidecar *envinfo.Sidecar
	switch _, statErr := os.Stat(sidecarPath); {
	case statErr == nil:
		sidecar, err = envinfo.ParseSidecar(sidecarPath)
		if err != nil {
			return nil, cli.UsageError{Err: err}
		}
	case os.IsNotExist(statErr):
		sidecarPath = "" // optional and absent (docs/design/environments.md §5.2)
	default:
		return nil, fmt.Errorf("pix: environment %s: %w", sel.Name, statErr)
	}

	merged, err := envinfo.Merge(doc)
	if err != nil {
		return nil, cli.UsageError{Err: err}
	}
	tree, err := envinfo.BuildTree(merged)
	if err != nil {
		return nil, cli.UsageError{Err: err}
	}

	// A bare `${VAR}` with no default and no value on this host is refused
	// rather than silently interpolated as empty (PRD: "Undefined bare
	// `${VAR}` interpolation is refused rather than accepted as empty") —
	// v1 resolved it to "", which produced an environment nobody authored
	// (an empty registry host, an empty MCP URL). os.LookupEnv is the real
	// host-environment probe; this runs on every load, so `pix env show`,
	// `--effective`, and a real launch (which all funnel through LoadHome)
	// refuse identically rather than only the launch path catching it.
	//
	// Pix's OWN variables (envinfo.PixManagedVars — `${PIX_HOME}`) are
	// layered over that probe: Pix resolves its home whether or not a shell
	// ever exported it, so an environment that names it is never refused for
	// a variable the launcher itself always knows.
	if err := envinfo.RefuseUndefinedInterpolations(tree, envinfo.LookupPixManaged(pixManagedVars(), os.LookupEnv)); err != nil {
		return nil, cli.UsageError{Err: err}
	}

	if sidecar != nil {
		skillWorkspaces := append(workspacePaths(effective), workspacePaths(authored)...)
		skillWorkspaces = append(skillWorkspaces, root)
		if err := envinfo.ValidateSkillWorkspaces(sidecarPath, sidecar, skillWorkspaces); err != nil {
			return nil, cli.UsageError{Err: err}
		}
	}

	if err := refuseLocalReferenceSymlinks(doc, sidecar, tree, root, lookPath); err != nil {
		return nil, err
	}

	return &Environment{
		Name:        sel.Name,
		Root:        root,
		SbxenvPath:  sbxenvPath,
		Document:    doc,
		Tree:        tree,
		SidecarPath: sidecarPath,
		Sidecar:     sidecar,
		Subject:     Subject(root),
	}, nil
}
