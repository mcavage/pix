// setup_models_test.go — S08: `pix setup` local-model readiness + pulls.
//
// Invariants under test, all asserted at the SUBPROCESS-INVOCATION level (the
// recorded env.run/env.runInteractive calls are the exact `ollama ...` argv a
// real setup would exec):
//   - a NON-interactive setup NEVER pulls without the explicit --pull-models
//     consent flag — a broad --yes must not silently download gigabytes;
//   - --pull-models is explicit consent in ANY mode;
//   - the interactive flow shows ONE aggregate default-No prompt listing every
//     missing configured tag with a disk warning; empty answer / EOF = No;
//   - models are probed ONCE up front; only CONFIRMED-missing tags are ever
//     pulled; an unverifiable `ollama list` never counts as missing and never
//     pulls; one verification probe after the pulls;
//   - tags are DEDUPLICATED before pulling (watcher+bridge share a default);
//   - a partial pull failure fails setup (non-zero) with the exact retry
//     command, and the completion summary stays truthful;
//   - retired config keys (mcp_static/mcp_dynamic) are dropped on save with
//     ONE concise notice; unknown keys are never called retired;
//   - the completion summary reports keys / knowledge / pack / local models /
//     gog on separate readiness axes (empty pack is TODO, never green; gog
//     guidance is `pix gworkspace setup` only, never a raw gog auth command);
//   - the consent/pull outcome is receipted into launcher state via a
//     symlink-safe atomic write.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/workflow/man"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"strings"
	"testing"
	"time"
)

// ollamaWorld is a stateful fake Ollama installation: `ollama list` reflects
// `have`, `ollama pull <tag>` mutates it (unless told to fail or lie), and
// every invocation is recorded so tests assert on the exact subprocess calls.
type ollamaWorld struct {
	have       map[string]bool
	listErr    bool
	pullFail   map[string]bool
	pullLies   bool // pull "succeeds" but the tag never shows up in list
	gogAuthErr bool
	calls      []string
}

func (w *ollamaWorld) count(prefix string) int {
	n := 0
	for _, c := range w.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// modelsSetupEnv builds a hermetic hostenv.Env + temp config/state/data homes for
// driving setupHostPhase end to end with a stubbed provider-key flow.
func modelsSetupEnv(t *testing.T, w *ollamaWorld) hostenv.Env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	if w.have == nil {
		w.have = map[string]bool{}
	}
	if w.pullFail == nil {
		w.pullFail = map[string]bool{}
	}
	return hostenv.Env{
		System: &systest.Fake{
			GetenvFn:   func(string) string { return "" },
			LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
			ReadFileFn: func(p string) (string, error) {
				if strings.HasSuffix(p, "hostmode.env") {
					return "ANTHROPIC_API_KEY=op://v/a/k\nOPENAI_API_KEY=op://v/o/k\nGEMINI_API_KEY=op://v/g/k\n", nil
				}
				return "", os.ErrNotExist
			},
			WriteFileFn: func(string, []byte, os.FileMode) error { return nil },
			RunFn:       func(name string, args ...string) (string, error) { return ollamaWorldRun(w, name, args...) },
			// `ollama pull` is INTERACTIVE (it streams progress) and production has
			// always preferred this seam; the plain-runner fallback existed only for
			// fixtures that left it nil, so this fixture spent its life exercising a
			// path no user ever took. Same world, so the argv assertions still hold.
			RunInteractiveFn: func(name string, args ...string) error {
				_, err := ollamaWorldRun(w, name, args...)
				return err
			},
		},
		// The fixture's premise is that keys ARE provisioned (three op:// refs in
		// hostmode.env, setupProvisionKeysFn stubbed to succeed), so the credential
		// resolves and the probe answers.
		DirectInference: func(string, string, string) error { return nil },
		// Answers iff the fake daemon actually has the tag, which is what keeps the
		// pull-consent tests honest: an unpulled tag must still fail its probe.
		OllamaInference: func(_, model string, _ int, _ time.Duration) error {
			if w.have[model] {
				return nil
			}
			return fmt.Errorf("model %q is not pulled on this fake daemon", model)
		},
	}
}

