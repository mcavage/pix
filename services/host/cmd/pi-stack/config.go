package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"pi-stack/host/config"
)

// runConfig implements the `config` verb tree: `show`, `path`, `get`, `set`,
// `unset`. `set`/`unset` are THE answer to "why do I hand-edit the toml" — you
// don't, you run `pi-stack config set <key> <value>` and it loads, mutates, and
// Save()s the machine-managed config for you. `get` is the machine-readable
// read half: one resolved value, no decoration, so scripts (and the Makefile's
// operational targets) source runtime config from config.toml instead of
// keeping a second config file.
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
	case "get":
		runConfigGet(argv[1:])
	case "set":
		runConfigWrite(false, argv[1:])
	case "unset":
		runConfigWrite(true, argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pi-stack config: unknown subcommand %q (want: show, path, get, set, unset)\n", sub)
		os.Exit(2)
	}
}

// runConfigGet prints ONE resolved config value to stdout with no decoration —
// the machine-readable accessor the Makefile shells out to (`$(shell pi-stack
// config get mcp)`). The value comes straight from config.Load() (profiles were
// removed; the active PACK is now the unit of context, see
// docs/design/packs.md). List keys (mcp, services, knowledge_bundles) print
// space-separated. An unknown key is a loud error on stderr + exit 2, never a
// silent empty value.
func runConfigGet(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(configUsage)
		return
	}
	if len(argv) != 1 {
		fmt.Fprintf(os.Stderr, "usage: pi-stack config get <key>\n%s", configKeysHelp)
		os.Exit(2)
	}
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack config get: %v\n", err)
		os.Exit(1)
	}
	val, err := configValue(cfg, argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack config get: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(val)
}

// configValue resolves one key against the loaded config and renders it for
// machine consumption: scalars verbatim, lists space-separated. Pure +
// testable; the key set mirrors configKeysHelp exactly.
func configValue(cfg *config.Config, key string) (string, error) {
	switch key {
	case "google_workspace_account":
		return cfg.GogAccount, nil
	case "mcp":
		return strings.Join(cfg.MCP, " "), nil
	case "services":
		return strings.Join(cfg.Services, " "), nil
	case "knowledge_bundles":
		return strings.Join(cfg.KnowledgeBundles, " "), nil
	case "memory_watcher_model":
		return cfg.MemoryWatcherModel, nil
	case "memory_embed_model":
		return cfg.MemoryEmbedModel, nil
	case "ollama_bridge_model":
		return cfg.OllamaBridgeModel, nil
	case "run_intent":
		return cfg.RunIntent, nil
	case "pack":
		return cfg.Pack, nil
	case "host.enabled":
		return strconv.FormatBool(cfg.Host.Enabled), nil
	case "host.autonomy":
		return cfg.Host.Autonomy, nil
	case "host.autoserve":
		return strconv.FormatBool(cfg.AutoserveEnabled()), nil
	default:
		return "", fmt.Errorf("unknown key %q\n%s", key, configKeysHelp)
	}
}

