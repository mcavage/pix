package main

import (
	"errors"

	"pi-stack/host/config"
)

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
	"help": true, "serve": true, "doctor": true, "onboard": true, "setup": true, "status": true,
	"ls": true, "rm": true,
	"config": true, "mcp": true, "gog": true, "memory": true, "monitor": true, "knowledge": true,
	"pack": true, "version": true, "run": true, "secret": true,
	"reset": true, "uninstall": true, "man": true,
	"backup": true, "restore": true, "state": true,
	"task": true, "route": true, "agent": true,
	"host": true,
}

// suggestVerb returns the closest known verb to input within edit distance 2,
// used to power the did-you-mean hint on an unknown command.
func suggestVerb(input string) (string, bool) {
	best, bestD := "", 3
	for v := range knownVerbs {
		if d := levenshtein(input, v); d < bestD {
			best, bestD = v, d
		}
	}
	return best, best != ""
}

// levenshtein is the classic edit distance (insert/delete/substitute), used by
// suggestVerb to rank near-miss verb typos.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// helpAllText is the full command listing revealed by `pi-stack help --all`. It
// names every verb across all tiers (Core / occasional / rare + expert) so a
// power user can see the whole surface, not just the curated Core view.
const helpAllText = `pi-stack: a personal, multi-model pi coding agent in a Docker sandbox.

Usage:  pi-stack <command> [args]

New here?   pi-stack setup     configure the host, then hand off to an agent for a guided tour

Workflow
  run [DIR]           launch the sandbox in DIR (default: .). This is the main one.
  ls [--json]         list your pi-stack sandboxes (name, state, dir)
  rm <name>...        remove pi-stack sandboxes (--all [--except <name>])
  serve [args...]     start the host services (memory, knowledge); serve stop|status
  status              what is up, what is down, what is next   (also the bare command)

Setup & health
  setup               guided onboarding: host config, then agent handoff for a guided tour
  onboard             host-side config only (flags/CI); no agent handoff
  doctor              diagnose host + sandbox health, print the fix commands

Data
  memory <cmd>        recall | remember | forget | learnings | stats   (:11435)
  knowledge <cmd>     init | use | ls | query | sync | remote          (:11436)
  pack <cmd>          new | add | ls | show | use | rm (git-backed context bundle)

Observability
  monitor [name]       live-follow a sandbox's out-of-sandbox traffic (:11437)

Models & agents (cost/latency/accuracy routing)
  agent <cmd>         ls | new | edit | rm | reassess (subagents as objects)
  route <cmd>         pick | compile | show | models (intent -> model)

Config & context
  config show|path    show the resolved config path and contents
  config get K        print one resolved value (for scripts/make)
  config set|unset    change config without hand-editing the toml

Parallel work
  task <cmd>          new | ls | path | rm | gc | harvest: parallel task clones of one repo

Integrations & credentials
  mcp <cmd>           register|ls|load|auth|bundle MCP servers (sbx gateway)
  secret <cmd>        ls|set|rm|check the 1Password op-refs (host MCP creds)

State (on-disk lifecycle)
  state <cmd>         backup|restore|reset|uninstall (grouped aliases)
  backup [--out P]    hot FULL backup (memory + config + op-refs) -> tar.gz
  restore <archive>   restore a FULL backup (safe swap)   [--force]
  reset [flags]       move stack state aside (reversible)   [--keep-memory --sbx --yes]
  uninstall [flags]   reset, then remove the bin symlinks    [--keep-memory --yes]

Expert (dangerous; read the man page first)
  host [DIR]          run pi DIRECTLY on this machine: no sandbox, no network
                      fence, real credentials. Gated off by default; enable with
                      'host setup' (provisions AND enables it in one step).
                      Guardrails, not a security boundary.

Meta
  version             print the launcher version
  man                 render the embedded man page (no MANPATH needed; also --man)
  help [verb]         print this help (or a verb's usage)

run flags:    --dev --skills DIR --kit K --mcp M --name N --model M -- pi-args...
`

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
	case "ls":
		return lsUsage, true
	case "rm":
		return rmUsage, true
	case "doctor":
		return doctorUsage, true
	case "onboard":
		return onboardUsage, true
	case "setup":
		return setupUsage, true
	case "config":
		return configUsage, true
	case "mcp":
		return mcpUsage, true
	case "gog":
		return gogUsage, true
	case "pack":
		return packUsage, true
	case "memory", "mem":
		return memoryUsage + "\n", true
	case "monitor":
		return monitorUsage, true
	case "backup":
		return backupUsage, true
	case "restore":
		return restoreUsage, true
	case "knowledge", "kb":
		return knowledgeUsage, true
	case "secret":
		return secretUsage, true
	case "version":
		return versionUsage, true
	case "man":
		return manUsage, true
	case "reset":
		return resetUsage, true
	case "uninstall":
		return uninstallUsage, true
	case "state":
		return stateUsage, true
	case "task":
		return taskUsage, true
	case "host":
		return hostUsage, true
	case "route":
		return routeUsage(), true
	case "agent":
		return agentUsage, true
	}
	return "", false
}

