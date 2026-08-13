// pix-host — the single compiled binary for everything that runs on the HOST
// (outside the sandbox). Convention: host code is Go, in-sandbox code (pi extensions, in-box MCP) is TypeScript — see AGENTS.md.
//
// Subcommands: memory (:11435 JSON-RPC store + snapshot/restore), route (model
// router CLI), mcp --list (local stdio servers: NONE), plugin memory (built-in
// go-plugin server, self-exec via `serve`), serve (the long-running services).
//
// Company-specific integrations are never compiled in: they ship as a pack, as
// a container MCP server the sbx gateway runs, or as a standalone host daemon
// (docs/design/packs.md). The only host-side extension point is the generic,
// SHA-pinned [plugins.*] external-process mechanism (serve_plugin.go).
//
// Cross-cutting rationale (lock ordering, readiness identity, restore commit
// ordering): docs/design/runtime-invariants.md.

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
	case "uat-mcp":
		runUatMcp(os.Args[2:])
	case "uat-browser-open":
		runUatBrowserOpen(os.Args[2:])
	case "route":
		runRouteHost(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "pix-host: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// runMcpNames answers `pix-host mcp --list` (alias `list`): the names this
// binary serves as a local stdio MCP server, one per line. That set is EMPTY
// and printing it is the whole point — the launcher/doctor partition servers
// into local-vs-remote from this output. Exit 0 with no lines is honest;
// exit 2 would make every remote catalog server look host-executing.
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

// usageText is the host binary's whole discoverable surface.
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

// --- small shared helpers: the params/JSON helpers the memory JSON-RPC
// surface reads ---------------------------------------------------------------

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
// clamps it into [lo,hi], falling back to def when absent or unparseable. Every
// caller is a store query bound, so out-of-range is clamped, not refused.
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
