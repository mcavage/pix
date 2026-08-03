package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/sys/systest"
	"pix/host/workflow/doctor"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
)

// TestSetupSandboxName derives pix-<base> under the default profile.
func TestSetupSandboxName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	name, ok := setupSandboxName("/some/path/tact")
	if !ok {
		t.Fatal("expected name resolution to succeed under default profile")
	}
	if want := "pix-tact"; name != want {
		t.Errorf("setupSandboxName = %q, want %q", name, want)
	}
}

func TestSyncGitHubCredentialFromHost(t *testing.T) {
	const token = "github-secret-value"
	var calls []string
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "gh" {
			return token + "\n", nil
		}
		return "", nil
	}}}
	if err := syncGitHubCredentialFromHost(env); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "gh auth token" || calls[1] != "sbx secret set github -f -t "+token {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestSyncGitHubCredentialFromHostRedactsFailure(t *testing.T) {
	const token = "github-secret-value"
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "gh" {
			return token, nil
		}
		return "rejected " + token, errors.New("failed with " + token)
	}}}
	err := syncGitHubCredentialFromHost(env)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("error must be useful and redacted, got %v", err)
	}
}

// --- item 4: DIR must be validated BEFORE the host phase mutates anything ---

// runSetupCore must refuse a NONEXISTENT DIR before ever invoking hostPhase —
// the stub fails the test if called, proving the invalid-DIR path never
// reaches (and therefore never mutates) op-refs.env/hostmode.env/config.toml/
// the default pack/memory/host-mode.
func TestRunSetupCore_NonexistentDir_NoHostPhaseInvocation(t *testing.T) {
	invoked := false
	stub := func(hostenv.Env, []string, io.Reader, io.Writer, bool) error {
		invoked = true
		return nil
	}
	err := runSetupCore(hostenv.Env{System: &systest.Fake{}}, filepath.Join(t.TempDir(), "does-not-exist"), nil, strings.NewReader(""), &bytes.Buffer{}, false, stub)
	if err == nil {
		t.Fatal("a nonexistent DIR must fail runSetupCore")
	}
	if invoked {
		t.Fatal("hostPhase must NEVER be invoked for a nonexistent DIR")
	}
}

// runSetupCore must refuse a DIR that names a FILE (not a directory) before
// ever invoking hostPhase, for the same reason.
func TestRunSetupCore_FileNotDir_NoHostPhaseInvocation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	invoked := false
	stub := func(hostenv.Env, []string, io.Reader, io.Writer, bool) error {
		invoked = true
		return nil
	}
	err := runSetupCore(hostenv.Env{System: &systest.Fake{}}, file, nil, strings.NewReader(""), &bytes.Buffer{}, false, stub)
	if err == nil {
		t.Fatal("a DIR that is a file (not a directory) must fail runSetupCore")
	}
	if invoked {
		t.Fatal("hostPhase must NEVER be invoked when DIR is a file, not a directory")
	}
}

// runSetupCore must invoke hostPhase (and return its result) for a DIR that
// genuinely exists and is a directory, and for the "." default.
func TestRunSetupCore_ValidDir_InvokesHostPhase(t *testing.T) {
	dir := t.TempDir()
	for _, valid := range []string{dir, "."} {
		invoked := false
		stub := func(hostenv.Env, []string, io.Reader, io.Writer, bool) error {
			invoked = true
			return nil
		}
		if err := runSetupCore(hostenv.Env{System: &systest.Fake{}}, valid, nil, strings.NewReader(""), &bytes.Buffer{}, false, stub); err != nil {
			t.Fatalf("dir=%q: unexpected error: %v", valid, err)
		}
		if !invoked {
			t.Fatalf("dir=%q: hostPhase must be invoked for a valid dir", valid)
		}
	}
}

// runSetupCore must propagate hostPhase's own error/return value unchanged
// (it's a thin validate-then-call, not a swallow).
func TestRunSetupCore_PropagatesHostPhaseError(t *testing.T) {
	wantErr := fmt.Errorf("boom")
	stub := func(hostenv.Env, []string, io.Reader, io.Writer, bool) error { return wantErr }
	if err := runSetupCore(hostenv.Env{System: &systest.Fake{}}, ".", nil, strings.NewReader(""), &bytes.Buffer{}, false, stub); err != wantErr {
		t.Errorf("expected hostPhase's own error to propagate, got %v", err)
	}
}