// runConfigWrite loads the config, applies a set/unset, Save()s it, and prints
// the new value + path so the user sees the effect without opening the file.
// There is a single config.toml (profiles were removed; the active PACK is the
// unit of context) and this is the only mutation path — it is never hand-edited
// (AGENTS.md forbids it).
func runConfigWrite(unset bool, argv []string) {
	verb := "set"
	if unset {
		verb = "unset"
	}
	if wantsHelp(argv) {
		fmt.Print(configUsage)
		return
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
	// Config propagation: a daemon-affecting key (services, memory_*_model,
	// knowledge_bundles) only takes effect when serve restarts — do that for the
	// user per the detected lifecycle mode (managed/lazy restart; foreground/down
	// just advise). Composes with sparse Save: the change is both persisted
	// correctly AND live, no manual step.
	if isDaemonAffecting(argv[0]) {
		propagateServeConfig(defaultServeReloader(), os.Stdout)
	}
}

// configKeysHelp lists the supported keys for set/unset.
const configKeysHelp = `keys:
  google_workspace_account <email>
                           Google Workspace account for the google-workspace MCP server
  mcp <server>              add/remove an MCP server in the mcp list; every
                            configured server preloads at sandbox create
  services <name>           add/remove a host service in the services list
  knowledge_bundles <dir>   add/remove an OKF knowledge bundle dir (set also
                            enables the knowledge service)
  memory_watcher_model <m>  ollama model for fact capture (host, resident)
  memory_embed_model <m>    ollama model for semantic recall (host)
  ollama_bridge_model <m>   local model the sandbox exposes to pi + the router
  run_intent <intent>       default routing intent for the top-level interactive
                            session (the "overlord"); resolves the session model
                            when neither --model nor --intent is passed. Use
                            'none' to opt out to pi's own default model
  pack <path>               active pack dir (run mounts its skills + knowledge);
                            usually set via 'pi-stack pack use'
  host.enabled true|false   gate for "pi-stack host" (UNSANDBOXED; default false)
  host.autonomy <mode>      reserved for the host-guard strictness (unused yet)
  host.autoserve true|false lazy auto-start of the services daemon on run/
                            memory/knowledge (default true; PI_STACK_NO_AUTOSERVE
                            env also disables it)
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
	case "google_workspace_account":
		if unset {
			cfg.SetGogAccount("")
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set google_workspace_account <email>: needs exactly one value")
			}
			cfg.SetGogAccount(args[0])
		}
		return fmt.Sprintf("google_workspace_account = %q", cfg.GogAccount), nil

	case "mcp":
		if len(args) != 1 {
			return "", fmt.Errorf("config %s mcp <server>: needs a server name (e.g. google-workspace, slack)", verb)
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

	case "run_intent":
		if unset {
			cfg.RunIntent = config.DefaultRunIntent
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set run_intent <intent>: needs exactly one value (e.g. overlord, strategy)")
			}
			cfg.RunIntent = args[0]
		}
		return fmt.Sprintf("run_intent = %q", cfg.RunIntent), nil

	case "pack":
		if unset {
			cfg.Pack = ""
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set pack <path>: needs exactly one value")
			}
			cfg.Pack = args[0]
		}
		return fmt.Sprintf("pack = %q", cfg.Pack), nil

	case "host.enabled":
		// The gate for `pi-stack host` (unsandboxed). Default false; unset resets
		// it. Set requires an explicit true/false — never inferred — so enabling
		// the dangerous path is always a deliberate, legible command.
		if unset {
			cfg.Host.Enabled = false
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set host.enabled <true|false>: needs exactly one value")
			}
			v, err := strconv.ParseBool(args[0])
			if err != nil {
				return "", fmt.Errorf("config set host.enabled: %q is not a boolean (want true or false)", args[0])
			}
			cfg.Host.Enabled = v
		}
		return fmt.Sprintf("host.enabled = %v", cfg.Host.Enabled), nil

	case "host.autoserve":
		// Opt-out flag for lazy auto-start (ensureServe). Unset = nil = inherit the
		// default (true) so a future default change reaches users (no petrified
		// bool). NOT daemon-affecting: it changes launcher behavior, not serve.
		if unset {
			cfg.Host.Autoserve = nil
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set host.autoserve <true|false>: needs exactly one value")
			}
			v, err := strconv.ParseBool(args[0])
			if err != nil {
				return "", fmt.Errorf("config set host.autoserve: %q is not a boolean (want true or false)", args[0])
			}
			cfg.Host.Autoserve = &v
		}
		return fmt.Sprintf("host.autoserve = %v", cfg.AutoserveEnabled()), nil

	case "host.autonomy":
		// RESERVED: stored for the future host-guard strictness knob; nothing
		// reads it in Phase 1.
		if unset {
			cfg.Host.Autonomy = ""
		} else {
			if len(args) != 1 {
				return "", fmt.Errorf("config set host.autonomy <mode>: needs exactly one value")
			}
			cfg.Host.Autonomy = args[0]
		}
		return fmt.Sprintf("host.autonomy = %q (reserved; unused in Phase 1)", cfg.Host.Autonomy), nil

	default:
		return "", fmt.Errorf("unknown key %q\n%s", key, configKeysHelp)
	}
}
