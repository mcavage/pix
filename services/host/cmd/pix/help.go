package main

import (
	"errors"

	"pix/host/config"
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
	"help": true, "serve": true, "doctor": true, "setup": true, "status": true,
	"ls": true, "rm": true,
	"config": true, "mcp": true, "gworkspace": true, "slack": true, "memory": true, "monitor": true, "knowledge": true,
	"pack": true, "version": true, "run": true, "secret": true,
	"reset": true, "uninstall": true, "man": true,
	"backup": true, "restore": true, "state": true,
	"task": true, "route": true, "agent": true,
	"host": true,
}

// retiredVerbs maps a DELETED verb to the command that replaced it. Edit
// distance cannot find these — `onboard` is nowhere near `setup` — but a user
// (or a shell history, or a CI script) who types the old name deserves the new
// one rather than a bare "no command named". The verb itself stays deleted:
// this is a suggestion on the standard unknown-verb path (exit 2), NOT an
// alias that keeps the old door open.
var retiredVerbs = map[string]string{
	// AC-P0-308: `pix onboard` became `pix setup --no-agent`.
	"onboard": "setup --no-agent",
	// The `gog` verb tree is DELETED, not renamed (6b39a69): the public surface
	// is `pix gworkspace setup|status|disable`. `gog` and `gworkspace` are not
	// within edit distance 2, so this is a retired-verb entry, not something
	// suggestVerb's levenshtein search would ever find on its own.
	"gog": "gworkspace",
}

