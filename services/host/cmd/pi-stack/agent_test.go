package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/routing"
)

func TestParseAgent(t *testing.T) {
	fm, body, ok := parseAgent("---\ndescription: hi\nintent: code\n---\n\nBody here.\n")
	if !ok {
		t.Fatal("expected frontmatter")
	}
	if !strings.Contains(fm, "intent: code") {
		t.Fatalf("fm = %q", fm)
	}
	if strings.TrimSpace(body) != "Body here." {
		t.Fatalf("body = %q", body)
	}
	// No frontmatter.
	_, b2, ok2 := parseAgent("just a body\n")
	if ok2 || strings.TrimSpace(b2) != "just a body" {
		t.Fatalf("no-fm case: ok=%v body=%q", ok2, b2)
	}
}

func testRouting() (*routing.Registry, *routing.Scorecard, *routing.Policy) {
	reg := &routing.Registry{Models: []routing.Model{
		{ID: "anthropic/opus", Provider: "anthropic", Available: true},
		{ID: "openai/gpt", Provider: "openai", Available: true},
	}}
	sc := &routing.Scorecard{Scores: []routing.Score{
		{Model: "anthropic/opus", TaskType: "code", Accuracy: 0.9},
		{Model: "openai/gpt", TaskType: "code", Accuracy: 0.86},
	}}
	pol := &routing.Policy{DefaultFallback: "anthropic/opus", Intents: []routing.Intent{
		{Name: "code", TaskType: "code", Objective: "accuracy"},
	}}
	return reg, sc, pol
}

func TestResolveAgentModel(t *testing.T) {
	reg, sc, pol := testRouting()

	// Pinned model wins.
	m, why := resolveAgentModel(agentMeta{Model: "x/y"}, reg, sc, pol)
	if m != "x/y" || !strings.Contains(why, "pinned") {
		t.Fatalf("pinned: %q %q", m, why)
	}
	// Intent resolves; WHY explains the pick (objective + what it beat).
	m, why = resolveAgentModel(agentMeta{Intent: "code"}, reg, sc, pol)
	if m != "anthropic/opus" || !strings.Contains(why, "code:") || !strings.Contains(why, "beat gpt") {
		t.Fatalf("intent: %q %q", m, why)
	}
	// No intent -> inherit.
	m, why = resolveAgentModel(agentMeta{}, reg, sc, pol)
	if !strings.Contains(m, "inherit") || !strings.Contains(why, "no intent") {
		t.Fatalf("inherit: %q %q", m, why)
	}
	// Unknown intent -> inherit, flagged.
	m, why = resolveAgentModel(agentMeta{Intent: "ghost"}, reg, sc, pol)
	if !strings.Contains(m, "inherit") || !strings.Contains(why, "not in policy") {
		t.Fatalf("unknown: %q %q", m, why)
	}
}

// TestAgentNewEditRm exercises the file lifecycle. new/edit write files; rm
// removes them. We chdir into a temp workspace so ./agents and ./evals resolve
// there, and force the embedded routing defaults via a fresh ROUTING_DIR.
func TestAgentNewEditRm(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ROUTING_DIR", t.TempDir())
	if err := os.MkdirAll("agents", 0o755); err != nil {
		t.Fatal(err)
	}

	agentNew([]string{"go-eng", "--intent", "code", "--budget", "0.25", "--tools", "read,edit"})

	mdPath := filepath.Join("agents", "go-eng.md")
	m, _, err := loadAgentMeta(mdPath)
	if err != nil {
		t.Fatalf("loadAgentMeta after new: %v", err)
	}
	if m.Intent != "code" || m.BudgetUSD != 0.25 || m.Tools != "read,edit" {
		t.Fatalf("new frontmatter: %+v", m)
	}
	if _, err := os.Stat(filepath.Join("evals", "suites", "agents", "go-eng.yaml")); err != nil {
		t.Fatalf("starter suite not created: %v", err)
	}

	// Edit: change intent + budget; parse must still succeed (the quoted-number bug).
	agentEdit([]string{"go-eng", "--intent", "reasoning", "--budget", "0.30"})
	m2, _, err := loadAgentMeta(mdPath)
	if err != nil {
		t.Fatalf("loadAgentMeta after edit: %v", err)
	}
	if m2.Intent != "reasoning" || m2.BudgetUSD != 0.30 {
		t.Fatalf("edit frontmatter: %+v (a quoted budget would blank these)", m2)
	}
	if m2.Tools != "read,edit" {
		t.Fatalf("edit dropped an untouched field: tools=%q", m2.Tools)
	}

	// Remove.
	agentRm([]string{"go-eng", "--yes"})
	if _, err := os.Stat(mdPath); !os.IsNotExist(err) {
		t.Fatalf("agent md should be gone: %v", err)
	}
}
