// show.go — E1.9's `pix env show [NAME]`: docs/design/environments.md §8.1,
// PRD §5.10, AC-46/53/55/64. Three DISTINCT renderings share one
// resolution step (ComputeShow):
//
//   - default: a one-screen lossy summary (root, authored files, review
//     state, live-sandbox drift state) ending in the --effective pointer
//     (D7, AC-53);
//   - --path: ONLY the canonical root plus a trailing newline, nothing
//     else (AC-55);
//   - --json: the same facts, schema_version-carrying (AC-64), "none" for
//     `environment` exactly when nothing is selected (D17, AC-46);
//   - --effective: declared now, but answers ErrEffectiveNotAvailable
//     until E2.1 exists to render it (D8) — never an alternative
//     rendering in its place.
//
// Per doc.go's "no process globals" rule, ComputeShow takes cfg explicitly
// and never calls config.Load itself.
package env

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"pix/host/cli"
	"pix/host/config"
)

// ShowSchemaVersion is `env show --json`'s schema_version (AC-64).
const ShowSchemaVersion = 1

// ErrEffectiveNotAvailable is `--effective`'s answer until E2.1 ships the
// renderer it points at (D8; this unit's own scope line: "--effective is
// declared and errors 'not yet available' only until E2.1 ... it is not a
// user-selectable alternative path"). Asking for a real, declared flag is
// not a usage mistake — the feature genuinely does not exist yet — so this
// is a plain error: cli.ExitCode reports it as an OPERATIONAL failure (1),
// matching D19's "non-zero-not-2 for operational failure", never exit 2.
var ErrEffectiveNotAvailable = errors.New(
	"pix: `--effective` is not yet available (E2.1 renders the effective document); there is no alternative rendering to fall back to")

// ShowResult is what `env show` found, before any of its three renderings
// reads it. Selected is false only when no NAME was given AND no machine
// default is set (D17's `none` state); every other field is zero then.
type ShowResult struct {
	Selected bool
	Name     string
	Root     string
	// SbxenvPresent/SidecarPresent name which authored files Load actually
	// found: the required `.sbxenv.yaml` is always present on a
	// successfully loaded environment, so SbxenvPresent is carried mainly
	// for the JSON view's symmetry with SidecarPresent (pix.toml is
	// genuinely optional — Load treats its absence as fine, never an
	// error).
	SbxenvPresent  bool
	SidecarPresent bool
	// ReviewState is the full four-state answer (computeReviewState,
	// reviewstate.go) — the source of truth this ShowResult carries.
	// Accepted is kept alongside it, never derived independently, purely
	// for backward compatibility with a caller still reading the
	// pre-existing bool: it is exactly `ReviewState == ReviewAccepted`.
	ReviewState ReviewState
	Accepted    bool
	// Fingerprint is "" unless ReviewState is ReviewAccepted — docs/design/
	// environments.md D21's own invariant (workflow/reset's
	// reset_env_invariants_test.go): a re-registered, unaccepted-again
	// environment must never report a fingerprint, and ReviewChanged's
	// freshly computed digest is not what was ever actually accepted, so it
	// is never surfaced here either — only `--verbose`'s full detail (a
	// later wave) or `pix env review` itself computes and shows that one.
	Fingerprint string
	// ModelCount, MountCount and MCPCount are the concise "what NAME is"
	// facts envCmd's own help text promises ("files, models, mounts, MCP,
	// review state, drift") but the default screen never rendered: counts
	// only, derived from the parsed Sidecar/BillOfMaterials — never the
	// model ids, mount paths, or server names themselves (those stay
	// `pix env review`'s job).
	ModelCount int
	MountCount int
	MCPCount   int
}

// resolvedShowName returns the exact name `env show` resolves against: the
// explicit positional if given, otherwise cfg.Environment (the machine
// default) — and whether anything resolved at all. Never a fuzzy or
// prefix match: AC-10's exact-name rule extends to `show` exactly as it
// does to every other verb.
func resolvedShowName(cfg *config.Config, explicit string) (string, bool) {
	if explicit != "" {
		return explicit, true
	}
	return cfg.Environment, cfg.Environment != ""
}

