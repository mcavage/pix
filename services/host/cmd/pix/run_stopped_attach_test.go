//go:build unix

// run_stopped_attach_test.go pins the retained-sandbox regression at the real
// command boundary: `pix run` against an existing but STOPPED sandbox must use
// the same `sbx exec` handoff every other attach uses, carrying THIS run's
// invocation. The pre-cutover `sbx run --name` argv asks sbx to re-derive the
// agent command from the container's own spec, so live skills and the injected
// trusted host state never reach pi, and it forces a tty on a redirected stdin.
//
// These tests drive dispatch("run", ...), read the argv the fake `sbx` was
// actually invoked with (recorded NUL-delimited, so argument boundaries are
// observable), and capture the child's real stdout. Nothing here sets
// SessionSpec.AttachExec by hand.
//
// They swap the process's os.Stdout for the duration of a dispatch, which is
// why no test in this file may run in parallel.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/lease"
	"pix/host/workflow/launch"
)

// execChildStdout is what the fake `sbx exec` prints. The real handoff gives
// the child the launcher process's own stdout, so a test proves forwarding by
// capturing that descriptor, not by reading a file the fixture wrote.
const execChildStdout = "pix 0.85.1"

// stoppedSbxFixture installs a fake `sbx` on PATH that reports name as an
// existing, schema-verified sandbox in state, records every invocation's argv
// NUL-delimited under <dir>/argv.<n>, and makes `exec` print execChildStdout on
// its inherited stdout before exiting with exitCode.
func stoppedSbxFixture(t *testing.T, name, state string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	bin := t.TempDir()
	script := `#!/bin/sh
dir=` + dir + `
n=0
if [ -f "$dir/seq" ]; then IFS= read -r n < "$dir/seq"; fi
n=$((n + 1))
echo "$n" > "$dir/seq"
for a in "$@"; do printf '%s\000' "$a"; done > "$dir/argv.$n"
case "$1" in
  --version|version) echo 'sbx version 0.41.0' ;;
  ls)
    if [ "$2" = "--json" ]; then
      echo '[{"name":"` + name + `","state":"` + state + `","instance_id":"inst-1"}]'
    else
      echo 'NAME STATUS'
      echo '` + name + ` x ` + state + `'
    fi
    ;;
  exec)
    printf '%s\n' '` + execChildStdout + `'
    exit ` + strconv.Itoa(exitCode) + `
    ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return dir
}

// sbxCalls returns every recorded invocation, in call order, as the argument
// arrays the fake was handed. Each record is NUL-delimited, so an argument
// containing spaces or newlines stays one element instead of being reflowed
// into several by a space-joined log line.
func sbxCalls(t *testing.T, dir string) [][]string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "argv.*"))
	if err != nil {
		t.Fatal(err)
	}
	seq := func(p string) int {
		n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(p), "argv."))
		return n
	}
	sort.Slice(names, func(i, j int) bool { return seq(names[i]) < seq(names[j]) })
	var calls [][]string
	for _, p := range names {
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			t.Fatal(rerr)
		}
		fields := strings.Split(string(data), "\x00")
		if n := len(fields); n > 0 && fields[n-1] == "" {
			fields = fields[:n-1] // the terminator after the last argument
		}
		calls = append(calls, fields)
	}
	return calls
}

// lastCallOf returns the last recorded invocation whose first argument is verb.
func lastCallOf(calls [][]string, verb string) []string {
	var found []string
	for _, c := range calls {
		if len(c) > 0 && c[0] == verb {
			found = c
		}
	}
	return found
}

// captureProcessStdout redirects the process's stdout to a file for the
// duration of fn and returns what was written there. interactiveSessionSpawn
// hands the child os.Stdout by design (run_stdio_seams_test.go pins that as
// the one raw-stdio seam), so this is how a caller-level test observes output
// the child actually forwarded.
func captureProcessStdout(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = f
	restore := func() {
		os.Stdout = saved
		_ = f.Close()
	}
	defer restore()
	fn()
	restore()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// testAttachModel is an explicit --model: a hermetic PIX_HOME configures no
// provider, and model selection legitimately refuses rather than falling back
// (safety invariant 10). Naming one keeps these tests on the attach dispatch.
const testAttachModel = "anthropic/claude-sonnet-5"

// stoppedAttachHome is a hermetic PIX_HOME carrying one zero-footprint
// environment (trustTestHome's document), so the trust gate neither prompts
// nor refuses and what these tests measure is the attach dispatch itself.
func stoppedAttachHome(t *testing.T) string {
	t.Helper()
	return trustTestHome(t, "default")
}

// seedCreationRecord writes the instance-bound lease record a reattach gate
// demands, through the lease package's own writers, so these are the same files
// a real create leaves behind. No session fingerprint is written: an attach
// with no recorded fingerprint has nothing to diverge from.
func seedCreationRecord(t *testing.T, sandboxName, instanceID string) {
	t.Helper()
	state, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(state, "sandboxes")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := lease.SandboxDir(root, sandboxName)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.EnsureSandboxDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.CreateRecord(dir, instanceID); err != nil {
		t.Fatal(err)
	}
}

// attachResult is one real `pix run` attach: its exit code, everything the
// fake sbx was called with, the bytes the child forwarded to the process's
// stdout, and the PIX_HOME it ran under.
type attachResult struct {
	code   int
	calls  [][]string
	stdout string
	home   string
}

func (r attachResult) exec() []string { return lastCallOf(r.calls, "exec") }

// runAttach runs the real `run` verb against an existing sandbox in state.
func runAttach(t *testing.T, state string, interactive bool, exitCode int, extra ...string) attachResult {
	t.Helper()
	home := stoppedAttachHome(t)
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_UAT_SMOKE", "1") // the isolated smoke path: no personal provider-key interview
	ws := t.TempDir()
	name, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatal(err)
	}
	fixture := stoppedSbxFixture(t, name, state, exitCode)
	seedCreationRecord(t, name, "inst-1")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, Interactive: interactive}
	argv := append([]string{"run", ws, "--env", "default", "--model", testAttachModel}, extra...)
	code := 0
	childOut := captureProcessStdout(t, func() { code = dispatch(argv, d) })
	t.Logf("pix run exited %d\nchild stdout:\n%s\nstdout:\n%s\nstderr:\n%s", code, childOut, out.String(), errb.String())
	return attachResult{code: code, calls: sbxCalls(t, fixture), stdout: childOut, home: home}
}

// TestRunStopped_AttachesWithExecNotLegacyRun is the regression itself: the
// stopped reattach execs (`-i` on this piped run), forwards the child's output,
// reports the child's status, and never spawns the legacy `run --name` argv.
func TestRunStopped_AttachesWithExecNotLegacyRun(t *testing.T) {
	got := runAttach(t, "stopped", false, 0)
	call := got.exec()
	if call == nil {
		t.Fatalf("a stopped reattach never exec'd; sbx was called with: %q", got.calls)
	}
	if head, want := call[:2], []string{"exec", "-i"}; !reflect.DeepEqual(head, want) {
		t.Errorf("piped stopped attach argv[:2] = %q, want %q (a redirected stdin must not be handed a tty)", head, want)
	}
	if got.code != 0 {
		t.Errorf("exit code = %d, want 0 (the child's own status)", got.code)
	}
	// The child's output reached the launcher's stdout: the legacy path
	// produced none at all.
	if !strings.Contains(got.stdout, execChildStdout) {
		t.Errorf("attach forwarded no child output; captured stdout = %q, want it to contain %q", got.stdout, execChildStdout)
	}
	if legacy := lastCallOf(got.calls, "run"); legacy != nil {
		t.Errorf("the legacy `sbx run --name` reattach is still dispatched: %q", legacy)
	}
}

// TestRunStopped_InteractiveAttachUsesTTY: the same dispatch on a real
// terminal asks for `-it`.
func TestRunStopped_InteractiveAttachUsesTTY(t *testing.T) {
	call := runAttach(t, "stopped", true, 0).exec()
	if call == nil {
		t.Fatal("an interactive stopped reattach never exec'd")
	}
	if head, want := call[:2], []string{"exec", "-it"}; !reflect.DeepEqual(head, want) {
		t.Errorf("interactive stopped attach argv[:2] = %q, want %q", head, want)
	}
}

// TestRunStopped_CarriesThisRunsFullInvocation is AC-2: the stopped path
// carries THIS run's model, resume session, live skills, injected trusted host
// state, and the exact `--` passthrough, with argument boundaries intact. The
// legacy args carried only --session-dir/--model/--session/passthrough: no live
// skills and no host state at all.
//
// The generated prompt is the ONE argument InjectTrustedHostState may touch
// (it targets the launch.GeneratedInputMarker prefix and nothing else), so a
// harmless generated prompt is what proves the payload reaches pi on this path.
func TestRunStopped_CarriesThisRunsFullInvocation(t *testing.T) {
	generated := launch.GeneratedInputMarker + "hi"
	shellish := "$(echo pwned) && rm -rf /"
	multiline := "first line\nsecond line"
	got := runAttach(t, "stopped", false, 0,
		"--resume", "sess-7",
		"--", "-p", generated, "two words", multiline, shellish)
	call := got.exec()
	if call == nil {
		t.Fatalf("a stopped reattach never exec'd; sbx was called with: %q", got.calls)
	}
	personalSkills := filepath.Join(got.home, "context", "skills")
	for _, want := range [][]string{
		{"--", "pi", "--session-dir", ".pi-sessions"},
		{"--model", testAttachModel},
		{"--session", "sess-7"},
		{"--skill", personalSkills},
	} {
		if !containsSeq(call, want) {
			t.Errorf("stopped attach argv = %q, want the consecutive arguments %q", call, want)
		}
	}
	// The passthrough tail arrives as the SAME arguments the user typed: one
	// element each, spaces, newlines and shell metacharacters intact, in order,
	// last. Only the generated prompt differs, and only by the appended block.
	tail := call[len(call)-5:]
	prompt := tail[1]
	tail[1] = generated
	if want := []string{"-p", generated, "two words", multiline, shellish}; !reflect.DeepEqual(tail, want) {
		t.Errorf("passthrough tail = %q, want %q verbatim and last", tail, want)
	}
	// Trusted host state: appended to the generated prompt, delimited, valid
	// JSON, and the prompt text itself unchanged.
	if !strings.HasPrefix(prompt, generated) {
		t.Fatalf("generated prompt = %q, want it to still start with %q", prompt, generated)
	}
	begin := strings.Index(prompt, launch.TrustedHostStateBegin)
	end := strings.Index(prompt, launch.TrustedHostStateEnd)
	if begin < 0 || end < begin {
		t.Fatalf("generated prompt = %q, want the trusted host state block delimited by %q/%q", prompt, launch.TrustedHostStateBegin, launch.TrustedHostStateEnd)
	}
	if extra := prompt[len(generated):begin]; extra != "" {
		t.Errorf("text appeared between the prompt and the host-state block: %q", extra)
	}
	payload := prompt[begin+len(launch.TrustedHostStateBegin) : end]
	var hs launch.HostState
	if err := json.Unmarshal([]byte(payload), &hs); err != nil {
		t.Errorf("host-state payload is not valid JSON: %v\npayload: %s", err, payload)
	}
	// A plain user-typed passthrough argument must never carry the payload.
	for _, a := range []string{"two words", multiline, shellish} {
		for _, seen := range call {
			if strings.HasPrefix(seen, a) && seen != a {
				t.Errorf("argument %q was modified to %q", a, seen)
			}
		}
	}
}

// containsSeq reports whether want appears as consecutive elements of args.
func containsSeq(args, want []string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// TestRunStopped_ExitCodeIsTheChildsAndNamesNoKitFailure is AC-4: an ordinary
// session exit is reported with the child's own status and must not be dressed
// up as an sbx kit-resolution failure. No kit is resolved on an attach at all,
// and the advice named flags this launch never used.
func TestRunStopped_ExitCodeIsTheChildsAndNamesNoKitFailure(t *testing.T) {
	for _, tc := range []struct {
		state string
		code  int
	}{{"stopped", 1}, {"stopped", 128}, {"running", 1}, {"running", 128}} {
		home := stoppedAttachHome(t)
		t.Setenv("PIX_HOME", home)
		t.Setenv("PIX_UAT_SMOKE", "1")
		ws := t.TempDir()
		name, err := resolveSandboxName("", ws)
		if err != nil {
			t.Fatal(err)
		}
		stoppedSbxFixture(t, name, tc.state, tc.code)
		seedCreationRecord(t, name, "inst-1")

		var out, errb bytes.Buffer
		d := &cli.Deps{Out: &out, Err: &errb}
		code := 0
		captureProcessStdout(t, func() {
			code = dispatch([]string{"run", ws, "--env", "default", "--model", testAttachModel}, d)
		})
		if code != tc.code {
			t.Errorf("%s exit %d: pix run returned %d, want the child's own status", tc.state, tc.code, code)
		}
		combined := out.String() + errb.String()
		for _, forbidden := range []string{"could not resolve the kit at ref", "--kit-ref", "pix run --kit ", "pix run: attach failed", "pix rm " + name} {
			if strings.Contains(combined, forbidden) {
				t.Errorf("%s exit %d: a session exit invented a failure diagnosis or removal advice:\n%s", tc.state, tc.code, combined)
			}
		}
	}
}
