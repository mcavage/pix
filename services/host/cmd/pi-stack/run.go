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
// config, resolves the run options (including a repo checkout for --dev and the
// broker token), composes the sbx argv, and execs it with stdio inherited.
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

	// --dev needs a resolvable repo checkout; fail loud otherwise.
	if o.Dev {
		root, err := resolveDevRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack run --dev: %v\n", err)
			os.Exit(1)
		}
		o.DevRoot = root
		if version == "dev" {
			fmt.Fprintln(os.Stderr, "pi-stack: dev build — kit tracks #ref=main (build with -ldflags \"-X main.version=<v>\" to pin a release)")
		}
	} else if version == "dev" {
		fmt.Fprintln(os.Stderr, "pi-stack: dev build — kit tracks #ref=main (build with -ldflags \"-X main.version=<v>\" to pin a release)")
	}

	// Mint/read the shared broker bearer so the sandbox can reach host services.
	if tok, err := config.EnsureToken(); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack run: broker token: %v\n", err)
		os.Exit(1)
	} else {
		o.Token = tok
	}

	args := buildSbxArgs(cfg, o, version)

	if os.Getenv("PI_STACK_DEBUG") != "" {
		// Never print the raw broker bearer (F3): argv is process-inspectable and
		// debug output ends up in logs/terminals. Redact the value, keep the shape.
		fmt.Fprintln(os.Stderr, "+ sbx "+strings.Join(redactArgs(args), " "))
	}

	cmd := exec.Command("sbx", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack run: exec sbx: %v\n", err)
		os.Exit(1)
	}
}

// redactArgs returns a copy of args with the broker bearer's value masked, so a
// debug print (or anything that logs the composed command) never leaks the
// token. The functional injection is unchanged — only the printed copy differs.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "GWS_TOKEN_AUTH=") {
			out[i] = "GWS_TOKEN_AUTH=***"
		} else {
			out[i] = a
		}
	}
	return out
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

// resolveDevRoot finds a repo checkout for Mode B: $PI_STACK_DEV_ROOT if set,
// else the current working directory if it looks like the repo (has
// pi-kit/spec.yaml). Fails loud when neither resolves.
func resolveDevRoot() (string, error) {
	if r := strings.TrimSpace(os.Getenv("PI_STACK_DEV_ROOT")); r != "" {
		if isRepoRoot(r) {
			return r, nil
		}
		return "", fmt.Errorf("$PI_STACK_DEV_ROOT=%q is not a pi-stack checkout (no pi-kit/spec.yaml)", r)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// Walk up from cwd looking for a repo root.
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
	return "", fmt.Errorf("no repo checkout found (cwd %q is not inside a pi-stack repo); set $PI_STACK_DEV_ROOT", cwd)
}

// isRepoRoot reports whether dir looks like a pi-stack repo checkout.
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pi-kit", "spec.yaml"))
	return err == nil
}
