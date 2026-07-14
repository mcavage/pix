package main

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
	"pi-stack/host/config"
)

// runConfig implements the `config` verb tree: `show`, `path`, `set`, `unset`.
// `set`/`unset` are THE answer to "why do I hand-edit the toml" — you don't, you
// run `pi-stack config set <key> <value>` and it loads, mutates, and Save()s the
// machine-managed config for you.
func runConfig(argv []string) {
	sub := "show"
	if len(argv) > 0 {
		sub = argv[0]
	}
	switch sub {
	case "path":
		fmt.Println(config.Path())
	case "show":
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
// the new value + path so the user sees the effect without opening the file.
func runConfigWrite(unset bool, argv []string) {
	verb := "set"
	if unset {
		verb = "unset"
	}
	if len(argv) == 0 {
		fmt.Fprintf(os.Stderr, "usage: pi-stack config %s <key> [value]\n%s", verb, configKeysHelp)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack config %s: loading config: %v\n", verb, err)
		os.Exit(1)
	}
	summary, err := applyConfigChange(cfg, unset, argv[0], argv[1:])
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

// configKeysHelp lists the supported keys for set/unset.
const configKeysHelp = `keys:
  gog_account <email>       Google Workspace account for the gog MCP server
  mcp <server>              add/remove an MCP server in the mcp list
  services <name>           add/remove a host service in the services list
  knowledge_bundles <dir>   add/remove an OKF knowledge bundle dir (set also
                            enables the knowledge service)
  memory_watcher_model <m>  ollama model for fact capture
  memory_embed_model <m>    ollama model for semantic recall
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

	default:
		return "", fmt.Errorf("unknown key %q\n%s", key, configKeysHelp)
	}
}