// Semantic flag/value mistakes are rejected by the pre-adoption validator.
// This function is deliberately pure/read-only: runSetupCmd invokes it before
// pack use, pack hooks, OAuth, prerequisites, or any host-state mutation.
func TestValidateSetupSemantics_RejectsBeforeMutationBoundary(t *testing.T) {
	cfg := &config.Config{}
	env := hostenv.Env{System: &systest.Fake{RunFn: func(string, ...string) (string, error) {
		t.Fatal("semantic validation must not execute a command for these invalid inputs")
		return "", nil
	}}}
	resolver := func() (string, error) { return "", fmt.Errorf("not available") }

	cases := []struct {
		name string
		opts onboard.Opts
		want string
	}{
		{"with without pack", onboard.Opts{WithSetup: []string{"oauth"}}, "--with requires --pack"},
		{"account without opt-in", onboard.Opts{Account: "me@example.com"}, "--account requires --google-workspace"},
		{"credentials without opt-in", onboard.Opts{Credentials: "/tmp/client.json"}, "--credentials requires --google-workspace"},
		{"unknown mcp", onboard.Opts{Mcp: []string{"not-a-real-server"}}, "not an allowlisted server"},
		{"model whitespace", onboard.Opts{Model: "bad model"}, "must not contain whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSetupSemantics(tc.opts, cfg, env, resolver)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateSetupSemantics() error = %v, want containing %q", err, tc.want)
			}
			var usage errUsage
			if !errors.As(err, &usage) {
				t.Fatalf("semantic error must map to usage/exit 2, got %T: %v", err, err)
			}
		})
	}
}

func TestValidateSetupSemantics_AcceptsValidCatalogAndGoogleOptions(t *testing.T) {
	opts := onboard.Opts{
		GoogleWorkspace: true,
		Account:         "me@example.com",
		Credentials:     "/tmp/client.json",
		Packs:           []string{"team-pack"},
		WithSetup:       []string{"oauth"},
		Mcp:             []string{"notion"},
		Model:           "qwen3.5:9b",
		Knowledge:       "/tmp/knowledge",
	}
	if err := validateSetupSemantics(opts, &config.Config{}, hostenv.Env{System: &systest.Fake{}}, noHostResolver); err != nil {
		t.Fatalf("valid setup semantics rejected: %v", err)
	}
}

// Exercise the real dispatcher boundary in a child process because
// runSetupCmd intentionally exits on usage errors. A valid local pack is
// supplied so reaching adoption would leave both config and pack.lock residue;
// malformed --model must exit 2 before either can happen.
func TestRunSetupCmd_SemanticErrorPrecedesPackAdoption(t *testing.T) {
	if os.Getenv("PIX_TEST_SETUP_SEMANTIC_CHILD") == "1" {
		runSetupCmd([]string{
			os.Getenv("PIX_TEST_SETUP_WORKSPACE"),
			"--pack", os.Getenv("PIX_TEST_SETUP_PACK"),
			"--model", "bad model",
			"--no-agent",
		})
		return
	}

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	packDir := filepath.Join(root, "pack")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestPack(t, packDir, "semantic-boundary-test")
	configPath := filepath.Join(root, "config", "config.toml")

	cmd := exec.Command(os.Args[0], "-test.run", "^TestRunSetupCmd_SemanticErrorPrecedesPackAdoption$")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_SETUP_SEMANTIC_CHILD=1",
		"PIX_TEST_SETUP_WORKSPACE="+workspace,
		"PIX_TEST_SETUP_PACK="+packDir,
		"PIX_CONFIG="+configPath,
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("child exit = %v, output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "must not contain whitespace") {
		t.Fatalf("child did not report the semantic error:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(packDir, pack.PackLockName)); !os.IsNotExist(err) {
		t.Fatalf("pack adoption ran before semantic validation; pack.lock stat = %v", err)
	}
	if b, err := os.ReadFile(configPath); err == nil && strings.Contains(string(b), packDir) {
		t.Fatalf("pack adoption ran before semantic validation; config contains pack path:\n%s", b)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading child config: %v", err)
	}
}

// setupHostPhase must ABORT (return error, write nothing further) when no model
// key can be provisioned and it's non-interactive (no prompt) — the fix for the
// double-prompt + false "ready".
func TestSetupHostPhase_NoKeyAborts(t *testing.T) {
	// Isolate from the developer's real config: setupHostPhase consults
	// configuredKeylessInference -> config.Load(), so a real pix config with
	// keyless Ollama bindings on disk would make setup proceed without a model
	// key, defeating the "must abort" assertion.
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(n string) (string, error) { return "/usr/bin/" + n, nil }, ReadFileFn: func(string) (string, error) { return "", os.ErrNotExist }, RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
			return "github\n", nil // no model key
		}
		return "", nil
	}}}
	var out bytes.Buffer
	// flags present -> non-interactive -> no prompt; missing key -> error.
	err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, true)
	if err == nil {
		t.Fatal("setup must abort when no model key is configured")
	}
	if strings.Contains(out.String(), "1Password") && strings.Contains(out.String(), "Manage model keys") {
		t.Error("non-interactive setup must not show the 1Password prompt")
	}
}

