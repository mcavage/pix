// setup_cmd.go — `pix setup` as a typed root child (docs/design/
// pix-v2-surface.md §3.6, pix-v2-architecture.md §12). It is the whole of
// the v2 setup surface this launcher owns: idempotently initialize
// PIX_HOME, record the installed release manifest, reconcile the one named
// pix-memory container, and register/verify its reserved sbx MCP name —
// using real adapters (os/exec git, real Docker, a real HTTP prober, a real
// `sbx mcp` registrar). Nothing else runs here: no pack, no MCP allowlist,
// no model-provider interview, and no sandbox handoff. `pix doctor` reports
// the rest, and `pix run` starts a session.
package main

import (
	"fmt"
	"io"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/sandbox"
	"pix/host/sys"
	"pix/host/workflow/launch"
	"pix/host/workflow/provision"
)

// setupOnboardingKickoff is the first message a first-launch handoff would
// hand the agent: deliberately short and human, because the `onboarding`
// skill owns the actual flow.
const setupOnboardingKickoff = "I just ran pix setup. Give me the upfront guide and help me get started."

// runSetupHandoff is the pure, fail-closed decision for whether a caller may
// launch a sandbox after a host-side step: an unprobeable sbx (launch.SbxUnknown)
// NEVER launches, since doing so could re-attach a live session and replay a
// kickoff message into it. It is not wired into `pix setup` (setup performs no
// handoff — see setupCmd.Run), but the safety property it proves
// (TestSetupHandoff_HangingSbxFailsClosed) is retained here as the one shared
// place that decision is made correctly, for a future caller that needs it.
func runSetupHandoff(dir, name string, state sandbox.State, out io.Writer, runFn func([]string) error) error {
	kickoffArgs := func() []string {
		args := []string{}
		if dir != "." {
			args = append(args, dir)
		}
		return append(args, "--", launch.GeneratedInputMarker+setupOnboardingKickoff)
	}
	dirArg := ""
	if dir != "." {
		dirArg = " " + sys.ShellQuote(dir)
	}

	switch state {
	case launch.SbxUnknown:
		// FAIL CLOSED: launching would re-attach a live session and replay the kickoff
		// into it. The host phase completed, so a retry is cheap.
		which := fmt.Sprintf("sandbox %q", name)
		if name == "" {
			which = fmt.Sprintf("the sandbox for %s", dir)
		}
		return fmt.Errorf("cannot determine the state of %s (`sbx ls` failed or sbx is unavailable). Host setup completed; install or fix sbx and retry with: pix setup%s", which, dirArg)
	case launch.SbxRunning, launch.SbxStopped:
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "Host configuration reconciled. Existing sandbox %q was left alone.\n", name)
		fmt.Fprintln(out, "Attaching keeps the sandbox exactly as it was created (its pack, MCP")
		fmt.Fprintln(out, "servers, and skills were attached at create time). To pick up current")
		fmt.Fprintln(out, "settings instead, remove it first: removal is proof-gated, so it refuses")
		fmt.Fprintln(out, "while another shell is still attached. Choose one:")
		fmt.Fprintf(out, "  pix run%s                 # attach as-is\n", dirArg)
		fmt.Fprintf(out, "  pix rm %s && pix setup%s  # recreate with current settings + get the tour\n", sys.ShellQuote(name), dirArg)
		return nil
	}

	// launch.SbxAbsent (positively confirmed): normal first launch.
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Launching Pix: the agent will take it from here.")
	return runFn(kickoffArgs())
}

func (c *setupCmd) Help() string { return provision.Description }

// setupCmd provisions PIX_HOME. It takes no workspace argument and performs
// no agent handoff: `pix run` is the only thing that starts a sandbox.
type setupCmd struct {
	Verbose bool `help:"Show the pix-memory container and MCP registration detail, not just the summary."`
}

