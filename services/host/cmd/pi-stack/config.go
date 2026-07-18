package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"pi-stack/host/config"
)

// runConfig implements the `config` verb tree: `show`, `path`, `set`, `unset`.
// `set`/`unset` are THE answer to "why do I hand-edit the toml" — you don't, you
// run `pi-stack config set <key> <value>` and it loads, mutates, and Save()s the
// machine-managed config for you.
func runConfig(argv []string) {
	// A leading -h/--help (with or without a subcommand) prints config usage.
	if wantsHelp(argv) {
		fmt.Print(configUsage)
		return
	}
	sub := "show"
	if len(argv) > 0 {
		sub = argv[0]
	}
	switch sub {
	case "path":
		// `config path op-refs` prints the absolute op-refs.env path (discoverability
		// sugar mirroring `config path`); bare `config path` prints config.toml. Any
		// other trailing token is a typo, not a silently-ignored arg.
		if len(argv) > 1 && argv[1] == "op-refs" && len(argv) == 2 {
			fmt.Println(config.OpRefsPath())
			return
		}
		if len(argv) > 1 {
			fmt.Fprintf(os.Stderr, "pi-stack config path: unexpected argument %q (want: op-refs)\n", argv[1])
			os.Exit(2)
		}
		fmt.Println(config.Path())
	case "show":
		if len(argv) > 1 {
			fmt.Fprintf(os.Stderr, "pi-stack config show: unexpected argument %q\n", argv[1])
			os.Exit(2)
		}
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("# path: %s\n", config.Path())
		if err := toml.NewEncoder(os.Stdout).Encode(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack config: encoding: %v\n", err)
			os.Exit(1)
		}
	case "set":
		runConfigWrite(false, argv[1:])
	case "unset":
		runConfigWrite(true, argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pi-stack config: unknown subcommand %q (want: show, path, set, unset)\n", sub)
		os.Exit(2)
	}
}

// runConfigWrite loads the config, applies a set/unset, Save()s it, and prints
// the new value + path so the user sees the effect without opening the file. A
// `--profile <name>` flag (parsed out of argv, or inherited from the global
// --profile main already extracted) targets a [profiles.<name>] table instead of
// the base config, so per-profile config is never hand-edited (AGENTS.md forbids
// it). An unknown profile name is scaffolded on first write.
func runConfigWrite(unset bool, argv []string) {
	verb := "set"
	if unset {
		verb = "unset"
	}
	if wantsHelp(argv) {
		fmt.Print(configUsage)
		return
	}
	profile, argv := splitProfileArg(argv)
	if profile == "" {
		// The global `pi-stack --profile <name> config set …` form is stripped
		// before dispatch (main.extractProfileFlag), so fall back to it here.
		profile = flagProfile
	}
	if len(argv) == 0 {
		fmt.Fprintf(os.Stderr, "usage: pi-stack config %s [--profile <name>] <key> [value]\n%s", verb, configKeysHelp)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack config %s: loading config: %v\n", verb, err)
		os.Exit(1)
	}
	var summary string
	if profile != "" && profile != config.DefaultProfile {
		summary, err = applyProfileConfigChange(cfg, unset, profile, argv[0], argv[1:])
	} else {
		summary, err = applyConfigChange(cfg, unset, argv[0], argv[1:])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack config %s: %v\n", verb, err)
		os.Exit(2)
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack config %s: saving config: %v\n", verb, err)
		os.Exit(1)
	}
	fmt.Printf("%s\n# saved to %s\n", summary, config.Path())
}

// splitProfileArg pulls a `--profile <name>` / `--profile=<name>` flag out of a
// config-write argv (anywhere in the slice) and returns the name plus the
// remaining args. Empty name means no flag was present. An empty value after
// `--profile` is ignored (falls back to the global flag / base config).
func splitProfileArg(argv []string) (string, []string) {
	var name string
	var rest []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--profile" {
			if i+1 < len(argv) {
				name = strings.TrimSpace(argv[i+1])
				i++
			}
			continue
		}
		if v, ok := cutPrefix(a, "--profile="); ok {
			name = strings.TrimSpace(v)
			continue
		}
		rest = append(rest, a)
	}
	return name, rest
}

// configKeysHelp lists the supported keys for set/unset.
const configKeysHelp = `keys:
  gog_account <email>       Google Workspace account for the gog MCP server
  mcp <server>              add/remove an MCP server in the mcp list
  services <name>           add/remove a host service in the services list
  knowledge_bundles <dir>   add/remove an OKF knowledge bundle dir (set also
                            enables the knowledge service)
  memory_watcher_model <m>  ollama model for fact capture (host, resident)
  memory_embed_model <m>    ollama model for semantic recall (host)
  ollama_bridge_model <m>   local model the sandbox exposes to pi + the router

With --profile <name>, edits the [profiles.<name>] table instead of the base
config (creating it if absent). Per-profile keys: gog_account, mcp,
knowledge_bundles, kit. services and memory_* are GLOBAL — reject --profile.
`

