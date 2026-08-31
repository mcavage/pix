package secret

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// scoped_test.go pins Wave C's credential de-globalization: Pix resolves the
// refs THIS PIX_HOME configures and writes them as SANDBOX-SCOPED sbx service
// secrets, on every create and every attach. Nothing here may write, read or
// depend on a global (`-g`) secret.

const scopedRefs = "ANTHROPIC_API_KEY=op://Private/anthropic/key\n" +
	"GEMINI_API_KEY=op://Private/gemini/key\n" +
	"PARALLEL_API_KEY=op://Private/parallel/key\n" +
	"GITHUB_TOKEN=op://Private/github/token\n"

// TestPrepareSandboxSecrets_ExactScopedArgv is the argv contract: `sbx secret
// set -f --sandbox <name> <service>`, in that order, once per configured
// known ref — no value in argv, and never a global flag.
func TestPrepareSandboxSecrets_ExactScopedArgv(t *testing.T) {
	var calls []string
	env := fakeSyncEnv(scopedRefs, "sk-secret-value\n", nil, &calls)
	var out bytes.Buffer
	if err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{}); err != nil {
		t.Fatalf("PrepareSandboxSecrets: %v (out=%q)", err, out.String())
	}
	joined := strings.Join(calls, "\n")
	for _, service := range []string{"anthropic", "google", "parallel", "github"} {
		want := "sbx secret set -f --sandbox pix-demo " + service
		if !strings.Contains(joined, want) {
			t.Errorf("missing scoped write %q; got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, " -t ") {
		t.Errorf("the value is still passed as an argv element:\n%s", joined)
	}
	if strings.Contains(joined, "sk-secret-value") {
		t.Errorf("the resolved value reached an argv:\n%s", joined)
	}
	if strings.Contains(joined, "secret set -f -g ") || strings.Contains(joined, "--global") {
		t.Errorf("a global secret was written:\n%s", joined)
	}
	if strings.Contains(out.String(), "sk-secret-value") {
		t.Errorf("the resolved value leaked into output:\n%s", out.String())
	}
}

// TestPrepareSandboxSecrets_ValueTravelsOverStdin is the finding this seam
// exists for: the resolved credential is written to `sbx secret set`'s STDIN,
// so it is never readable in the host's process table. The value must appear
// in the injected input and NOWHERE else — not in the argv, not in the fake's
// recorded calls, not in printed output.
func TestPrepareSandboxSecrets_ValueTravelsOverStdin(t *testing.T) {
	const val = "sk-stdin-only-value"
	var argvs, inputs []string
	fake := &systest.Fake{
		ReadFileFn: func(string) (string, error) {
			return "ANTHROPIC_API_KEY=op://Private/anthropic/key\n", nil
		},
		LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		RunFn: func(name string, args ...string) (string, error) {
			argvs = append(argvs, name+" "+strings.Join(args, " "))
			if name == "op" && len(args) >= 1 && args[0] == "read" {
				return val + "\n", nil
			}
			return "2.0", nil
		},
		RunInputFn: func(input, name string, args ...string) (string, error) {
			argvs = append(argvs, name+" "+strings.Join(args, " "))
			inputs = append(inputs, input)
			return "", nil
		},
	}
	env := hostenv.Env{System: fake}
	var out bytes.Buffer
	if err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{}); err != nil {
		t.Fatalf("PrepareSandboxSecrets: %v (out=%q)", err, out.String())
	}
	if len(inputs) != 1 || inputs[0] != val {
		t.Fatalf("stdin inputs = %q, want exactly one, the resolved value", inputs)
	}
	joined := strings.Join(argvs, "\n")
	if !strings.Contains(joined, "sbx secret set -f --sandbox pix-demo anthropic") {
		t.Errorf("the scoped write argv is wrong:\n%s", joined)
	}
	if strings.Contains(joined, val) {
		t.Errorf("the value reached an argv:\n%s", joined)
	}
	if strings.Contains(strings.Join(fake.Calls, "\n"), val) {
		t.Errorf("the value reached the recorded calls: %v", fake.Calls)
	}
	if strings.Contains(out.String(), val) {
		t.Errorf("the value reached printed output:\n%s", out.String())
	}
}

