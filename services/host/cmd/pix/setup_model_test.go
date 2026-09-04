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
	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/pixhome"
	"pix/host/sys"
	"pix/host/sys/systest"
	"pix/host/workflow/launch"
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

// TestSetupModelSelection_NonInteractiveNeverPromptsOrWrites: a script (no
// TTY) gets no prompt and secrets.env/pix.toml stay untouched, even with a
// configured provider and a scaffolded environment.
func TestSetupModelSelection_NonInteractiveNeverPromptsOrWrites(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("n\n2\n"), Interactive: false}

	setupModelSelection(d, home, defaultShellEnv(), true)

	if out.Len() != 0 {
		t.Errorf("non-interactive setup must print nothing new: %s", out.String())
	}
	if got := readFile(t, sidecar); got != before {
		t.Errorf("non-interactive setup must never write pix.toml:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestSetupModelSelection_NoProviderNoOllamaSkipsSilently: with no
// provider ref configured AND no Ollama answering, the picker has nothing
// to list and prints nothing (setupCredentials already covers that story).
func TestSetupModelSelection_NoProviderNoOllamaSkipsSilently(t *testing.T) {
	home, sidecar := modelSetupHome(t, "")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("1\n"), Interactive: true}

	setupModelSelection(d, home, ollamaEnvAt(t, "127.0.0.1:1", ""), true)

	if out.Len() != 0 {
		t.Errorf("no provider and no ollama must print nothing from the picker: %s", out.String())
	}
	if got := readFile(t, sidecar); got != before {
		t.Errorf("no configured provider must never write pix.toml")
	}
}

// TestSetupModelSelection_OllamaOnlyHomeCanPickALocalModel: a home with NO
// API key and a reachable Ollama is not a home with no models. The picker
// offers the catalog-known installed tags, records the pick as
// [models].main, and that recorded id must be one a run accepts — the
// selectable-equals-usable property, proven here against the real roster
// validator rather than assumed.
func TestSetupModelSelection_OllamaOnlyHomeCanPickALocalModel(t *testing.T) {
	home, sidecar := modelSetupHome(t, "")
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("1\n"), Interactive: true}
	env := ollamaEnvAt(t, "", `{"models":[{"name":"qwen3.5:9b"},{"name":"nomic-embed-text:latest"},{"name":"homegrown:7b"}]}`)

	setupModelSelection(d, home, env, true)

	got := readFile(t, sidecar)
	if !strings.Contains(got, `main = "ollama/qwen3.5:9b"`) {
		t.Fatalf("pix.toml did not record the picked local model:\n%s\noutput:\n%s", got, out.String())
	}
	if strings.Contains(out.String(), "nomic-embed-text") {
		t.Errorf("an embedding-only model must never be offered as a session model: %s", out.String())
	}
	if !strings.Contains(out.String(), "homegrown:7b") {
		t.Errorf("an installed but uncataloged tag must be reported, not silently dropped: %s", out.String())
	}
	sc, perr := envinfo.ParseSidecar(sidecar)
	if perr != nil {
		t.Fatal(perr)
	}
	if verr := validateRunRoster(&config.Config{}, launch.EnvSelection{Name: "default", Root: filepath.Dir(sidecar), Sidecar: sc}, nil); verr != nil {
		t.Errorf("the recorded model must be launchable on this same home: %v", verr)
	}
}

// ollamaEnvAt fakes this host's Ollama for the picker: an endpoint that
// answers /api/tags with models, or (with an unroutable host) one that does
// not answer at all.
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

