package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/routing"
	"pix/host/sys/systest"

	"gopkg.in/yaml.v3"
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

// TestParseAgentCRLF proves a Windows-authored (CRLF) agent file is recognized
// the same as an LF one: the `---\n` prefix check and `\n---` terminator
// search must not both silently miss on `\r\n` line endings and fall through
// to the no-frontmatter path.
func TestParseAgentCRLF(t *testing.T) {
	fm, body, ok := parseAgent("---\r\ndescription: hi\r\nintent: code\r\n---\r\n\r\nBody here.\r\n")
	if !ok {
		t.Fatal("expected frontmatter from a CRLF file")
	}
	if !strings.Contains(fm, "intent: code") {
		t.Fatalf("fm = %q", fm)
	}
	if strings.TrimSpace(body) != "Body here." {
		t.Fatalf("body = %q", body)
	}

	// The frontmatter round-trips through YAML once normalized to LF.
	var m agentMeta
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		t.Fatalf("unmarshal CRLF frontmatter: %v", err)
	}
	if m.Intent != "code" || m.Description != "hi" {
		t.Fatalf("meta = %+v", m)
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
	// branch writes to Deps.Out like every other command, so it's assertable
	// straight off the injected buffer — no os.Stdout swap, no pipe, nothing
	// that can deadlock if the writer ever outpaces an unread pipe.
	d2, out2, _ := rootDeps()
	d2.Sys = &systest.Fake{}
	if err := runRootParse([]string{"agent", "ls"}, d2); err != nil {
		t.Fatalf("agent ls: %v", err)
	}
	if !strings.Contains(out2.String(), "AGENT") || !strings.Contains(out2.String(), "go-eng") {
		t.Errorf("agent ls table missing header/row, got:\n%s", out2.String())
	}
}

// TestAgentLsMalformedYAML proves a broken agents/*.md frontmatter surfaces as
// a named error on that agent's own row — both in the table and the --json
// form — instead of silently falling through to the same "(inherit parent)"
// a well-formed, intent-less agent gets. loadAgentMeta's error was being
// discarded (`m, _, _ := loadAgentMeta(...)`), which hid a malformed file
// behind a misleadingly benign roster row.
func TestAgentLsMalformedYAML(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ROUTING_DIR", t.TempDir())
	if err := os.MkdirAll("agents", 0o755); err != nil {
		t.Fatal(err)
	}
	const good = "---\ndescription: fine\nintent: code\n---\n\nBody.\n"
	// Unterminated flow sequence: invalid YAML, not merely an unknown key.
	const bad = "---\nintent: [unterminated\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join("agents", "good.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("agents", "bad.md"), []byte(bad), 0o644); err != nil {
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
	if len(rows) != 2 {
		t.Fatalf("roster = %+v, want two rows", rows)
	}
	var badRow *agentRow
	for i := range rows {
		if rows[i].Name == "bad" {
			badRow = &rows[i]
		}
	}
	if badRow == nil {
		t.Fatalf("no row for bad.md in %+v", rows)
	}
	if strings.Contains(badRow.Why, "inherit") {
		t.Fatalf("malformed agent silently reported as inherit: %+v", badRow)
	}
	if !strings.Contains(badRow.Why, "bad frontmatter") {
		t.Fatalf("malformed agent's WHY should name the error, got %+v", badRow)
	}

	// Same story in the human table: the bad row and its error text render,
	// they don't just vanish or fall back to a plain roster line.
	d2, out2, _ := rootDeps()
	d2.Sys = &systest.Fake{}
	if err := runRootParse([]string{"agent", "ls"}, d2); err != nil {
		t.Fatalf("agent ls: %v", err)
	}
	table := out2.String()
	if !strings.Contains(table, "bad") || !strings.Contains(table, "bad frontmatter") {
		t.Errorf("table missing malformed-agent row/error, got:\n%s", table)
	}
}

// TestAgentLs_WorksFromAnySubdirectory: DX finding 3 — the roster resolver
// used to hard-require the EXACT cwd to hold ./agents ("run from the repo
// root"), which breaks a Homebrew install (no repo checkout to be in) and,
// even inside a checkout, breaks the moment you cd into a subdirectory.
// resolveAgentsDir must climb from cwd, so `pix agent ls` works from any
// depth under a checkout, not only its root.
func TestAgentLs_WorksFromAnySubdirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	const fm = "---\nintent: code\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(root, "agents", "go-eng.md"), []byte(fm), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	t.Setenv("ROUTING_DIR", t.TempDir())
	t.Setenv("PIX_AGENTS_DIR", "")

	names, dir, err := listAgents()
	if err != nil {
		t.Fatalf("listAgents from a nested subdirectory: %v", err)
	}
	if len(names) != 1 || names[0] != "go-eng" {
		t.Fatalf("names = %v, want [go-eng]", names)
	}
	if dir != filepath.Join(root, "agents") {
		t.Errorf("dir = %q, want %q", dir, filepath.Join(root, "agents"))
	}
}

// TestResolveAgentsDir_NoRosterFound_AccurateGuidance: with no
// $PIX_AGENTS_DIR, no bundled dir beside the test binary, and an isolated cwd
// with nothing above it named "agents", the error must be accurate: it must
// NOT blindly command "run from the repo root" (nonsensical on a packaged
// install with no checkout) and must name the actual escape hatch
// ($PIX_AGENTS_DIR).
func TestResolveAgentsDir_NoRosterFound_AccurateGuidance(t *testing.T) {
	isolated := t.TempDir()
	t.Chdir(filepath.Join(isolated))
	t.Setenv("PIX_AGENTS_DIR", "")

	_, err := resolveAgentsDir()
	if err == nil {
		t.Fatal("want an error when no roster exists anywhere reachable")
	}
	msg := err.Error()
	if !strings.Contains(msg, "PIX_AGENTS_DIR") {
		t.Errorf("error must name the escape hatch $PIX_AGENTS_DIR, got: %v", err)
	}
	if strings.Contains(msg, "run from the repo root") {
		t.Errorf("error must not blindly command \"run from the repo root\" (no checkout exists here): %v", err)
	}
}

// TestResolveAgentsDir_ExplicitEnvMustExist: a typo'd $PIX_AGENTS_DIR must
// fail loudly, never silently fall through to a different roster the user
// did not ask for.
func TestResolveAgentsDir_ExplicitEnvMustExist(t *testing.T) {
	t.Setenv("PIX_AGENTS_DIR", filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := resolveAgentsDir(); err == nil {
		t.Fatal("want an error for a nonexistent $PIX_AGENTS_DIR")
	}
}
