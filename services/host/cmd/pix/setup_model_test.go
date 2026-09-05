package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/pixhome"
	"pix/host/sys"
	"pix/host/sys/systest"
)

// modelSetupHome creates a temp PIX_HOME with the exact scaffolded default
// environment EnsureDefaultEnvironment would have written, and returns its
// resolved pixhome.Paths plus the pix.toml path.
func modelSetupHome(t *testing.T, refs string) (home pixhome.Paths, sidecarPath string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_HOME", dir)
	home, err := pixhome.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if refs != "" {
		if err := os.WriteFile(filepath.Join(dir, "secrets.env"), []byte(refs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := home.EnvironmentDir("default")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sidecarPath = filepath.Join(root, "pix.toml")
	body := `schema = 1

[models]
# main = "anthropic/claude-sonnet-5"  # a model NAME (provider/id); empty
# means the shipped default for whichever provider this home configures
# (never Pi's own native default). Run 'pix env show' to see the model
# that will answer and the rule that chose it.

[memory]
scope = "shared"
`
	if err := os.WriteFile(sidecarPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, sidecarPath
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ollamaEnvAt(t *testing.T, host, tags string) hostenv.Env {
	t.Helper()
	if host == "" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tags))
		}))
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		host = u.Host
	}
	return hostenv.Env{System: &systest.Fake{
		Base:       sys.Real{},
		GetenvFn:   func(string) string { return host },
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
	}}
}

func TestSetupModelSelection_LocalChoiceAndRerun(t *testing.T) {
	home, path := modelSetupHome(t, "")
	env := ollamaEnvAt(t, "", `{"models":[{"name":"qwen3.5:9b"}]}`)
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out, In: strings.NewReader("99\n1\n"), Interactive: true}
	if err := setupModelSelection(d, home, env, "default"); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	if !strings.Contains(before, `main = "ollama/qwen3.5:9b"`) {
		t.Fatal(before)
	}
	out.Reset()
	d = &cli.Deps{Out: &out, Err: &out, In: strings.NewReader(""), Interactive: true}
	if err := setupModelSelection(d, home, env, "default"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Model number") || readFile(t, path) != before {
		t.Fatal("rerun prompted or rewrote the model")
	}
}

func TestSetupModelSelection_CancelPreservesEnvironment(t *testing.T) {
	home, path := modelSetupHome(t, "")
	env := ollamaEnvAt(t, "", `{"models":[{"name":"qwen3.5:9b"}]}`)
	before := readFile(t, path)
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out, In: strings.NewReader("\n"), Interactive: true}
	if err := setupModelSelection(d, home, env, "default"); err == nil {
		t.Fatal("cancel reported success")
	}
	if readFile(t, path) != before {
		t.Fatal("cancel changed the environment")
	}
}

func TestSetupModelSelection_ArbitraryEnvironmentKeepsDeclaredModel(t *testing.T) {
	home, path := modelSetupHome(t, "")
	if err := os.Rename(filepath.Dir(path), home.EnvironmentDir("research")); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(home.EnvironmentDir("research"), "pix.toml")
	body := strings.Replace(readFile(t, path), "[models]", "[models]\nmain = \"ollama/qwen3.5:9b\"", 1)
	body = strings.ReplaceAll(body, `\"`, `"`)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	if err := setupModelSelection(d, home, defaultShellEnv(), "research"); err != nil {
		t.Fatal(err)
	}
	if readFile(t, path) != body {
		t.Fatal("changed declared model")
	}
}
