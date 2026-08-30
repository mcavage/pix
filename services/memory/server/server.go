// Package server builds the pix-memory MCP server: eight tools
// (memory_recall, memory_stats, memory_remember, memory_forget,
// memory_observe, memory_status, memory_snapshot, memory_restore) over
// Streamable HTTP at /mcp, plus a non-MCP /healthz. See
// docs/design/pix-v2-architecture.md §9.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"pix-memory/store"
)

// Version is stamped into the MCP Implementation and /healthz; overridden at
// build time with -ldflags "-X pix-memory/server.Version=...".
var Version = "dev"

func boolPtr(b bool) *bool { return &b }

// New builds the MCP server (tools registered, no transport attached yet)
// bound to st.
func New(st *store.Store) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "pix-memory", Version: Version}, nil)
	registerTools(srv, st)
	return srv
}

// --- memory_recall -----------------------------------------------------------

type recallInput struct {
	Query      string `json:"query" jsonschema:"the search query; \"*\" lists recent memories without ranking"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max rows to return (default 8)"`
	CharBudget int    `json:"char_budget,omitempty" jsonschema:"max total content characters across all rows (default 1200)"`
	Kind       string `json:"kind,omitempty" jsonschema:"restrict to one kind, e.g. fact or learning"`
	Project    string `json:"project,omitempty" jsonschema:"boost rows tagged with this project"`
	Profile    string `json:"profile,omitempty" jsonschema:"memory scope; defaults to the shared \"default\" bucket"`
}

