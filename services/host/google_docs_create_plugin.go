package main

// googleDocsCreateMcpAdapter is intentionally narrower than gog's write MCP
// surface. It exposes one operation: create a NEW Google Doc, optionally with
// initial Markdown content. There is no document-id input, so this server
// cannot update, delete, move, share, or otherwise mutate an existing file.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/plugin"
)

const (
	googleDocsCreateServerName = "google-docs-create"
	googleDocsCreateToolName   = "google_docs_create"
	maxGoogleDocTitleBytes     = 512
	maxGoogleDocContentBytes   = 2 << 20
)

type googleDocsCreateMcpAdapter struct{}

func googleDocsCreateArgs(account, title, contentPath string, pageless bool) []string {
	args := []string{
		"--account", account,
		"--gmail-no-send",
		"--enable-commands-exact=docs.create",
		"--no-input",
		"--json",
		"docs", "create", title,
	}
	if contentPath != "" {
		args = append(args, "--file", contentPath)
	}
	if pageless {
		args = append(args, "--pageless")
	}
	return args
}

func (googleDocsCreateMcpAdapter) Info() (plugin.ServerInfo, error) {
	return plugin.ServerInfo{
		Name:            "pix-google-docs-create",
		Version:         "0.0.1",
		ProtocolVersion: "2025-06-18",
	}, nil
}

func (googleDocsCreateMcpAdapter) ListTools() ([]plugin.ToolSpec, error) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"title":{"type":"string","description":"Title for the new Google Doc"},"content":{"type":"string","description":"Optional initial Markdown content"},"pageless":{"type":"boolean","description":"Create the document in pageless mode"}},"required":["title"]}`)
	return []plugin.ToolSpec{{
		Name:        googleDocsCreateToolName,
		Description: "Create a new Google Doc. This tool cannot modify an existing document.",
		InputSchema: schema,
	}}, nil
}

func (googleDocsCreateMcpAdapter) CallTool(name string, raw json.RawMessage) (json.RawMessage, error) {
	if name != googleDocsCreateToolName {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	var in struct {
		Title    string `json:"title"`
		Content  string `json:"content"`
		Pageless bool   `json:"pageless"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return nil, fmt.Errorf("title is required")
	}
	if len(in.Title) > maxGoogleDocTitleBytes {
		return nil, fmt.Errorf("title is too long (maximum %d bytes)", maxGoogleDocTitleBytes)
	}
	if len(in.Content) > maxGoogleDocContentBytes {
		return nil, fmt.Errorf("content is too large (maximum %d bytes)", maxGoogleDocContentBytes)
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load Pix config: %w", err)
	}
	account := strings.TrimSpace(cfg.GogAccount)
	if account == "" {
		return nil, fmt.Errorf("Google Workspace is not configured; run pix gworkspace setup")
	}
	gog, err := exec.LookPath("gog")
	if err != nil {
		return nil, fmt.Errorf("Google Workspace dependency not found; run pix gworkspace setup")
	}

	var contentPath string
	if in.Content != "" {
		f, err := os.CreateTemp("", "pix-google-doc-*.md")
		if err != nil {
			return nil, fmt.Errorf("prepare document content: %w", err)
		}
		contentPath = f.Name()
		defer os.Remove(contentPath)
		if _, err := f.WriteString(in.Content); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("prepare document content: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("prepare document content: %w", err)
		}
	}
	args := googleDocsCreateArgs(account, in.Title, contentPath, in.Pageless)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gog, args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("creating Google Doc timed out")
	}
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("create Google Doc: %s", detail)
	}
	if !json.Valid(out) {
		return json.Marshal(map[string]string{"result": strings.TrimSpace(string(out))})
	}
	return json.RawMessage(out), nil
}
