package main

// doctor_review_round2_test.go covers review round 2 finding R2-01:
//
//   R2-01  recognizedMCPArgv's pi-stack-host gate checked only
//          filepath.IsAbs + filepath.Base(cmd[0]) == "pi-stack-host" — an
//          ABSOLUTE PATH WITH THE RIGHT BASENAME ANYWHERE ON DISK (e.g.
//          /tmp/malicious/pi-stack-host) satisfied it. mcp.go registration
//          ALWAYS uses the exact binary hostBinaryResolver (findHostBinary)
//          resolves — sibling-to-launcher first, PATH fallback, never a bare
//          name — so the gate must require that SAME canonical answer, not
//          basename alone. shellEnv.hostBinary is the injected/hermetic trust
//          seam (mirrors env.lookPath for gog/op) so these tests exercise the
//          real trust decision without touching a real installed binary.

import (
	"strings"
	"testing"
)

// TestDoctor_R201_MaliciousHostBinaryPathNeverExecuted: a registered command
// whose argv[0] is an absolute path with basename "pi-stack-host" but is NOT
// the canonical binary (env.hostBinary()'s answer) must never be executed —
// the probe is skipped as unrecognized/untrusted, and the fake run FAILS the
// test if doctor ever tries to exec the malicious path.
func TestDoctor_R201_MaliciousHostBinaryPathNeverExecuted(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	malicious := "/tmp/malicious/pi-stack-host"
	regCmd := malicious + " mcp slack"
	f := gogConfirmed(fakeEnv{
		present:    map[string]bool{"sbx": true, "op": true},
		hostBinary: "/usr/local/bin/pi-stack-host", // the REAL canonical answer
		output: map[string]string{
			"sbx secret ls":          "anthropic openai google github",
			"sbx mcp ls":             "gog\nslack\n",
			"sbx mcp get slack":      "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools": "slack_search\n", // would prove the exploit if ever exec'd
		},
		ports: map[int]bool{11435: true},
	})
	env := fatalOnExec(t, f.env(), malicious)
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil {
		t.Fatalf("expected a slack check, groups=%+v", r.groups)
	}
	if c.evidence != EvidenceUnverifiable {
		t.Errorf("a malicious pi-stack-host path must be unverifiable (never a confirmed green), got %+v", c)
	}
	if !strings.Contains(c.detail, "probe skipped") {
		t.Errorf("detail should say the probe was skipped, got %q", c.detail)
	}
	if c.state() == stateOK {
		t.Errorf("a malicious registered path must never render healthy, got %+v", c)
	}
}