// ollamaWorldRun is the fake Ollama installation's one behaviour, shared by the
// plain and interactive runners so a command behaves the same whichever seam
// production picks.
func ollamaWorldRun(w *ollamaWorld, name string, args ...string) (string, error) {
	w.calls = append(w.calls, name+" "+strings.Join(args, " "))
	if name == "op" && len(args) == 2 && args[0] == "read" {
		return "test-provider-key\n", nil
	}
	if name == "ollama" && len(args) > 0 {
		switch args[0] {
		case "list":
			if w.listErr {
				return "", fmt.Errorf("could not connect to ollama")
			}
			var b strings.Builder
			b.WriteString("NAME ID SIZE\n")
			for tag := range w.have {
				b.WriteString(tag + " x 1GB\n")
			}
			return b.String(), nil
		case "pull":
			tag := args[1]
			if w.pullFail[tag] {
				return "", fmt.Errorf("pull failed")
			}
			if !w.pullLies {
				w.have[tag] = true
			}
			return "", nil
		}
	}
	if name == "gog" && w.gogAuthErr {
		return "", fmt.Errorf("not authed")
	}
	return "", nil
}

func stubLiveInferenceOK(env *hostenv.Env) {
	run := systest.Of(env.System).RunFn
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		if name == "op" && len(args) == 2 && args[0] == "read" {
			return "test-provider-key\n", nil
		}
		return run(name, args...)
	}
	env.DirectInference = func(string, string, string) error { return nil }
}

// stubProvisionKeysOK bypasses the (separately tested) strict 1Password key
// flow so these tests exercise only the S08 model/summary machinery.
func stubProvisionKeysOK(t *testing.T) {
	t.Helper()
	orig := setupProvisionKeysFn
	setupProvisionKeysFn = func(hostenv.Env, io.Reader, io.Writer, bool, bool) bool { return true }
	t.Cleanup(func() { setupProvisionKeysFn = orig })
}

// --- requirement 1: consent -------------------------------------------------

// A non-interactive setup (here: --yes) must NEVER run `ollama pull`, even
// though every configured tag is confirmed missing — broad --yes is not
// consent to download gigabytes. It must print the explicit consent path.
func TestSetupModels_NoninteractiveNeverPulls(t *testing.T) {
	w := &ollamaWorld{}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, true); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if n := w.count("ollama pull"); n != 0 {
		t.Errorf("non-interactive setup ran %d `ollama pull` calls, want 0:\n%v", n, w.calls)
	}
	if !strings.Contains(out.String(), "--pull-models") {
		t.Errorf("non-interactive setup must point at the explicit --pull-models consent, got:\n%s", out.String())
	}
	// --yes on a TTY is still non-interactive: no prompt may even be shown.
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("--yes must never reach the pull prompt, got:\n%s", out.String())
	}
}

// --pull-models is explicit consent in any mode: every confirmed-missing
// DISTINCT tag is pulled exactly once (watcher+bridge share qwen3.5:9b — the
// dedupe requirement), then verified with exactly ONE post-pull list probe.
func TestSetupModels_ExplicitPullModels_PullsDeduped(t *testing.T) {
	w := &ollamaWorld{}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	for _, tag := range []string{"qwen3.5:9b", "nomic-embed-text"} {
		if n := w.count("ollama pull " + tag); n != 1 {
			t.Errorf("`ollama pull %s` ran %d times, want exactly 1 (deduped):\n%v", tag, n, w.calls)
		}
	}
	if n := w.count("ollama pull"); n != 2 {
		t.Errorf("total pulls = %d, want 2 distinct tags:\n%v", n, w.calls)
	}
	// Three: setupLocalModels probes once up front and verifies once after the
	// pulls, and setup's VERIFY phase then re-probes from scratch because the
	// report is a pure function of post-mutation evidence (AC-P0-302) and may
	// not read back what the mutation believed it did.
	if n := w.count("ollama list"); n != 3 {
		t.Errorf("`ollama list` ran %d times, want exactly 3 (probe, post-pull verify, verify phase):\n%v", n, w.calls)
	}
	if !strings.Contains(out.String(), "✓ local models") {
		t.Errorf("summary must report local models ready after verified pulls, got:\n%s", out.String())
	}
}

