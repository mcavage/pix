package memory

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/rpc"
)

// fakeRPCServer stands up an httptest JSON-RPC server that returns canned
// results per method, and returns an rpc.Client pointed at it.
func fakeRPCServer(t *testing.T, results map[string]any) rpc.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		res, ok := results[method]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32601, "message": "method not found"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
	}))
	t.Cleanup(srv.Close)
	// httptest URL is http://127.0.0.1:PORT — extract the port.
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parsing test server port %q: %v", port, err)
	}
	return rpc.Client{Port: p}
}

// capturingRPCServer stands up a JSON-RPC server that records the params of the
// most recent request per method (so tests can assert what the CLI forwarded)
// and returns a canned result.
func capturingRPCServer(t *testing.T, results map[string]any, seen map[string]map[string]any) rpc.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		if params, ok := req["params"].(map[string]any); ok {
			seen[method] = params
		} else {
			seen[method] = map[string]any{}
		}
		res, ok := results[method]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32601, "message": "method not found"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
	}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parsing test server port %q: %v", port, err)
	}
	return rpc.Client{Port: p}
}

// TestMemoryProfileForwarded checks the active profile is forwarded on the
// profile-scoped verbs (recall, remember, stats).
func TestMemoryProfileForwarded(t *testing.T) {
	seen := map[string]map[string]any{}
	c := capturingRPCServer(t, map[string]any{
		"recall":   map[string]any{"hits": []any{}},
		"remember": map[string]any{"id": "x", "reaffirmed": false},
		"stats":    map[string]any{"active": 0.0},
	}, seen)

	cases := []struct {
		sub    string
		call   func() error
		method string
	}{
		{"recall", func() error { return (CLI{c, &bytes.Buffer{}, "work"}).Recall("q", 8, "", false) }, "recall"},
		{"remember", func() error { return (CLI{c, &bytes.Buffer{}, "work"}).Remember("a fact", false) }, "remember"},
		{"stats", func() error { return (CLI{c, &bytes.Buffer{}, "work"}).Stats(false) }, "stats"},
	}
	for _, tc := range cases {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.sub, err)
		}
		if got, _ := seen[tc.method]["profile"].(string); got != "work" {
			t.Errorf("%s forwarded profile = %q, want %q", tc.sub, got, "work")
		}
	}
}

func TestMemoryRecall(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{
		"recall": map[string]any{"hits": []any{
			map[string]any{"id": "abc12345-de", "content": "likes midi guitar", "kind": "fact", "durability": "perishable", "project": "recipes", "score": 0.59, "createdAt": "2026-07-22T16:15:03Z"},
		}},
	})
	var out bytes.Buffer
	if err := (CLI{c, &out, "default"}).Recall("guitar", 8, "", false); err != nil {
		t.Fatalf("recall: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "abc12345") || !strings.Contains(got, "likes midi guitar") {
		t.Errorf("recall output missing content: %q", got)
	}
	if !strings.Contains(got, "0.59") {
		t.Errorf("recall output missing score: %q", got)
	}
	// FIX 3: a leading LOCAL-time ISO8601 timestamp, parsed from the RPC's
	// createdAt, must precede the id column.
	wantLocal := time.Date(2026, 7, 22, 16, 15, 3, 0, time.UTC).Local().Format(time.RFC3339)
	if !strings.HasPrefix(got, wantLocal) {
		t.Errorf("recall output must lead with the local-time timestamp %q, got: %q", wantLocal, got)
	}
}

