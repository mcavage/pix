package main

import (
	"strings"
	"testing"
)

// doctor_mcp_remote_test.go covers finding #1 (ship-review): doctor must
// classify a configured non-gog MCP server as LOCAL vs REMOTE gateway-catalog
// (via localMCPNames/hostBinaryResolver, the same source of truth
// registerServers already uses) before deciding how to check it. A confirmed
// remote server is NEVER probed/exec'd as a local command and is NEVER told
// to `pi-stack mcp register` (that only knows local stdio servers); it gets
// registration + a bounded native auth-status check instead. An unknown
// classification must not guess either way.

// remoteEnv builds a fakeEnv where "notion" is CONFIRMED remote: hostBinary
// resolves, and its `mcp --list` output does NOT include "notion" (mirroring
// a real pi-stack-host build, which never serves a gateway-catalog name
// locally).
func remoteEnv(base fakeEnv) fakeEnv {
	if base.hostBinary == "" {
		base.hostBinary = localHostBinary
	}
	if base.localMCP == nil {
		base.localMCP = []string{"slack"} // some OTHER local name; notion stays remote
	}
	return base
}

func TestDoctor_MCPRemote_HealthyRegisteredAuthenticated(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := remoteEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls":              "anthropic openai google github",
			"ollama list":                "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":                 "gog\nnotion\n",
			"sbx mcp auth status notion": "notion: authenticated",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	// A confirmed remote server must NEVER be exec'd as a local command --
	// there is no `sbx mcp get notion` fixture at all, so if doctor ever tried
	// to read/exec a registered command for it, the fake run would error and
	// the check would NOT come back healthy.
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "notion")
	if c == nil {
		t.Fatalf("expected a notion check, groups=%+v", r.groups)
	}
	if c.evidence != EvidenceHealthy || c.state() != stateOK {
		t.Fatalf("a registered+authenticated remote server must be healthy, got %+v", c)
	}
	if !strings.Contains(c.detail, "authenticated") {
		t.Errorf("expected authenticated in detail, got %q", c.detail)
	}
	if c.todo != "" {
		t.Errorf("a healthy remote check must carry no todo, got %q", c.todo)
	}
}

func TestDoctor_MCPRemote_RegisteredButUnauthenticated(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := remoteEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls":              "anthropic openai google github",
			"ollama list":                "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":                 "gog\nnotion\n",
			"sbx mcp auth status notion": "notion: not authenticated",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "notion")
	if c == nil || c.evidence != EvidenceFailed || c.state() != stateTODO {
		t.Fatalf("a registered but unauthenticated remote server must be a TODO, got %+v", c)
	}
	if c.todo != "pi-stack mcp auth notion" {
		t.Errorf("expected the exact `pi-stack mcp auth notion` fix-it, got %q", c.todo)
	}
}

func TestDoctor_MCPRemote_Absent(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := remoteEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\n", // notion missing
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "notion")
	if c == nil || c.evidence != EvidenceFailed {
		t.Fatalf("an unregistered remote server must be a verified failure, got %+v", c)
	}
	if c.todo != "pi-stack mcp bundle" {
		t.Errorf("an unregistered REMOTE server must recommend `pi-stack mcp bundle`, never `pi-stack mcp register`, got %q", c.todo)
	}
}

// TestDoctor_MCPRemote_NeverExecsLocalCommand: even if sbx happens to expose a
// `sbx mcp get notion` command (e.g. a stale/odd registration), a confirmed
// remote server must never have that command read or exec'd -- the fake run
// fails the test if doctor ever tries.
func TestDoctor_MCPRemote_NeverExecsLocalCommand(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}
	f := remoteEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls":                   "anthropic openai google github",
			"ollama list":                     "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":                      "gog\nnotion\n",
			"sbx mcp get notion":              "name: notion\ncommand: /some/local/binary\n",
			"/some/local/binary --list-tools": "would_prove_it_execd\n",
			"sbx mcp auth status notion":      "notion: authenticated",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	env := fatalOnExec(t, f.env(), "/some/local/binary")
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "notion")
	if c == nil || c.evidence != EvidenceHealthy {
		t.Fatalf("expected a healthy notion check via the remote auth-status path, got %+v", c)
	}
}

// TestDoctor_MCP_UnknownClassification_NoWrongCommand: when localMCPNames
// itself can't be established (pi-stack-host unresolved), doctor must not
// guess local or remote -- unverifiable, and no repair command that could be
// wrong for the actual kind of server.
func TestDoctor_MCP_UnknownClassification_NoWrongCommand(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"mystery"}
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		// hostBinary intentionally left unset -> localMCPNames returns unknown.
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\nmystery\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	}
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "mystery")
	if c == nil {
		t.Fatalf("expected a mystery check, groups=%+v", r.groups)
	}
	if c.evidence != EvidenceUnverifiable {
		t.Errorf("an unknown-classification server must be unverifiable, got %+v", c)
	}
	if c.todo != "" {
		t.Errorf("an unknown-classification server must carry NO repair command, got %q", c.todo)
	}
	if strings.Contains(c.detail, "pi-stack mcp register") || strings.Contains(c.detail, "pi-stack mcp bundle") {
		t.Errorf("an unknown-classification detail must not name either repair command, got %q", c.detail)
	}
}