// When Ollama is already installed, setup offers one default-No memory-model
// pull. Declining keeps memory disabled and never blocks inference.
func TestSetupModels_InteractiveDefaultNo(t *testing.T) {
	for _, tc := range []struct {
		name, input string
	}{
		{"empty answer", "\n"},
		{"explicit n", "n\n"},
		{"eof", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &ollamaWorld{}
			env := modelsSetupEnv(t, w)
			stubProvisionKeysOK(t)
			var out bytes.Buffer
			// inference default, model-roster default, then this case's memory answer
			if err := setupHostPhase(env, nil, strings.NewReader("\n\n"+tc.input), &out, true); err != nil {
				t.Fatalf("unexpected error: %v\n%s", err, out.String())
			}
			if n := w.count("ollama pull"); n != 0 {
				t.Errorf("ordinary setup must not pull (input %q), got %d pulls:\n%v", tc.input, n, w.calls)
			}
			s := out.String()
			if !strings.Contains(s, "[y/N]") {
				t.Errorf("detected Ollama should offer the optional memory models, got:\n%s", s)
			}
			if !strings.Contains(s, "qwen3.5:9b") || !strings.Contains(s, "nomic-embed-text") {
				t.Errorf("the aggregate prompt must list every missing tag, got:\n%s", s)
			}
			if !strings.Contains(s, "pix setup --pull-models") {
				t.Errorf("must show the explicit opt-in command, got:\n%s", s)
			}
		})
	}
}

// An explicit interactive yes opts into the one aggregate model pull.
func TestSetupModels_InteractiveYesPulls(t *testing.T) {
	w := &ollamaWorld{}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	// inference default, roster default (all), then this run's memory answer.
	// The roster prompt appears because the provider keys now actually verify:
	// this fixture left both probe seams nil, so verification silently did
	// nothing, nothing became callable, and the roster step was skipped. The
	// prompt was always supposed to be here.
	if err := setupHostPhase(env, nil, strings.NewReader("\n\ny\n"), &out, true); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if n := w.count("ollama pull"); n != 2 {
		t.Errorf("interactive consent pulled %d times, want 2:\n%v", n, w.calls)
	}
	if strings.Contains(out.String(), "✓ qwen3.5:9b pulled and verified") || strings.Contains(out.String(), "✓ nomic-embed-text pulled and verified") {
		t.Errorf("the model mutation must not print success; success comes from post-mutation probes:\n%s", out.String())
	}
}

// --- requirement 2: unverifiable is never missing ----------------------------

// A failed `ollama list` proves nothing: no tag may be treated as missing, and
// NOTHING may be pulled — even with the explicit --pull-models consent. Not an
// error either (unverifiable never blocks).
func TestSetupModels_UnverifiableNeverPulled(t *testing.T) {
	w := &ollamaWorld{listErr: true}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	// --pull-models is an explicit REQUEST for the model axes, so a run that
	// cannot verify them has positively failed that request: exit 1
	// (AC-P0-210). The unverifiable tags are still never pulled, and nothing
	// is ever called "missing".
	err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatalf("--pull-models with Ollama down must fail setup:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "model.watcher") {
		t.Errorf("the error must name the requested axes that did not end ready, got: %v", err)
	}
	if n := w.count("ollama pull"); n != 0 {
		t.Errorf("unverifiable tags must NEVER be pulled, got %d pulls:\n%v", n, w.calls)
	}
	s := out.String()
	if !strings.Contains(s, "could not verify") {
		t.Errorf("output must say the models could not be verified, got:\n%s", s)
	}
	if strings.Contains(s, "missing") {
		t.Errorf("an unverifiable tag must never be called missing, got:\n%s", s)
	}
	if !strings.Contains(s, "⚠ local models") {
		t.Errorf("summary must render the unverifiable axis, got:\n%s", s)
	}
}

// --- requirement 2: partial failure is non-zero + truthful -------------------