// ComputeShow resolves name (or the machine default when explicit == "")
// to a ShowResult. An explicit name that is not registered returns a
// typed error (through Load's own ResolveEnvironment: *config.
// UnknownEnvironmentError wrapped as a cli.UsageError) exactly as every
// other exact-name lookup in this package does. No name at all, and no
// machine default either, is NOT an error (AC-46): it returns a zero
// ShowResult with Selected == false and a nil error.
func ComputeShow(cfg *config.Config, explicit string) (ShowResult, error) {
	name, ok := resolvedShowName(cfg, explicit)
	if !ok {
		return ShowResult{}, nil
	}
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		return ShowResult{}, err
	}
	loaded, err := Load(cfg, &ts.AcceptanceStore, name, nil, nil)
	if err != nil {
		return ShowResult{}, err
	}
	status, err := computeReviewState(loaded, ts, nil, nil)
	if err != nil {
		return ShowResult{}, err
	}
	modelCount := 0
	if loaded.Sidecar != nil {
		modelCount = len(loaded.Sidecar.ModelReferences())
	}
	// Fingerprint stays "" for anything but ReviewAccepted (docs/design/
	// environments.md D21; reset_env_invariants_test.go pins this in
	// workflow/reset): a re-registered, unaccepted-again environment must
	// never report a fingerprint at all, and a CHANGED environment's freshly
	// computed digest is not what was ever actually accepted — there is no
	// accepted fingerprint to name until a fresh `pix env review` writes one.
	fingerprint := ""
	if status.State == ReviewAccepted {
		fingerprint = status.Fingerprint
	}
	return ShowResult{
		Selected:       true,
		Name:           loaded.Name,
		Root:           loaded.Root,
		SbxenvPresent:  loaded.SbxenvPath != "",
		SidecarPresent: loaded.SidecarPath != "",
		ReviewState:    status.State,
		Accepted:       status.State == ReviewAccepted,
		Fingerprint:    fingerprint,
		ModelCount:     modelCount,
		MountCount:     len(status.BoM.EffectiveMounts),
		MCPCount:       len(status.BoM.MCPServers),
	}, nil
}

// NoSelectionForPathError is `--path`'s refusal when nothing resolved at
// all (ComputeShow's Selected == false): there is no root to print, so
// `--path`'s "only the canonical path, nothing else" contract (AC-55) has
// nothing honest to satisfy. It follows §5's three-part form (what failed,
// ground truth, exactly one runnable command) and, like resolve.go/load.
// go's error types, self-prefixes "pix: " — cmd/pix/env_cmd.go's envRun is
// what keeps that from ever being doubled.
func NoSelectionForPathError(cfg *config.Config) error {
	known := "none (built-in defaults)"
	if names := Known(cfg); len(names) > 0 {
		known = strings.Join(names, ", ")
	}
	return cli.UsageError{Err: fmt.Errorf(
		"pix: no environment selected; nothing to show a path for.\n     known: %s\n     select one: pix env show <name> --path",
		known)}
}

// RenderShowDefault writes the one-screen lossy summary (D7, AC-53): root,
// authored files, review state, live-sandbox drift state, ending in the
// --effective pointer line PRD §5.10 names. r.Selected == false renders
// D17's `none`/"built-in defaults" state instead — still exit 0 (AC-46),
// never an error.
func RenderShowDefault(out io.Writer, r ShowResult) {
	if !r.Selected {
		fmt.Fprintln(out, "environment: none (pix runs with its own built-in defaults)")
		fmt.Fprintln(out, "register one: pix env add <name> [path]")
		return
	}
	fmt.Fprintf(out, "environment %q\n", r.Name)
	fmt.Fprintf(out, "  root:      %s\n", r.Root)
	fmt.Fprintf(out, "  files:     %s\n", showAuthoredFiles(r))
	fmt.Fprintf(out, "  declares:  %s\n", showDeclaredCounts(r))
	fmt.Fprintf(out, "  review:    %s\n", showReviewState(r))
	// No Wave D launch cutover exists yet (E2.x): there is no honest way to
	// ask whether a live sandbox matches this environment, so this line
	// says exactly that rather than fabricating a state. See the unit's
	// scope: "live-sandbox drift state if safely observable (otherwise
	// explicit not running/unknown, never fabricated)".
	fmt.Fprintln(out, "  sandbox:   unknown (live-launch drift lands with a later wave)")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "full rendered environment: pix env show %s --effective\n", r.Name)
}