// setupHostPhase must reject unsupported setup flags and validate
// --mcp/--knowledge/--model BEFORE touching provider keys at all. An invalid
// flag must abort before setupProvisionKeysFn, with no 1Password prompt, ref
// write, or sbx reconciliation for a request that cannot be applied.
func TestSetupHostPhase_InvalidFlags_NeverInvokesProviderKeyFlow(t *testing.T) {
	orig := setupProvisionKeysFn
	t.Cleanup(func() { setupProvisionKeysFn = orig })

	cases := []struct {
		name  string
		flags []string
	}{
		{"unallowlisted mcp name", []string{"--yes", "--mcp", "not-a-real-server"}},
		{"whitespace in ollama_bridge_model", []string{"--yes", "--model", "bad model"}},
		{"onboard-only apply", []string{"--yes", "--apply"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))

			invoked := false
			setupProvisionKeysFn = func(hostenv.Env, io.Reader, io.Writer, bool, bool) bool {
				invoked = true
				return true
			}

			var out bytes.Buffer
			err := setupHostPhase(hostenv.Env{System: &systest.Fake{}}, tc.flags, strings.NewReader(""), &out, false)
			if err == nil {
				t.Fatalf("expected setupHostPhase to fail for flags %v", tc.flags)
			}
			if invoked {
				t.Errorf("setupProvisionKeysFn must NEVER be invoked when flag validation fails (flags %v); no provider-key/ref/sbx work may run for a request that will be rejected", tc.flags)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(statErr) {
				t.Errorf("config.toml must not be written when flag validation fails, stat err = %v", statErr)
			}
		})
	}
}

// host-state reports host readiness only when enabled AND provisioned.
func TestHostStateHostReadiness(t *testing.T) {
	cfg := &config.Config{MemoryWatcherModel: "x", MemoryEmbedModel: "y"}
	cfg.Host.Enabled = true
	hs := buildHostState(cfg, "", false, func(int) bool { return false }, "", hostStatePack{})
	if !hs.Host.Enabled {
		t.Error("host.enabled should reflect config")
	}
	// In this test env host mode is not provisioned, so Ready must be false even
	// though Enabled is true (the exact enabled!=ready bug).
	if hs.Host.Ready && !hs.Host.Provisioned {
		t.Error("ready must be false when not provisioned")
	}
}

func TestSetupSelectRunnableIntentForSingleProvider(t *testing.T) {
	for _, tc := range []struct {
		refs, start, want string
		changed           bool
	}{
		{"ANTHROPIC_API_KEY=op://v/a/k\n", config.DefaultRunIntent, "strategy", true},
		{"GEMINI_API_KEY=op://v/g/k\n", config.DefaultRunIntent, "review", true},
		{"OPENAI_API_KEY=op://v/o/k\n", config.DefaultRunIntent, config.DefaultRunIntent, false},
		{"ANTHROPIC_API_KEY=op://v/a/k\nOPENAI_API_KEY=op://v/o/k\n", config.DefaultRunIntent, config.DefaultRunIntent, false},
		{"ANTHROPIC_API_KEY=op://v/a/k\n", "code", "code", false},
	} {
		env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return tc.refs, nil }}}
		cfg := &config.Config{RunIntent: tc.start}
		if got := setupSelectRunnableIntent(cfg, env); got != tc.changed {
			t.Errorf("refs=%q changed=%v, want %v", tc.refs, got, tc.changed)
		}
		if cfg.RunIntent != tc.want {
			t.Errorf("refs=%q intent=%q, want %q", tc.refs, cfg.RunIntent, tc.want)
		}
	}
}

// setupProvisionKeys with `op` entirely missing is a HARD precondition
// failure (never fail-open) — without op there's nothing to source keys
// from at all. See setup_keys_flow_test.go's SbxUnavailable_FailsOpen for the
// case that DOES fail open (valid refs, sbx itself unreachable).
func TestSetupProvisionKeys_OpMissingNeverFailsOpen(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }, GetenvFn: func(string) string { return "/cfg" }, ReadFileFn: func(string) (string, error) { return "", os.ErrNotExist }}}
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, false) {
		t.Error("op missing must fail setup, never fail open")
	}
	if strings.Contains(out.String(), "Paste an op://") {
		t.Error("non-interactive must not prompt for refs")
	}
}

func TestHostModeProviderKeys(t *testing.T) {
	mk := func(hostmode string) hostenv.Env {
		return hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return "/cfg"
			}
			return ""
		}, ReadFileFn: func(p string) (string, error) {
			if p == filepath.Join("/cfg", "pix", "hostmode.env") {
				return hostmode, nil
			}
			return "", os.ErrNotExist
		}}}
	}
	if got, err := secret.HostModeProviderKeys(mk("")); len(got) != 0 || err != nil {
		t.Errorf("empty hostmode.env: want none/nil, got %v, %v", got, err)
	}
	got, err := secret.HostModeProviderKeys(mk("OPENAI_API_KEY=op://v/o/k\nANTHROPIC_API_KEY=op://v/a/k\nSLACK_TOKEN=op://v/s/t\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "anthropic" || got[1] != "openai" {
		t.Errorf("want [anthropic openai] sorted, got %v", got)
	}
}

// TestHostModeProviderKeys_ENOENTIsNoneNotError: a genuinely absent
// hostmode.env is "no refs configured yet" (nil error), never treated the
// same as a real read failure.
func TestHostModeProviderKeys_ENOENTIsNoneNotError(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(string) string { return "/cfg" }, ReadFileFn: func(string) (string, error) { return "", os.ErrNotExist }}}
	got, err := secret.HostModeProviderKeys(env)
	if err != nil || len(got) != 0 {
		t.Errorf("ENOENT: got %v, %v; want none, nil", got, err)
	}
}

