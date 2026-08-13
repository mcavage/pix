package uat

type MCPPlanner struct {
	pixHost    string
	repoPath   string
	statePath  string
	sessionID string
}

func NewMCPPlanner(pixHost, repoPath, statePath, sessionID string) *MCPPlanner {
	return &MCPPlanner{
		pixHost:    pixHost,
		repoPath:   repoPath,
		statePath:  statePath,
		sessionID: sessionID,
	}
}

func (p *MCPPlanner) PlanAdd(name, url string) []string {
	return []string{
		p.pixHost,
		"mcp", "add", name,
		"--url", url,
		"--repo", p.repoPath,
		"--state", p.statePath,
		"--session", p.sessionID,
	}
}

func (p *MCPPlanner) PlanTag(image, tag string) []string {
	return []string{
		"docker", "tag", image, "uat-" + tag,
	}
}