// suggestVerb returns the replacement for a retired verb, or else the closest
// known verb to input within edit distance 2 — the did-you-mean hint on an
// unknown command.
func suggestVerb(input string) (string, bool) {
	if r, ok := retiredVerbs[input]; ok {
		return r, true
	}
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

// helpAllText is the full command listing revealed by `pix help --all`. It
// names every verb across all tiers (Core / occasional / rare + expert) so a
// power user can see the whole surface, not just the curated Core view.
const helpAllText = `pix: a personal, multi-model pi coding agent in a Docker sandbox.

Usage:  pix <command> [args]

New here?   pix setup     configure the host, then hand off to an agent for a guided tour

Workflow
  run [DIR]           launch the sandbox in DIR (default: .). This is the main one.
  ls [--json]         list your pix sandboxes (name, state, dir)
  rm <name>...        remove pix sandboxes (--all [--except <name>])
  serve [args...]     start the host services (memory, knowledge); serve stop|status
  status              what is up, what is down, what is next   (also the bare command)

Setup & health
  setup               guided onboarding: host config, then agent handoff for a guided tour
  setup --no-agent    host-side config only (flags/CI); no sandbox, no handoff
  doctor              diagnose host + sandbox health, print the fix commands
  upgrade [flags]     replace the pix + pix-host binaries with a published
                      release (sha256-verified). run already tracks the latest
                      kit/image itself.            [--check --version X.Y.Z --force]

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
  gworkspace <cmd>    setup | status | disable: Google Workspace access
  slack <cmd>         setup | auth | status | disable: Slack OAuth and access
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

run flags:    --dev --skills DIR --kit K --template REF --mcp M --name N --model M -- pi-args...
`

// verbUsage maps a verb (including its aliases) to its usage text, so
// `pix help <verb>` can route to each verb's own help. The returned string
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
	case "setup":
		return setupUsage, true
	case "config":
		return configUsage, true
	case "mcp":
		return mcpUsage, true
	case "gworkspace":
		return gworkspaceUsage, true
	case "slack":
		return slackUsage, true
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
	case "upgrade":
		return upgradeUsage, true
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

const serveUsage = `usage: pix serve [args...]
       pix serve start   (alias: install)
       pix serve stop
       pix serve status [--json]
       pix serve install
       pix serve uninstall

Run the long-running host services (execs the sibling pix-host serve):
memory (:11435) and knowledge (:11436, when enabled). Any args are passed
through to pix-host serve unchanged.

You usually do NOT need to run this yourself: pix run / memory /
knowledge query auto-start a detached serve when its ports are down (lazy
auto-start; logs in ~/.local/state/pix/serve.log). Opt out with
PIX_NO_AUTOSERVE=1 or 'pix config set host.autoserve false'.

subcommands:
  stop              stop a running 'pix-host serve' (safe: verifies the
                    process is ours before signalling; SIGTERM then SIGKILL if
                    it doesn't exit). Mode-aware: a MANAGED service (launchd/
                    systemd) is stopped via its supervisor so KeepAlive/Restart=
                    can't respawn it; if the pidfile is missing it falls back to
                    discovering a verified 'pix-host serve' (e.g. an orphan
                    left after 'pix reset' moved the config dir).
  status [--json]   report whether serve is running (pid) and which service
                    ports (:11435 / :11436) are up
  start             alias for 'install'; (re)start the managed service, picking
                    up a freshly-rebuilt binary. The partner to 'stop'.
  install           install serve as a managed login service (launchd on macOS,
                    systemd --user on Linux): starts at login, auto-restarts.
                    stops a lazily-started daemon first; refuses over a
                    foreground serve. captures install-time env into the unit
                    (PIX_CONFIG always; XDG_CONFIG_HOME, MEMORY_DB,
                    MEMORY_PORT, KNOWLEDGE_PORT, OLLAMA_HOST when set) and
                    verifies the service came up.
                    logs: ~/.local/state/pix/serve.log (same file the
                    lazy auto-start uses, on both macOS and Linux)
  uninstall         remove the managed login service
`

const statusUsage = `usage: pix status [--json]

Fast, read-only control panel: services, provider keys, knowledge bundles,
MCP registration, and running pix-* sandboxes. Launches nothing.

flags:
  --json   emit the machine-readable status snapshot
`

const doctorUsage = `usage: pix doctor [--json] [--verbose]

Diagnose host + sandbox health (provider keys, ollama/models, memory, Google Workspace, mcp),
leading with a one-line verdict and copy-pasteable TODO commands. The default
output is concise (verified-ready checks collapse per group); --verbose shows
every check.

flags:
  --json      emit the machine-readable report (schema_version 2)
  --verbose   show every check, including verified-ready detail

exit codes:
  0  ready, or only optional/unverifiable gaps (nothing verified-broken that
     pix requires)
  1  a positively verified core failure (or the config failed to load)
  2  usage error
`

const configUsage = `usage: pix config <show|path|get|set|unset> [args]

  show                     print the resolved config path + contents
  path [op-refs]           print the config file path (or the op-refs.env path)
  get K                    print ONE resolved value, no decoration (lists are
                            space-separated); for scripts/make to source
  set K V                   set a config key (never hand-edit the toml)
  unset K [V]               reset/clear a scalar key, or remove value V from a
                            list key (mcp/services/knowledge_bundles)

` + configKeysHelp

const mcpUsage = `usage: pix mcp <register|ls|load|auth|bundle> [args]

  register [name...]   register local stdio MCP servers with the sbx gateway
                       (no names = every local server in the resolved mcp list)
  ls                   list servers registered with the gateway (sbx mcp ls).
                       HOST registration only, not what's attached to your
                       current sandbox; see 'pix status'/'pix doctor' for
                       what's live, 'pix mcp load' to attach one now, or
                       'pix run --replace' to recreate with everything preloaded
  load <name> [DIR]    attach an already-registered server to the RUNNING sandbox
                       for DIR (default cwd); live, no recreate (sbx mcp load)
  auth [args...]       authorize remote OAuth servers via the hosted control
                       plane (sbx mcp auth; e.g. auth --all, auth status --all)
  bundle [ls|rm ...]   register the shipped public catalog bundle
                       (notion/atlassian/granola) in one step; ls/rm forward to
                       sbx mcp bundle. Then: pix mcp auth --all
`

const knowledgeUsage = `usage: pix knowledge <init|use|ls|query|sync|remote> [args]

  init [DIR]                     scaffold + wire a global OKF bundle
  use <path|url>                 point the global KB at a bundle (path made
                                 absolute; not checked for existence/OKF)
  use --project <path|url> [--dir D]   write a per-repo .pix/knowledge pointer
  ls [--json]                    list configured bundles + daemon health
  query <text...> [--limit N] [--json]   search the knowledge daemon (:11436)
  sync [-m MSG] [--bundle D] [--allow-main]   commit + push the bundle
  remote [set <url>] [--bundle D]   show or set the bundle's git remote
`

const knowledgeInitUsage = `usage: pix knowledge init [DIR]

Scaffold a spec-correct OKF bundle (default <config-dir>/knowledge), git-init it,
and wire it into config (services += knowledge, knowledge_bundles += DIR).
Idempotent: never clobbers an existing bundle.
`

// secretHelpBody is the mental model reused verbatim from config so the concept
// reads identically in setup, doctor, the template header, and `secret -h`.
const secretUsage = `usage: pix secret <ls|set|rm|check|sync>

Manage the 1Password refs (op-refs.env) the sbx gateway resolves for host MCP
servers AND the cloud model provider keys. Values live in 1Password, never on
disk; this verb only reads, writes, and reports REFS (op://vault/item/field
lines). It never writes a resolved secret.

` + config.OpRefsMentalModel + `

  ls                       op installed? signed in? which refs are filled vs
                           placeholder (the default; prints no secret values)
  set ENV_VAR op://ref     upsert a ref (seeds op-refs.env if absent); a raw
                           space in the ref (a field name like "api key") is
                           kept as a literal space (op read/op run reject %20)
  rm ENV_VAR               remove a ref (a no-op if it isn't set)
  check                    resolve each op:// ref with "op read" and report
                           OK/FAIL per key (never prints the resolved value)
  sync                     FORCE re-resolve every provider-key ref
                           (ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY)
                           into sbx (rotate/repair). You normally never run this:
                           run and setup auto-resolve a MISSING key from its ref
                           by themselves. Never prints or stores a value.

The file lives at the absolute XDG path: see "pix config path op-refs".
`

const versionUsage = `usage: pix version

Print the stamped launcher version.
`

const resetUsage = `usage: pix reset [--keep-memory] [--sbx] [--yes] [--force]

Reset the stack to a clean slate (REVERSIBLE). Nothing is hard-deleted: state is
moved aside to a timestamped <path>.bak-<unixts> sibling you can rename back.

Moves aside the config dir (~/.config/pix) and the data dir (~/.local/share/pix:
captured memory + the knowledge index). Best-effort stops a running
'pix-host serve' first.

flags:
  --keep-memory   preserve ~/.local/share/pix/memory (your captured facts); reset the rest
  --sbx           also remove every pix-* sandbox and unregister the
                  configured local MCP servers (provider secrets are left alone)
  --force         move the data dir even if 'pix-host serve' still appears
                  to be running (otherwise the data move is refused to avoid
                  splitting a live sqlite db from its wal)
  --yes, -y       don't prompt (REQUIRED on a non-interactive terminal)

Without --yes on a TTY it prints exactly what will move and prompts before acting.
On a non-TTY it refuses unless --yes is given.
`

const uninstallUsage = `usage: pix uninstall [--keep-memory] [--purge-data] [--yes] [--force]

Run the full reset (see 'pix reset'), THEN remove the installed pix +
pix-host bin symlinks (~/.local/bin). Only symlinks are removed; a real
file there is left untouched. State is moved aside, never hard-deleted.

Harvested task artifacts (~/.local/share/pix/artifacts) are user work
product and are KEPT by default (their path + size are printed). Pass
--purge-data to move them aside too.

flags:
  --keep-memory   preserve ~/.local/share/pix/memory (your captured facts)
  --purge-data    also move aside harvested task artifacts (kept by default)
  --force         move the data dir even if 'pix-host serve' still appears
                  to be running
  --yes, -y       don't prompt (REQUIRED on a non-interactive terminal)
`
