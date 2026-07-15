package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"pi-stack/host/config"
)

// flagProfile holds the value of a global `--profile <name>` flag, extracted
// from os.Args before subcommand dispatch. Empty means "not set on the CLI".
var flagProfile string

// extractProfileFlag pulls a global `--profile <name>` / `--profile=<name>` out
// of argv (anywhere before a `--` terminator) and returns the remaining args.
// It sets flagProfile as a side effect. This lets `pi-stack --profile work run`
// and `pi-stack run --profile work` both work. A `--profile` with no value (or an
// empty value) is an error — never a silent fallback to the base config.
func extractProfileFlag(argv []string) ([]string, error) {
	var rest []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		if a == "--profile" {
			if i+1 >= len(argv) || strings.TrimSpace(argv[i+1]) == "" {
				return nil, fmt.Errorf("--profile needs a name")
			}
			flagProfile = argv[i+1]
			i++
			continue
		}
		if v, ok := cutPrefix(a, "--profile="); ok {
			if strings.TrimSpace(v) == "" {
				return nil, fmt.Errorf("--profile needs a name")
			}
			flagProfile = v
			continue
		}
		rest = append(rest, a)
	}
	return rest, nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// resolveProfileName picks the active profile by precedence:
// --profile flag > PI_STACK_PROFILE env > config active_profile > "default".
func resolveProfileName(cfg *config.Config) string {
	if flagProfile != "" {
		return flagProfile
	}
	if e := os.Getenv("PI_STACK_PROFILE"); e != "" {
		return e
	}
	if cfg.ActiveProfile != "" {
		return cfg.ActiveProfile
	}
	return config.DefaultProfile
}

// activeProfileName is resolveProfileName for display (status/doctor headers).
func activeProfileName(cfg *config.Config) string { return resolveProfileName(cfg) }

// loadResolvedConfig loads config and resolves the active profile into a flat
// config every consumer can use unchanged. It returns the resolved config and
// the active profile NAME, and errors when a non-default profile name is not
// actually configured — so a typo (`--profile wrok`) fails loud instead of
// silently running the base config with the wrong identity.
func loadResolvedConfig() (*config.Config, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", err
	}
	name := resolveProfileName(cfg)
	if name != config.DefaultProfile {
		if _, ok := cfg.Profiles[name]; !ok {
			return nil, "", fmt.Errorf("no profile %q — configured: %s", name, strings.Join(cfg.ProfileNames(), ", "))
		}
	}
	return cfg.Resolve(name), name, nil
}

// sanitizeProfileName keeps a profile name safe as a sandbox-name suffix.
func sanitizeProfileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// runProfile is the `profile` verb tree: list profiles and set the active one.
//
//	pi-stack profile ls              list profiles (* = active)
//	pi-stack profile use <name>      set active_profile (persisted)
//	pi-stack profile use default     revert to the base config
func runProfile(argv []string) {
	if len(argv) == 0 {
		runProfileLs(os.Stdout)
		return
	}
	switch argv[0] {
	case "ls", "list":
		runProfileLs(os.Stdout)
	case "use", "set":
		if len(argv) < 2 {
			fmt.Fprintln(os.Stderr, "usage: pi-stack profile use <name>")
			os.Exit(2)
		}
		runProfileUse(argv[1], os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "pi-stack profile: unknown subcommand %q (want: ls, use)\n", argv[0])
		os.Exit(2)
	}
}

func runProfileLs(out io.Writer) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack profile ls: %v\n", err)
		os.Exit(1)
	}
	active := resolveProfileName(cfg)
	fmt.Fprintf(out, "# config: %s\n", config.Path())
	for _, name := range cfg.ProfileNames() {
		marker := "  "
		if name == active {
			marker = "* "
		}
		res := cfg.Resolve(name)
		fmt.Fprintf(out, "%s%-12s gog=%s  mcp=%v  bundles=%d  kits=%d\n",
			marker, name, dashIfEmpty(res.GogAccount), res.MCP, len(res.KnowledgeBundles), len(res.Kits.Stack))
	}
}

func runProfileUse(name string, out io.Writer) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack profile use: %v\n", err)
		os.Exit(1)
	}
	if name != config.DefaultProfile {
		if _, ok := cfg.Profiles[name]; !ok {
			fmt.Fprintf(os.Stderr, "pi-stack profile use: no profile %q — add a [profiles.%s] table to %s\n", name, name, config.Path())
			os.Exit(1)
		}
	}
	if name == config.DefaultProfile {
		cfg.ActiveProfile = ""
	} else {
		cfg.ActiveProfile = name
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack profile use: saving config: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "active profile: %s\n", name)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
