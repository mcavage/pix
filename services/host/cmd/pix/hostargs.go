package main

import (
	"fmt"
	"pix/host/cli"
	"strings"
)

// hostOpts carries the resolved `pix host [DIR]` launch options. Like
// runOpts it is side-effect-free: parseHostArgs never touches the filesystem
// beyond the workspace existence check, and never reads config.
type hostOpts struct {
	Workspace   string   // positional DIR (default ".")
	Model       string   // --model M: active pi model (passed through to pi)
	Passthrough []string // args after `--`, handed straight to pi
}

// hostSubcommands are the words `pix host` treats as subcommands, NOT as a
// workspace DIR. `host` has its OWN parser (separate from parseRunArgs) exactly
// so a bare `setup` is never misread as a directory: `pix host setup`
// provisions, `pix host ./setup` launches in a dir literally named setup.
var hostSubcommands = map[string]bool{"setup": true}

// parseHostArgs parses the `host` verb's argv. It returns the subcommand name
// ("" = launch, "setup" = provision) plus the launch options. A leading
// -h/--help returns cli.ErrHelpRequested (usage to stdout, exit 0).
func parseHostArgs(argv []string) (sub string, o hostOpts, err error) {
	if cli.WantsHelp(argv) {
		return "", hostOpts{}, cli.ErrHelpRequested
	}
	o = hostOpts{Workspace: "."}

	// Subcommand: only recognized as the FIRST token, so a later positional
	// named "setup" is still a (rejected, non-directory) workspace, not a verb.
	if len(argv) > 0 && hostSubcommands[argv[0]] {
		sub = argv[0]
		if len(argv) > 1 {
			return sub, o, fmt.Errorf("host %s: unexpected argument %q", sub, argv[1])
		}
		return sub, o, nil
	}

	// Split off the `--` passthrough first (mirrors parseRunArgs).
	pre := argv
	for i, a := range argv {
		if a == "--" {
			pre = argv[:i]
			o.Passthrough = append([]string(nil), argv[i+1:]...)
			break
		}
	}
	if perr := checkHostPassthrough(o.Passthrough); perr != nil {
		return "", o, perr
	}

	valueOf := func(a string, i *int) (string, error) {
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			return a[eq+1:], nil
		}
		if *i+1 >= len(pre) {
			return "", fmt.Errorf("flag %s needs a value", a)
		}
		*i++
		return pre[*i], nil
	}

	wsSet := false
	for i := 0; i < len(pre); i++ {
		a := pre[i]
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
		}
		switch {
		case name == "--model":
			v, verr := valueOf(a, &i)
			if verr != nil {
				return "", o, verr
			}
			o.Model = v
		case strings.HasPrefix(a, "-"):
			return "", o, fmt.Errorf("unknown flag %q", a)
		default:
			if wsSet {
				return "", o, fmt.Errorf("unexpected extra argument %q (only one DIR allowed; use -- for pi args)", a)
			}
			o.Workspace = a
			wsSet = true
		}
	}
	if err := validateRunWorkspace(o.Workspace); err != nil {
		return "", o, err
	}
	return "", o, nil
}

// checkHostPassthrough refuses passthrough flags that would displace the
// host-guard extension. The launcher appends o.Passthrough to pi's argv AFTER
// `-e <host-guard.ts>`, so `pix host -- --no-extensions` (or an
// --extensions/-e override) would launch pi UNGUARDED — the exact thing host
// mode promises never happens. Both `--flag value` and `--flag=value`
// spellings are matched. Phase-1 security blocker: fail the parse, never warn.
func checkHostPassthrough(args []string) error {
	for _, a := range args {
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
		}
		switch name {
		case "--no-extensions", "--extensions", "-e", "--extension":
			return fmt.Errorf("passthrough flag %q would disable or displace the host-guard extension — host mode never launches unguarded (drop it, or use `pix run` for an unrestricted sandbox)", a)
		}
	}
	return nil
}

const hostUsage = `usage: pix host [DIR] [--model M] [-- pi-args...]
       pix host setup

Run pi DIRECTLY ON THIS MACHINE — no sandbox, no network fence, real
credentials. This is a narrow, deliberate escape hatch for working on pix
itself (self-development); it is NEVER a fallback when sbx is missing. Its
guardrails (the host-guard extension, workspace checks) reduce accidents; they
are NOT a security boundary. For anything you wouldn't hand a shell to, use
` + "`pix run`" + `.

Gated OFF by default. One command provisions AND enables it (the gate stays off
unless provisioning succeeds):
  pix host setup

subcommands:
  setup            provision the host agent dir ($XDG_STATE_HOME/pix/
                   host-agent): symlinks {skills,agents,extensions,themes} +
                   {capabilities,routing,keybindings}.json to your pix
                   checkout (mcp.json is skipped — the sbx gateway does not
                   exist on the host), a host-specific settings.json, a
                   sessions/ dir, and the curated pi extension packages.
                   (A dir literally named setup: use ./setup.)

flags:
  --model M        active pi model (passed through to pi)

Credentials: if ` + "`hostmode.env`" + ` exists in the pix config dir (op:// refs,
see ` + "`pix config path`" + `), the launch wraps pi in ` + "`op run --env-file`" + ` so
keys are resolved just-in-time and never persisted. Without it, host mode is
Ollama-only (local models, no cloud key) — also valid.

Naming: pix host (this verb, the agent ON the host) is distinct from
pix-host (the Go daemon binary) and pix serve (the host services).

DIR defaults to the current directory. Everything after -- is passed to pi,
EXCEPT flags that would disable or displace the host-guard extension
(--no-extensions, --extensions, -e/--extension) — those are refused, because
host mode never launches unguarded.
`
