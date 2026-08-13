package uat

import (
	"fmt"
	"path/filepath"
)

type MCPPlanner struct {
	pixHost   string
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

func (p *MCPPlanner) PlanRegistrationAdd(name string) []string {
	return []string{
		"mcp", "add", name,
		"--command", p.pixHost,
		"--args", "uat-mcp",
		"--args", "--repo", "--args", p.repoPath,
		"--args", "--state", "--args", p.statePath,
		"--args", "--session", "--args", p.sessionID,
	}
}

func (p *MCPPlanner) PlanRegistrationRemove(name string) []string {
	return []string{
		"mcp", "rm", name,
	}
}

func (p *MCPPlanner) PlanTag() []string {
	return []string{
		"docker", "tag", p.repoPath, "uat-" + p.sessionID,
	}
}
