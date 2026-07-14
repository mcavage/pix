// pi-stack-host — the single compiled binary for everything that runs on the
// HOST (outside the sandbox). Convention: host code is Go (one static binary, no
// interpreter spawning child processes — the shape EDR trusts); in-sandbox code
// (pi extensions, in-box MCP) is TypeScript.
//
// Subcommands (one per host service):
//
//	memory         self-learning memory store  (:11435, JSON-RPC)
//	knowledge      OKF knowledge retrieval idx (:11436, JSON-RPC)
//	mcp <name>     stdio MCP bridge            (run by the sbx gateway)
//	slack          alias for `mcp slack`       (stdio; run by the sbx gateway)
//	plugin <kind>  built-in go-plugin server   (self-exec, launched by `serve`)
//	serve          run the long-running HTTP services together (memory, knowledge)
//
// The MCP servers are stdio and spawned by the sbx gateway via `sbx mcp add`
// (see `make mcp-register`), not by `serve`; the gateway now runs `mcp <name>`
// (the generic bridge), of which `slack` is a back-compatible alias.
//
// `plugin <kind>` is the self-exec entry `serve` launches when config selects a
// non-builtin implementation for a capability slot; it is not meant to be run
// by hand (go-plugin refuses without the handshake cookie).
//
// Company-specific integrations (a data-warehouse exec proxy, an HR-directory MCP)
// live in a private overlay: when their source files are present in the build they
// self-register here via init(); the public tree ships without them.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"pi-stack/host/plugin"
)

// Overlay subcommands/services self-register here via init() when their (private,
// gitignored) source files are present in the build. Empty in the public tree.
var (
	extraCommands         = map[string]func(){}
	extraUsage            []string
	extraServiceFactories []func() hostService
	// extraServiceAliases maps a config-friendly SERVICES name to the internal
	// service name a factory registers (e.g. a short "warehouse" -> "warehouse-proxy").
	// Overlay plugins add their own here so the public tree never names one.
	extraServiceAliases = map[string]string{}
	// extraBrokerFactory lets an overlay register a BUILT-IN CredentialBroker
	// served over the `plugin broker` self-exec path. nil in the public tree —
	// there is no built-in broker (the built-in Google broker was removed), so the broker slot is
	// overlay-only and the seam stays dormant. An external overlay broker binary
	// (see examples/broker-example) ships its own main() and does not use this.
	extraBrokerFactory func() plugin.CredentialBroker
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "slack":
		// Back-compat alias: the Slack MCP is now served through the generic
		// stdio bridge (behaviourally identical to the old runSlack()).
		runMcpBridge("slack")
	case "mcp":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "pi-stack-host mcp: missing <name>")
			os.Exit(2)
		}
		runMcpBridge(os.Args[2])
	case "plugin":
		runPlugin(os.Args[2:])
	case "memory":
		runMemory()
	case "serve":
		runServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		if fn := extraCommands[os.Args[1]]; fn != nil {
			fn()
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack-host: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// runPlugin is the self-exec entry `serve` launches for a non-builtin capability
// slot: it serves the selected built-in implementation as a go-plugin over the
// shared handshake. kind is memory|knowledge|broker|mcp (mcp also needs a <name>).
func runPlugin(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "pi-stack-host plugin: missing <kind> (memory|knowledge|broker|mcp)")
		os.Exit(2)
	}
	switch args[0] {
	case "memory":
		servePluginMemory()
	case "knowledge":
		servePluginKnowledge()
	case "broker":
		servePluginBroker("broker")
	case "mcp":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "pi-stack-host plugin mcp: missing <name>")
			os.Exit(2)
		}
		servePluginMcp(args[1])
	default:
		fmt.Fprintf(os.Stderr, "pi-stack-host plugin: unknown kind %q (memory|knowledge|broker|mcp)\n", args[0])
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `pi-stack-host — host-side services for pi-stack

usage: pi-stack-host <subcommand>

subcommands:
  memory         self-learning memory store, JSON-RPC (:11435)
  mcp <name>     stdio MCP bridge (run by the sbx gateway); slack is an alias
  slack          alias for "mcp slack"
  plugin <kind>  built-in go-plugin server, self-exec (memory|knowledge|broker|mcp)
  serve          run the long-running HTTP services (memory, knowledge)
`)
	for _, line := range extraUsage {
		fmt.Fprintln(os.Stderr, line)
	}
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

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}
