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
	if wantsHelp(argv) {
		fmt.Print(profileUsage)
		return
	}
	if len(argv) == 0 {
		runProfileLs(os.Stdout, false)
		return
	}
	switch argv[0] {
	case "ls", "list":
		jsonOut, err := parseProfileLsArgs(argv[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(2)
		}
		runProfileLs(os.Stdout, jsonOut)
	case "use", "set":
		name, err := validateProfileUseArgs(argv[1:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(2)
		}
		runProfileUse(name, os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "pi-stack profile: unknown subcommand %q (want: ls, use)\n", argv[0])
		os.Exit(2)
	}
}

func runProfileLs(out io.Writer, jsonOut bool) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack profile ls: %v\n", err)
		os.Exit(1)
	}
	active := resolveProfileName(cfg)
	if jsonOut {
		_ = writeJSONOut(out, profileLsView(cfg, active))
		return
	}
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

// validateProfileUseArgs validates the tokens after `profile use`: exactly one
// positional profile name, no flags, no extra positionals. It rejects trailing
// junk (`profile use work --jsom`, `profile use a b`) BEFORE any save, so a typo
// never silently mutates active_profile. -h/--help is handled by the wantsHelp
// gate in runProfile before this is reached.
func validateProfileUseArgs(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", usageErr("usage: pi-stack profile use <name>")
	}
	name := argv[0]
	if strings.HasPrefix(name, "-") {
		return "", usageErr(fmt.Sprintf("unknown flag %q\nusage: pi-stack profile use <name>", name))
	}
	if len(argv) > 1 {
		return "", usageErr(fmt.Sprintf("unexpected extra argument %q\nusage: pi-stack profile use <name>", argv[1]))
	}
	return name, nil
}

// parseProfileLsArgs validates the tokens after `profile ls`: only an optional
// --json. Any other token (a flag typo like --jsom, or a stray positional) is a
// usage error, so `profile ls --jsom` fails loud instead of silently running as
// plain ls. -h/--help is handled by the wantsHelp gate in runProfile.
func parseProfileLsArgs(argv []string) (bool, error) {
	jsonOut := false
	for _, a := range argv {
		switch a {
		case "--json":
			jsonOut = true
		default:
			return false, usageErr(fmt.Sprintf("unknown argument %q\nusage: pi-stack profile ls [--json]", a))
		}
	}
	return jsonOut, nil
}

// profileLsSnapshot is the machine-readable snapshot behind `profile ls --json`.
type profileLsSnapshot struct {
	ConfigPath string            `json:"config_path"`
	Active     string            `json:"active"`
	Profiles   []profileLineView `json:"profiles"`
}

type profileLineView struct {
	Name       string   `json:"name"`
	Active     bool     `json:"active"`
	GogAccount string   `json:"gog_account,omitempty"`
	MCP        []string `json:"mcp"`
	Bundles    int      `json:"knowledge_bundles"`
	Kits       int      `json:"kits"`
}

func profileLsView(cfg *config.Config, active string) profileLsSnapshot {
	snap := profileLsSnapshot{ConfigPath: config.Path(), Active: active}
	for _, name := range cfg.ProfileNames() {
		res := cfg.Resolve(name)
		snap.Profiles = append(snap.Profiles, profileLineView{
			Name:       name,
			Active:     name == active,
			GogAccount: res.GogAccount,
			MCP:        res.MCP,
			Bundles:    len(res.KnowledgeBundles),
			Kits:       len(res.Kits.Stack),
		})
	}
	return snap
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
