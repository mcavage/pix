// pix-host — the single compiled binary for everything that runs on the
// HOST (outside the sandbox). Convention: host code is Go (one static binary, no
// interpreter spawning child processes — the shape EDR trusts); in-sandbox code
// (pi extensions, in-box MCP) is TypeScript.
//
// Subcommands (one per host service):
//
//	memory         self-learning memory store  (:11435, JSON-RPC)
//	               plus its snapshot/restore data-safety primitives
//	mcp <name>     stdio MCP bridge            (run by the sbx gateway)
//	plugin <kind>  built-in go-plugin server   (self-exec, launched by `serve`)
//	serve          run the long-running HTTP services together (memory)
//
// The MCP servers are stdio and spawned by the sbx gateway via `sbx mcp add`
// (see `make mcp-register`), not by `serve`; the gateway runs `mcp <name>`, the
// generic bridge. The old per-server `slack` alias is retired (see retired.go).
//
// `plugin <kind>` is the self-exec entry `serve` launches when config selects a
// non-builtin implementation for a capability slot; it is not meant to be run
// by hand (go-plugin refuses without the handshake cookie).
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
		runMcpSubcommand(os.Args[2:])
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

// runPlugin is the self-exec entry `serve` launches for a non-builtin capability
// slot: it serves the selected built-in implementation as a go-plugin over the
// shared handshake. kind is memory|mcp (mcp also needs a <name>).
func runPlugin(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "pix-host plugin: missing <kind> (memory|mcp)")
		os.Exit(2)
	}
	switch args[0] {
	case "memory":
		servePluginMemory()
	case "mcp":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "pix-host plugin mcp: missing <name>")
			os.Exit(2)
		}
		servePluginMcp(args[1])
	default:
		fmt.Fprintf(os.Stderr, "pix-host plugin: unknown kind %q (memory|mcp)\n", args[0])
		os.Exit(2)
	}
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
  mcp <name>     stdio MCP bridge (run by the sbx gateway)
  plugin <kind>  built-in go-plugin server, self-exec (memory|mcp)
  serve          run the long-running HTTP services (memory)
`
}

// --- small shared helpers ----------------------------------------------------

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