// TestHostModeProviderKeys_RealReadErrorIsError: EACCES/ELOOP/etc must surface
// as a non-nil error naming the path, never silently downgraded to "none".
func TestHostModeProviderKeys_RealReadErrorIsError(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(string) string { return "/cfg" }, ReadFileFn: func(string) (string, error) { return "", os.ErrPermission }}}
	got, err := secret.HostModeProviderKeys(env)
	if err == nil {
		t.Fatal("a real read error must not be masked as none")
	}
	if !strings.Contains(err.Error(), "hostmode.env") {
		t.Errorf("error must name the path, got: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil on error", got)
	}
}

// TestHostModeProviderKeys_SymlinkLoopIsError: a symlink-loop-shaped error
// (ELOOP via os.PathError) classifies identically to any other non-ENOENT
// error.
func TestHostModeProviderKeys_SymlinkLoopIsError(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(string) string { return "/cfg" }, ReadFileFn: func(string) (string, error) {
		return "", &os.PathError{Op: "open", Path: "hostmode.env", Err: os.ErrInvalid}
	}}}
	_, err := secret.HostModeProviderKeys(env)
	if err == nil {
		t.Error("a symlink-loop-shaped read error must classify as an error, never none")
	}
}

// TestOnboardingKickoffCarriesGeneratedMarker: onboardingKickoff is a
// synthesized user-role message, not something the user typed. It must carry
// generatedInputMarker so extensions/memory-capture.ts's shouldCaptureUserText
// recognizes it and skips capture — the fix for the watcher inventing
// pix facts/events from the kickoff line (see generatedInputMarker's doc
// comment in setup.go).
func TestOnboardingKickoffCarriesGeneratedMarker(t *testing.T) {
	if !strings.HasPrefix(onboardingKickoff, generatedInputMarker) {
		t.Fatalf("onboardingKickoff must start with generatedInputMarker %q, got %q", generatedInputMarker, onboardingKickoff)
	}
	if !strings.HasPrefix(generatedInputMarker, "[pix-generated:") {
		t.Fatalf("generatedInputMarker must start with the [pix-generated: contract prefix, got %q", generatedInputMarker)
	}
}

// --- item C: runSetupHandoff (post-host-phase decision) -------------------

// An EXISTING sandbox without --replace is left alone: setup reports success
// ("reconciled", not "current"), prints the exact choices, and never calls
// runFn (never replays the onboarding kickoff into a live session).
func TestRunSetupHandoff_ExistingSandbox_LeftAloneNoRunFn(t *testing.T) {
	for _, state := range []doctor.SbxState{sbxRunning, sbxStopped} {
		var out bytes.Buffer
		called := false
		if err := runSetupHandoff(".", "pix-demo", state, false, &out, func([]string) { called = true }); err != nil {
			t.Fatalf("state %v: unexpected error: %v", state, err)
		}
		if called {
			t.Errorf("state %v: an existing sandbox must never be handed the onboarding kickoff", state)
		}
		if !strings.Contains(out.String(), `Host configuration reconciled. Existing sandbox "pix-demo" was left alone.`) {
			t.Errorf("state %v: must print the exact reconciled line, got:\n%s", state, out.String())
		}
		if !strings.Contains(out.String(), "pix run ") {
			t.Errorf("state %v: must print the reattach choice, got:\n%s", state, out.String())
		}
		if !strings.Contains(out.String(), "pix setup --replace") {
			t.Errorf("state %v: recreate choice must be `pix setup --replace` (setup owns the tour), got:\n%s", state, out.String())
		}
		if strings.Contains(out.String(), "pix run --replace") {
			t.Errorf("state %v: must NOT steer at bare `pix run --replace` (it would recreate without the tour), got:\n%s", state, out.String())
		}
		// The explanation of what each choice means (attachments are create-time).
		if !strings.Contains(out.String(), "create time") {
			t.Errorf("state %v: must explain reattach keeps create-time attachments, got:\n%s", state, out.String())
		}
	}
}

// An EXISTING sandbox WITH --replace relaunches through `run --replace`
// carrying the kickoff, so the recreated sandbox actually receives the tour.
func TestRunSetupHandoff_ExistingSandbox_ReplaceRecreatesWithKickoff(t *testing.T) {
	var out bytes.Buffer
	var gotArgs []string
	if err := runSetupHandoff("/some/repo", "pix-repo", sbxRunning, true, &out, func(args []string) { gotArgs = args }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"/some/repo", "--replace", "--", onboardingKickoff}
	if len(gotArgs) != len(want) {
		t.Fatalf("runFn args = %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("runFn args = %v, want %v", gotArgs, want)
		}
	}
}

