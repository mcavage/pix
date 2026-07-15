package main

import "errors"

// errHelpRequested is the shared sentinel a parser returns when the argv asks
// for help (a leading -h/--help). Callers print the relevant usage to STDOUT
// and exit 0, distinguishing a help request from a usage ERROR (stderr, exit 2).
var errHelpRequested = errors.New("help requested")

// wantsHelp reports whether argv requests help: a -h or --help token appears
// before any `--` terminator (everything after `--` is passthrough and must not
// be scanned for help). This is the single shared contract every verb + its
// subcommands use so `<verb> -h` / `<verb> --help` always prints usage + exit 0.
func wantsHelp(argv []string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

// knownVerbs is the set of top-level verbs, used to suggest a fix when a bare
// positional (a would-be run DIR) is actually a mistyped verb.
var knownVerbs = map[string]bool{
	"help": true, "serve": true, "doctor": true, "setup": true, "status": true,
	"config": true, "mcp": true, "memory": true, "knowledge": true,
	"profile": true, "version": true, "run": true,
}

// verbUsage maps a verb (including its aliases) to its usage text, so
// `pi-stack help <verb>` can route to each verb's own help. The returned string
// is newline-terminated and ready to Print. ok is false for an unknown verb.
func verbUsage(verb string) (string, bool) {
	switch verb {
	case "run":
		return runUsage, true
	case "serve":
		return serveUsage, true
	case "status", "st":
		return statusUsage, true
	case "doctor":
		return doctorUsage, true
	case "setup":
		return setupUsage, true
	case "config":
		return configUsage, true
	case "mcp":
		return mcpUsage, true
	case "memory", "mem":
		return memoryUsage + "\n", true
	case "knowledge", "kb":
		return knowledgeUsage, true
	case "profile":
		return profileUsage, true
	case "version":
		return versionUsage, true
	}
	return "", false
}

const serveUsage = `usage: pi-stack serve [args...]

Run the long-running host services (execs the sibling pi-stack-host serve):
memory (:11435) and knowledge (:11436, when enabled). Any args are passed
through to pi-stack-host serve unchanged.
`

const statusUsage = `usage: pi-stack status [--json]

Fast, read-only control panel — services, provider keys, knowledge bundles,
MCP registration, and running pi-stack-* sandboxes. Launches nothing.

flags:
  --json   emit the machine-readable status snapshot
`

const doctorUsage = `usage: pi-stack doctor [--json]

Diagnose host + sandbox health (provider keys, ollama/models, memory, gog, mcp),
leading with a one-line verdict and copy-pasteable TODO commands.

flags:
  --json   emit the machine-readable report
`

const setupUsage = `usage: pi-stack setup [flags]

Guided, idempotent first-run setup: writes config and registers MCP servers.

flags:
  --account <email>          set the Google Workspace (gog) account
  --knowledge <path|url>     set up the global knowledge base
  --yes, --non-interactive   never prompt; print outstanding steps as commands
`

const configUsage = `usage: pi-stack config <show|path|set|unset> [args]

  show                     print the resolved config path + contents
  path                     print the config file path
  set [--profile N] K V    set a config key (never hand-edit the toml)
  unset [--profile N] K    reset/clear a config key

` + configKeysHelp

const mcpUsage = `usage: pi-stack mcp <register|ls> [name...]

  register [name...]   register local stdio MCP servers with the sbx gateway
                       (no names = every local server in the resolved mcp list)
  ls                   list servers registered with the gateway (sbx mcp ls)
`

const knowledgeUsage = `usage: pi-stack knowledge <init|use|ls|query|sync|remote> [args]

  init [DIR]                     scaffold + wire a global OKF bundle
  use <path|url>                 point the global KB at an existing bundle
  use --project <path|url> [--dir D]   write a per-repo .pi-stack/knowledge pointer
  ls [--json]                    list configured bundles + daemon health
  query <text...> [--limit N] [--json]   search the knowledge daemon (:11436)
  sync [-m MSG] [--bundle D] [--allow-main]   commit + push the bundle
  remote [set <url>] [--bundle D]   show or set the bundle's git remote
`

const knowledgeInitUsage = `usage: pi-stack knowledge init [DIR]

Scaffold a spec-correct OKF bundle (default <config-dir>/knowledge), git-init it,
and wire it into config (services += knowledge, knowledge_bundles += DIR).
Idempotent: never clobbers an existing bundle.
`

const profileUsage = `usage: pi-stack profile <ls|use> [name]

  ls [--json]      list profiles (* = active)
  use <name>       set the active profile (use "default" to revert to the base)
`

const versionUsage = `usage: pi-stack version

Print the stamped launcher version.
`
