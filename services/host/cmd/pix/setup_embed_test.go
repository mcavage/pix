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
	"pix/host/sys"
	"pix/host/sys/systest"
)

// reportEmbedEnv fakes an ollama daemon (or its absence) for
// setupMemoryEmbeddings, mirroring the pattern the inference and
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
	setupMemoryEmbeddings(d, hostenv.Env{})
	if out.Len() != 0 {
		t.Errorf("output = %q, want none for an unwired env", out.String())
	}
}

func TestReportMemoryEmbeddingStatus_NotInstalled(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	env := reportEmbedEnv(t, false, "127.0.0.1:1", `{"models":[]}`)
	setupMemoryEmbeddings(d, env)
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("output = %q, want a not-installed note", out.String())
	}
}

func TestReportMemoryEmbeddingStatus_LocalPresent(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	env := reportEmbedEnv(t, true, "", `{"models":[{"name":"nomic-embed-text","size":274000000}]}`)
	setupMemoryEmbeddings(d, env)
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
	setupMemoryEmbeddings(d, env)
	if !strings.Contains(out.String(), "ollama pull nomic-embed-text") {
		t.Errorf("output = %q, want the copy-pasteable pull command", out.String())
	}
}

func TestReportMemoryEmbeddingStatus_LocalMissingInteractiveDeclineNeverPulls(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out, Interactive: true, In: strings.NewReader("n\n")}
	env := reportEmbedEnv(t, true, "", `{"models":[]}`)
	setupMemoryEmbeddings(d, env)
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
	setupMemoryEmbeddings(d, hostenv.Env{System: fake})
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
	setupMemoryEmbeddings(d, hostenv.Env{System: fake})
	if strings.Contains(out.String(), "Pull ") || fake.Ran("ollama pull") {
		t.Errorf("output = %q, must never offer or run a pull for a remote endpoint", out.String())
	}
}

// TestSetupMemoryEmbeddings_ReprobesAfterAPull: `ollama pull` exiting 0 is
// not proof the model is callable. A pull whose endpoint STILL does not
// list the model must be reported as such, not narrated as success — the
// success line is earned by the reprobe (safety invariant 15).
func TestSetupMemoryEmbeddings_ReprobesAfterAPull(t *testing.T) {
	for _, tc := range []struct {
		name      string
		after     string
		wantPhr   string
		unwantPhr string
	}{
		{"listed after the pull", `{"models":[{"name":"nomic-embed-text:latest"}]}`, "now lists it", "reported success, but"},
		{"still missing after the pull", `{"models":[]}`, "reported success, but", "now lists it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			pulled := false
			tags := `{"models":[]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tags))
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			fake := &systest.Fake{
				LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
				GetenvFn:   func(string) string { return u.Host },
				RunInteractiveFn: func(name string, args ...string) error {
					pulled = true
					tags = tc.after
					return nil
				},
			}
			d := &cli.Deps{Out: &out, Err: &out, Interactive: true, In: strings.NewReader("y\n")}
			setupMemoryEmbeddings(d, hostenv.Env{System: fake})
			if !pulled {
				t.Fatal("the pull never ran")
			}
			if !strings.Contains(out.String(), tc.wantPhr) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tc.wantPhr)
			}
			if strings.Contains(out.String(), tc.unwantPhr) {
				t.Errorf("output = %q, must not contain %q", out.String(), tc.unwantPhr)
			}
		})
	}
}

// TestSetupMemoryEmbeddings_PresentUnderTheDaemonsOwnTagSpelling: the model
// IS installed, just listed as `:latest`. Setup must report it present and
// never offer to pull it again.
func TestSetupMemoryEmbeddings_PresentUnderTheDaemonsOwnTagSpelling(t *testing.T) {
	var out bytes.Buffer
	env := reportEmbedEnv(t, true, "", `{"models":[{"name":"nomic-embed-text:latest"}]}`)
	d := &cli.Deps{Out: &out, Err: &out, Interactive: true, In: strings.NewReader("y\n")}
	setupMemoryEmbeddings(d, env)
	if !strings.Contains(out.String(), "present") || strings.Contains(out.String(), "Pull ") {
		t.Errorf("output = %q, want the already-installed model reported present with no pull offer", out.String())
	}
}

// TestSetupRunsOllamaBeforeTheContainerReconcile: the pix-memory
// container's OLLAMA_HOST/MEMORY_EMBED_MODEL environment is composed from
// the SAME detection this step drives, and the container's fingerprint
// includes it — so a model pulled after the reconcile lands in the next
// run's container, not this one. The order is the fix; this pins it.
func TestSetupRunsOllamaBeforeTheContainerReconcile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, _ := fakeInstallDir(t, "2.0.0")
	docker := &setupFakeDocker{}
	seams := setupSeamsFor(t, dir, docker, &setupFakeMCP{})
	fake := &systest.Fake{
		Base:             sys.Real{},
		LookPathFn:       func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		GetenvFn:         func(string) string { return "127.0.0.1:1" },
		RunInteractiveFn: func(name string, args ...string) error { return nil },
	}
	seams.Env = hostenv.Env{System: fake}
	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := (&setupCmd{}).run(d, seams); err != nil {
		t.Fatalf("pix setup: %v\n%s%s", err, out.String(), errb.String())
	}
	got := out.String() + errb.String()
	ollamaAt := strings.Index(got, "pix setup: ollama:")
	homeAt := strings.Index(got, "PIX_HOME")
	if ollamaAt < 0 || homeAt < 0 {
		t.Fatalf("output did not carry both the ollama step and the home report:\n%s", got)
	}
	if ollamaAt > homeAt {
		t.Errorf("the ollama/embedding step ran AFTER the container reconcile:\n%s", got)
	}
}
