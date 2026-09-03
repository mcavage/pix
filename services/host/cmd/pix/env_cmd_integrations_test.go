package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// env_cmd_integrations_test.go proves `pix env show` at its real command
// boundary: an environment declaring MCP servers gets a declared/registered/
// reachable answer for each one, never a bare registration count standing
// in for "working" (docs/design/integrations-remediation.md's own naming
// for that gap).

const integrationsSbxenv = `schemaVersion: "1"
agent: pix
mcp:
  servers:
    - name: notion
      url: https://mcp.notion.com/mcp
    - name: atlassian
      url: https://mcp.atlassian.com/v1/mcp
`

// writeIntegrationsEnv creates a PIX_HOME with one environment named "work"
// declaring two remote MCP servers, and returns the home root.
func writeIntegrationsEnv(t *testing.T) string {
	t.Helper()
	home := envCmdTestHome(t)
	envRoot := filepath.Join(home, "envs", "work")
	if err := os.MkdirAll(envRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envRoot, ".sbxenv.yaml"), []byte(integrationsSbxenv), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// installFakeSbxMcpLs puts a `sbx` on PATH whose `mcp ls` prints only
// "notion", so "atlassian" is a positively proven absence, never a guess.
func installFakeSbxMcpLs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1 $2\" = \"mcp ls\" ]; then echo 'notion remote'; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "sbx"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestEnvShow_IntegrationsSection_TextRendersAllThreeStates(t *testing.T) {
	home := writeIntegrationsEnv(t)
	installFakeSbxMcpLs(t)
	t.Setenv("PIX_HOME", home)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if code := dispatch([]string{"env", "show", "work"}, d); code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "integrations:") {
		t.Fatalf("output has no integrations section:\n%s", got)
	}
	if !strings.Contains(got, "notion") || !strings.Contains(got, "registered:ready") {
		t.Errorf("notion should be registered:ready:\n%s", got)
	}
	if !strings.Contains(got, "atlassian") || !strings.Contains(got, "registered:absent") {
		t.Errorf("atlassian should be registered:absent (positively proven, never guessed):\n%s", got)
	}
	// Neither server declares a pix.toml probe, so BOTH must read
	// reachable:unknown — never a guessed "ready" just because they are
	// registered (the exact false-ready-claim this feature exists to
	// prevent).
	if strings.Count(got, "reachable:unknown") != 2 {
		t.Errorf("want exactly 2 reachable:unknown lines (no probe declared for either server):\n%s", got)
	}
	if strings.Contains(got, "reachable:ready") {
		t.Errorf("no server declares a probe; reachable:ready would be a false claim:\n%s", got)
	}
}

func TestEnvShow_IntegrationsSection_JSONShapeAndDetails(t *testing.T) {
	home := writeIntegrationsEnv(t)
	installFakeSbxMcpLs(t)
	t.Setenv("PIX_HOME", home)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if code := dispatch([]string{"env", "show", "work", "--json"}, d); code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}

	var fields map[string]any
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	raw, ok := fields["integrations"]
	if !ok {
		t.Fatalf("no \"integrations\" field in JSON output: %v", fields)
	}
	list, ok := raw.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("integrations = %#v, want a 2-element list", raw)
	}
	byName := map[string]map[string]any{}
	for _, item := range list {
		row := item.(map[string]any)
		byName[row["name"].(string)] = row
	}
	notion, ok := byName["notion"]
	if !ok {
		t.Fatalf("no notion row: %v", list)
	}
	if notion["declared"] != true {
		t.Errorf("notion.declared = %v, want true", notion["declared"])
	}
	if notion["registered"] != "ready" {
		t.Errorf("notion.registered = %v, want ready", notion["registered"])
	}
	if notion["reachable"] != "unknown" {
		t.Errorf("notion.reachable = %v, want unknown", notion["reachable"])
	}
	if detail, _ := notion["reachable_detail"].(string); !strings.Contains(detail, "no probe declared") {
		t.Errorf("notion.reachable_detail = %q, want an explanation naming the missing probe", detail)
	}

	atlassian, ok := byName["atlassian"]
	if !ok {
		t.Fatalf("no atlassian row: %v", list)
	}
	if atlassian["registered"] != "absent" {
		t.Errorf("atlassian.registered = %v, want absent", atlassian["registered"])
	}
}

// TestEnvShow_NoDeclaredMCPServers_NoIntegrationsSection proves an
// environment that declares nothing gets no fabricated section either way,
// text or JSON, and never runs `sbx mcp ls` in the first place: it is not
// on PATH, so a spurious call would fail the whole command.
func TestEnvShow_NoDeclaredMCPServers_NoIntegrationsSection(t *testing.T) {
	home := envCmdTestHome(t)
	envRoot := filepath.Join(home, "envs", "work")
	if err := os.MkdirAll(envRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envRoot, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_HOME", home)
	t.Setenv("PATH", t.TempDir()) // no sbx at all: a spurious `mcp ls` call fails dispatch

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if code := dispatch([]string{"env", "show", "work"}, d); code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	if strings.Contains(out.String(), "integrations:") {
		t.Errorf("output should have no integrations section for a no-MCP environment:\n%s", out.String())
	}

	out.Reset()
	if code := dispatch([]string{"env", "show", "work", "--json"}, d); code != 0 {
		t.Fatalf("dispatch --json exit = %d, stderr=%s", code, errb.String())
	}
	var fields map[string]any
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if _, ok := fields["integrations"]; ok {
		t.Errorf("JSON output should have no \"integrations\" key: %v", fields)
	}
}
