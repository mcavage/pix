package env

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/mcp"
	"pix/host/stack"
)

// EnvironmentFacts composes the same reviewed MCP declarations for preview and launch.
func EnvironmentFacts(doc *envinfo.Document, sidecar *envinfo.Sidecar, home string) ([]envinfo.MCPWrapperFact, error) {
	if doc == nil {
		return nil, nil
	}
	var hostMCP map[string]envinfo.HostMCPEntry
	if sidecar != nil {
		hostMCP = sidecar.Host.MCP
	}
	var out []envinfo.MCPWrapperFact
	for _, srv := range doc.MCP.Servers {
		fact := envinfo.MCPWrapperFact{Name: srv.Name, URL: srv.URL}
		if srv.Command == "" {
			out = append(out, fact)
			continue
		}
		if strings.TrimSpace(home) == "" {
			return nil, fmt.Errorf("cannot compose local MCP registrations without PIX_HOME")
		}
		id, err := stack.ID(home)
		if err != nil {
			return nil, err
		}
		fact.Name, err = stack.LocalMCPName(id, srv.Name)
		if err != nil {
			return nil, err
		}
		argv := envinfo.ExpandPixManagedArgv(append([]string{srv.Command}, srv.Args...), envinfo.PixManagedVars(home))
		if entry, ok := hostMCP[srv.Name]; ok && len(entry.EnvKeys) > 0 {
			argv = opRunWrapIfAvailable(argv)
		}
		fact.Command = argv[0]
		fact.Args = argv[1:]
		out = append(out, fact)
	}
	return out, nil
}

// opRunWrapIfAvailable calls mcp.OpRunWrap with this host's own resolved
// `op` binary path and op-refs.env location, or returns argv unchanged
// when either is absent (1Password remains optional — mcp.OpRunWrap's own
// no-op behavior for opPath == "" || opRefs == "").
func opRunWrapIfAvailable(argv []string) []string {
	opPath, err := exec.LookPath("op")
	if err != nil {
		return argv
	}
	refs := config.OpRefsPath()
	if _, err := os.Stat(refs); err != nil {
		return argv
	}
	return mcp.OpRunWrap(opPath, refs, argv)
}
