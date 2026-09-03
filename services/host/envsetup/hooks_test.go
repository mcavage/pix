package envsetup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// hooks_test.go is deliberately PROCESS-level where it can be: the whole
// point of a setup hook is that a real executable runs with a real exit
// code, so check-pass / check-apply-check / post-check-failure are proven
// by spawning actual programs through the production os/exec runner, not by
// asserting against a mock that agrees with the code under test. Only the
// cases a real process cannot express (a no-TTY refusal, an executable
// swapped between review and exec) use a fake Executor, and even those run
// the SAME Run body.

// script writes an executable shell script and returns its path plus its
// content hash, i.e. exactly what a reviewed Hook carries.
func script(t *testing.T, dir, name, body string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("setup hooks are executed as POSIX programs; this test needs a /bin/sh")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	sha, err := HashSetupExecutable(path)
	if err != nil {
		t.Fatalf("HashSetupExecutable: %v", err)
	}
	return path, sha
}

func hook(id, path, sha string, required bool, kind string) Hook {
	return Hook{
		ID: id, Command: "./" + filepath.Base(path), Resolved: path, SHA: sha,
		CheckArgs: []string{"check"}, ApplyArgs: []string{"install"},
		Kind: kind, Required: required,
	}
}

func opts(out, errb *bytes.Buffer, interactive bool) Options {
	return Options{EnvName: "work", Out: out, Err: errb, In: strings.NewReader(""), Interactive: interactive}
}

// ── check passes: nothing is mutated at all ───────────────────────────────

func TestRun_CheckPasses_NothingIsApplied(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "applied")
	path, sha := script(t, dir, "setup-tool", fmt.Sprintf(`
case "$1" in
  check) exit 0 ;;
  install) touch %q; exit 0 ;;
esac
exit 2
`, marker))

	var out, errb bytes.Buffer
	res, err := Run(dir, []Hook{hook("tool", path, sha, true, "install")}, opts(&out, &errb, true))
	if err != nil {
		t.Fatalf("Run: %v\n%s%s", err, out.String(), errb.String())
	}
	if got := res.Outcomes[0].State; got != StateAlreadyReady {
		t.Fatalf("state = %q, want %q", got, StateAlreadyReady)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("apply ran even though check passed; a converged host must not be mutated")
	}
	// Rerunning is idempotent for the same reason: still no mutation.
	if _, err := Run(dir, []Hook{hook("tool", path, sha, true, "install")}, opts(&out, &errb, true)); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a second run mutated the host")
	}
}

// ── check fails -> apply -> re-check passes ───────────────────────────────

func TestRun_CheckApplyCheck_ConvergesAndOnlyThenSaysReady(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "installed")
	path, sha := script(t, dir, "setup-tool", fmt.Sprintf(`
case "$1" in
  check) [ -f %q ] && exit 0 || exit 1 ;;
  install) touch %q; exit 0 ;;
esac
exit 2
`, state, state))

	var out, errb bytes.Buffer
	res, err := Run(dir, []Hook{hook("tool", path, sha, true, "install")}, opts(&out, &errb, true))
	if err != nil {
		t.Fatalf("Run: %v\n%s%s", err, out.String(), errb.String())
	}
	if got := res.Outcomes[0].State; got != StateApplied {
		t.Fatalf("state = %q, want %q", got, StateApplied)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("apply did not run: %v", err)
	}
	if !strings.Contains(out.String(), "post-check passed") {
		t.Fatalf("the success line must name the PROBE that earned it, got:\n%s", out.String())
	}
	// The second run is the idempotence proof: converged now, so it reports
	// already-ready without applying anything.
	out.Reset()
	res2, err := Run(dir, []Hook{hook("tool", path, sha, true, "install")}, opts(&out, &errb, true))
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if got := res2.Outcomes[0].State; got != StateAlreadyReady {
		t.Fatalf("rerun state = %q, want %q (setup must be idempotent)", got, StateAlreadyReady)
	}
}

// ── a successful apply is NOT a success word: the post-check decides ──────