func TestSetupModels_PartialPullFailureFailsSetup(t *testing.T) {
	w := &ollamaWorld{pullFail: map[string]bool{"nomic-embed-text": true}}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatalf("a partial pull failure must fail setup, got success:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "nomic-embed-text") || !strings.Contains(err.Error(), "ollama pull nomic-embed-text") {
		t.Errorf("the error must name the failed tag with the exact retry command, got: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "✗ local models") || !strings.Contains(s, "pull failed: nomic-embed-text") {
		t.Errorf("the summary must truthfully report the failed tag, got:\n%s", s)
	}
	if !strings.Contains(s, "qwen3.5:9b") {
		t.Errorf("the summary must still account for the successfully pulled tag, got:\n%s", s)
	}
}

// A pull that "succeeds" but is not visible in the single post-pull
// verification is a failure, not a success claim.
func TestSetupModels_PullLiesCaughtByVerification(t *testing.T) {
	w := &ollamaWorld{pullLies: true}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatalf("an unverified pull must fail setup, got success:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "does not show it") {
		t.Errorf("verification must call out the pull/list contradiction, got:\n%s", out.String())
	}
}

// --- requirement 5: retired config keys -------------------------------------

func TestSetup_RetiredKeysDroppedWithOneNotice(t *testing.T) {
	w := &ollamaWorld{have: map[string]bool{"qwen3.5:9b": true, "nomic-embed-text": true}}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	cfgPath := os.Getenv("PIX_CONFIG")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("mcp_static = [\"slack\"]\nmcp_dynamic = [\"notion\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "retired") || !strings.Contains(s, "mcp_static") || !strings.Contains(s, "mcp_dynamic") {
		t.Errorf("setup must print a retired-key notice naming the keys, got:\n%s", s)
	}
	if strings.Count(s, "retired") != 1 {
		t.Errorf("want exactly ONE concise retired-key notice, got:\n%s", s)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "mcp_static") || strings.Contains(string(saved), "mcp_dynamic") {
		t.Errorf("retired keys must be dropped on save, config still has:\n%s", saved)
	}
}

// An UNKNOWN key (froopy) is not retired and must never be swept into the
// retired notice.
func TestSetup_UnknownKeysNotCalledRetired(t *testing.T) {
	w := &ollamaWorld{have: map[string]bool{"qwen3.5:9b": true, "nomic-embed-text": true}}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	cfgPath := os.Getenv("PIX_CONFIG")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("froopy = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "retired") {
		t.Errorf("an unknown key must not trigger (or be named in) the retired notice, got:\n%s", out.String())
	}
}

// --- requirement 4: the completion summary -----------------------------------

// The exact normal summary hides packs and optional knowledge. It reports only
// verified inference and explicitly enabled local models.
func TestSetupModels_ExactSummary(t *testing.T) {
	w := &ollamaWorld{have: map[string]bool{"qwen3.5:9b": true, "nomic-embed-text": true}}
	env := modelsSetupEnv(t, w)
	stubLiveInferenceOK(&env)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	// Google Workspace is OPTIONAL and ABSENT from the default path
	// (AC-P0-319): with no opt-in, the summary says nothing about it at all —
	// no `workspace`/`gog` row here.
	want := "Setup summary:\n" +
		fmt.Sprintf("  %s %-12s %s\n", "✓", "inference", "9 callable model(s) via anthropic, google, openai") +
		fmt.Sprintf("  %s %-12s %s\n", "✓", "local models", "pulled: nomic-embed-text, qwen3.5:9b") +
		"Core ready: verified inference is configured.\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("summary mismatch.\nwant block:\n%s\ngot output:\n%s", want, out.String())
	}
}

// With a knowledge bundle configured and a NON-empty active pack, the core is
// provisioned — and the pack axis only goes green with actual content.
func TestSetupModels_SummaryProvisionedWhenCoreReady(t *testing.T) {
	w := &ollamaWorld{have: map[string]bool{"qwen3.5:9b": true, "nomic-embed-text": true}}
	env := modelsSetupEnv(t, w)
	stubLiveInferenceOK(&env)
	stubProvisionKeysOK(t)

	// Pre-create a non-empty default pack and activate it in config, plus a
	// knowledge bundle path.
	root := pack.DefaultPackRoot()
	if err := os.MkdirAll(filepath.Join(root, "skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pack.WriteManifest(root, pack.Manifest{Name: "default", Schema: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "demo", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	kb := t.TempDir()
	cfgPath := os.Getenv("PIX_CONFIG")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "pack = " + fmt.Sprintf("%q", root) + "\nknowledge_bundles = [" + fmt.Sprintf("%q", kb) + "]\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "✓ knowledge") || !strings.Contains(s, "✓ pack") {
		t.Errorf("knowledge + non-empty pack must be green, got:\n%s", s)
	}
	if !strings.Contains(s, "Core ready: verified inference is configured.") {
		t.Errorf("core must be provisioned, got:\n%s", s)
	}
}

// --- requirement 6: gog guidance is `pix gworkspace setup` only ----------------

func TestSetupModels_GogGuidanceIsGogSetupOnly(t *testing.T) {
	w := &ollamaWorld{have: map[string]bool{"qwen3.5:9b": true, "nomic-embed-text": true}, gogAuthErr: true}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	cfgPath := os.Getenv("PIX_CONFIG")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("google_workspace_account = \"me@example.com\"\nmcp = [\""+config.GWServerName+"\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "✗ workspace") || !strings.Contains(s, "pix gworkspace setup") {
		t.Errorf("an unhealthy configured gog must point at `pix gworkspace setup`, got:\n%s", s)
	}
	if strings.Contains(s, "gog auth login") || strings.Contains(s, "sbx mcp auth") {
		t.Errorf("gog is a LOCAL stdio MCP: setup must never print a raw gog auth command or native sbx mcp auth for it, got:\n%s", s)
	}
}

// --- requirement 3: the launcher state receipt -------------------------------

func TestSetupModels_ReceiptWritten(t *testing.T) {
	w := &ollamaWorld{}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes", "--pull-models"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	path := filepath.Join(os.Getenv("XDG_STATE_HOME"), "pix", "setup", "models.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("receipt not written: %v", err)
	}
	var rec setupModelsReceipt
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("receipt is not valid JSON: %v\n%s", err, b)
	}
	if rec.Consent != "--pull-models" {
		t.Errorf("receipt consent = %q, want %q", rec.Consent, "--pull-models")
	}
	got := map[string]string{}
	for _, m := range rec.Models {
		got[m.Tag] = m.Status
	}
	if got["qwen3.5:9b"] != "pulled" || got["nomic-embed-text"] != "pulled" {
		t.Errorf("receipt statuses = %v, want both tags pulled", got)
	}
}

