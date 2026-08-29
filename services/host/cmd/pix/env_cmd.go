// env_cmd.go — `pix env`: the dispatch skeleton (E1.9) for the seven-verb
// native-environment surface docs/design/environments.md §8 and PRD §5.10
// define (`ls add use show edit review forget`; `pix env rm` is never one
// of them). E1.9 wired the first three verbs workflow/env already had
// behind it — `ls` (workflow/env/ls.go), `show` (workflow/env/show.go),
// `review` (E1.8's workflow/env/review.go). E1.10 added `add`
// (workflow/env/add.go): register a caller-authored directory, or
// scaffold a fresh one, then ALWAYS run the same E1.8 review before
// anything commits. E1.11 added `use`, `forget`, and the `rm` pointer
// error below. E1.12 (this unit) added `edit` (workflow/env/edit.go):
// open pix.toml or .sbxenv.yaml in $VISUAL/$EDITOR, then reload and
// validate. All seven verbs (units.json's file-conflict table: "E1.9 owns
// the struct; each verb unit adds one field line + its own file. Land in
// ID order, rebase.") are now wired, in PRD §5.10 order: ls, add, use,
// show, edit, review, forget, plus the hidden `rm` pointer.
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
	"pix/host/sys"
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
'pix env rm': registering a name is not owning its files; forget only
unregisters, and it deletes nothing. All seven work now.

'edit NAME pix|sbxenv' opens pix.toml or .sbxenv.yaml in $VISUAL/$EDITOR
(exact positional enum, no flag), then reloads and validates: it never
prompts to accept a host-execution change inline: a changed footprint
prints 'pix env review NAME' as the next step instead.

