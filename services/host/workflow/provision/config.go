package provision

import (
	"fmt"
	"strconv"
	"strings"

	"pix/host/config"
)

// ConfigValue resolves one key against the loaded config and renders it for
// machine consumption: scalars verbatim, lists space-separated. Pure +
// testable; the key set mirrors ConfigKeysHelp exactly.
func ConfigValue(cfg *config.Config, key string) (string, error) {
	switch key {
	case "google_workspace_account":
		return cfg.GogAccount, nil
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

// ConfigKeysHelp lists the supported keys for set/unset.
const ConfigKeysHelp = `keys:
  google_workspace_account <email>
                           Google Workspace account for the google-workspace MCP server
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
                            memory (default true; PIX_NO_AUTOSERVE env also
                            disables it)
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
