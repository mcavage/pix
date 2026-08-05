package provision

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/service"
	"pix/host/workspace"

	"github.com/BurntSushi/toml"
)

// RunConfig implements the `config` verb tree: `show`, `path`, `get`, `set`,
// `unset`. `set`/`unset` are THE answer to "why do I hand-edit the toml" — you
// don't, you run `pix config set <key> <value>` and it loads, mutates, and
// Save()s the machine-managed config for you. `get` is the machine-readable
// read half: one resolved value, no decoration, so scripts (and the Makefile's
// operational targets) source runtime config from config.toml instead of
// keeping a second config file.
func RunConfig(argv []string) {
	// A leading -h/--help (with or without a subcommand) prints config usage.
	if cli.WantsHelp(argv) {
		fmt.Print(ConfigUsage)
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
			fmt.Fprintf(os.Stderr, "pix config path: unexpected argument %q (want: op-refs)\n", argv[1])
			os.Exit(2)
		}
		fmt.Println(config.Path())
	case "show":
		if len(argv) > 1 {
			fmt.Fprintf(os.Stderr, "pix config show: unexpected argument %q\n", argv[1])
			os.Exit(2)
		}
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pix config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("# path: %s\n", config.Path())
		if err := toml.NewEncoder(os.Stdout).Encode(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "pix config: encoding: %v\n", err)
			os.Exit(1)
		}
	case "get":
		runConfigGet(argv[1:])
	case "set":
		runConfigWrite(false, argv[1:])
	case "unset":
		runConfigWrite(true, argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pix config: unknown subcommand %q (want: show, path, get, set, unset)\n", sub)
		os.Exit(2)
	}
}

// runConfigGet prints ONE resolved config value to stdout with no decoration —
// the machine-readable accessor the Makefile shells out to (`$(shell pix
// config get mcp)`). The value comes straight from config.Load() (profiles were
// removed; the active PACK is now the unit of context, see
// docs/design/packs.md). List keys (mcp, services, knowledge_bundles) print
// space-separated. An unknown key is a loud error on stderr + exit 2, never a
// silent empty value.
func runConfigGet(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(ConfigUsage)
		return
	}
	if len(argv) != 1 {
		fmt.Fprintf(os.Stderr, "usage: pix config get <key>\n%s", ConfigKeysHelp)
		os.Exit(2)
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix config get: %v\n", err)
		os.Exit(1)
	}
	val, err := ConfigValue(cfg, argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix config get: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(val)
}

// ConfigValue resolves one key against the loaded config and renders it for
// machine consumption: scalars verbatim, lists space-separated. Pure +
// testable; the key set mirrors ConfigKeysHelp exactly.
func ConfigValue(cfg *config.Config, key string) (string, error) {
	switch key {
	case "google_workspace_account":
		return cfg.GogAccount, nil
	case "google_workspace_access":
		return cfg.GoogleWorkspaceAccess, nil
	case "mcp":
		return strings.Join(cfg.MCP, " "), nil
	case "services":
		return strings.Join(cfg.Services, " "), nil
	case "knowledge_bundles":
		return "", fmt.Errorf("knowledge_bundles was retired (W2 U03A removed the built-in knowledge service); no replacement key")
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
	case "host.enabled", "host.autonomy":
		return "", fmt.Errorf("%s is retired: `pix host` (the unsandboxed escape hatch) was removed — the sandbox is the only supported execution boundary now; this key does nothing", key)
	case "host.autoserve":
		return strconv.FormatBool(cfg.AutoserveEnabled()), nil
	default:
		return "", fmt.Errorf("unknown key %q\n%s", key, ConfigKeysHelp)
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
	if cli.WantsHelp(argv) {
		fmt.Print(ConfigUsage)
		return
	}
	if len(argv) == 0 {
		fmt.Fprintf(os.Stderr, "usage: pix config %s <key> [value]\n%s", verb, ConfigKeysHelp)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix config %s: loading config: %v\n", verb, err)
		os.Exit(1)
	}
	summary, err := ApplyConfigChange(cfg, unset, argv[0], argv[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix config %s: %v\n", verb, err)
		os.Exit(2)
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "pix config %s: saving config: %v\n", verb, err)
		os.Exit(1)
	}
	fmt.Printf("%s\n# saved to %s\n", summary, config.Path())
	// Config propagation: a daemon-affecting key (services, memory_*_model,
	// knowledge_bundles) only takes effect when serve restarts — do that for the
	// user per the detected lifecycle mode (managed/lazy restart; foreground/down
	// just advise). Composes with sparse Save: the change is both persisted
	// correctly AND live, no manual step.
	if service.IsDaemonAffecting(argv[0]) {
		service.PropagateConfig(service.DefaultReloader(), os.Stdout)
	}
}

// ConfigKeysHelp lists the supported keys for set/unset.
const ConfigKeysHelp = `keys:
  google_workspace_account <email>
                           Google Workspace account for the google-workspace MCP server
  google_workspace_access  get-only permission profile written by
                           'pix gworkspace setup'
  mcp <server>              add/remove an MCP server in the mcp list; every
                            configured server preloads at sandbox create
  services <name>           add/remove a host service in the services list
  memory_watcher_model <m>  ollama model for fact capture (host, resident)
  memory_embed_model <m>    ollama model for semantic recall (host)
  ollama_bridge_model <m>   local model the sandbox exposes to pi + the router
  run_intent <intent>       default routing intent for the top-level interactive
                            session (the "overlord"); resolves the session model
                            when neither --model nor --intent is passed. Use
                            'none' to opt out to pi's own default model
  pack <path>               active pack dir (run mounts its skills + knowledge);
                            usually set via 'pix pack use'
  host.autoserve true|false lazy auto-start of the services daemon on run/
                            memory/knowledge (default true; PIX_NO_AUTOSERVE
                            env also disables it)
`

// ApplyConfigChange applies a set (unset=false) or unset to cfg and returns a
// one-line summary of the new value. Pure + testable: it mutates cfg but does
// NOT save. List keys (mcp, services) take a single value to add/remove; scalar
// keys take a single value on set and reset/clear on unset.
func ApplyConfigChange(cfg *config.Config, unset bool, key string, args []string) (string, error) {
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
		return "", fmt.Errorf("config %s knowledge_bundles: retired (W2 U03A removed the built-in knowledge service); no replacement key", verb)

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

	case "host.enabled", "host.autonomy":
		// RETIRED: `pix host` (the unsandboxed escape hatch) was deleted — the
		// sandbox is the only supported execution boundary now. Neither key does
		// anything; refuse rather than silently accept a no-op set/unset.
		return "", fmt.Errorf("%s is retired: `pix host` was removed; this key does nothing (nothing was changed)", key)

	case "host.autoserve":
		// Opt-out flag for lazy auto-start (service.Ensure). Unset = nil = inherit the
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

	default:
		return "", fmt.Errorf("unknown key %q\n%s", key, ConfigKeysHelp)
	}
}

const ConfigUsage = `usage: pix config <show|path|get|set|unset> [args]

  show                     print the resolved config path + contents
  path [op-refs]           print the config file path (or the op-refs.env path)
  get K                    print ONE resolved value, no decoration (lists are
                            space-separated); for scripts/make to source
  set K V                   set a config key (never hand-edit the toml)
  unset K [V]               reset/clear a scalar key, or remove value V from a
                            list key (mcp/services/knowledge_bundles)

` + ConfigKeysHelp