type recallHit struct {
	ID        string  `json:"id"`
	Content   string  `json:"content"`
	Kind      string  `json:"kind"`
	Source    string  `json:"source"`
	Project   string  `json:"project,omitempty"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

type recallOutput struct {
	Hits []recallHit `json:"hits"`
}

// --- memory_stats -------------------------------------------------------------

type statsInput struct {
	Profile string `json:"profile,omitempty" jsonschema:"memory scope; defaults to the shared \"default\" bucket"`
}

type statsOutput struct {
	Active    int `json:"active"`
	Facts     int `json:"facts"`
	Learnings int `json:"learnings"`
	Deleted   int `json:"deleted"`
}

// --- memory_remember -----------------------------------------------------------

type rememberInput struct {
	Content    string   `json:"content" jsonschema:"the durable fact or correction text to store"`
	Kind       string   `json:"kind,omitempty" jsonschema:"fact or learning (default fact)"`
	Project    string   `json:"project,omitempty" jsonschema:"associate this row with a project"`
	Profile    string   `json:"profile,omitempty" jsonschema:"memory scope to write into; defaults to \"default\""`
	Confidence float64  `json:"confidence,omitempty" jsonschema:"0-1 confidence (default 0.8)"`
	Tags       []string `json:"tags,omitempty"`
	Dedupe     float64  `json:"dedupe,omitempty" jsonschema:"cosine-similarity threshold (0-1) for semantic dedupe; 0 disables it"`
}

type rememberOutput struct {
	ID             string `json:"id"`
	Reaffirmed     bool   `json:"reaffirmed"`
	BudgetExceeded bool   `json:"budget_exceeded,omitempty"`
}

// --- memory_forget -------------------------------------------------------------

type forgetInput struct {
	ID      string `json:"id" jsonschema:"exact memory id, or an unambiguous id prefix"`
	Profile string `json:"profile,omitempty" jsonschema:"memory scope to delete from; defaults to \"default\""`
}

type forgetOutput struct {
	OK bool `json:"ok"`
}

// --- memory_observe -------------------------------------------------------------

type observeInput struct {
	Text    string `json:"text" jsonschema:"one completed user message to consider for automatic capture"`
	Project string `json:"project,omitempty"`
	Profile string `json:"profile,omitempty" jsonschema:"memory scope captured rows land in; defaults to \"default\""`
}

type observeOutput struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// --- memory_status -------------------------------------------------------------

type statusInput struct{}

type statusOutput struct {
	Ready          bool   `json:"ready"`
	SchemaVersion  int    `json:"schema_version"`
	CaptureMode    string `json:"capture_mode"`
	EmbedModel     string `json:"embed_model"`
	WatcherModel   string `json:"watcher_model"`
	EmbedHealthy   *bool  `json:"embed_healthy"`
	WatcherHealthy *bool  `json:"watcher_healthy"`
	WatcherReason  string `json:"watcher_reason,omitempty"`
}

// --- memory_snapshot -------------------------------------------------------------

type snapshotInput struct {
	Path string `json:"path,omitempty" jsonschema:"destination path; defaults to a timestamped file under /data/backups"`
}

type snapshotOutput struct {
	Path          string `json:"path"`
	Rows          int    `json:"rows"`
	Size          int64  `json:"size"`
	SchemaVersion int    `json:"schema_version"`
}

// --- memory_restore -------------------------------------------------------------

type restoreInput struct {
	Path  string `json:"path" jsonschema:"snapshot path to install as the live database"`
	Force bool   `json:"force,omitempty" jsonschema:"required when a live database already exists (the usual case); the replaced db is kept as a .bak"`
}

type restoreOutput struct {
	Path       string `json:"path"`
	Rows       int    `json:"rows"`
	BackupPath string `json:"backup_path,omitempty"`
}

func registerTools(srv *mcp.Server, st *store.Store) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_recall",
		Description: "Search stored memory for rows relevant to a query, or list recent rows with query \"*\". Read-only.",
		Annotations: &mcp.ToolAnnotations{
			Title:          "Recall memory",
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in recallInput) (*mcp.CallToolResult, recallOutput, error) {
		hits, err := st.Recall(in.Query, in.Limit, in.CharBudget, in.Kind, in.Project, in.Profile)
		if err != nil {
			return nil, recallOutput{}, err
		}
		out := recallOutput{Hits: make([]recallHit, 0, len(hits))}
		for _, h := range hits {
			out.Hits = append(out.Hits, recallHit{
				ID: h.ID, Content: h.Content, Kind: h.Kind, Source: h.Source,
				Project: h.Project, Score: h.Score, CreatedAt: h.CreatedAt,
			})
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_stats",
		Description: "Report active/fact/learning/deleted row counts for a memory scope. Read-only.",
		Annotations: &mcp.ToolAnnotations{
			Title:          "Memory stats",
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in statsInput) (*mcp.CallToolResult, statsOutput, error) {
		stats := st.Stats(in.Profile)
		return nil, statsOutput{Active: stats.Active, Facts: stats.Facts, Learnings: stats.Learnings, Deleted: stats.Deleted}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_remember",
		Description: "Explicitly store a durable fact or correction, or reaffirm an existing identical/near-duplicate row.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Remember",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in rememberInput) (*mcp.CallToolResult, rememberOutput, error) {
		res, err := st.Remember(store.RememberInput{
			Content: in.Content, Kind: in.Kind, Source: "mcp",
			Project: in.Project, HasProject: in.Project != "",
			Profile: in.Profile, Confidence: in.Confidence, Tags: in.Tags,
			Dedupe: in.Dedupe, HasDedupe: in.Dedupe > 0,
		})
		if err != nil {
			return nil, rememberOutput{}, err
		}
		return nil, rememberOutput{ID: res.ID, Reaffirmed: res.Reaffirmed, BudgetExceeded: res.BudgetExceeded}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_forget",
		Description: "Soft-delete one memory row by exact id or unambiguous id prefix.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Forget",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in forgetInput) (*mcp.CallToolResult, forgetOutput, error) {
		ok := st.Forget(in.ID, in.Profile)
		return nil, forgetOutput{OK: ok}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_observe",
		Description: "Opted-in watcher extraction from one completed user message: only stores anything when memory_capture=experimental-auto and a local watcher model is available; a no-op otherwise.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Observe",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in observeInput) (*mcp.CallToolResult, observeOutput, error) {
		accepted, reason := st.Observe(in.Text, in.Project, in.Project != "", in.Profile)
		return nil, observeOutput{Accepted: accepted, Reason: reason}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_status",
		Description: "Report schema version, capture mode, and embedding/watcher backend health. Read-only.",
		Annotations: &mcp.ToolAnnotations{
			Title:          "Memory status",
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ statusInput) (*mcp.CallToolResult, statusOutput, error) {
		watcherHealthy, watcherReason := store.WatcherHealthState()
		return nil, statusOutput{
			Ready:          st.Ping() == nil,
			SchemaVersion:  st.SchemaVersion(),
			CaptureMode:    store.CaptureMode(),
			EmbedModel:     store.EmbedModel(),
			WatcherModel:   store.WatcherModel(),
			EmbedHealthy:   store.EmbedHealthState(),
			WatcherHealthy: watcherHealthy,
			WatcherReason:  watcherReason,
		}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_snapshot",
		Description: "Write a verified snapshot of the live database to /data/backups (or an explicit path). Safe to run while serving.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Snapshot",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in snapshotInput) (*mcp.CallToolResult, snapshotOutput, error) {
		path := in.Path
		if path == "" {
			path = filepath.Join(store.SnapshotDir(), fmt.Sprintf("memory-%s.db", time.Now().UTC().Format("20060102-150405")))
		}
		res, err := st.Snapshot(path)
		if err != nil {
			return nil, snapshotOutput{}, err
		}
		return nil, snapshotOutput{Path: res.Path, Rows: res.Rows, Size: res.Size, SchemaVersion: res.UserVersion}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_restore",
		Description: "Atomically install a snapshot as the live database, moving the replaced db aside as a .bak. DESTRUCTIVE: overwrites the current store.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Restore",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(true),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in restoreInput) (*mcp.CallToolResult, restoreOutput, error) {
		res, err := st.Restore(in.Path, in.Force)
		if err != nil {
			return nil, restoreOutput{}, err
		}
		return nil, restoreOutput{Path: res.LivePath, Rows: res.Rows, BackupPath: res.BackupPath}, nil
	})
}

// NewMux serves the streamable-HTTP /mcp endpoint and a non-MCP /healthz over
// one handler, the shape docs/design/pix-v2-architecture.md §9.1 describes.
// /healthz is liveness+readiness: 200 only once the store answers a live
// PRAGMA/ping.
func NewMux(st *store.Store) http.Handler {
	srv := New(st)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Ping(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": Version})
	})
	return mux
}
