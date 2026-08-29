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
	"pix: `--effective` is not yet available; there is no alternative rendering to fall back to")

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
	Accepted       bool
	// Fingerprint is "" when Accepted is false.
	Fingerprint string
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
	rec, _ := ts.Get(loaded.Subject)
	return ShowResult{
		Selected:       true,
		Name:           loaded.Name,
		Root:           loaded.Root,
		SbxenvPresent:  loaded.SbxenvPath != "",
		SidecarPresent: loaded.SidecarPath != "",
		Accepted:       loaded.Accepted,
		Fingerprint:    rec.Fingerprint,
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

func showReviewState(r ShowResult) string {
	if !r.Accepted {
		return fmt.Sprintf("unaccepted (run: pix env review %s)", r.Name)
	}
	return fmt.Sprintf("accepted (fingerprint %s)", shortFingerprint(r.Fingerprint))
}

// RenderShowPath writes ONLY the canonical root plus a trailing newline
// (AC-55) — no header, no color, nothing else. The caller (env_cmd.go)
// never calls this for a not-Selected result — see
// NoSelectionForPathError — so r.Root is always the real canonical root
// here.
func RenderShowPath(out io.Writer, r ShowResult) {
	fmt.Fprintln(out, r.Root)
}

// showJSONView is `env show --json`'s wire shape.
type showJSONView struct {
	SchemaVersion  int    `json:"schema_version"`
	Environment    string `json:"environment"`
	Root           string `json:"root,omitempty"`
	SbxenvPresent  bool   `json:"sbxenv_present,omitempty"`
	SidecarPresent bool   `json:"sidecar_present,omitempty"`
	Accepted       bool   `json:"accepted,omitempty"`
	Fingerprint    string `json:"fingerprint,omitempty"`
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
		Accepted: r.Accepted, Fingerprint: r.Fingerprint,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(view)
}