// sbxUnknown FAILS CLOSED: no launch, no leave-alone success message — an
// error telling the user to retry, because launching blind could replay the
// kickoff into a live session the probe couldn't see.
func TestRunSetupHandoff_UnknownState_FailsClosed(t *testing.T) {
	for _, replace := range []bool{false, true} {
		var out bytes.Buffer
		called := false
		err := runSetupHandoff(".", "pix-demo", sbxUnknown, replace, &out, func([]string) { called = true })
		if err == nil {
			t.Fatalf("replace=%v: unknown state must return an error", replace)
		}
		if called {
			t.Errorf("replace=%v: unknown state must never call runFn", replace)
		}
		if !strings.Contains(err.Error(), "cannot determine") {
			t.Errorf("replace=%v: error must say the state cannot be determined, got: %v", replace, err)
		}
		if !strings.Contains(err.Error(), "pix setup") {
			t.Errorf("replace=%v: error must tell the user to retry setup, got: %v", replace, err)
		}
	}
}

// sbxUnknown's retry command must preserve an EXPLICIT DIR and a requested
// --replace, both shell-quoted correctly, so copy-pasting the printed retry
// command reproduces exactly what the user originally asked for (dropping
// --replace here would silently downgrade a requested recreate into a plain
// reattach on retry).
func TestRunSetupHandoff_UnknownState_RetryCommandPreservesDirAndReplace(t *testing.T) {
	var out bytes.Buffer
	called := false
	err := runSetupHandoff("/some/repo", "pix-repo", sbxUnknown, true, &out, func([]string) { called = true })
	if err == nil {
		t.Fatal("unknown state must return an error")
	}
	if called {
		t.Error("unknown state must never call runFn")
	}
	want := "pix setup /some/repo --replace"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error must contain the exact retry command %q, got: %v", want, err)
	}

	// A DIR needing shell quoting must still round-trip, with --replace after it.
	var out2 bytes.Buffer
	err2 := runSetupHandoff("/some/repo's dir", "pix-repo", sbxUnknown, true, &out2, func([]string) {})
	if err2 == nil {
		t.Fatal("unknown state must return an error")
	}
	want2 := "pix setup " + sys.ShellQuote("/some/repo's dir") + " --replace"
	if !strings.Contains(err2.Error(), want2) {
		t.Errorf("error must contain the exact quoted retry command %q, got: %v", want2, err2)
	}
}

// An ABSENT sandbox gets the normal first-launch handoff: runFn IS called with
// the onboarding kickoff.
func TestRunSetupHandoff_AbsentSandbox_LaunchesWithKickoff(t *testing.T) {
	var out bytes.Buffer
	var gotArgs []string
	if err := runSetupHandoff(".", "pix-demo", sbxAbsent, false, &out, func(args []string) { gotArgs = args }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotArgs == nil {
		t.Fatal("an absent sandbox must launch via runFn")
	}
	if len(gotArgs) == 0 || gotArgs[len(gotArgs)-1] != onboardingKickoff {
		t.Errorf("launch args must end with the onboarding kickoff, got: %v", gotArgs)
	}
	if !strings.Contains(out.String(), "Launching sandbox") {
		t.Errorf("must print the first-launch message, got:\n%s", out.String())
	}
}

// A non-"." dir is passed through as the leading positional to runFn.
func TestRunSetupHandoff_AbsentSandbox_PassesDir(t *testing.T) {
	var out bytes.Buffer
	var gotArgs []string
	if err := runSetupHandoff("/some/repo", "pix-repo", sbxAbsent, false, &out, func(args []string) { gotArgs = args }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotArgs) < 3 || gotArgs[0] != "/some/repo" {
		t.Errorf("dir must be forwarded as the leading positional, got: %v", gotArgs)
	}
}

// Absent + --replace: the flag is harmless — forwarded to run (create path
// ignores it), and the kickoff still rides along.
func TestRunSetupHandoff_AbsentSandbox_ReplaceHarmless(t *testing.T) {
	var out bytes.Buffer
	var gotArgs []string
	if err := runSetupHandoff(".", "pix-demo", sbxAbsent, true, &out, func(args []string) { gotArgs = args }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"--replace", "--", onboardingKickoff}
	if len(gotArgs) != len(want) {
		t.Fatalf("runFn args = %v, want %v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("runFn args = %v, want %v", gotArgs, want)
		}
	}
}

// An explicit DIR with spaces and an apostrophe is preserved in the printed
// repeat commands via POSIX single-quoting (with the '\” escape), so the
// commands are copy-paste safe.
func TestRunSetupHandoff_ExistingSandbox_QuotesExplicitDir(t *testing.T) {
	dir := "/tmp/my repo's checkout"
	var out bytes.Buffer
	if err := runSetupHandoff(dir, "pix-checkout", sbxStopped, false, &out, func([]string) {
		t.Fatal("must not launch")
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	quoted := `'/tmp/my repo'\''s checkout'`
	if !strings.Contains(out.String(), "pix run "+quoted) {
		t.Errorf("reattach command must carry the quoted DIR %s, got:\n%s", quoted, out.String())
	}
	if !strings.Contains(out.String(), "pix setup "+quoted+" --replace") {
		t.Errorf("recreate command must carry the quoted DIR %s, got:\n%s", quoted, out.String())
	}
}

// sys.ShellQuote: safe tokens pass through; anything else is single-quoted with
// apostrophes escaped.
func TestShellQuoteArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"/plain/path-1.2_3", "/plain/path-1.2_3"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"$HOME", `'$HOME'`},
		{"a;b", "'a;b'"},
	}
	for _, c := range cases {
		if got := sys.ShellQuote(c.in); got != c.want {
			t.Errorf("sys.ShellQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// --- item 1: ordinary value flags must NOT suppress interactive prompts ---

func TestSetupInteractivePrompts_OnlyAssumeYesSuppresses(t *testing.T) {
	cases := []struct {
		tty, assumeYes, want bool
	}{
		{true, false, true},   // TTY, no opt-out -> interactive
		{true, true, false},   // TTY but --yes -> non-interactive
		{false, false, false}, // no TTY at all -> non-interactive
		{false, true, false},
	}
	for _, c := range cases {
		if got := setupInteractivePrompts(c.tty, c.assumeYes); got != c.want {
			t.Errorf("setupInteractivePrompts(tty=%v, assumeYes=%v) = %v, want %v", c.tty, c.assumeYes, got, c.want)
		}
	}
}

// --- seedIdentity honesty: memory claims track actual RPC outcomes ---------

// fakeIdentityMemory simulates the memory daemon for seedIdentity: up or not,
// per-fact remember failures keyed by content substring (a real RPC error),
// and per-fact "emptyID" substrings simulating the daemon's OWN no-error but
// nothing-persisted response ({"id": "", "reaffirmed": false} — memory.go's
// remember handler for empty content). A fact matching neither returns the
// real daemon's success shape: a nonempty "id".
type fakeIdentityMemory struct {
	up      bool
	fail    []string // content substrings whose remember call errors
	emptyID []string // content substrings whose remember call succeeds with NO id
	calls   []string
}

func (f *fakeIdentityMemory) Up() bool { return f.up }
func (f *fakeIdentityMemory) Call(method string, params map[string]any) (map[string]any, error) {
	content, _ := params["content"].(string)
	f.calls = append(f.calls, content)
	for _, sub := range f.fail {
		if strings.Contains(content, sub) {
			return nil, fmt.Errorf("remember failed")
		}
	}
	for _, sub := range f.emptyID {
		if strings.Contains(content, sub) {
			return map[string]any{"id": "", "reaffirmed": false}, nil
		}
	}
	return map[string]any{"id": "mem-" + content, "reaffirmed": false}, nil
}

func gitIdentityEnv(name, email string) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) {
		if cmd == "git" && len(args) >= 4 && args[3] == "user.name" {
			return name, nil
		}
		if cmd == "git" && len(args) >= 4 && args[3] == "user.email" {
			return email, nil
		}
		return "", fmt.Errorf("unexpected: %s %v", cmd, args)
	}}}
}

