// env_cmd.go — `pix env`: the dispatch skeleton (E1.9) for the seven-verb
// native-environment surface docs/design/environments.md §8 and PRD §5.10
// define (`ls add use show edit review forget`; `pix env rm` is never one
// of them). This unit wires exactly the three verbs workflow/env already
// has behind it — `ls` (workflow/env/ls.go), `show` (workflow/env/show.go),
// `review` (E1.8's workflow/env/review.go) — as the FIRST fields of envCmd.
// Every later verb unit (E1.10 `add`, E1.11 `use`/`forget`/the `rm`
// pointer, E1.12 `edit`) adds its own ONE field line here and lands in ID
// order (units.json's file-conflict table: "E1.9 owns the struct; each
// verb unit adds one field line + its own file. Land in ID order,
// rebase.").
//
// There is deliberately NO placeholder field for a verb that does not
// exist yet: an unregistered subcommand answers with kong's own generic
// "unexpected argument" rather than a hand-authored stub that could be
// mistaken for a real, if incomplete, verb — nothing here advertises a
// success path a later unit has not built.
//
// Bare `pix env` is `env ls` (Ls's kong `default` tag — the same idiom
// pack_cmd.go's packCmd already uses for `pix pack` -> `pack ls`).
package main

import (
	"fmt"
	"strings"

	"pix/host/cli"
	"pix/host/workflow/env"
)

// envRun is env's analog of pack_cmd.go's packRun: the ONE place a
// workflow/env error becomes a printed line and an exit code. It exists
// because workflow/env's own error types (resolve.go/load.go's
// UnknownEnvironmentError, ContainmentError, SymlinkError,
// MissingRequiredFileError, NoncanonicalRootError; show.go's
// NoSelectionForPathError; show.go's ErrEffectiveNotAvailable) already
// self-prefix "pix: " — the root's own generic
// `fmt.Fprintf(d.Err, "pix: %v\n", err)` would otherwise print
// "pix: pix: ...". Printing here and returning the error SILENT (so the
// root never touches it again) is correct either way: an
// already-prefixed message prints verbatim, and an unprefixed one (a
// plain operational error surfacing from, say, a corrupt trust store)
// gets exactly one prefix added rather than zero.
func envRun(d *cli.Deps, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "pix: ") {
		msg = "pix: " + msg
	}
	fmt.Fprintln(d.Err, msg)
	return cli.SilentError{Code: cli.ExitCode(err)}
}

// envCmd is a child of the kong root; bare `pix env` is `env ls`.
func (c *envCmd) Help() string {
	return `A named launch context: what runs, what mounts, which models, which MCP
servers. See docs/design/environments.md.

Seven verbs: ls, add, use, show, edit, review, forget. There is no
'pix env rm' — registering a name is not owning its files; forget only
unregisters, and it deletes nothing. 'add'/'use'/'edit'/'forget' land in
later units; ls, show and review work now.

An environment that runs code on your host or hands it a credential halts
at 'pix env review NAME': [y/N], default No. A non-TTY review fails closed
unless --yes.`
}

// envCmd's field ORDER is significant only in that Ls is `default:"1"`
// (bare `pix env` -> ls); it is not yet the full seven-verb table — see
// this file's package doc comment.
type envCmd struct {
	Ls     envLsCmd     `cmd:"" default:"1" help:"List registered environments. Marks the default."`
	Show   envShowCmd   `cmd:"" help:"What NAME is: files, models, mounts, MCP, review state, drift."`
	Review envReviewCmd `cmd:"" help:"Read and accept what NAME runs on your host."`
}

// ── ls ───────────────────────────────────────────────────────────────────

type envLsCmd struct {
	JSON bool `help:"Emit machine-readable JSON (schema_version)."`
}

func (c *envLsCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	r, err := env.ComputeLs(cfg)
	if err != nil {
		return envRun(d, err)
	}
	if c.JSON {
		return env.RenderLsJSON(d.Out, r)
	}
	env.RenderLs(d.Out, r)
	return nil
}

// ── show ─────────────────────────────────────────────────────────────────

// envShowCmd's three flags are mutually exclusive (enforced in Run, before
// any config load): each is a COMPLETE, distinct answer to "what is NAME"
// (a lossy summary, a bare path, the byte-identical effective document),
// never a modifier on one another.
type envShowCmd struct {
	Name      string `arg:"" optional:"" help:"Exact environment name (default: the selected environment)."`
	Path      bool   `help:"Print only the canonical root, plus a trailing newline."`
	JSON      bool   `help:"Emit machine-readable JSON (schema_version)."`
	Effective bool   `help:"Render the byte-identical document sbx would receive (not yet available)."`
}

func (c *envShowCmd) Run(d *cli.Deps) error {
	if countTrue(c.Path, c.JSON, c.Effective) > 1 {
		return cli.Usagef("env show: --path, --json and --effective are mutually exclusive; choose one")
	}
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	if c.Effective {
		// Declared now, but a caller-selectable alternative path does not
		// exist until E2.1 builds the effective renderer (D8) — never any
		// other rendering in its place.
		return envRun(d, env.ErrEffectiveNotAvailable)
	}
	r, err := env.ComputeShow(cfg, c.Name)
	if err != nil {
		return envRun(d, err)
	}
	if c.Path {
		if !r.Selected {
			return envRun(d, env.NoSelectionForPathError(cfg))
		}
		env.RenderShowPath(d.Out, r)
		return nil
	}
	if c.JSON {
		return env.RenderShowJSON(d.Out, r)
	}
	env.RenderShowDefault(d.Out, r)
	return nil
}

// countTrue is the shared arity check behind envShowCmd's mutual-exclusion
// gate — a tiny local helper rather than a cli package export, since no
// other verb needs it yet.
func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// ── review ───────────────────────────────────────────────────────────────

type envReviewCmd struct {
	Name    string `arg:"" help:"Exact environment name."`
	Verbose bool   `help:"Full argv and content digests for every host command/service."`
	Yes     bool   `help:"Accept the host-execution bill without an interactive prompt."`
}

func (c *envReviewCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	// No composition-owned effective mounts yet (nil workspaces, nil
	// EffectiveMounts — "effective mounts (none until composition owns
	// them)") and a nil lookPath (env.Review defaults it to the real
	// exec.LookPath): the same "not composed yet" state review.go's own
	// package doc comment already describes.
	_, err = env.Review(cfg, c.Name, nil, nil, nil, env.ReviewOptions{
		Verbose: c.Verbose, Yes: c.Yes, TTY: d.Interactive, In: d.In, Out: d.Out,
	})
	return envRun(d, err)
}
