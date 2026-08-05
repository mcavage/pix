// pix-host — the single compiled binary for everything that runs on the
// HOST (outside the sandbox). Convention: host code is Go (one static binary, no
// interpreter spawning child processes — the shape EDR trusts); in-sandbox code
// (pi extensions, in-box MCP) is TypeScript.
//
// Subcommands (one per host service):
//
//	memory         self-learning memory store  (:11435, JSON-RPC)
//	               plus its snapshot/restore data-safety primitives
//	mcp --list     the local stdio MCP servers this binary serves: NONE
//	plugin memory  built-in go-plugin server   (self-exec, launched by `serve`)
//	serve          run the long-running HTTP services together (memory)
//
// The stdio MCP BRIDGE is gone (U11j). It served zero built-in servers: slack
// was externalized (W2/U02a) and the write-scoped google-docs-create companion
// retired with the built-in Workspace wizard (W2/U02B), so every line of
// dispatcher/stdio/tool-schema scaffolding, and the go-plugin MCP transport
// behind it, existed to serve an empty name set. A local stdio server now
// either registers as a container the sbx gateway runs, or arrives as a
// pack-trust-admitted [[services]] unit. What survives is `mcp --list`, and
// only because it is a CONTRACT: `pix mcp register` / doctor read it as the
// source of truth for "is <name> served locally by this binary", and they FAIL
// CLOSED on a failed probe (see mcp.LocalMCPNames) — so it still answers,
// exit 0, with the empty set it has always printed.
//
// `plugin <kind>` is the self-exec entry `serve` launches for a capability
// slot; it is not meant to be run by hand (go-plugin refuses without the
// handshake cookie).
//
// Company-specific integrations (a data-warehouse exec proxy, an HR-directory MCP)
// are never compiled into this binary. They ship as a **pack** (skills/knowledge/
// config), a **container** MCP server the sbx gateway runs, or a standalone host
// daemon — see docs/design/packs.md. The only host-side extension point that
// remains is the generic, SHA-pinned [plugins.*] external-process mechanism
// (serve_plugin.go): an operator points a capability slot at an external binary
// (path + sha256), and the supervisor launches it as a go-plugin subprocess.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// version is stamped at build time via -ldflags "-X main.version=..." for both
// release and local builds. Used for launcher/host compatibility checks.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version)
	case "mcp":
		runMcpNames(os.Args[2:])
	case "plugin":
		runPlugin(os.Args[2:])
	case "memory":
		runMemoryHost(os.Args[2:])
	case "route":
		runRouteHost(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		// A retired subcommand answers here (already exit 2) with its replacement.
		if notice, retired := retiredHostNotice(os.Args[1:]); retired {
			fmt.Fprint(os.Stderr, notice)
		} else {
			fmt.Fprintf(os.Stderr, "pix-host: unknown subcommand %q\n\n", os.Args[1])
			usage()
		}
		os.Exit(2)
	}
}

// runMcpNames answers `pix-host mcp --list` (alias `list`): the names this
// binary can serve as a local stdio MCP server, one per line. That set is
// EMPTY and printing it is the whole point — the launcher and doctor partition
// configured servers into local-vs-remote from this output, and treat a failed
// probe as unknown (fail closed, register nothing). Exit 0 with no lines is the
// honest "I serve none of them"; exiting 2 here would make every remote catalog
// server look host-executing.
//
// Any other argv is a caller still asking for the retired bridge, and says so.
func runMcpNames(args []string) {
	if len(args) == 1 && (args[0] == "--list" || args[0] == "list") {
		return // the empty set, exit 0
	}
	fmt.Fprintln(os.Stderr, "pix-host mcp: this binary serves no built-in MCP server (only `mcp --list`).")
	fmt.Fprintln(os.Stderr, "An MCP server ships as a container the sbx gateway runs, or as a pack [[services]] unit.")
	os.Exit(2)
}

// runPlugin is the self-exec entry `serve` launches for a capability slot: it
// serves the built-in implementation as a go-plugin over the shared handshake.
// memory is the only dispensable kind (plugin.PluginMap is the closed set).
func runPlugin(args []string) {
	if len(args) == 1 && args[0] == "memory" {
		servePluginMemory()
		return
	}
	fmt.Fprintln(os.Stderr, "pix-host plugin: usage: pix-host plugin memory")
	os.Exit(2)
}

func usage() { fmt.Fprint(os.Stderr, usageText()) }

// usageText is the host binary's whole discoverable surface, split out from
// usage() so the retirement test can assert a retired subcommand is not
// advertised here.
func usageText() string {
	return `pix-host — host-side services for pix

usage: pix-host <subcommand>

subcommands:
  version        print the stamped host-binary version
  memory         self-learning memory store, JSON-RPC (:11435)
  route <cmd>    model router: pick | compile | show | models
  mcp --list     local stdio MCP servers this binary serves (none)
  plugin memory  built-in go-plugin server, self-exec
  serve          run the long-running HTTP services (memory)
`
}

// --- small shared helpers ----------------------------------------------------
//
// What is left of the former util.go: the params/JSON helpers the memory
// JSON-RPC surface reads. The MCP stdio scaffolding that shared the file
// (frame reader, dispatcher, tool schemas) went with the bridge, and the
// form-post/JSON-object helpers went with the built-in servers that used them.

// jsonObj is the JSON-RPC wire shape: an unmarshalled object.
type jsonObj = map[string]any

// getStr reads a string param, "" when absent or another type.
func getStr(m jsonObj, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// clampInt reads a numeric param (JSON number, int or numeric string) and
// clamps it into [lo,hi], falling back to def when it is absent or unparseable.
// Every caller is a store query bound, so an out-of-range value is clamped
// rather than refused.
func clampInt(v any, def, lo, hi int) int {
	n := def
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	case string:
		if p, err := strconv.Atoi(x); err == nil {
			n = p
		}
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return n
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