// A hit with no createdAt gets a blank, column-aligned placeholder instead of
// crashing or dropping the column.
func TestMemoryRecall_NoTimestampDoesNotCrash(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{
		"recall": map[string]any{"hits": []any{
			map[string]any{"id": "abc12345-de", "content": "no timestamp on this one"},
		}},
	})
	var out bytes.Buffer
	if err := (CLI{c, &out, "default"}).Recall("x", 8, "", false); err != nil {
		t.Fatalf("recall: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "abc12345") || !strings.Contains(got, "no timestamp on this one") {
		t.Errorf("recall output missing content: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat(" ", len(time.RFC3339))) {
		t.Errorf("missing hit must get a blank column-aligned placeholder, got: %q", got)
	}
}

func TestMemoryTimestamp(t *testing.T) {
	if got := memoryTimestamp(""); got != strings.Repeat(" ", len(time.RFC3339)) {
		t.Errorf("empty createdAt should render a blank placeholder, got %q", got)
	}
	if got := memoryTimestamp("not-a-time"); got != strings.Repeat(" ", len(time.RFC3339)) {
		t.Errorf("unparseable createdAt should render a blank placeholder, got %q", got)
	}
	utc := time.Date(2026, 7, 22, 16, 15, 3, 0, time.UTC)
	want := utc.Local().Format(time.RFC3339)
	if got := memoryTimestamp(utc.Format(time.RFC3339)); got != want {
		t.Errorf("memoryTimestamp(RFC3339) = %q, want %q", got, want)
	}
	if got := memoryTimestamp(utc.Format(time.RFC3339Nano)); got != want {
		t.Errorf("memoryTimestamp(RFC3339Nano) = %q, want %q", got, want)
	}
}

func TestMemoryRecallJSON(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"recall": map[string]any{"hits": []any{}}})
	var out bytes.Buffer
	if err := (CLI{c, &out, "default"}).Recall("x", 8, "", true); err != nil {
		t.Fatalf("recall --json: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Errorf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
}

func TestMemoryRecallNoQuery(t *testing.T) {
	c := rpc.Client{Port: 1}
	if err := (CLI{c, &bytes.Buffer{}, "default"}).Recall("", 8, "", false); !cli.IsUsage(err) {
		t.Errorf("recall with no query: err = %v, want cli.UsageError2", err)
	}
}

func TestMemoryRemember(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"remember": map[string]any{"id": "deadbeef-00", "reaffirmed": false}})
	var out bytes.Buffer
	if err := (CLI{c, &out, "default"}).Remember("a new fact", false); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if !strings.Contains(out.String(), "remembered deadbeef") {
		t.Errorf("remember output = %q", out.String())
	}
}

func TestMemoryForget(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"forget": map[string]any{"ok": true}})
	var out bytes.Buffer
	if err := (CLI{c, &out, "default"}).Forget("abc123", false); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !strings.Contains(out.String(), "forgot abc123") {
		t.Errorf("forget output = %q", out.String())
	}
}

// The daemon's stats response still carries durable/perishable counts (the
// column survives until the U5 schema work), but the plain-text render must
// not surface them: every row this binary writes is durable now, so the
// split is no longer a meaningful default-view distinction. --json is a raw
// passthrough and is untouched by this test.
func TestMemoryStats(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"stats": map[string]any{
		"active": 10.0, "durable": 3.0, "perishable": 7.0, "facts": 8.0, "learnings": 2.0, "deleted": 1.0,
	}})
	var out bytes.Buffer
	if err := (CLI{c, &out, "default"}).Stats(false); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(out.String(), "active 10") {
		t.Errorf("stats output = %q", out.String())
	}
	if strings.Contains(out.String(), "durable") || strings.Contains(out.String(), "perishable") {
		t.Errorf("stats output must drop the durable/perishable split, got: %q", out.String())
	}
}

func TestMemoryServiceDown(t *testing.T) {
	// Nothing listening on this port -> rpc.ErrServiceDown.
	c := rpc.Client{Port: 1}
	err := (CLI{c, &bytes.Buffer{}, "default"}).Recall("x", 8, "", false)
	if err != rpc.ErrServiceDown {
		t.Errorf("recall against down service: err = %v, want rpc.ErrServiceDown", err)
	}
}