const serveUsage = `usage: pi-stack serve [args...]
       pi-stack serve start   (alias: install)
       pi-stack serve stop
       pi-stack serve status [--json]
       pi-stack serve install
       pi-stack serve uninstall

Run the long-running host services (execs the sibling pi-stack-host serve):
memory (:11435) and knowledge (:11436, when enabled). Any args are passed
through to pi-stack-host serve unchanged.

You usually do NOT need to run this yourself: pi-stack run / memory /
knowledge query auto-start a detached serve when its ports are down (lazy
auto-start; logs in ~/.local/state/pi-stack/serve.log). Opt out with
PI_STACK_NO_AUTOSERVE=1 or 'pi-stack config set host.autoserve false'.

subcommands:
  stop              stop a running 'pi-stack-host serve' (safe: verifies the
                    process is ours before signalling; SIGTERM then SIGKILL if
                    it doesn't exit). Mode-aware: a MANAGED service (launchd/
                    systemd) is stopped via its supervisor so KeepAlive/Restart=
                    can't respawn it; if the pidfile is missing it falls back to
                    discovering a verified 'pi-stack-host serve' (e.g. an orphan
                    left after 'pi-stack reset' moved the config dir).
  status [--json]   report whether serve is running (pid) and which service
                    ports (:11435 / :11436) are up
  start             alias for 'install'; (re)start the managed service, picking
                    up a freshly-rebuilt binary. The partner to 'stop'.
  install           install serve as a managed login service (launchd on macOS,
                    systemd --user on Linux): starts at login, auto-restarts.
                    stops a lazily-started daemon first; refuses over a
                    foreground serve. captures install-time env into the unit
                    (PI_STACK_CONFIG always; XDG_CONFIG_HOME, MEMORY_DB,
                    MEMORY_PORT, KNOWLEDGE_PORT, OLLAMA_HOST when set) and
                    verifies the service came up.
                    logs: ~/.local/state/pi-stack/serve.log (same file the
                    lazy auto-start uses, on both macOS and Linux)
  uninstall         remove the managed login service
`

const statusUsage = `usage: pi-stack status [--json]

Fast, read-only control panel: services, provider keys, knowledge bundles,
MCP registration, and running pi-stack-* sandboxes. Launches nothing.

flags:
  --json   emit the machine-readable status snapshot
`

const doctorUsage = `usage: pi-stack doctor [--json] [--verbose]

Diagnose host + sandbox health (provider keys, ollama/models, memory, gog, mcp),
leading with a one-line verdict and copy-pasteable TODO commands. The default
output is concise (verified-ready checks collapse per group); --verbose shows
every check.

flags:
  --json      emit the machine-readable report (schema_version 2)
  --verbose   show every check, including verified-ready detail

exit codes:
  0  ready, or only optional/unverifiable gaps (nothing verified-broken that
     pi-stack requires)
  1  a positively verified core failure (or the config failed to load)
  2  usage error
`

const configUsage = `usage: pi-stack config <show|path|get|set|unset> [args]

  show                     print the resolved config path + contents
  path [op-refs]           print the config file path (or the op-refs.env path)
  get K                    print ONE resolved value, no decoration (lists are
                            space-separated); for scripts/make to source
  set K V                   set a config key (never hand-edit the toml)
  unset K [V]               reset/clear a scalar key, or remove value V from a
                            list key (mcp/services/knowledge_bundles)

` + configKeysHelp