// TestPrepareSandboxSecrets_StdinFailureNeverEchoesTheValue: redaction stays
// as defence in depth even though the argv no longer carries the value — sbx
// is free to echo whatever it read, and this path prints its first line.
func TestPrepareSandboxSecrets_StdinFailureNeverEchoesTheValue(t *testing.T) {
	const val = "sk-echoed-back"
	env := hostenv.Env{System: &systest.Fake{
		ReadFileFn: func(string) (string, error) {
			return "ANTHROPIC_API_KEY=op://Private/anthropic/key\n", nil
		},
		LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		RunFn: func(name string, args ...string) (string, error) {
			if name == "op" && len(args) >= 1 && args[0] == "read" {
				return val + "\n", nil
			}
			return "2.0", nil
		},
		RunInputFn: func(input, name string, args ...string) (string, error) {
			return "sbx: rejected the value it read on stdin: " + input,
				errors.New("exit status 1: " + input)
		},
	}}
	var out bytes.Buffer
	err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{})
	if err == nil {
		t.Fatal("want a failure when the only model key cannot be written")
	}
	if strings.Contains(err.Error(), val) || strings.Contains(out.String(), val) {
		t.Errorf("value leaked: err=%v out=%q", err, out.String())
	}
	if !strings.Contains(out.String(), "***") {
		t.Errorf("expected the redaction marker in place of the value, got:\n%s", out.String())
	}
}

// TestPrepareSandboxSecrets_RedactsFailureDetail: sbx can echo back whatever
// it was handed, so the reported detail must never contain the value.
func TestPrepareSandboxSecrets_RedactsFailureDetail(t *testing.T) {
	var calls []string
	env := fakeSyncEnv("ANTHROPIC_API_KEY=op://Private/anthropic/key\n", "sk-leak-me\n",
		errors.New("boom"), &calls)
	var out bytes.Buffer
	err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{})
	if err == nil {
		t.Fatal("want an error when the only configured model key cannot be scoped")
	}
	if strings.Contains(err.Error(), "sk-leak-me") || strings.Contains(out.String(), "sk-leak-me") {
		t.Errorf("value leaked: err=%v out=%q", err, out.String())
	}
}