func TestRun_ApplySucceedsButPostCheckFails_RequiredHookFails(t *testing.T) {
	dir := t.TempDir()
	path, sha := script(t, dir, "setup-tool", `
case "$1" in
  check) echo "tool is not on PATH"; exit 1 ;;
  install) exit 0 ;;
esac
exit 2
`)
	var out, errb bytes.Buffer
	res, err := Run(dir, []Hook{hook("tool", path, sha, true, "install")}, opts(&out, &errb, true))
	if err == nil {
		t.Fatalf("a required hook whose post-check fails must fail setup; out:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "not ready") || !strings.Contains(err.Error(), "tool") {
		t.Fatalf("error must name the hook and the honest state, got %q", err)
	}
	if res.Ready() {
		t.Fatal("Result.Ready() must be false when a required hook is not ready")
	}
	if strings.Contains(out.String(), "ready (apply ran") {
		t.Fatalf("a zero exit from apply must never print a success word:\n%s", out.String())
	}
}

// TestRun_ApplySucceedsButPostCheckFails_MultiLineOutputNamesOnlyTheFirstLine
// guards the other half of the same regression: the captured check output
// was run through sys.TerminalSafe (which turns a real newline into the
// visible two-character placeholder `\n`, deliberately, for terminal
// safety) BEFORE firstLineSuffix ever split on "\n" to find the first
// line. With no real newline left to split on, the "first line" was the
// WHOLE multi-line transcript, glued together with literal `\n` text
// (and a trailing one, from the script's own trailing newline) instead of
// the single clean line a human reads.
func TestRun_ApplySucceedsButPostCheckFails_MultiLineOutputNamesOnlyTheFirstLine(t *testing.T) {
	dir := t.TempDir()
	path, sha := script(t, dir, "setup-tool", `
case "$1" in
  check) printf 'tool is not on PATH\nsee https://example.test/install\n'; exit 1 ;;
  install) exit 0 ;;
esac
exit 2
`)
	var out, errb bytes.Buffer
	_, err := Run(dir, []Hook{hook("tool", path, sha, true, "install")}, opts(&out, &errb, true))
	if err == nil {
		t.Fatal("a required hook whose post-check fails must fail setup")
	}
	msg := err.Error()
	if strings.Contains(msg, `\n`) {
		t.Fatalf("error must not contain a literal escaped newline, got %q", msg)
	}
	if !strings.HasSuffix(msg, "tool is not on PATH") {
		t.Fatalf("error must end with only the first line of output, got %q", msg)
	}
	if strings.Contains(msg, "see https://example.test/install") {
		t.Fatalf("error must name only the first line, not the whole transcript, got %q", msg)
	}
}

func TestRun_OptionalHookThatNeverBecomesReady_WarnsHonestlyAndSucceeds(t *testing.T) {
	dir := t.TempDir()
	path, sha := script(t, dir, "setup-tool", `
case "$1" in
  check) exit 1 ;;
  install) exit 0 ;;
esac
exit 2
`)
	var out, errb bytes.Buffer
	res, err := Run(dir, []Hook{hook("tool", path, sha, false, "install")}, opts(&out, &errb, true))
	if err != nil {
		t.Fatalf("an OPTIONAL hook must not fail setup: %v", err)
	}
	if got := res.Outcomes[0].State; got != StateSkipped {
		t.Fatalf("state = %q, want %q", got, StateSkipped)
	}
	if !strings.Contains(errb.String(), "warning: optional setup hook") || !strings.Contains(errb.String(), "NOT ready") {
		t.Fatalf("an optional failure must be warned about honestly, got:\n%s", errb.String())
	}
	if !res.Ready() {
		t.Fatal("an optional hook's failure must not make the run not-ready")
	}
}

// ── argv is argv: no shell, ever ──────────────────────────────────────────

func TestRun_ArgvIsNeverPassedThroughAShell(t *testing.T) {
	dir := t.TempDir()
	pwned := filepath.Join(dir, "pwned")
	path, sha := script(t, dir, "setup-tool", `
# Print each argument on its own line so the test can prove the injection
# text arrived as ONE literal argument rather than being interpreted.
for a in "$@"; do echo "arg:$a"; done
exit 0
`)
	h := hook("tool", path, sha, true, "install")
	h.CheckArgs = []string{"check; touch " + pwned}

	var out, errb bytes.Buffer
	if _, err := Run(dir, []Hook{h}, opts(&out, &errb, true)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Fatal("the check argv was interpreted by a shell; hooks must exec argv directly")
	}
}

// ── TOCTOU: the executable is re-proven immediately before it runs ────────

func TestRun_ExecutableMutatedAfterReview_RefusesBeforeExecuting(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	path, sha := script(t, dir, "setup-tool", "exit 0\n")

	// Swap the reviewed content for something else, exactly as an attacker
	// (or an unlucky `git pull`) would between review and use.
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch "+ran+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	res, err := Run(dir, []Hook{hook("tool", path, sha, true, "install")}, opts(&out, &errb, true))
	if err == nil {
		t.Fatal("a hook whose executable changed since review must refuse")
	}
	if !strings.Contains(err.Error(), "changed on disk") || !strings.Contains(err.Error(), "pix env trust") {
		t.Fatalf("the refusal must name the drift and the exact remedy, got %q", err)
	}
	if _, statErr := os.Stat(ran); statErr == nil {
		t.Fatal("the swapped executable RAN; the re-proof must happen BEFORE exec")
	}
	if res.Outcomes[0].State != StateFailed {
		t.Fatalf("state = %q, want %q", res.Outcomes[0].State, StateFailed)
	}
	// Even an OPTIONAL hook is refused: "optional" describes a tool the
	// environment can live without, not a licence to run unreviewed code.
	if _, err := Run(dir, []Hook{hook("tool", path, sha, false, "install")}, opts(&out, &errb, true)); err == nil {
		t.Fatal("an optional hook with mutated content must still refuse")
	}
}

func TestHashSetupExecutable_RefusesSymlinkAndNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := HashSetupExecutable(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked hook command must be refused, got %v", err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := HashSetupExecutable(plain); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("a non-executable hook command must be refused, got %v", err)
	}
	if _, err := HashSetupExecutable(dir); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("a directory must be refused, got %v", err)
	}
	if _, err := HashSetupExecutable(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("a missing hook command must be refused")
	}
}

