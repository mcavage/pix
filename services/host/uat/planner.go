package uat

import (
	"fmt"
	"path/filepath"
)

type MCPPlanner struct {
	pixHost string
	// repoPath is retained for the uat-worker start-argv planning that lands in
	// U2 (the gateway command itself no longer names a repo: it only connects
	// to a socket).
	repoPath  string
	statePath string
	sessionID string
}

func NewMCPPlanner(pixHost, repoPath, statePath, sessionID string) (*MCPPlanner, error) {
	if !filepath.IsAbs(pixHost) {
		return nil, fmt.Errorf("pixHost must be absolute: %s", pixHost)
	}
	if !filepath.IsAbs(repoPath) {
		return nil, fmt.Errorf("repoPath must be absolute: %s", repoPath)
	}
	if !filepath.IsAbs(statePath) {
		return nil, fmt.Errorf("statePath must be absolute: %s", statePath)
	}
	if sessionID == "" {
		return nil, fmt.Errorf("sessionID cannot be empty")
	}
	return &MCPPlanner{
		pixHost:   pixHost,
		repoPath:  repoPath,
		statePath: statePath,
		sessionID: sessionID,
	}, nil
}

// PlanRegistrationAdd plans the `sbx mcp add` argv the sbx gateway will use to
// spawn `pix-host uat-mcp` per client connection. That process is a dumb
// stdio<->Unix-socket relay (see docs/design/self-development-uat.md): it
// carries no repo, no state root, and no session id, only the socket a
// separately started `pix-host uat-worker` is listening on.
func (p *MCPPlanner) PlanRegistrationAdd(name string) []string {
	return []string{
		"mcp", "add", name,
		"--command", p.pixHost,
		"--args", "uat-mcp",
		"--args", "--connect",
		"--args", SessionSocketPath(p.statePath),
	}
}

func (p *MCPPlanner) PlanRegistrationRemove(name string) []string {
	return []string{
		"mcp", "rm", name,
	}
}

func (p *MCPPlanner) PlanTag(sourceRef string) ([]string, error) {
	if sourceRef == "" || sourceRef[0] == '-' {
		return nil, fmt.Errorf("invalid source image ref: %q", sourceRef)
	}
	return []string{
		"docker", "tag", sourceRef, "pix:uat-" + p.sessionID,
	}, nil
}