func TestFlagSetParse(t *testing.T) {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	limit := fs.Int("limit", 8)
	project := fs.Str("project", "")
	pos, err := fs.Parse([]string{"hello", "world", "--limit", "3", "--project=recipes", "--json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *limit != 3 {
		t.Errorf("limit = %d, want 3", *limit)
	}
	if *project != "recipes" {
		t.Errorf("project = %q, want recipes", *project)
	}
	if !fs.Json {
		t.Error("json flag not set")
	}
	if strings.Join(pos, " ") != "hello world" {
		t.Errorf("positional = %v, want [hello world]", pos)
	}
}

func TestFlagSetShortAlias(t *testing.T) {
	fs := cli.NewFlagSet()
	msg := fs.Str("message", "", "m")
	if _, err := fs.Parse([]string{"-m", "hello there"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *msg != "hello there" {
		t.Errorf("-m = %q, want 'hello there'", *msg)
	}
}

func TestFlagSetBool(t *testing.T) {
	fs := cli.NewFlagSet()
	b := fs.Bool("allow-main")
	pos, err := fs.Parse([]string{"--allow-main", "x"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*b {
		t.Error("allow-main not set")
	}
	if len(pos) != 1 || pos[0] != "x" {
		t.Errorf("pos = %v, want [x]", pos)
	}
}

func TestFlagSetErrors(t *testing.T) {
	cases := [][]string{
		{"--nope"},         // unknown flag
		{"--limit"},        // missing value
		{"--limit", "abc"}, // non-integer
	}
	for _, argv := range cases {
		fs := cli.NewFlagSet()
		fs.Int("limit", 8)
		if _, err := fs.Parse(argv); !cli.IsUsage(err) {
			t.Errorf("parse(%v) err = %v, want cli.UsageError2", argv, err)
		}
	}
}

// TestFlagSetHelpWinsOverValue is the F3 gate: a value-taking flag must NOT
// swallow a -h/--help token as its value. `-m --help` sets help and consumes
// no value (msg stays empty), so help wins over the pending value and the caller
// prints usage instead of running the side-effecting action.
func TestFlagSetHelpWinsOverValue(t *testing.T) {
	for _, argv := range [][]string{
		{"-m", "--help"}, {"--message", "-h"}, {"-m", "-h", "extra"}, {"foo", "-h"},
	} {
		fs := cli.NewFlagSet()
		msg := fs.Str("message", "", "m")
		pos, err := fs.Parse(argv)
		if err != nil {
			t.Fatalf("parse(%v): %v", argv, err)
		}
		if !fs.Help {
			t.Errorf("parse(%v) did not set help", argv)
		}
		if *msg != "" {
			t.Errorf("parse(%v) consumed a value for -m (%q) — help must win", argv, *msg)
		}
		if len(pos) != 0 {
			t.Errorf("parse(%v) returned positionals %v — help must short-circuit", argv, pos)
		}
	}
	// A help token AFTER a `--` terminator is passthrough, not ours: help stays off.
	fs := cli.NewFlagSet()
	fs.Str("message", "", "m")
	if _, err := fs.Parse([]string{"--", "--help"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fs.Help {
		t.Error("--help after -- must not set help")
	}
}

// TestKnowledgeSyncHelp_NoCommit is the F3 sync gate: `sync -m --help` must set
// help via the cli.FlagSet pre-scan and NOT consume `--help` as the -m value, so the
// commit/push path is never reached. Asserted at the cli.FlagSet level (the same
// parser runKnowledgeSync uses) so no git repo is required.
func TestKnowledgeSyncHelp_NoCommit(t *testing.T) {
	fs := cli.NewFlagSet()
	msg := fs.Str("message", "", "m")
	fs.Str("bundle", "")
	fs.Bool("allow-main")
	pos, err := fs.Parse([]string{"-m", "--help"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !fs.Help {
		t.Fatal("sync -m --help must set help (so it prints usage, not commit)")
	}
	if *msg != "" || len(pos) != 0 {
		t.Errorf("sync -m --help consumed a value/positional (msg=%q pos=%v) — commit path reachable", *msg, pos)
	}
}

func TestFlagSetTerminator(t *testing.T) {
	fs := cli.NewFlagSet()
	pos, err := fs.Parse([]string{"a", "--", "--not-a-flag", "b"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(pos, " ") != "a --not-a-flag b" {
		t.Errorf("pos = %v, want [a --not-a-flag b]", pos)
	}
}