// TestDoctor_R201_CanonicalHostBinaryProbed: the trust gate must not
// over-block — a registration whose argv[0] EXACTLY equals env.hostBinary()'s
// answer is probed and confirms green (bare, no op wrapper).
func TestDoctor_R201_CanonicalHostBinaryProbed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	canonical := "/usr/local/bin/pi-stack-host"
	regCmd := canonical + " mcp slack"
	f := gogGreen(fakeEnv{
		present:    map[string]bool{"sbx": true, "ollama": true},
		hostBinary: canonical,
		output: map[string]string{
			"sbx secret ls":          "anthropic openai google github",
			"ollama list":            "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":             "gog\nslack\n",
			"sbx mcp get slack":      "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools": "slack_search\nslack_post\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceHealthy || c.state() != stateOK {
		t.Fatalf("a canonical pi-stack-host registration must probe to a confirmed green, got %+v", c)
	}
}

// TestDoctor_R201_SymlinkedHostBinaryProbed: a registration whose argv[0] is a
// DIFFERENT path string that resolves (via env.evalSymlinks) to the SAME real
// file as env.hostBinary()'s answer (e.g. an install where the launcher
// resolved through a symlink) is still trusted and probed.
func TestDoctor_R201_SymlinkedHostBinaryProbed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	symlinked := "/opt/pi-stack/current/pi-stack-host" // a versioned-release symlink path
	canonical := "/usr/local/bin/pi-stack-host"
	real := "/opt/pi-stack/releases/1.2.3/pi-stack-host" // what both actually resolve to
	regCmd := symlinked + " mcp slack"
	f := gogGreen(fakeEnv{
		present:    map[string]bool{"sbx": true, "ollama": true},
		hostBinary: canonical,
		symlinks: map[string]string{
			symlinked: real,
			canonical: real,
		},
		output: map[string]string{
			"sbx secret ls":          "anthropic openai google github",
			"ollama list":            "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":             "gog\nslack\n",
			"sbx mcp get slack":      "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools": "slack_search\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceHealthy || c.state() != stateOK {
		t.Fatalf("a symlinked-but-legitimate pi-stack-host registration must still probe green, got %+v", c)
	}
}

// TestDoctor_R201_OpWrappedCanonicalHostBinaryProbed: the same canonical
// pi-stack-host trust applies when the registration is op-wrapped (op run
// --env-file=... -- pi-stack-host mcp slack), the actual shape mcp.go's
// addArgs produces when op-refs.env is present.
func TestDoctor_R201_OpWrappedCanonicalHostBinaryProbed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	canonical := "/usr/local/bin/pi-stack-host"
	regCmd := "/usr/bin/op run --no-masking --env-file=/x/op-refs.env -- " + canonical + " mcp slack"
	f := gogGreen(fakeEnv{
		present:    map[string]bool{"sbx": true, "ollama": true, "op": true},
		hostBinary: canonical,
		output: map[string]string{
			"sbx secret ls":          "anthropic openai google github",
			"ollama list":            "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":             "gog\nslack\n",
			"sbx mcp get slack":      "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools": "slack_search\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceHealthy || c.state() != stateOK {
		t.Fatalf("an op-wrapped canonical pi-stack-host registration must still probe green, got %+v", c)
	}
}

// TestDoctor_R201_OpWrappedMaliciousHostBinaryNeverExecuted: an op-wrapped
// registration wrapping a MALICIOUS pi-stack-host path is never executed
// either — the inner-binary canonical check applies regardless of the op
// wrapper.
func TestDoctor_R201_OpWrappedMaliciousHostBinaryNeverExecuted(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	malicious := "/tmp/malicious/pi-stack-host"
	regCmd := "/usr/bin/op run --no-masking --env-file=/x/op-refs.env -- " + malicious + " mcp slack"
	f := gogGreen(fakeEnv{
		present:    map[string]bool{"sbx": true, "ollama": true, "op": true},
		hostBinary: "/usr/local/bin/pi-stack-host",
		output: map[string]string{
			"sbx secret ls":          "anthropic openai google github",
			"ollama list":            "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":             "gog\nslack\n",
			"sbx mcp get slack":      "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools": "slack_search\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	env := fatalOnExec(t, f.env(), malicious)
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("an op-wrapped malicious pi-stack-host path must be unverifiable, never healthy, got %+v", c)
	}
}

// TestDoctor_R201_UnresolvedHostBinaryFailsClosed: when env.hostBinary cannot
// resolve a canonical answer (e.g. pi-stack-host not found anywhere), doctor
// must NOT fall back to trusting basename alone — the probe is skipped, never
// executed.
func TestDoctor_R201_UnresolvedHostBinaryFailsClosed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	regCmd := "/usr/local/bin/pi-stack-host mcp slack"
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		// hostBinary intentionally left unset -> env.hostBinary() errors.
		output: map[string]string{
			"sbx secret ls":          "anthropic openai google github",
			"ollama list":            "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":             "gog\nslack\n",
			"sbx mcp get slack":      "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools": "slack_search\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	env := fatalOnExec(t, f.env(), "/usr/local/bin/pi-stack-host")
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("an unresolvable canonical binary must fail closed (unverifiable), got %+v", c)
	}
}
