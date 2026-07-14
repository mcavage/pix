package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// runRun implements bare `pi-stack [DIR]` and `pi-stack run ...`. It reads the
// config, resolves the run options (including a repo checkout for --dev),
// composes the sbx argv, and execs it with stdio inherited.
//
// The default path forwards NO credential bearer into the sandbox: Google
// Workspace now rides the sbx gateway as the host-side `gog` MCP server (the
// `slack` pattern), authed entirely on the host — there is nothing to inject.
func runRun(argv []string) {
	o, err := parseRunArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack run: %v\n\n", err)
		fmt.Fprint(os.Stderr, runUsage)
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack run: loading config: %v\n", err)
		os.Exit(1)
	}

	// Kit selection. A CLEAN released version (e.g. "0.0.16") pins the matching
	// git tag; anything else — an unstamped "dev" build, a "0.0.16+local" local
	// build, or non-semver — is UNRELEASED, its tag does not exist, so we never
	// pin v<version>. --dev forces the local checkout kit; an unreleased build
	// uses it too when a checkout is resolvable, else falls back to #ref=main.
	released := isReleased(version)
	kitOverride := len(o.Kits) > 0

	if o.Dev {
		// --dev needs a resolvable repo checkout; fail loud otherwise.
		root, err := resolveRepoRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack run --dev: %v\n", err)
			os.Exit(1)
		}
		o.DevRoot = root
		o.LocalKit = filepath.Join(root, "pi-kit")
		o.LocalImageTag = readLocalImageTag(root)
	} else if !released && !kitOverride {
		if root, err := resolveRepoRoot(); err == nil {
			o.LocalKit = filepath.Join(root, "pi-kit")
			o.LocalImageTag = readLocalImageTag(root)
			note := ""
			if o.LocalImageTag != "" {
				note = " (local image :" + o.LocalImageTag + ")"
			}
			fmt.Fprintf(os.Stderr, "pi-stack: unreleased build %q — using local checkout kit %s%s\n", version, o.LocalKit, note)
		} else {
			fmt.Fprintf(os.Stderr, "pi-stack: unreleased build %q and no pi-stack checkout found — "+
				"kit tracks #ref=main (may not match this binary). Use `pi-stack run --dev` from a "+
				"checkout or `pi-stack run --kit <path-or-git-url>` to override.\n", version)
		}
	}

	// --mcp is only a valid sbx flag when the gateway is enabled (SBX_MCP_URL set,
	// like the Makefile gates it). Set MCPEnabled from the env so buildSbxArgs stays
	// pure, and warn (once) when MCP servers are configured but the gateway is off,
	// rather than letting sbx bail with `unknown flag: --mcp`.
	o.MCPEnabled = strings.TrimSpace(os.Getenv("SBX_MCP_URL")) != ""
	if !o.MCPEnabled {
		configured := append(append([]string(nil), cfg.MCP...), o.MCP...)
		if msg := mcpGatewayOffWarning(configured); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	}

	args := buildSbxArgs(cfg, o, version)

	if os.Getenv("PI_STACK_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "+ sbx "+strings.Join(args, " "))
	}

	cmd := exec.Command("sbx", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The default path injects no credential bearer: gog authenticates on the host
	// inside the gateway-spawned MCP server, so the sandbox never sees a Google
	// token. A future overlay credential broker would set its own bearer through
	// the retained generic seam and own that plumbing itself.
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			// If we pinned a git #ref kit and sbx bailed (classically git exit 128
			// "Remote branch not found"), the raw error is opaque — replace it with
			// an actionable note instead of leaking the git 128.
			if msg := kitResolveFailureMsg(pinnedGitKit(args)); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack run: exec sbx: %v\n", err)
		os.Exit(1)
	}
}

// parseRunArgs is a small hand-rolled parser (no cobra, no third-party flags) so
// DIR can appear before or after the flags, matching the flexibility of the old
// bin/pi-stack shell launcher. Everything after `--` is pi passthrough.
func parseRunArgs(argv []string) (runOpts, error) {
	o := runOpts{Workspace: "."}
	wsSet := false

	// Split off the `--` passthrough first.
	pre := argv
	for i, a := range argv {
		if a == "--" {
			pre = argv[:i]
			o.Passthrough = append([]string(nil), argv[i+1:]...)
			break
		}
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

	for i := 0; i < len(pre); i++ {
		a := pre[i]
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
		}
		switch {
		case a == "--dev":
			o.Dev = true
		case name == "--name":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Name = v
		case name == "--model":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Model = v
		case name == "--skills":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Skills = append(o.Skills, v)
		case name == "--kit":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Kits = append(o.Kits, v)
		case name == "--mcp":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.MCP = append(o.MCP, v)
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %q", a)
		default:
			if wsSet {
				return o, fmt.Errorf("unexpected extra argument %q (only one DIR allowed; use -- for pi args)", a)
			}
			o.Workspace = a
			wsSet = true
		}
	}
	return o, nil
}

// resolveRepoRoot finds a pi-stack repo checkout for the local kit path, in
// order: $PI_STACK_DEV_ROOT if set, else walking up from the current working
// directory, else the launcher binary's own location (make install symlinks
// ~/.local/bin/pi-stack -> <repo>/out/pi-stack, so the repo is two levels up
// from the resolved binary). Fails when none resolves.
func resolveRepoRoot() (string, error) {
	if r := strings.TrimSpace(os.Getenv("PI_STACK_DEV_ROOT")); r != "" {
		if isRepoRoot(r) {
			return r, nil
		}
		return "", fmt.Errorf("$PI_STACK_DEV_ROOT=%q is not a pi-stack checkout (no pi-kit/spec.yaml)", r)
	}
	// Walk up from cwd looking for a repo root.
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for {
			if isRepoRoot(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	// The launcher binary's own location (symlink-resolved).
	if repo, ok := repoFromBinary(); ok {
		return repo, nil
	}
	return "", fmt.Errorf("no pi-stack checkout found (set $PI_STACK_DEV_ROOT or run from inside a checkout)")
}

// repoFromBinary resolves the launcher binary (following symlinks) and reports
// the repo root two levels up (<repo>/out/pi-stack -> <repo>) when it looks
// like a checkout.
func repoFromBinary() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	repo := filepath.Dir(filepath.Dir(self))
	if isRepoRoot(repo) {
		return repo, true
	}
	return "", false
}

// readLocalImageTag returns the trimmed contents of <root>/out/.local-image-tag
// (written by `make load`), or "" when absent — in which case the caller skips
// the --template pin.
func readLocalImageTag(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "out", ".local-image-tag"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// isRepoRoot reports whether dir looks like a pi-stack repo checkout.
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pi-kit", "spec.yaml"))
	return err == nil
}
