package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/routing"
	"pix/host/sys/systest"
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

// TestAgentLs proves the surviving roster path end to end: it discovers every
// agents/*.md file (listAgents), resolves each one's model + WHY
// (resolveAgentModel via loadAgentMeta), and renders both the table and the
// --json form the way subagents.ts's own (independent) roster read expects the
// files to look, without going through any of the retired mutation surfaces.
func TestAgentLs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ROUTING_DIR", t.TempDir())
	if err := os.MkdirAll("agents", 0o755); err != nil {
		t.Fatal(err)
	}
	const fm = "---\ndescription: go engineer\nintent: code\nbudget_usd: 0.25\ntools: read,edit\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join("agents", "go-eng.md"), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}

	d, out, _ := rootDeps()
	d.Sys = &systest.Fake{}
	if err := runRootParse([]string{"agent", "ls", "--json"}, d); err != nil {
		t.Fatalf("agent ls --json: %v", err)
	}

	var rows []agentRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal roster JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0].Name != "go-eng" {
		t.Fatalf("roster = %+v, want one row named go-eng", rows)
	}
	if rows[0].Intent != "code" || rows[0].Budget != 0.25 || rows[0].Tools != "read,edit" {
		t.Fatalf("row = %+v", rows[0])
	}

	// Human table form: same roster, rendered with the WHY. agentLs's table
	// branch writes straight to os.Stdout (a pre-existing quirk, not something
	// this change touches), so capture that fd rather than Deps.Out.
	d2, _, _ := rootDeps()
	d2.Sys = &systest.Fake{}
	old := os.Stdout
	rp, wp, _ := os.Pipe()
	os.Stdout = wp
	runErr := runRootParse([]string{"agent", "ls"}, d2)
	_ = wp.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(rp)
	if runErr != nil {
		t.Fatalf("agent ls: %v", runErr)
	}
	if !strings.Contains(buf.String(), "AGENT") || !strings.Contains(buf.String(), "go-eng") {
		t.Errorf("agent ls table missing header/row, got:\n%s", buf.String())
	}
}
