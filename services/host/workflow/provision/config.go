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
	case "mcp":
		return strings.Join(cfg.MCP, " "), nil
	case "services":
		return strings.Join(cfg.Services, " "), nil
	case "memory_watcher_model":
		return cfg.MemoryWatcherModel, nil
	case "memory_embed_model":
		return cfg.MemoryEmbedModel, nil
	case "ollama_bridge_model":
		return cfg.OllamaBridgeModel, nil
	case "run_intent":
		return cfg.RunIntent, nil
	case "memory_capture":
		return cfg.MemoryCapture, nil
	case "pack":
		return cfg.Pack, nil
	case "host.autoserve":
		return strconv.FormatBool(cfg.AutoserveEnabled()), nil
	default:
		return "", fmt.Errorf("unknown key %q\n%s", key, ConfigKeysHelp)
	}
}

// ConfigKeysHelp lists the supported keys for set/unset.
const ConfigKeysHelp = `keys:
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
  memory_capture <mode>     watcher capture admission: explicit (default, no
                            automatic observation) or experimental-auto (write
                            straight to memories under a fixed daily budget).
                            Turning it OFF (explicit) takes effect at once, the
                            host refuses capture for running sandboxes too;
                            turning it ON reaches NEW sandboxes only
  pack <path>               active pack dir (run mounts its skills + knowledge);
                            usually set via 'pix pack use'
  host.autoserve true|false lazy auto-start of the services daemon on run/
                            memory (default true; PIX_NO_AUTOSERVE env also
                            disables it)
  environment / environments.*  NOT settable here — the default environment
                            is managed by 'pix env use', the registry by
                            'pix env add'/'pix env forget' (see pix env --help)
`

// memoryCaptureSummary reports what the new memory_capture value actually
// changes, which is NOT symmetric — the old single line ("applies to new
// sandboxes") was wrong in the direction that matters most:
//
//   - explicit: the HOST's own admission gate (memObserve) reads the live
//     config on every call and refuses immediately, so capture stops for
//     ALREADY-RUNNING sandboxes too. A stale in-sandbox marker can only cause
//     an observe attempt the host then refuses; it cannot store a row.
//   - experimental-auto: turning capture ON needs the sandbox to send observe
//     at all, and each sandbox reads its mode once at launch, so this reaches
//     NEW sandboxes only. It ALSO needs a reachable watcher model, which this
//     command does not (and cannot, without a network call) verify — so it
//     names the check instead of claiming the feature is now working.
func memoryCaptureSummary(mode string) string {
	if mode == config.MemoryCaptureExperimentalAuto {
		return fmt.Sprintf("memory_capture = %q (reaches NEW sandboxes only; an already-running one keeps the mode it launched with)\n"+
			"  capture also requires a reachable Ollama watcher model, not checked here: verify with `ollama list` (the model `pix config get memory_watcher_model` names)", mode)
	}
	return fmt.Sprintf("memory_capture = %q (effective immediately: the host refuses automatic capture for already-running sandboxes too)", mode)
}

// ApplyConfigChange applies a set (unset=false) or unset to cfg and returns a
// one-line summary of the new value. Pure + testable: it mutates cfg but does
// NOT save. List keys (mcp, services) take a single value to add/remove; scalar
// keys take a single value on set and reset/clear on unset.
func ApplyConfigChange(cfg *config.Config, unset bool, key string, args []string) (string, error) {
	verb := "set"
	if unset {
		verb = "unset"
	}
	if key == "environment" || key == "environments" || strings.HasPrefix(key, "environments.") {
		return "", environmentKeyRefusal(unset, key)
	}
	switch key {
	case "mcp":
		if len(args) != 1 {
			// No example server is named here on purpose. Pix ships none, so any
			// name in this message would be one particular pack's — which reads
			// as "this works out of the box" to a user whose pack does not
			// declare it. The names a host can actually use are the ones its
			// active pack declares, and `pix doctor` lists them.
			return "", fmt.Errorf("config %s mcp <server>: needs a server name (the names your active pack declares; see pix doctor)", verb)
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

	case "memory_capture":
		if unset {
			cfg.MemoryCapture = config.DefaultMemoryCapture
		} else {
			if len(args) != 1 || !config.ValidMemoryCapture(args[0]) {
				return "", fmt.Errorf("config set memory_capture <mode>: needs exactly one of %s", strings.Join(config.MemoryCaptureModes, "|"))
			}
			cfg.MemoryCapture = args[0]
		}
		return memoryCaptureSummary(cfg.MemoryCapture), nil

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

// environmentKeyRefusal is the fixed refusal for `environment`, the exact
// `environments` key, and every `environments.<name>` key (Story 1, native
// sandbox environments, docs/design/environments.md §5.3): all ARE real
// config-schema fields (config.Config.Environment / .Environments), so
// falling through to the generic "unknown key" error would hide that they
// exist. But they have no hand-edit path — `pix env use` is the only writer
// of the default, and `pix env add`/`pix env forget` (Wave C) are the only
// writers of the registry (`pix env rm` names no working command; it is
// reserved as a future pointer error, never guidance) — so `pix config
// set/unset` refuses every shape outright and names the command that
// actually writes them, rather than silently no-op'ing (the failure mode
// host.enabled's removal already guards against elsewhere in this file).
func environmentKeyRefusal(unset bool, key string) error {
	verb := "set"
	if unset {
		verb = "unset"
	}
	if key == "environment" {
		return fmt.Errorf("config %s environment: the default environment is managed by `pix env use <name>`, never `pix config %s` (see `pix env --help`)", verb, verb)
	}
	// Exactness is decided by the KEY STRING itself (`key == "environments"`),
	// never inferred from the trimmed suffix: `environments.environments` (the
	// registry entry for an environment literally named "environments") also
	// trims down to the substring "environments", so a suffix-equality check
	// would wrongly collapse it into the bare-key case and drop the specific
	// env name from the guidance.
	if key == "environments" {
		if unset {
			return fmt.Errorf("config unset environments: the environment registry is managed by `pix env forget <name>`, never `pix config unset` (see `pix env --help`)")
		}
		return fmt.Errorf("config set environments: the environment registry is managed by `pix env add <name> [path]`, never `pix config set` (see `pix env --help`)")
	}
	name := strings.TrimPrefix(key, "environments.")
	if unset {
		return fmt.Errorf("config unset environments.%s: the environment registry is managed by `pix env forget %s`, never `pix config unset` (see `pix env --help`)", name, name)
	}
	return fmt.Errorf("config set environments.%s: the environment registry is managed by `pix env add %s [path]`, never `pix config set` (see `pix env --help`)", name, name)
}