// TestSetupModelSelection_EmptyInputAcceptsTheDefault: the plain-language
// [Y/n] confirm defaults to yes — a bare Enter accepts the shown fallback,
// never writes, and never shows internal "configured provider default"
// wording.
func TestSetupModelSelection_EmptyInputAcceptsTheDefault(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	if got := readFile(t, sidecar); got != before {
		t.Errorf("empty input must never write pix.toml:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	gotOut := out.String()
	if !strings.Contains(gotOut, "Default model: Anthropic is first among your configured providers. Use Claude Opus 5? [Y/n]") {
		t.Errorf("want the plain-language confirm naming the provider and model:\n%s", gotOut)
	}
	if !strings.Contains(gotOut, "using Claude Opus 5 (anthropic/claude-opus-5).") {
		t.Errorf("want the accepted default confirmed:\n%s", gotOut)
	}
	if strings.Contains(gotOut, "configured provider default") {
		t.Errorf("must never print the internal source wording:\n%s", gotOut)
	}
}

// TestSetupModelSelection_NoListsOnlyThatProvidersModels: answering "no"
// lists ONLY the fallback provider's models — no context window, no
// "configured provider default" wording, and never another provider's
// models.
func TestSetupModelSelection_NoListsOnlyThatProvidersModels(t *testing.T) {
	home, _ := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\nOPENAI_API_KEY=op://Vault/OpenAI/key\n")
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("n\nnot-a-number\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	gotOut := out.String()
	// The shipped fallback order tries openai before anthropic, so with both
	// configured OpenAI is the one provider this listing may name.
	if !strings.Contains(gotOut, "OpenAI models:") {
		t.Errorf("want the fallback provider's own list heading:\n%s", gotOut)
	}
	if strings.Contains(gotOut, "context") {
		t.Errorf("must never print a context window:\n%s", gotOut)
	}
	if strings.Contains(gotOut, "configured provider default") {
		t.Errorf("must never print the internal source wording:\n%s", gotOut)
	}
	if strings.Contains(gotOut, "anthropic/claude") || strings.Contains(gotOut, "Claude ") {
		t.Errorf("must never list a different provider's models:\n%s", gotOut)
	}
	if !strings.Contains(gotOut, "keeping GPT-5.6 Sol.") {
		t.Errorf("want the fallback named on an invalid choice:\n%s", gotOut)
	}
}

// TestSetupModelSelection_ChoosingListedModelWritesFullyQualifiedMain proves
// the whole write path: choosing an entry from the printed list records
// EXACTLY that fully-qualified catalog id under [models].main, and nothing
// else in the file changes.
func TestSetupModelSelection_ChoosingListedModelWritesFullyQualifiedMain(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	var out bytes.Buffer
	// Catalog order for anthropic: 1) claude-opus-5 (default/fallback)
	// 2) claude-fable-5 3) claude-sonnet-5 4) claude-haiku-4-5.
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("n\n3\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	got := readFile(t, sidecar)
	if !strings.Contains(got, `main = "anthropic/claude-sonnet-5"`) {
		t.Errorf("want [models].main recorded with the fully qualified id:\n%s", got)
	}
	if !strings.Contains(out.String(), `recorded [models].main = "anthropic/claude-sonnet-5"`) {
		t.Errorf("want setup to confirm what it recorded:\n%s", out.String())
	}
	// Nothing else in the file was touched: the memory section and the
	// original commented guidance both survive verbatim.
	if !strings.Contains(got, "[memory]\nscope = \"shared\"") {
		t.Errorf("unrelated sidecar content must survive untouched:\n%s", got)
	}
	activeMainLines := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "main = ") {
			activeMainLines++
		}
	}
	if activeMainLines != 1 {
		t.Errorf("want exactly one active main= line:\n%s", got)
	}
}

// TestSetupModelSelection_ExistingEnvironmentNeverRewritten proves W1: when
// the default environment was NOT scaffolded this run, the picker never
// prompts and never writes — it only displays.
func TestSetupModelSelection_ExistingEnvironmentNeverRewritten(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	before := readFile(t, sidecar)
	var out bytes.Buffer
	// A reader that WOULD answer "3" if ever read from — proving the
	// existing-environment path never even asks.
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("3\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), false)

	if got := readFile(t, sidecar); got != before {
		t.Errorf("an existing default environment must never be rewritten:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if strings.Contains(out.String(), "models:\n") {
		t.Errorf("an existing default environment must not be offered the picker:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "already exists; it will use Claude Opus 5") {
		t.Errorf("want the display-only fallback line naming the model and file:\n%s", out.String())
	}
	if strings.Contains(out.String(), "configured provider default") {
		t.Errorf("must never print the internal source wording:\n%s", out.String())
	}
	if !strings.Contains(out.String(), sidecar) {
		t.Errorf("want the exact pix.toml path named for a hand edit:\n%s", out.String())
	}
}

// TestSetupModelSelection_RefusesToOverwriteAnAlreadySetMain: a second run
// against a pix.toml that already declares [models].main (e.g. a hand edit
// mid-run) refuses rather than clobbering it.
func TestSetupModelSelection_RefusesToOverwriteAnAlreadySetMain(t *testing.T) {
	home, sidecar := modelSetupHome(t, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	existing := readFile(t, sidecar)
	existing = strings.Replace(existing, `# main = "anthropic/claude-sonnet-5"`, `main = "anthropic/claude-haiku-4-5"`, 1)
	if err := os.WriteFile(sidecar, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d := &cli.Deps{Sys: sys.Real{}, Out: &out, Err: &out, In: strings.NewReader("n\n3\n"), Interactive: true}

	setupModelSelection(d, home, defaultShellEnv(), true)

	got := readFile(t, sidecar)
	if got != existing {
		t.Errorf("an already-declared main must never be overwritten:\nbefore:\n%s\nafter:\n%s", existing, got)
	}
	if !strings.Contains(out.String(), "could not record the model choice") {
		t.Errorf("want the refusal reported:\n%s", out.String())
	}
}
