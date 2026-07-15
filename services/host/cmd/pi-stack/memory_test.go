package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeRPCServer stands up an httptest JSON-RPC server that returns canned
// results per method, and returns an rpcClient pointed at it.
func fakeRPCServer(t *testing.T, results map[string]any) rpcClient {
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
	return rpcClient{Port: p}
}

// capturingRPCServer stands up a JSON-RPC server that records the params of the
// most recent request per method (so tests can assert what the CLI forwarded)
// and returns a canned result.
func capturingRPCServer(t *testing.T, results map[string]any, seen map[string]map[string]any) rpcClient {
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
	return rpcClient{Port: p}
}

// TestMemoryProfileForwarded checks the active profile is forwarded on the
// profile-scoped verbs (recall, remember, learnings, stats).
func TestMemoryProfileForwarded(t *testing.T) {
	seen := map[string]map[string]any{}
	c := capturingRPCServer(t, map[string]any{
		"recall":     map[string]any{"hits": []any{}},
		"remember":   map[string]any{"id": "x", "reaffirmed": false},
		"promotable": map[string]any{"candidates": []any{}},
		"stats":      map[string]any{"active": 0.0},
	}, seen)

	cases := []struct {
		sub    string
		argv   []string
		method string
	}{
		{"recall", []string{"q"}, "recall"},
		{"remember", []string{"a", "fact"}, "remember"},
		{"learnings", nil, "promotable"},
		{"stats", nil, "stats"},
	}
	for _, tc := range cases {
		if err := dispatchMemory(tc.sub, tc.argv, c, &bytes.Buffer{}, "work"); err != nil {
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
			map[string]any{"id": "abc12345-de", "content": "likes midi guitar", "kind": "fact", "durability": "perishable", "project": "recipes", "score": 0.59},
		}},
	})
	var out bytes.Buffer
	if err := dispatchMemory("recall", []string{"guitar"}, c, &out, "default"); err != nil {
		t.Fatalf("recall: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "abc12345") || !strings.Contains(got, "likes midi guitar") {
		t.Errorf("recall output missing content: %q", got)
	}
	if !strings.Contains(got, "0.59") {
		t.Errorf("recall output missing score: %q", got)
	}
}

func TestMemoryRecallJSON(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"recall": map[string]any{"hits": []any{}}})
	var out bytes.Buffer
	if err := dispatchMemory("recall", []string{"x", "--json"}, c, &out, "default"); err != nil {
		t.Fatalf("recall --json: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Errorf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
}

func TestMemoryRecallNoQuery(t *testing.T) {
	c := rpcClient{Port: 1}
	if err := dispatchMemory("recall", nil, c, &bytes.Buffer{}, "default"); !isUsage(err) {
		t.Errorf("recall with no query: err = %v, want usageError", err)
	}
}

func TestMemoryRemember(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"remember": map[string]any{"id": "deadbeef-00", "reaffirmed": false}})
	var out bytes.Buffer
	if err := dispatchMemory("remember", []string{"a", "new", "fact"}, c, &out, "default"); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if !strings.Contains(out.String(), "remembered deadbeef") {
		t.Errorf("remember output = %q", out.String())
	}
}

func TestMemoryForget(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"forget": map[string]any{"ok": true}})
	var out bytes.Buffer
	if err := dispatchMemory("forget", []string{"abc123"}, c, &out, "default"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !strings.Contains(out.String(), "forgot abc123") {
		t.Errorf("forget output = %q", out.String())
	}
}

func TestMemoryLearnings(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"promotable": map[string]any{"candidates": []any{
		map[string]any{"id": "aa11-bb", "content": "always run tests", "frequency": 5.0},
	}}})
	var out bytes.Buffer
	if err := dispatchMemory("learnings", nil, c, &out, "default"); err != nil {
		t.Fatalf("learnings: %v", err)
	}
	if !strings.Contains(out.String(), "5x") || !strings.Contains(out.String(), "always run tests") {
		t.Errorf("learnings output = %q", out.String())
	}
}

func TestMemoryStats(t *testing.T) {
	c := fakeRPCServer(t, map[string]any{"stats": map[string]any{
		"active": 10.0, "durable": 3.0, "perishable": 7.0, "facts": 8.0, "learnings": 2.0, "deleted": 1.0,
	}})
	var out bytes.Buffer
	if err := dispatchMemory("stats", nil, c, &out, "default"); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(out.String(), "active 10") {
		t.Errorf("stats output = %q", out.String())
	}
}

func TestMemoryServiceDown(t *testing.T) {
	// Nothing listening on this port -> errServiceDown.
	c := rpcClient{Port: 1}
	err := dispatchMemory("recall", []string{"x"}, c, &bytes.Buffer{}, "default")
	if err != errServiceDown {
		t.Errorf("recall against down service: err = %v, want errServiceDown", err)
	}
}

func TestMemoryUnknownSub(t *testing.T) {
	if err := dispatchMemory("frobnicate", nil, rpcClient{Port: 1}, &bytes.Buffer{}, "default"); !isUsage(err) {
		t.Errorf("unknown sub: err = %v, want usageError", err)
	}
}

func TestFlagSetParse(t *testing.T) {
	fs := newFlagSet()
	limit := fs.int("limit", 8)
	project := fs.str("project", "")
	pos, err := fs.parse([]string{"hello", "world", "--limit", "3", "--project=recipes", "--json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *limit != 3 {
		t.Errorf("limit = %d, want 3", *limit)
	}
	if *project != "recipes" {
		t.Errorf("project = %q, want recipes", *project)
	}
	if !fs.json {
		t.Error("json flag not set")
	}
	if strings.Join(pos, " ") != "hello world" {
		t.Errorf("positional = %v, want [hello world]", pos)
	}
}

func TestFlagSetShortAlias(t *testing.T) {
	fs := newFlagSet()
	msg := fs.str("message", "", "m")
	if _, err := fs.parse([]string{"-m", "hello there"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *msg != "hello there" {
		t.Errorf("-m = %q, want 'hello there'", *msg)
	}
}

func TestFlagSetBool(t *testing.T) {
	fs := newFlagSet()
	b := fs.bool("allow-main")
	pos, err := fs.parse([]string{"--allow-main", "x"})
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
		fs := newFlagSet()
		fs.int("limit", 8)
		if _, err := fs.parse(argv); !isUsage(err) {
			t.Errorf("parse(%v) err = %v, want usageError", argv, err)
		}
	}
}

func TestFlagSetTerminator(t *testing.T) {
	fs := newFlagSet()
	pos, err := fs.parse([]string{"a", "--", "--not-a-flag", "b"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(pos, " ") != "a --not-a-flag b" {
		t.Errorf("pos = %v, want [a --not-a-flag b]", pos)
	}
}