const mcpUsage = `usage: pi-stack mcp <register|ls|load|auth|bundle> [args]

  register [name...]   register local stdio MCP servers with the sbx gateway
                       (no names = every local server in the resolved mcp list)
  ls                   list servers registered with the gateway (sbx mcp ls)
  load <name> [DIR]    attach an already-registered server to the RUNNING sandbox
                       for DIR (default cwd); live, no recreate (sbx mcp load)
  auth [args...]       authorize remote OAuth servers via the hosted control
                       plane (sbx mcp auth; e.g. auth --all, auth status --all)
  bundle [ls|rm ...]   register the shipped public catalog bundle
                       (notion/atlassian/granola) in one step; ls/rm forward to
                       sbx mcp bundle. Then: pi-stack mcp auth --all
`

const knowledgeUsage = `usage: pi-stack knowledge <init|use|ls|query|sync|remote> [args]

  init [DIR]                     scaffold + wire a global OKF bundle
  use <path|url>                 point the global KB at a bundle (path made
                                 absolute; not checked for existence/OKF)
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

// secretHelpBody is the mental model reused verbatim from config so the concept
// reads identically in setup, doctor, the template header, and `secret -h`.
const secretUsage = `usage: pi-stack secret <ls|set|rm|check|sync>

Manage the 1Password refs (op-refs.env) the sbx gateway resolves for host MCP
servers AND the cloud model provider keys. Values live in 1Password, never on
disk; this verb only reads, writes, and reports REFS (op://vault/item/field
lines). It never writes a resolved secret.

` + config.OpRefsMentalModel + `

  ls                       op installed? signed in? which refs are filled vs
                           placeholder (the default; prints no secret values)
  set ENV_VAR op://ref     upsert a ref (seeds op-refs.env if absent); a raw
                           space in the ref is URL-encoded to %20
  rm ENV_VAR               remove a ref (a no-op if it isn't set)
  check                    resolve each op:// ref with "op read" and report
                           OK/FAIL per key (never prints the resolved value)
  sync                     FORCE re-resolve every provider-key ref
                           (ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY)
                           into sbx (rotate/repair). You normally never run this:
                           run and setup auto-resolve a MISSING key from its ref
                           by themselves. Never prints or stores a value.

The file lives at the absolute XDG path: see "pi-stack config path op-refs".
`

const versionUsage = `usage: pi-stack version

Print the stamped launcher version.
`

const resetUsage = `usage: pi-stack reset [--keep-memory] [--sbx] [--yes] [--force]

Reset the stack to a clean slate (REVERSIBLE). Nothing is hard-deleted: state is
moved aside to a timestamped <path>.bak-<unixts> sibling you can rename back.

Moves aside the config dir (~/.config/pi-stack) and the data dir (~/.local/share/pi-stack:
captured memory + the knowledge index). Best-effort stops a running
'pi-stack-host serve' first.

flags:
  --keep-memory   preserve ~/.local/share/pi-stack/memory (your captured facts); reset the rest
  --sbx           also remove every pi-stack-* sandbox and unregister the
                  configured local MCP servers (provider secrets are left alone)
  --force         move the data dir even if 'pi-stack-host serve' still appears
                  to be running (otherwise the data move is refused to avoid
                  splitting a live sqlite db from its wal)
  --yes, -y       don't prompt (REQUIRED on a non-interactive terminal)

Without --yes on a TTY it prints exactly what will move and prompts before acting.
On a non-TTY it refuses unless --yes is given.
`

const uninstallUsage = `usage: pi-stack uninstall [--keep-memory] [--purge-data] [--yes] [--force]

Run the full reset (see 'pi-stack reset'), THEN remove the installed pi-stack +
pi-stack-host bin symlinks (~/.local/bin). Only symlinks are removed; a real
file there is left untouched. State is moved aside, never hard-deleted.

Harvested task artifacts (~/.local/share/pi-stack/artifacts) are user work
product and are KEPT by default (their path + size are printed). Pass
--purge-data to move them aside too.

flags:
  --keep-memory   preserve ~/.local/share/pi-stack/memory (your captured facts)
  --purge-data    also move aside harvested task artifacts (kept by default)
  --force         move the data dir even if 'pi-stack-host serve' still appears
                  to be running
  --yes, -y       don't prompt (REQUIRED on a non-interactive terminal)
`