func (c *setupCmd) Run(d *cli.Deps) error {
	home, err := pixhome.Resolve()
	if err != nil {
		return err
	}
	// Generate (or reuse) the pix-memory bearer token BEFORE the container
	// spec is built: homeContainerSpec's EnvFile names this same path, and
	// `docker create --env-file <path>` fails outright if that file does not
	// exist yet (security re-review HIGH finding — never a literal `-e`
	// argument, which would leak the value into this host's own process
	// listing of the `docker create` invocation).
	token, terr := container.EnsureMemoryAuthToken(home)
	if terr != nil {
		return fmt.Errorf("pix-memory auth token: %w", terr)
	}
	spec := homeContainerSpec(home)
	res, err := provision.Setup(provision.Deps{
		Home:            home,
		ContainerRunner: container.DefaultRunner,
		Prober:          httpProber{},
		ContainerSpec:   spec,
		ConfirmReplace:  confirmContainerReplace(d),
		MCP:             sbxMemoryRegistrar{},
		MemoryAuthToken: token,
	})
	if err != nil {
		return err
	}
	renderSetupResult(d, home, res, c.Verbose)
	if !res.Ready() {
		return cli.SilentError{Code: 1}
	}
	return nil
}

// renderSetupResult prints what Setup actually did. Always shown, not only
// under --verbose: setup is a rerunnable idempotent step and a silent
// success gives the user nothing to confirm against.
func renderSetupResult(d *cli.Deps, home pixhome.Paths, res provision.Result, verbose bool) {
	switch {
	case res.Init.CreatedHome:
		fmt.Fprintf(d.Out, "pix setup: initialized PIX_HOME at %s\n", home.Home)
	default:
		fmt.Fprintf(d.Out, "pix setup: PIX_HOME already initialized at %s\n", home.Home)
	}
	fmt.Fprintf(d.Out, "pix setup: pix-memory container: %s\n", res.Container.Action)
	switch {
	case !res.MCPRegistered:
		fmt.Fprintln(d.Out, "pix setup: pix-memory MCP registration: not attempted (no registrar wired)")
	case res.MCPMatched:
		fmt.Fprintln(d.Out, "pix setup: pix-memory MCP registration: ok")
	default:
		fmt.Fprintln(d.Out, "pix setup: pix-memory MCP registration: an existing registration under this name could not be verified to match; it was left untouched")
	}
	if verbose {
		fmt.Fprintf(d.Err, "pix setup: pix home %s: container %s\n", home.Home, res.Container.Action)
	}
	if res.Ready() {
		fmt.Fprintln(d.Out, "pix setup: ready. For the full host report: pix doctor")
		return
	}
	fmt.Fprintln(d.Out, "pix setup: not ready. Run `pix doctor` for the exact fix.")
}

// confirmContainerReplace is the exact prompt architecture §9.1 requires
// before a mismatched pix-memory container is stopped, removed, and
// recreated: show the drift, ask, default to declining on anything that
// cannot ask (non-interactive, no confirmation requested with --yes).
func confirmContainerReplace(d *cli.Deps) func(current container.Info, want container.Spec) bool {
	return func(current container.Info, want container.Spec) bool {
		fmt.Fprintf(d.Err, "pix setup: the running pix-memory container does not match the pinned release:\n")
		fmt.Fprintf(d.Err, "  running: %s (fingerprint %s)\n", current.Image, current.Fingerprint())
		fmt.Fprintf(d.Err, "  wanted:  %s (fingerprint %s)\n", want.Image, want.Fingerprint())
		if !d.Interactive {
			fmt.Fprintln(d.Err, "pix setup: refusing to replace it on a non-interactive terminal; rerun interactively or remove it yourself: docker rm -f pix-memory")
			return false
		}
		fmt.Fprint(d.Err, "Replace it? Its /data volume is preserved either way. [y/N] ")
		var line string
		fmt.Fscanln(d.In, &line)
		return line == "y" || line == "Y"
	}
}

// init supplies the one composition provision declares but cannot perform:
// registering MCP servers with credentials resolved over secret. This wires
// unconditionally at process start (not only when `pix setup` runs) because
// `pix run`'s onboarding reconcile (provision.ReconcileOnboarding) needs the
// same registrar.
func init() {
	provision.Injected = provision.Composition{
		Register: registerServers,
	}
}