// TestPrepareSandboxSecrets_TwoSandboxesGetDistinctWrites: a second stack's
// sandbox cannot inherit the first one's credentials, because each write names
// its own sandbox.
func TestPrepareSandboxSecrets_TwoSandboxesGetDistinctWrites(t *testing.T) {
	var calls []string
	env := fakeSyncEnv("ANTHROPIC_API_KEY=op://Private/anthropic/key\n", "sk-v\n", nil, &calls)
	var out bytes.Buffer
	for _, name := range []string{"pix-alpha", "pix-beta"} {
		if err := PrepareSandboxSecrets(env, name, &out, ScopedSecretOptions{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	joined := strings.Join(calls, "\n")
	for _, name := range []string{"pix-alpha", "pix-beta"} {
		if !strings.Contains(joined, "sbx secret set -f --sandbox "+name+" anthropic") {
			t.Errorf("no scoped write for %s:\n%s", name, joined)
		}
	}
}

// TestPrepareSandboxSecrets_RefreshesEveryCall: rotation takes effect on the
// next run, so every call re-resolves and re-writes rather than skipping a
// name sbx already holds.
func TestPrepareSandboxSecrets_RefreshesEveryCall(t *testing.T) {
	var calls []string
	env := fakeSyncEnv("ANTHROPIC_API_KEY=op://Private/anthropic/key\n", "sk-v\n", nil, &calls)
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	var writes, reads int
	for _, c := range calls {
		if strings.HasPrefix(c, "sbx secret set") {
			writes++
		}
		if strings.HasPrefix(c, "op read") {
			reads++
		}
	}
	if writes != 2 || reads != 2 {
		t.Errorf("want 2 resolves + 2 scoped writes across two calls, got reads=%d writes=%d:\n%s",
			reads, writes, strings.Join(calls, "\n"))
	}
	// It must not ask sbx what is already set: a listing answer is not what
	// decides a refresh any more.
	for _, c := range calls {
		if strings.HasPrefix(c, "sbx secret ls") {
			t.Errorf("scoped preparation consulted the secret listing: %q", c)
		}
	}
}

// TestPrepareSandboxSecrets_OnlyKnownRefs: an arbitrary secrets.env entry (an
// MCP server's own credential, say) is NOT a proxy service secret and must
// never be pushed into sbx under its own name.
func TestPrepareSandboxSecrets_OnlyKnownRefs(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://Private/anthropic/key\nSLACK_BOT_TOKEN=op://Private/slack/token\n"
	var calls []string
	env := fakeSyncEnv(refs, "sk-v\n", nil, &calls)
	var out bytes.Buffer
	if err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "SLACK") || strings.Contains(joined, "slack") {
		t.Errorf("an unknown ref became an sbx service secret:\n%s", joined)
	}
	if strings.Count(joined, "sbx secret set") != 1 {
		t.Errorf("want exactly one scoped write (anthropic):\n%s", joined)
	}
}

// TestPrepareSandboxSecrets_GitHubIsScoped pins GITHUB_TOKEN -> github as a
// member of the scoped set, and that a github-only host is not an error (no
// model key was ever promised by that ref).
func TestPrepareSandboxSecrets_GitHubIsScoped(t *testing.T) {
	var calls []string
	env := fakeSyncEnv("GITHUB_TOKEN=op://Private/github/token\n", "ghp-v\n", nil, &calls)
	var out bytes.Buffer
	if err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{}); err != nil {
		t.Fatalf("a github-only ref set must not fail the launch: %v", err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "sbx secret set -f --sandbox pix-demo github") {
		t.Errorf("GITHUB_TOKEN was not scoped:\n%s", strings.Join(calls, "\n"))
	}
	if isModelProviderKey(GitHubKeyRef.EnvVar) {
		t.Error("GITHUB_TOKEN must not count as a model provider key: it authorizes git, not a model")
	}
}

// TestPrepareSandboxSecrets_NoRefsIsNotAFailure: a host with no refs file at
// all (keyless inference, say) prepares nothing and refuses nothing.
func TestPrepareSandboxSecrets_NoRefsIsNotAFailure(t *testing.T) {
	var calls []string
	env := fakeSyncEnv("", "", nil, &calls)
	var out bytes.Buffer
	if err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{}); err != nil {
		t.Fatalf("no configured refs must not be an error: %v", err)
	}
	if strings.Contains(strings.Join(calls, "\n"), "sbx secret set") {
		t.Errorf("nothing configured, yet something was written:\n%s", strings.Join(calls, "\n"))
	}
}

// TestPrepareSandboxSecrets_ModelKeyOptionalTolerates: a keyless-inference host
// must not lose its sandbox because op could not answer.
func TestPrepareSandboxSecrets_ModelKeyOptionalTolerates(t *testing.T) {
	var calls []string
	env := fakeSyncEnv("ANTHROPIC_API_KEY=op://Private/anthropic/key\n", "sk-v\n", errors.New("boom"), &calls)
	var out bytes.Buffer
	if err := PrepareSandboxSecrets(env, "pix-demo", &out, ScopedSecretOptions{ModelKeyOptional: true}); err != nil {
		t.Fatalf("ModelKeyOptional must degrade to a warning, got: %v", err)
	}
	if !strings.Contains(out.String(), "anthropic") {
		t.Errorf("the failure must still be reported:\n%s", out.String())
	}
}

// TestPrepareSandboxSecrets_NeedsASandboxName: an empty name would make sbx
// write a GLOBAL secret, which is the exact thing this path exists to stop.
func TestPrepareSandboxSecrets_NeedsASandboxName(t *testing.T) {
	var calls []string
	env := fakeSyncEnv(scopedRefs, "sk-v\n", nil, &calls)
	var out bytes.Buffer
	if err := PrepareSandboxSecrets(env, "  ", &out, ScopedSecretOptions{}); err == nil {
		t.Fatal("want a refusal when no sandbox name is supplied")
	}
	if strings.Contains(strings.Join(calls, "\n"), "sbx secret set") {
		t.Errorf("wrote a secret with no scope:\n%s", strings.Join(calls, "\n"))
	}
}

// TestConfiguredModelRefs_TriState: the run gate's evidence. A positive
// absence refuses; an unreadable refs file is unknown, never a refusal.
func TestConfiguredModelRefs_TriState(t *testing.T) {
	present, state := ConfiguredModelRefs(fakeSyncEnv(scopedRefs, "", nil, nil))
	if state != RefsAnswered || len(present) != 2 {
		t.Errorf("configured refs: state=%v present=%v", state, present)
	}
	// A github-only host has NO model ref: positively absent.
	present, state = ConfiguredModelRefs(fakeSyncEnv("GITHUB_TOKEN=op://Private/github/token\n", "", nil, nil))
	if state != RefsAnswered || len(present) != 0 {
		t.Errorf("github-only host: state=%v present=%v", state, present)
	}
	// A placeholder is not a configured ref.
	present, _ = ConfiguredModelRefs(fakeSyncEnv("ANTHROPIC_API_KEY=op://<vault>/<item>/<field>\n", "", nil, nil))
	if len(present) != 0 {
		t.Errorf("an unfilled placeholder counts as configured: %v", present)
	}
}
