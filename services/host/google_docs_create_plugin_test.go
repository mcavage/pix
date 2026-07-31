package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGoogleDocsCreateToolIsCreateOnly(t *testing.T) {
	tools, err := (googleDocsCreateMcpAdapter{}).ListTools()
	if err != nil || len(tools) != 1 {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	if tools[0].Name != googleDocsCreateToolName {
		t.Fatalf("tool name = %q", tools[0].Name)
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	if _, exists := props["document_id"]; exists {
		t.Fatal("create-only tool must not accept an existing document id")
	}
	for _, forbidden := range []string{"update", "delete", "share", "move", "send"} {
		if strings.Contains(strings.ToLower(string(tools[0].InputSchema)), forbidden) {
			t.Fatalf("schema exposes forbidden capability %q", forbidden)
		}
	}
}

func TestGoogleDocsCreateCommandIsExactlyConstrained(t *testing.T) {
	got := strings.Join(googleDocsCreateArgs("person@example.com", "New doc", "/tmp/content.md", true), " ")
	want := "--account person@example.com --gmail-no-send --enable-commands-exact=docs.create --no-input --json docs create New doc --file /tmp/content.md --pageless"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"docs write", "docs update", "gmail send", "drive move", "drive share"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("command contains forbidden operation %q", forbidden)
		}
	}
}

func TestGoogleDocsCreateRejectsMissingTitleBeforeExec(t *testing.T) {
	_, err := (googleDocsCreateMcpAdapter{}).CallTool(googleDocsCreateToolName, json.RawMessage(`{"title":"  "}`))
	if err == nil || !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("error = %v", err)
	}
}
