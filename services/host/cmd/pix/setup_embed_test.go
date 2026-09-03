package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// reportEmbedEnv fakes an ollama daemon (or its absence) for
// reportMemoryEmbeddingStatus, mirroring the pattern the inference and
// health packages already use for the same integration.
func reportEmbedEnv(t *testing.T, cliPresent bool, host string, models string) hostenv.Env {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(models))
	}))
	t.Cleanup(srv.Close)
	if host == "" {
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		host = u.Host
	}
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) {
			if cliPresent {
				return "/usr/local/bin/ollama", nil
			}
			return "", fmt.Errorf("not found")
		},
		GetenvFn: func(name string) string { return host },
	}
	return hostenv.Env{System: fake}
}

func TestReportMemoryEmbeddingStatus_NilEnvIsANoOp(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	reportMemoryEmbeddingStatus(d, hostenv.Env{})
	if out.Len() != 0 {
		t.Errorf("output = %q, want none for an unwired env", out.String())
	}
}

func TestReportMemoryEmbeddingStatus_NotInstalled(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	env := reportEmbedEnv(t, false, "127.0.0.1:1", `{"models":[]}`)
	reportMemoryEmbeddingStatus(d, env)
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("output = %q, want a not-installed note", out.String())
	}
}

func TestReportMemoryEmbeddingStatus_LocalPresent(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	env := reportEmbedEnv(t, true, "", `{"models":[{"name":"nomic-embed-text","size":274000000}]}`)
	reportMemoryEmbeddingStatus(d, env)
	if !strings.Contains(out.String(), "present") {
		t.Errorf("output = %q, want the model reported present", out.String())
	}
}

// TestReportMemoryEmbeddingStatus_LocalMissingNonInteractiveNeverPulls proves
// a non-interactive run only ever PRINTS the pull command, and never runs
// `ollama pull` on its own — no seam exists here to observe a shelled-out
// command, so this leans on RunInteractiveFn being unset: systest.Fake fails
// the test outright if an unwired seam is actually invoked.
func TestReportMemoryEmbeddingStatus_LocalMissingNonInteractiveNeverPulls(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out, Interactive: false}
	env := reportEmbedEnv(t, true, "", `{"models":[{"name":"qwen3.5:9b","size":6600000000}]}`)
	reportMemoryEmbeddingStatus(d, env)
	if !strings.Contains(out.String(), "ollama pull nomic-embed-text") {
		t.Errorf("output = %q, want the copy-pasteable pull command", out.String())
	}
}

func TestReportMemoryEmbeddingStatus_LocalMissingInteractiveDeclineNeverPulls(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out, Interactive: true, In: strings.NewReader("n\n")}
	env := reportEmbedEnv(t, true, "", `{"models":[]}`)
	reportMemoryEmbeddingStatus(d, env)
	if !strings.Contains(out.String(), "skipping") {
		t.Errorf("output = %q, want the decline path acknowledged", out.String())
	}
}

func TestReportMemoryEmbeddingStatus_LocalMissingInteractiveYesPulls(t *testing.T) {
	var out bytes.Buffer
	pulled := false
	fake := &systest.Fake{
		LookPathFn:       func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		RunInteractiveFn: func(name string, args ...string) error { pulled = true; return nil },
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	fake.GetenvFn = func(string) string { return u.Host }
	d := &cli.Deps{Out: &out, Err: &out, Interactive: true, In: strings.NewReader("y\n")}
	reportMemoryEmbeddingStatus(d, hostenv.Env{System: fake})
	if !pulled {
		t.Error("RunInteractiveFn was never called for an interactive yes")
	}
	if !fake.Ran("ollama") {
		t.Error("expected an ollama invocation to be recorded")
	}
}

func TestReportMemoryEmbeddingStatus_RemoteNeverOffersPull(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out, Interactive: true, In: strings.NewReader("y\n")}
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		GetenvFn:   func(string) string { return "team-ollama.internal:11434" },
	}
	reportMemoryEmbeddingStatus(d, hostenv.Env{System: fake})
	if strings.Contains(out.String(), "Pull ") || fake.Ran("ollama pull") {
		t.Errorf("output = %q, must never offer or run a pull for a remote endpoint", out.String())
	}
}