func withIdentityMemory(t *testing.T, m identityMemory) {
	t.Helper()
	orig := newIdentityMemory
	newIdentityMemory = func() identityMemory { return m }
	t.Cleanup(func() { newIdentityMemory = orig })
}

// The single first-name fact saves: the output claims the memory save and names
// host state as the deterministic carrier. Only ONE fact is stored now (first
// name, no surname, no email), so exactly one remember call fires.
func TestSeedIdentity_AllSaved(t *testing.T) {
	m := &fakeIdentityMemory{up: true}
	withIdentityMemory(t, m)
	var out bytes.Buffer
	seedIdentity(gitIdentityEnv("Mark", "m@x.com"), &out)
	if len(m.calls) != 1 {
		t.Fatalf("expected exactly 1 remember call (first name only), got %v", m.calls)
	}
	if !strings.Contains(m.calls[0], "first name is Mark") {
		t.Errorf("stored fact must be the first name only, got: %q", m.calls[0])
	}
	if strings.Contains(m.calls[0], "email") || strings.Contains(m.calls[0], "m@x.com") {
		t.Errorf("must NOT store email as a memory fact, got: %q", m.calls[0])
	}
	if !strings.Contains(out.String(), "Saved to memory and available to sessions via host state.") {
		t.Errorf("success must claim the memory save, got:\n%s", out.String())
	}
	if strings.ContainsRune(out.String(), '—') {
		t.Errorf("user copy must not contain an em dash, got:\n%s", out.String())
	}
}