An environment that runs code on your host or hands it a credential halts
at 'pix env review NAME': [y/N], default No. A non-TTY review fails closed
unless --yes.`
}

// envCmd's field ORDER is significant only in that Ls is `default:"1"`
// (bare `pix env` -> ls); it is not yet the full seven-verb table — see
// this file's package doc comment.
type envCmd struct {
	Ls     envLsCmd     `cmd:"" default:"1" help:"List registered environments. Marks the default."`
	Add    envAddCmd    `cmd:"" help:"Register a directory, or scaffold a new one, then review it."`
	Use    envUseCmd    `cmd:"" help:"Set the machine default. Refuses an unreviewed or changed environment."`
	Show   envShowCmd   `cmd:"" help:"What NAME is: files, models, mounts, MCP, review state, drift."`
	Edit   envEditCmd   `cmd:"" help:"Open pix.toml or .sbxenv.yaml in $VISUAL/$EDITOR, then validate."`
	Review envReviewCmd `cmd:"" help:"Read and accept what NAME runs on your host."`
	Forget envForgetCmd `cmd:"" help:"Unregister NAME. Never deletes the environment directory."`
	// Rm is not a verb (Seven verbs, no more — this file's own package doc
	// comment, docs/design/environments.md §8): it exists ONLY so kong's own
	// dispatch resolves `pix env rm ...` to THIS deterministic pointer error
	// rather than kong's generic "unexpected argument" — see envRmCmd's own
	// doc comment. `hidden:""` is what keeps it off every help listing
	// (models_cmd.go's Status field is the same idiom) while leaving it
	// fully dispatchable; help_test.go's exact-seven-verb assertions are what
	// prove `hidden` actually holds that line.
	Rm envRmCmd `cmd:"" hidden:""`
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

// ── add ──────────────────────────────────────────────────────────────────

// envAddCmd is `pix env add NAME [PATH]` (E1.10, docs/design/
// environments.md §8.1, D10). Path optional (empty) means scaffold; any
// other value means register that exact directory. Verbose/Yes are
// forwarded to the SAME E1.8 review Add always ends with — review's own
// `--verbose`/`--yes` (envReviewCmd, above), not a second, independent
// trust gate `add` invents for itself.
type envAddCmd struct {
	Name    string `arg:"" help:"Exact environment name."`
	Path    string `arg:"" optional:"" help:"Canonical local directory (omit to scaffold a new one)."`
	Verbose bool   `help:"Full argv and content digests for every host command/service (forwarded to review)."`
	Yes     bool   `help:"Accept the host-execution bill without an interactive prompt (forwarded to review)."`
}

func (c *envAddCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	// nil LookPath below defaults (inside Load) to the real exec.LookPath —
	// see envReviewCmd.Run's identical nil-effective-mounts note for why
	// there is no separate, independently-suppliable workspace list here
	// either.
	_, err = env.Add(cfg, c.Name, c.Path, env.AddOptions{
		Verbose: c.Verbose, Yes: c.Yes, TTY: d.Interactive, In: d.In, Out: d.Out,
	})
	return envRun(d, err)
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
		// Three-part (C6): the failure statement, the valid modes as DATA
		// (never three separate command lines a reader could mistake for
		// three independent fixes), and exactly ONE runnable retry — the
		// plain `env show NAME` form, since it is the one invocation that is
		// always valid regardless of which conflicting combination was
		// given. Routed through envRun (rather than returned directly, as
		// this used to) so its self-prefixed "pix: " is never doubled by
		// dispatch's own generic `"pix: %v"` printer — the same reason every
		// other typed refusal in this package goes through envRun.
		// An empty NAME is a genuinely omitted positional, never a typed
		// value to echo back — `<name>` is the placeholder the caller fills
		// in, the same convention config's UnknownEnvironmentError already
		// establishes for a rejected name (never echo a typo back as the
		// "fix"). A NON-empty NAME is exactly what the caller typed and IS
		// the correct retry argument, so it is shell-quoted (sys.ShellQuote)
		// rather than replaced — this refusal is about the conflicting
		// FLAGS, never about NAME's own validity.
		name := "<name>"
		if c.Name != "" {
			name = sys.ShellQuote(c.Name)
		}
		return envRun(d, cli.Usagef(
			"pix: env show: --path, --json and --effective are mutually exclusive; pick exactly one.\n"+
				"     valid: --path, --json, --effective\n"+
				"     retry: pix env show %s",
			name))
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

// ── edit ─────────────────────────────────────────────────────────────────

// envEditCmd's Target is an exact positional enum ("pix" or "sbxenv"),
// never a flag — there is deliberately no `--sbxenv` flag anywhere in this
// dispatch. It is validated by hand in workflow/env.Edit, the same
// `optional:""` + hand-checked-switch idiom configPathCmd's Kind already
// uses, rather than kong's own `enum` tag: an unrecognized or omitted
// token needs THIS package's own two-explicit-forms message (env.Edit's
// editTargetUsageError), not kong's generic "expected one of" text.
type envEditCmd struct {
	Name   string `arg:"" help:"Exact environment name."`
	Target string `arg:"" optional:"" help:"'pix' for pix.toml, 'sbxenv' for .sbxenv.yaml. Omit on a TTY to be asked."`
}

func (c *envEditCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	_, err = env.Edit(cfg, d.Sys, c.Name, c.Target, env.EditOptions{
		TTY: d.Interactive, In: d.In, Out: d.Out,
	})
	return envRun(d, err)
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
	// No composition-owned EffectiveMounts yet: nil is the intrinsic
	// pre-E2 set, derived deterministically and CWD-independently —
	// Load itself adds the environment's own root, read-only, for skill
	// validation (load.go's own doc comment), and NEVER a project/current-
	// cwd mount, since neither this dispatcher nor Load ever consults
	// os.Getwd. There is no separate `workspaces` argument here to diverge
	// from this value — env.Review takes exactly one typed effective
	// workspace set and feeds it identically to Load and ComputeBoM (see
	// env.Review's own doc comment). A nil lookPath defaults to the real
	// exec.LookPath.
	//
	// Future E2's launch composition is FORCED, by this same signature, to
	// supply a real EffectiveMounts value here instead of nil — the
	// compile-time seam is env.Review's parameter type itself
	// (EffectiveMounts, not a bare []string a caller could satisfy with an
	// unrelated slice): E2 cannot add a writable project mount without
	// constructing one.
	_, err = env.Review(cfg, c.Name, nil, nil, env.ReviewOptions{
		Verbose: c.Verbose, Yes: c.Yes, TTY: d.Interactive, In: d.In, Out: d.Out,
	})
	return envRun(d, err)
}

// ── use ──────────────────────────────────────────────────────────────────

type envUseCmd struct {
	Name string `arg:"" help:"Exact environment name."`
}

// Run delegates env.Use's ONE gated mutation (cfg.Environment) whole:
// validation, the mutation, AND the save all happen inside env.Use, under
// the env-registry lock against a fresh under-lock config reload
// (workflow/env/commit.go) — never a cfg.Save() here on the Deps-cached
// copy, which may be stale against a concurrent pix process's commit.
// There is no lookPath override here for the same reason `env review`/
// `env show` pass nil: a real symlink/PATH check runs against the actual
// filesystem in production, and a test reaches env.Use directly to inject
// one. Use never launches anything — its whole effect ends at that one
// locked commit.
func (c *envUseCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	if err := env.Use(cfg, c.Name, nil); err != nil {
		return envRun(d, err)
	}
	fmt.Fprintf(d.Out, "pix: environment %q is now the default.\n", c.Name)
	return nil
}

// ── forget ───────────────────────────────────────────────────────────────

type envForgetCmd struct {
	Name string `arg:"" help:"Exact environment name."`
}

// Run delegates env.Forget's ONE gated mutation (unregistering c.Name)
// whole: refusals, the mutation, AND the save all happen inside env.Forget,
// under the env-registry lock against a fresh under-lock config reload
// (workflow/env/commit.go) — never a cfg.Save() here on the Deps-cached
// copy, which may be stale against a concurrent pix process's commit. No
// holder probe is wired here yet: no launch cutover exists that could make
// one true (env.NoLiveHolders' own doc comment), so nil defaults to it —
// the seam is real even though nothing populates it today.
func (c *envForgetCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	root, err := env.Forget(cfg, c.Name, nil)
	if err != nil {
		return envRun(d, err)
	}
	fmt.Fprintf(d.Out, "pix: environment %q unregistered. Source untouched: %s\n", c.Name, root)
	return nil
}

// ── rm: the pointer error, not a working alias ──────────────────────────

// envRmPointerError is docs/design/environments.md §8.1's exact rm refusal
// (PRD §5.5), verbatim: it names the three distinct things a user might
// actually want removed, so the wrong one is never removed by accident.
const envRmPointerError = "pix: `pix env rm` does not exist. Registering a name is not owning the files.\n" +
	"     pix env forget home     unregister the name (deletes no files)\n" +
	"     pix rm pix-repo-home    remove the sandbox\n" +
	"     rm -rf <path>           delete the source yourself; pix will not\n"

// envRmCmd exists only to give kong a deterministic node to dispatch `pix
// env rm ...` TO — see this struct field's own comment on envCmd. Args
// swallows anything after `rm` (a name, flags, garbage) so no shape of
// invocation ever falls through to kong's own "unexpected argument";
// every one of them lands here and gets the SAME pointer error. Run reads
// no config, resolves no name, and touches no file: this command performs
// ZERO mutation of any kind before returning its fixed exit 2, exactly
// because it never earns the chance to — there is nothing here it could
// even ask permission for.
type envRmCmd struct {
	Args []string `arg:"" optional:"" passthrough:"" hidden:""`
}

func (c *envRmCmd) Run(d *cli.Deps) error {
	fmt.Fprint(d.Err, envRmPointerError)
	return cli.SilentError{Code: 2}
}