// ── auth hooks need a human ───────────────────────────────────────────────

// recordingExec proves the no-TTY refusal happens BEFORE apply, which a
// real process cannot show (it would have to block on a prompt).
type recordingExec struct {
	checkCode int
	applied   bool
	checks    int
}

func (r *recordingExec) Check(dir, exe string, args []string) (int, string, error) {
	r.checks++
	return r.checkCode, "", nil
}

func (r *recordingExec) Apply(dir, exe string, args []string, in io.Reader, out, errw io.Writer) (int, error) {
	r.applied = true
	return 0, nil
}

func TestRun_AuthHookWithoutATerminal_RefusesWithTheExactCommand(t *testing.T) {
	dir := t.TempDir()
	path, sha := script(t, dir, "auth-tool", "exit 1\n")
	rec := &recordingExec{checkCode: 1}

	var out, errb bytes.Buffer
	o := opts(&out, &errb, false)
	o.Exec = rec
	_, err := Run(dir, []Hook{hook("gh", path, sha, true, "auth")}, o)
	if err == nil {
		t.Fatal("a required auth hook with no terminal must refuse, never hang on a prompt")
	}
	if !strings.Contains(err.Error(), "pix setup --env work") {
		t.Fatalf("the refusal must name the exact command to rerun, got %q", err)
	}
	if rec.applied {
		t.Fatal("apply ran without a terminal; an auth hook must refuse first")
	}
}

// TestRun_AuthHookWithoutATerminal_NamesTheCommandExactlyOnce guards the
// specific regression a user hit: the auth-hook detail used to spell out
// its own "rerun interactively: pix setup --env NAME" remedy INSIDE the
// detail soft() already prefixes with that exact same command, so the
// rendered error said "pix setup --env work: ... pix setup --env work"
// twice in one line instead of once.
func TestRun_AuthHookWithoutATerminal_NamesTheCommandExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	path, sha := script(t, dir, "auth-tool", "exit 1\n")
	rec := &recordingExec{checkCode: 1}

	var out, errb bytes.Buffer
	o := opts(&out, &errb, false)
	o.Exec = rec
	_, err := Run(dir, []Hook{hook("gh", path, sha, true, "auth")}, o)
	if err == nil {
		t.Fatal("a required auth hook with no terminal must refuse")
	}
	if n := strings.Count(err.Error(), "pix setup --env work"); n != 1 {
		t.Fatalf("the command must be named exactly once, got %d times in %q", n, err)
	}
}

