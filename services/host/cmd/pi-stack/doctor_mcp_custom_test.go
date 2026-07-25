package main

import (
	"strings"
	"testing"
)

// doctor_mcp_custom_test.go covers the FINAL custom-MCP false-green
// regression: a configured name confirmed NON-local and OUTSIDE the shipped
// public catalog (mcpClassCustom -- e.g. "linear", or a bespoke overlay
// remote server) must never report a clean bill of health when `sbx mcp ls`
// CONFIRMS it is absent. mcpCustomCheck used to ignore mcpOut/mcpOK entirely
// and always render unverifiable, so a confirmed-missing custom server never
// became an outstanding TODO (report.todos only surfaces evidence=Failed) --
// doctor could print "all checks pass" with a custom server that plainly
// isn't registered. It also must never recommend `pi-stack mcp bundle`
// (shipped catalog only, a silent no-op for a custom name) or `pi-stack mcp
// register` (local stdio only) -- the only honest repair is native
// `sbx mcp add` with the SERVER'S OWN url/transport, never an invented URL.

// customEnv builds a fakeEnv where "linear" is CONFIRMED custom: hostBinary
// resolves and localMCP lists some OTHER local name, and "linear" is not in
// mcpCatalogNames (notion/atlassian/granola) either.
func customEnv(base fakeEnv) fakeEnv {
	if base.hostBinary == "" {
		base.hostBinary = localHostBinary
	}
	if base.localMCP == nil {
		base.localMCP = []string{"slack"} // some OTHER local name; linear stays custom
	}
	return base
}

func TestDoctor_MCPCustom_ConfirmedAbsent_IsVerifiedFailure(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"linear"}
	f := customEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\n", // linear missing
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "linear")
	if c == nil {
		t.Fatalf("expected a linear check, groups=%+v", r.groups)
	}
	if c.evidence != EvidenceFailed || c.state() != stateTODO {
		t.Fatalf("a confirmed-absent custom server must be a VERIFIED failure (no false-green), got %+v", c)
	}
	if c.todo == "" {
		t.Fatalf("a confirmed-absent custom server must carry a repair TODO, got %+v", c)
	}
	if strings.Contains(c.todo, "pi-stack mcp bundle") || strings.Contains(c.todo, "pi-stack mcp register") {
		t.Errorf("must never prescribe `pi-stack mcp bundle` or `pi-stack mcp register` for a custom server, got %q", c.todo)
	}
	if !strings.Contains(c.todo, "sbx mcp add") || !strings.Contains(c.todo, "linear") {
		t.Errorf("expected native `sbx mcp add linear ...` guidance, got %q", c.todo)
	}
	// detail may EXPLAIN why bundle/register don't apply (the existing pattern
	// mcpUnknownClassificationCheck's predecessor also used), but the TODO field
	// itself -- what render/JSON actually prescribe as the fix -- must never be
	// either wrong command; already asserted above.

	// The report-level TODO list (what render/JSON actually surface) must
	// include this failure so the headline is not a false "all checks pass".
	todos := r.todos()
	found := false
	for _, tdo := range todos {
		if strings.Contains(tdo, "sbx mcp add") && strings.Contains(tdo, "linear") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected r.todos() to include the linear repair command, got %v", todos)
	}
}

func TestDoctor_MCPCustom_ConfirmedAbsent_NoInventedURL(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"linear"}
	f := customEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "linear")
	if c == nil {
		t.Fatalf("expected a linear check")
	}
	// Never a guessed real-looking URL like https://... for an unknown server --
	// only a placeholder the user must fill in themselves.
	if strings.Contains(c.todo, "https://") || strings.Contains(c.todo, "http://") {
		t.Errorf("must never invent a URL for a custom server, got %q", c.todo)
	}
}

func TestDoctor_MCPCustom_ConfirmedRegistered_UnverifiableNonOutstanding(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"linear"}
	f := customEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\nlinear\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "linear")
	if c == nil {
		t.Fatalf("expected a linear check")
	}
	if c.evidence != EvidenceUnverifiable {
		t.Fatalf("a registered custom server's auth/tool health is genuinely unverifiable, got %+v", c)
	}
	if c.todo != "" {
		t.Errorf("a registered custom server must carry no todo (non-outstanding), got %q", c.todo)
	}
	if !strings.Contains(c.detail, "registered") {
		t.Errorf("expected detail to acknowledge it is registered, got %q", c.detail)
	}
	// Must not appear in the report-level outstanding TODO list.
	for _, tdo := range r.todos() {
		if strings.Contains(tdo, "linear") {
			t.Errorf("a registered custom server must not be outstanding, got todos=%v", r.todos())
		}
	}
}

func TestDoctor_MCPCustom_RegistrationUnavailable_Unverifiable(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"linear"}
	f := customEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			// no "sbx mcp ls" fixture -> mcpOK is false.
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "linear")
	if c == nil {
		t.Fatalf("expected a linear check")
	}
	if c.evidence != EvidenceUnverifiable {
		t.Fatalf("registration listing unavailable must stay unverifiable, got %+v", c)
	}
	if c.todo != "" {
		t.Errorf("no todo when registration itself couldn't be confirmed, got %q", c.todo)
	}
}

// TestDoctor_MCPCustom_JSON_ConfirmedAbsentSurfacesEvidence checks the JSON
// view carries evidence=failed + the todo for a confirmed-absent custom
// server, and evidence=unverifiable + no todo once registered.
func TestDoctor_MCPCustom_JSON_ConfirmedAbsentSurfacesEvidence(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"linear"}
	f := customEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	v := r.jsonView("default")
	if v.Verdict != "outstanding" {
		t.Errorf("expected verdict=outstanding for a confirmed-absent custom server, got %q", v.Verdict)
	}
	var found bool
	for _, g := range v.Groups {
		for _, c := range g.Checks {
			if c.Label == "linear" {
				found = true
				if c.Evidence != "failed" {
					t.Errorf("expected evidence=failed, got %q", c.Evidence)
				}
				if c.Todo == "" {
					t.Errorf("expected a non-empty JSON todo for the confirmed-absent custom server")
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected a linear check in the JSON view")
	}
}