// A remember call that returns NO ERROR but an EMPTY "id" (the daemon's real
// no-op shape) must NOT count as a save; with one fact that's a full failure.
func TestSeedIdentity_EmptyID_TreatedAsFailure(t *testing.T) {
	m := &fakeIdentityMemory{up: true, emptyID: []string{"first name is"}}
	withIdentityMemory(t, m)
	var out bytes.Buffer
	seedIdentity(gitIdentityEnv("Mark", "m@x.com"), &out)
	if len(m.calls) != 1 {
		t.Fatalf("expected 1 remember call, got %v", m.calls)
	}
	if !strings.Contains(out.String(), "Could not save to memory") {
		t.Errorf("a no-error-but-empty-id response must be treated as a failed save, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Saved to memory") {
		t.Errorf("must not claim a save when the RPC returned no id, got:\n%s", out.String())
	}
}

// A successful remember call is counted ONLY via its result's actual "id" field
// (the fake mirrors the real daemon's nonempty id), proving the counting path
// reads it rather than merely checking err == nil.
func TestSeedIdentity_SuccessCountsViaPersistedID(t *testing.T) {
	m := &fakeIdentityMemory{up: true}
	withIdentityMemory(t, m)
	var out bytes.Buffer
	seedIdentity(gitIdentityEnv("Mark", "m@x.com"), &out)
	if len(m.calls) != 1 {
		t.Fatalf("expected 1 remember call, got %v", m.calls)
	}
	if !strings.Contains(out.String(), "Saved to memory and available to sessions via host state.") {
		t.Errorf("a genuine nonempty-id result must count as a save, got:\n%s", out.String())
	}
}

// Every remember RPC fails: no memory-save claim at all, no promise.
func TestSeedIdentity_AllRPCsFailNoClaim(t *testing.T) {
	m := &fakeIdentityMemory{up: true, fail: []string{"user's"}}
	withIdentityMemory(t, m)
	var out bytes.Buffer
	seedIdentity(gitIdentityEnv("Mark", "m@x.com"), &out)
	if !strings.Contains(out.String(), "Could not save to memory") {
		t.Errorf("full RPC failure must be stated, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Saved to memory") {
		t.Errorf("must not claim any save on full failure, got:\n%s", out.String())
	}
}

// Daemon down: identity still reported via host state, zero memory claims.
func TestSeedIdentity_DaemonDownNoClaim(t *testing.T) {
	m := &fakeIdentityMemory{up: false}
	withIdentityMemory(t, m)
	var out bytes.Buffer
	seedIdentity(gitIdentityEnv("Mark", ""), &out)
	if len(m.calls) != 0 {
		t.Fatalf("daemon down must attempt no remember calls, got %v", m.calls)
	}
	if !strings.Contains(out.String(), "Available to sessions via host state.") {
		t.Errorf("host-state availability must still be stated, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "memory") {
		t.Errorf("daemon down must make no memory claim, got:\n%s", out.String())
	}
}

// --- U-W2.01: the setup phase machine ---------------------------------------

// AC-P0-301: the transcript is numbered, in the fixed order, and every phase
// header is printed BEFORE that phase's work — so a run that hangs names the
// phase it hung in instead of showing a blank terminal.
func TestSetupPhases_NumberedHeadersInFixedOrder(t *testing.T) {
	want := []string{"parse", "inventory", "gate", "mutate", "consent", "verify", "report", "handoff"}
	if len(setupPhaseOrder) != len(want) {
		t.Fatalf("setupPhaseOrder has %d phases, want %d", len(setupPhaseOrder), len(want))
	}
	for i, w := range want {
		if setupPhaseOrder[i].name != w {
			t.Errorf("phase %d = %q, want %q", i+1, setupPhaseOrder[i].name, w)
		}
	}
	w := &ollamaWorld{}
	env := modelsSetupEnv(t, w)
	stubProvisionKeysOK(t)
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("setup failed: %v\n%s", err, out.String())
	}
	got := out.String()
	at := -1
	for i, p := range want[:7] { // the host phase owns 1..7; handoff is the caller's
		marker := fmt.Sprintf("[%d/8] %s —", i+1, p)
		j := strings.Index(got, marker)
		if j < 0 {
			t.Fatalf("missing phase header %q in:\n%s", marker, got)
		}
		if j <= at {
			t.Errorf("phase %q header is out of order in:\n%s", p, got)
		}
		at = j
	}
	// The header must precede the work: the report's first verdict line can
	// only appear AFTER the report header.
	if strings.Index(got, "[7/8] report") > strings.Index(got, "Setup summary:") {
		t.Errorf("the report header must be printed before the readiness.Report, got:\n%s", got)
	}
}

// AC-P0-303: the mutation order is a value, fixed, riskiest last. The two
// consenting steps are last, and model pulls (the only step that can cost
// gigabytes) are dead last.
func TestSetupMutationOrder_FixedRiskiestLast(t *testing.T) {
	want := "keys,config,pack,mcp,knowledge,identity,gworkspace,models,inference"
	if got := strings.Join(setupMutationOrder, ","); got != want {
		t.Errorf("setupMutationOrder = %s, want %s", got, want)
	}
	env := modelsSetupEnv(t, &ollamaWorld{})
	opts, err := onboard.ParseOnboardArgs([]string{"--yes"})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := takeSetupInventory(env, opts)
	if err != nil {
		t.Fatal(err)
	}
	var models setupModelsOutcome
	steps := setupMutationSteps(env, inv, opts, strings.NewReader(""), io.Discard, false, &models, &setupPromptBudget{})
	var names []string
	for _, s := range steps {
		names = append(names, s.name)
	}
	if got := strings.Join(names, ","); got != want {
		t.Errorf("the step table runs %s, want %s", got, want)
	}
}

// AC-P0-302: the mutate phase returns touched AXES, not prose. Stub every
// mutation to fail and no ✓ may be printed for those axes — the report is a
// pure function of post-mutation evidence, so a failed mutation has no way to
// print a success glyph.
func TestSetupMutations_StubbedToFail_PrintNoSuccessGlyph(t *testing.T) {
	var out bytes.Buffer
	steps := []setupMutationStep{
		{name: "keys", axes: []readiness.Axis{readiness.AxisProviders}, run: func() error { return fmt.Errorf("boom") }},
		{name: "pack", axes: []readiness.Axis{readiness.AxisPack}, run: func() error { return fmt.Errorf("boom") }},
	}
	for _, s := range steps {
		_ = s.run()
	}
	touched, err := runSetupMutations(steps)
	if err == nil {
		t.Fatal("a failing mutation must report an error")
	}
	if len(touched) != 2 {
		t.Errorf("mutate must return the touched axes, got %v", touched)
	}
	if strings.Contains(out.String(), "✓") {
		t.Errorf("the mutate phase must print no success glyph, got:\n%s", out.String())
	}
}

// A fatal step stops the table; a non-fatal one records its failure and lets
// the run reach the report, which then shows the axis as not ready.
func TestRunSetupMutations_FatalStopsNonFatalContinues(t *testing.T) {
	var ran []string
	steps := []setupMutationStep{
		{name: "a", run: func() error { ran = append(ran, "a"); return fmt.Errorf("soft") }},
		{name: "b", run: func() error { ran = append(ran, "b"); return nil }},
	}
	if _, err := runSetupMutations(steps); err == nil || strings.Join(ran, ",") != "a,b" {
		t.Errorf("a non-fatal failure must not stop the table: ran=%v err=%v", ran, err)
	}
	ran = nil
	steps[0].fatal = true
	if _, err := runSetupMutations(steps); err == nil || strings.Join(ran, ",") != "a" {
		t.Errorf("a fatal failure must stop the table: ran=%v err=%v", ran, err)
	}
}

// AC-P0-307: at most two interactive prompts per run; AC-P0-306: a
// non-interactive run gets none at all.
func TestSetupPromptBudget_AtMostTwoAndNoneWithoutATTY(t *testing.T) {
	b := &setupPromptBudget{interactive: true}
	if !b.reserve("model pull consent") || !b.reserve("google workspace route") {
		t.Fatal("the two named prompts must both fit in the budget")
	}
	if b.reserve("a third question") {
		t.Errorf("setup must never ask a third question, asked: %v", b.asked)
	}
	if b.spent != setupMaxPrompts {
		t.Errorf("budget spent = %d, want %d", b.spent, setupMaxPrompts)
	}
	quiet := &setupPromptBudget{interactive: false}
	if quiet.reserve("anything") {
		t.Error("a non-interactive run must never be granted a prompt slot")
	}
	var nilBudget *setupPromptBudget
	if nilBudget.reserve("anything") {
		t.Error("a nil budget must never grant a prompt slot")
	}
}

// AC-P0-302, the grep that IS the review: the render path must not read the
// inventory. A report that consults pre-mutation state is a report that can
// print what setup INTENDED rather than what it achieved, so the source itself
// is asserted — printSetupSummary and its helpers neither take a
// setupInventory nor name one.
func TestSetupReport_NeverReadsInventory(t *testing.T) {
	for _, file := range []string{"setup_models.go", "setup.go"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, fn := range []string{"func printSetupSummary(", "func setupProvidersAxis(", "func setupPackAxis(", "func setupGworkspaceAxis("} {
			i := strings.Index(src, fn)
			if i < 0 {
				continue
			}
			body := src[i:]
			if j := strings.Index(body, "\n}\n"); j > 0 {
				body = body[:j]
			}
			if strings.Contains(body, "setupInventory") || strings.Contains(body, "inv.") {
				t.Errorf("%s: %s reads the inventory; the report must be a pure function of post-mutation evidence", file, fn)
			}
		}
	}
}

// AC-P0-308: `--no-agent` is setup's own flag and is never forwarded to the
// host-config parser (which would reject it as unknown).
func TestSetupNoAgent_IsSetupsOwnFlag(t *testing.T) {
	if _, err := onboard.ParseOnboardArgs([]string{"--no-agent"}); err == nil {
		t.Error("--no-agent must be consumed by setup itself, not the host-config parser")
	}
	if !strings.Contains(setupUsage, "--no-agent") {
		t.Error("setup usage must document --no-agent")
	}
	if knownVerbs["onboard"] {
		t.Error("the `onboard` verb is deleted with no alias; it must not be a known verb")
	}
	if _, ok := verbUsage("onboard"); ok {
		t.Error("`pix help onboard` must not resolve to a usage page for a deleted verb")
	}
	if s, ok := suggestVerb("onboard"); !ok || s != "setup --no-agent" {
		t.Errorf("an `onboard` argv must take the unknown-verb path suggesting `setup`, got %q (%v)", s, ok)
	}
	msg, launch := classifyBareArg("onboard")
	if launch || !strings.Contains(msg, `Did you mean "setup --no-agent"?`) {
		t.Errorf("`pix onboard` must print a did-you-mean and exit 2, got %q (launch=%v)", msg, launch)
	}
}