func TestRun_AuthHookWithATerminal_MayInstallNonInteractivelyIsNotConfused(t *testing.T) {
	dir := t.TempDir()
	path, sha := script(t, dir, "install-tool", "exit 1\n")
	rec := &recordingExec{checkCode: 1}

	// An INSTALL hook is explicitly allowed on a non-interactive terminal:
	// the trusted, explicit `pix setup --env NAME` is the consent, and an
	// installer needs no human. Only kind=auth refuses.
	var out, errb bytes.Buffer
	o := opts(&out, &errb, false)
	o.Exec = rec
	if _, err := Run(dir, []Hook{hook("tool", path, sha, false, "install")}, o); err != nil {
		t.Fatalf("a non-interactive install hook must be allowed under an explicit setup: %v", err)
	}
	if !rec.applied {
		t.Fatal("the install hook's apply never ran")
	}
	if rec.checks != 2 {
		t.Fatalf("check must run before AND after apply, got %d checks", rec.checks)
	}
}

// ── the executable actually runs from a private snapshot, cwd == snapshot ──

// TestRun_ExecutesFromASnapshotDirNotTheEnvironmentRoot proves the exe/cwd
// a real process sees: the argv[0] path is UNDER a private temp directory
// (never the environment root path itself), and pwd == that same directory
// (the snapshot's own root, per the "set cwd to snapshot root" rule).
func TestRun_ExecutesFromASnapshotDirNotTheEnvironmentRoot(t *testing.T) {
	dir := t.TempDir()
	path, sha := script(t, dir, "setup-tool", `
echo "argv0:$0"
echo "pwd:$(pwd)"
exit 0
`)
	h := hook("tool", path, sha, true, "install")
	rec := &capturingExec{}

	var out, errb bytes.Buffer
	o := opts(&out, &errb, true)
	o.Exec = rec
	if _, err := Run(dir, []Hook{h}, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.dir == "" || rec.dir == dir {
		t.Fatalf("cwd = %q, want a private snapshot directory distinct from the environment root %q", rec.dir, dir)
	}
	if rec.exe == path {
		t.Fatalf("exe = %q, want a COPY under the snapshot, never the reviewed path itself", rec.exe)
	}
	if filepath.Dir(rec.exe) != rec.dir {
		t.Fatalf("exe %q is not inside the snapshot dir %q passed as cwd", rec.exe, rec.dir)
	}
	if _, err := os.Stat(rec.dir); err == nil {
		t.Fatal("the snapshot directory was not cleaned up after the run")
	}
}

// capturingExec records the dir/exe Check was called with, so a test can
// prove they are the snapshot's, not the caller's original root/path.
type capturingExec struct {
	dir, exe string
}

func (c *capturingExec) Check(dir, exe string, args []string) (int, string, error) {
	c.dir, c.exe = dir, exe
	return 0, "", nil
}

func (c *capturingExec) Apply(dir, exe string, args []string, in io.Reader, out, errw io.Writer) (int, error) {
	c.dir, c.exe = dir, exe
	return 0, nil
}

// ── boundedBuffer must survive concurrent stdout+stderr writers ───────────

// TestBoundedBuffer_ConcurrentWritesAreRaceFree pins the realExecutor.Check
// shape directly: cmd.Stdout and cmd.Stderr are the SAME *boundedBuffer, and
// os/exec copies each pipe on its own goroutine, so two Writes can land
// concurrently on a hook that writes to both descriptors. Run with
// `go test -race`, this must be clean; without the mutex it is a
// textbook data race on buf.
func TestBoundedBuffer_ConcurrentWritesAreRaceFree(t *testing.T) {
	var buf boundedBuffer
	buf.limit = checkOutputLimit
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				fmt.Fprintf(&buf, "writer-%d-line-%d\n", n, i)
			}
		}(g)
	}
	wg.Wait()
	_ = buf.String()
}

// TestRealExecutor_Check_ConcurrentStdoutStderrIsRaceFree drives the actual
// production seam end to end: a real process writing to stdout and stderr
// from two different underlying pipes, captured through realExecutor.Check.
func TestRealExecutor_Check_ConcurrentStdoutStderrIsRaceFree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "noisy")
	body := "#!/bin/sh\nfor i in $(seq 1 200); do echo \"out-$i\"; echo \"err-$i\" >&2; done\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (realExecutor{}).Check(dir, path, nil); err != nil {
		t.Fatalf("Check: %v", err)
	}
}