// applyConfigChange applies a set (unset=false) or unset to cfg and returns a
// one-line summary of the new value. Pure + testable: it mutates cfg but does
// NOT save. List keys (mcp, services) take a single value to add/remove; scalar
// keys take a single value on set and reset/clear on unset.
func applyConfigChange(cfg *config.Config, unset bool, key string, args []string) (string, error) {
	verb := "set"
	if unset {
		verb = "unset"
	}
	switch key {
	case "gog_account":
		if unset {
			cfg.SetGogAccount("")
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set gog_account <email>: needs exactly one value")
			}
			cfg.SetGogAccount(args[0])
		}
		return fmt.Sprintf("gog_account = %q", cfg.GogAccount), nil

	case "mcp":
		if len(args) != 1 {
			return "", fmt.Errorf("config %s mcp <server>: needs a server name (e.g. gog, slack)", verb)
		}
		if unset {
			cfg.RemoveMCP(args[0])
		} else {
			cfg.AddMCP(args[0])
		}
		return fmt.Sprintf("mcp = %v", cfg.MCP), nil

	case "services":
		if len(args) != 1 {
			return "", fmt.Errorf("config %s services <name>: needs a service name (e.g. memory)", verb)
		}
		if unset {
			cfg.RemoveService(args[0])
		} else {
			cfg.AddService(args[0])
		}
		return fmt.Sprintf("services = %v", cfg.Services), nil

	case "knowledge_bundles":
		if len(args) != 1 {
			return "", fmt.Errorf("config %s knowledge_bundles <dir>: needs a bundle directory path", verb)
		}
		if unset {
			cfg.RemoveKnowledgeBundle(args[0])
		} else {
			// Setting a bundle implies wanting the knowledge service that
			// indexes it, so ensure it's in the services list too.
			cfg.AddKnowledgeBundle(args[0])
			cfg.AddService("knowledge")
		}
		return fmt.Sprintf("knowledge_bundles = %v, services = %v", cfg.KnowledgeBundles, cfg.Services), nil

	case "memory_watcher_model":
		if unset {
			cfg.MemoryWatcherModel = config.DefaultMemoryWatcherModel
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set memory_watcher_model <model>: needs exactly one value")
			}
			cfg.MemoryWatcherModel = args[0]
		}
		return fmt.Sprintf("memory_watcher_model = %q", cfg.MemoryWatcherModel), nil

	case "memory_embed_model":
		if unset {
			cfg.MemoryEmbedModel = config.DefaultMemoryEmbedModel
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set memory_embed_model <model>: needs exactly one value")
			}
			cfg.MemoryEmbedModel = args[0]
		}
		return fmt.Sprintf("memory_embed_model = %q", cfg.MemoryEmbedModel), nil

	case "ollama_bridge_model":
		if unset {
			cfg.OllamaBridgeModel = config.DefaultOllamaBridgeModel
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set ollama_bridge_model <model>: needs exactly one value")
			}
			cfg.OllamaBridgeModel = args[0]
		}
		return fmt.Sprintf("ollama_bridge_model = %q", cfg.OllamaBridgeModel), nil

	default:
		return "", fmt.Errorf("unknown key %q\n%s", key, configKeysHelp)
	}
}

// applyProfileConfigChange is the per-profile sibling of applyConfigChange: it
// mutates cfg.Profiles[profile] (creating the entry when absent) instead of the
// base config. Only the runtime-swappable keys are per-profile — gog_account,
// mcp, knowledge_bundles, and the overlay kit stack (`kit`/`kits`). services and
// the memory_* models are GLOBAL (they configure the single host `serve`
// supervisor, not a sandbox context), so a --profile on those is rejected with a
// clear error rather than silently written to a table nothing reads. Pure +
// testable: mutates cfg but does NOT save.
func applyProfileConfigChange(cfg *config.Config, unset bool, profile, key string, args []string) (string, error) {
	verb := "set"
	if unset {
		verb = "unset"
	}
	switch key {
	case "services", "memory_watcher_model", "memory_embed_model":
		return "", fmt.Errorf("%s is global (not per-profile); drop --profile and run: pi-stack config %s %s <value>", key, verb, key)

	case "gog_account":
		if unset {
			cfg.SetProfileGogAccount(profile, "")
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set --profile %s gog_account <email>: needs exactly one value", profile)
			}
			cfg.SetProfileGogAccount(profile, args[0])
		}
		return fmt.Sprintf("profiles.%s.gog_account = %q", profile, cfg.Profiles[profile].GogAccount), nil

	case "mcp":
		if len(args) != 1 {
			return "", fmt.Errorf("config %s --profile %s mcp <server>: needs a server name (e.g. gog, slack)", verb, profile)
		}
		if unset {
			cfg.RemoveProfileMCP(profile, args[0])
		} else {
			cfg.AddProfileMCP(profile, args[0])
		}
		return fmt.Sprintf("profiles.%s.mcp = %v", profile, derefList(cfg.Profiles[profile].MCP)), nil

	case "knowledge_bundles":
		if len(args) != 1 {
			return "", fmt.Errorf("config %s --profile %s knowledge_bundles <dir>: needs a bundle directory path", verb, profile)
		}
		if unset {
			cfg.RemoveProfileKnowledgeBundle(profile, args[0])
		} else {
			cfg.AddProfileKnowledgeBundle(profile, args[0])
		}
		return fmt.Sprintf("profiles.%s.knowledge_bundles = %v", profile, derefList(cfg.Profiles[profile].KnowledgeBundles)), nil

	case "kit", "kits":
		if len(args) != 1 {
			return "", fmt.Errorf("config %s --profile %s kit <path>: needs an overlay kit path", verb, profile)
		}
		if unset {
			cfg.RemoveProfileKit(profile, args[0])
		} else {
			cfg.AddProfileKit(profile, args[0])
		}
		return fmt.Sprintf("profiles.%s.kits.stack = %v", profile, derefList(cfg.Profiles[profile].Kits.Stack)), nil

	default:
		return "", fmt.Errorf("unknown per-profile key %q (per-profile: gog_account, mcp, knowledge_bundles, kit)\n%s", key, configKeysHelp)
	}
}

// derefList renders a profile's tri-state override slice for a confirmation
// message: nil (inherit) prints as an empty list, otherwise the pointed-to
// slice — so a present-empty REPLACE also prints as [].
func derefList(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