// The receipt write is symlink-safe: a symlinked <state>/setup dir is refused,
// never written through.
func TestWriteSetupModelsReceipt_RefusesSymlinkedDir(t *testing.T) {
	stateDir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(stateDir, "setup")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := writeSetupModelsReceipt(stateDir, setupModelsReceipt{Schema: 1, Consent: "none"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlinked setup state dir must be refused, got err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, "models.json")); statErr == nil {
		t.Error("the receipt must never land through the symlink")
	}
}

func TestWriteSetupModelsReceipt_AtomicWrite(t *testing.T) {
	stateDir := t.TempDir()
	rec := buildSetupModelsReceipt(setupModelsOutcome{
		installed: true,
		consent:   "prompt-no",
		missing:   []missingModel{{tag: "qwen3.5:9b", roles: []string{"watcher", "bridge"}}},
	}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err := writeSetupModelsReceipt(stateDir, rec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "setup", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got setupModelsReceipt
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Consent != "prompt-no" || len(got.Models) != 1 || got.Models[0].Status != "missing" {
		t.Errorf("receipt round-trip mismatch: %+v", got)
	}
}

// --- flags/help/man ----------------------------------------------------------

func TestParseOnboardArgs_PullModels(t *testing.T) {
	o, err := onboard.ParseOnboardArgs([]string{"--pull-models", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.PullModels {
		t.Error("--pull-models must parse")
	}
}

func TestSetupUsageAndManMentionPullModels(t *testing.T) {
	if !strings.Contains(setupUsage, "--pull-models") {
		t.Error("setup usage must document --pull-models")
	}
	b, err := man.Source(), error(nil)
	if err != nil {
		t.Fatal(err)
	}
	man := string(b)
	if !strings.Contains(man, "pull\\-models") {
		t.Error("man page setup synopsis must mention --pull-models")
	}
	if strings.Contains(man, "use\\-sbx\\-keys") {
		t.Error("man page must not still advertise the removed --use-sbx-keys flag")
	}
}

func TestIsValidOllamaTag(t *testing.T) {
	tests := []struct {
		tag  string
		want bool
	}{
		{"", false},
		{"-foo", false},
		{"foo", true},
		{"llama3", true},
		{"llama3:70b", true},
		{"llama-3:70b", true},
		{"llama_3:70b", true},
		{"namespace/model:tag", true},
		{"namespace.model:tag", true},
		{"hello world", false},
		{"hello\tworld", false},
		{"hello\nworld", false},
		{"hello/../world", true}, // It passes the character check; maybe it should fail? wait, `.` and `/` are allowed.
		{"$hello", false},
		{"hello|world", false},
		{"hello>world", false},
		{"hello<world", false},
		{"hello&world", false},
		{"hello;world", false},
	}
	for _, tt := range tests {
		if got := isValidOllamaTag(tt.tag); got != tt.want {
			t.Errorf("isValidOllamaTag(%q) = %v, want %v", tt.tag, got, tt.want)
		}
	}
}