func showAuthoredFiles(r ShowResult) string {
	files := []string{".sbxenv.yaml"}
	if r.SidecarPresent {
		files = append(files, "pix.toml")
	}
	return strings.Join(files, ", ")
}

// showDeclaredCounts renders the concise "what NAME is" facts envCmd's own
// help text promises ("files, models, mounts, MCP, review state, drift")
// as counts only — never the model ids, mount paths, or MCP server names
// themselves, which stay `pix env review`'s (and its `--verbose`) job.
func showDeclaredCounts(r ShowResult) string {
	return fmt.Sprintf("%s, %s, %s",
		pluralize(r.ModelCount, "model"), pluralize(r.MountCount, "mount"), pluralize(r.MCPCount, "MCP server"))
}

// showReviewState renders the review line for every ReviewState
// computeReviewState can produce. ReviewNotRequired's exact human text
// ("nothing runs on your host") is the honest Tier0 answer — there IS no
// review to accept, so it is never worded as a variant of "unaccepted".
// ReviewChanged names `pix env review NAME` exactly once, the same next
// step ReviewUnaccepted already names, since either way that is the only
// command that changes this state.
func showReviewState(r ShowResult) string {
	switch r.ReviewState {
	case ReviewNotRequired:
		return "not-required (nothing runs on your host)"
	case ReviewAccepted:
		return fmt.Sprintf("accepted (fingerprint %s)", shortFingerprint(r.Fingerprint))
	case ReviewChanged:
		return fmt.Sprintf("changed (footprint differs from what was accepted; run: pix env review %s)", r.Name)
	default: // ReviewUnaccepted, and the zero value of a hand-built ShowResult
		return fmt.Sprintf("unaccepted (run: pix env review %s)", r.Name)
	}
}

// RenderShowPath writes ONLY the canonical root plus a trailing newline
// (AC-55) — no header, no color, nothing else. The caller (env_cmd.go)
// never calls this for a not-Selected result — see
// NoSelectionForPathError — so r.Root is always the real canonical root
// here.
func RenderShowPath(out io.Writer, r ShowResult) {
	fmt.Fprintln(out, r.Root)
}

// showJSONView is `env show --json`'s wire shape. Accepted deliberately
// carries NO `omitempty`: false is exactly as meaningful an answer as true
// (unaccepted vs accepted), and a caller reading it for its own boolean
// truthiness must never see the key vanish instead of read `false`.
type showJSONView struct {
	SchemaVersion  int         `json:"schema_version"`
	Environment    string      `json:"environment"`
	Root           string      `json:"root,omitempty"`
	SbxenvPresent  bool        `json:"sbxenv_present,omitempty"`
	SidecarPresent bool        `json:"sidecar_present,omitempty"`
	Accepted       bool        `json:"accepted"`
	ReviewState    ReviewState `json:"review_state,omitempty"`
	Fingerprint    string      `json:"fingerprint,omitempty"`
	ModelCount     int         `json:"model_count,omitempty"`
	MountCount     int         `json:"mount_count,omitempty"`
	MCPCount       int         `json:"mcp_count,omitempty"`
}

// RenderShowJSON writes `env show --json`. Environment is "none" exactly
// when r.Selected is false (D17, AC-46's `"environment":"none"`
// assertion).
func RenderShowJSON(out io.Writer, r ShowResult) error {
	name := r.Name
	if !r.Selected {
		name = "none"
	}
	view := showJSONView{
		SchemaVersion: ShowSchemaVersion, Environment: name, Root: r.Root,
		SbxenvPresent: r.SbxenvPresent, SidecarPresent: r.SidecarPresent,
		Accepted: r.Accepted, ReviewState: r.ReviewState, Fingerprint: r.Fingerprint,
		ModelCount: r.ModelCount, MountCount: r.MountCount, MCPCount: r.MCPCount,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(view)
}
