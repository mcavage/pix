// Package envsetup runs an environment's own `[[setup]]` hooks — the v2
// replacement for a v1 pack's authored install/auth hook, and the ONLY
// place Pix executes environment-authored code on the host.
//
// What makes it safe is not the runner, it is the gate around it:
//
//  1. A hook exists only inside the environment directory that declares it
//     (pix.toml `[[setup]]`, envinfo.SetupHook). There is no registry, no
//     search path, no plugin directory, and nothing outside that one
//     environment can contribute a hook.
//  2. Its resolved executable's CONTENT HASH, both argv lists, its kind and
//     its required bit are all part of the environment's trust bill of
//     materials (workflow/env.SetupHookFact) — so they are rendered in the
//     default consent screen and bound into the fingerprint a human
//     accepts with a default-No prompt.
//  3. It runs ONLY under an explicit `pix setup --env NAME`, after that
//     acceptance. `pix run`, `pix doctor`, and every implicit launch reach
//     none of this.
//  4. Immediately before each execution, Run re-proves the executable AND
//     every declared companion input on disk still hash to the accepted
//     snapshot's SHA, and copies those exact verified bytes — never a
//     second, independent read of the same path — into a fresh, private
//     0700 directory nothing else on this host can read. check/apply/
//     postcheck then execute ONLY that snapshot's bytes, with the
//     snapshot's own root as cwd. That is what actually closes the window
//     between "a human said yes" and "we exec'd it" (TOCTOU): a plain
//     "hash it, then exec the same path" sequence still leaves a race
//     between the hash and the exec; hashing and copying in the SAME read
//     does not. The snapshot is removed when the hook is done, success or
//     failure.
//
// Execution itself is deliberately boring: os/exec with an explicit argv,
// no shell, no interpolation, and no environment injection — the child
// inherits this process's environment unchanged and Pix adds nothing to
// it, so a hook can never be handed a credential it was not already going
// to see. Because the snapshot is a fresh, minimal directory containing
// ONLY the reviewed executable and its declared `inputs`, an environment
// sibling file the hook did not declare is simply not there: a hook must
// name every companion script or data file it needs via `inputs`, using
// paths relative to the snapshot root (its own cwd), or that file is
// unavailable to it by default.
package envsetup

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"os"
	"path/filepath"

	"pix/host/envinfo"
	"pix/host/hosttrust"
	"pix/host/sys"
)

// Hook is one RESOLVED `[[setup]]` entry: the environment-authored
// identity (envinfo.SetupHook) plus the two facts only this host can
// state — the absolute executable path the argv resolves to, and that
// executable's sha256 content hash. It is the type workflow/env's bill of
// materials fingerprints and renders (as env.SetupHookFact, a type alias
// for this one), so what a human accepts and what Run executes are
// literally the same value, never two structs a conversion could let
// drift.
//
// SHA is mandatory: a hook whose executable cannot be content-hashed
// (missing, a symlink, a directory, not executable, unreadable) is refused
// when the bill is computed rather than reviewed as absent — a reviewer
// cannot consent to code whose identity nothing can state, which is
// exactly why "absolute allowed only if the exact executable is
// fingerprinted".
//
// Resolved is deliberately NOT part of the fingerprint (json:"-"): it is
// this host's absolute path for the same reviewed content, so cloning an
// environment to another directory must not re-gate an unchanged hook.
type Hook struct {
	ID        string   `json:"id"`
	Command   string   `json:"command"`
	Resolved  string   `json:"-"`
	SHA       string   `json:"sha"`
	CheckArgs []string `json:"check_args"`
	ApplyArgs []string `json:"apply_args"`
	Kind      string   `json:"kind"`
	Required  bool     `json:"required"`
	// Inputs is every declared `inputs = [...]` companion this hook needs
	// copied alongside it into its private execution snapshot — relative to
	// the environment root the same way Command is, and fingerprinted
	// (path + content hash) exactly like the executable itself: a reviewer
	// consents to a companion script or data file the same way they consent
	// to the hook binary, and mutating one after acceptance re-gates the
	// whole environment (its content hash moves the fingerprint) exactly as
	// mutating the executable does.
	Inputs []HookInput `json:"inputs,omitempty"`
}

// HookInput is one declared `[[setup]].inputs` entry, resolved to the
// environment-root-relative path a reviewer read and the sha256 content
// hash of what they accepted. Path is intentionally the AUTHORED relative
// form (never an absolute host path): it is both the fingerprinted identity
// and the exact relative location the byte-verified copy lands at inside
// the hook's execution snapshot, so a hook reads it with the same relative
// path a human reviewed.
type HookInput struct {
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

// HashSetupExecutable is the ONE proof a setup hook's command (or a
// declared input) is something this host may fingerprint: an existing,
// REGULAR (never a symlink, never a directory, never a device) file,
// returned as its sha256 content hash — executable is required only for the
// hook's own Command, never for an input, so callers checking a
// non-executable companion data file use hosttrust.HashFile/ReadVerifiedFile
// directly instead. It is exported because the bill of materials computes it
// once at review time, and the TOCTOU snapshot below (takeSnapshot) proves
// content again immediately before exec — via the SAME read that copies the
// bytes, so the second proof can never be a looser check than the first.
func HashSetupExecutable(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s: is a symlink; refusing", path)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s: is a directory, not an executable", path)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: is not a regular file", path)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s: is not executable", path)
	}
	return hosttrust.HashFile(path, path)
}

// Resolve turns an authored hook command into the absolute path this host
// would execute: an absolute command unchanged, a relative one against the
// environment root. There is deliberately NO PATH lookup — envinfo's parse
// already refused a bare name, so resolution never depends on the calling
// process's cwd or environment.
//
// Resolve alone does NOT prove containment: a relative command whose
// resolved parent directory is, or sits behind, a symlink can lexically
// join inside root while its REAL location is somewhere else entirely. A
// caller computing the reviewed bill of materials uses ProveContained
// instead, for exactly that reason; Resolve is kept as the pure, no-syscall
// building block ProveContained and the absolute-command path both still
// need.
func Resolve(root, command string) string {
	if filepath.IsAbs(command) {
		return filepath.Clean(command)
	}
	return filepath.Clean(filepath.Join(root, command))
}

// ProveContained resolves a RELATIVE path (a hook's own relative Command,
// or one of its declared Inputs) against root and proves the result cannot
// have escaped root through a symlinked ancestor: it EvalSymlinks's root
// itself and the resolved path's parent directory (EvalSymlinks resolves
// every intermediate symlink in a path, not just its final component, so
// one call already covers every ancestor of the parent, not only the
// immediate one) and requires the two real locations to coincide or nest.
// The final path component itself is deliberately left un-resolved here —
// a caller that goes on to open it does so with an O_NOFOLLOW read
// (hosttrust.ReadVerifiedFile / HashSetupExecutable's own Lstat), which is
// the check that actually refuses the referenced file being a symlink,
// atomically with the read that trusts its content.
//
// rel must already be relative (envinfo's parse refuses an absolute or
// `..`-segmented input/command before this ever runs); ProveContained
// itself refuses an absolute rel as a defensive second check, since a
// caller that ever fed one through would otherwise silently resolve against
// root's PARENT via filepath.Join's own absolute-wins-nothing behavior.
func ProveContained(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s: must be a relative path", rel)
	}
	abs := Resolve(root, rel)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("environment root %s: %w", root, err)
	}
	dir := filepath.Dir(abs)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("%s: %w", rel, err)
	}
	if realDir != realRoot && !strings.HasPrefix(realDir, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: resolves outside the environment root %s through a symlinked parent directory; refusing", rel, root)
	}
	return abs, nil
}

// checkOutputLimit bounds how much of a check's stdout/stderr is retained
// for diagnostics. A check is a machine question ("are you ready?"), so its
// output is captured rather than inherited; capturing without a bound would
// let a hostile hook stream a gigabyte into the launcher's memory.
const checkOutputLimit = 8 << 10

// Options is everything Run needs from its caller. Out/Err/In are the
// command's own streams (cli.Deps.Out/Err/In in production, buffers under
// test); Interactive is whether In is a real terminal, which is the ONLY
// thing that decides whether an auth hook may run.
type Options struct {
	EnvName     string
	Out         io.Writer
	Err         io.Writer
	In          io.Reader
	Interactive bool

	// Exec is the process seam. Nil means the real one (os/exec). A test
	// substitutes it to prove sequencing without spawning anything; the
	// process tests in this package leave it nil and run real executables.
	Exec Executor
}

// Executor runs one hook argv. Both methods take the ALREADY-RESOLVED
// absolute executable path and its argv tail; neither ever sees a shell
// string, so there is no layer here that could grow one.
type Executor interface {
	// Check runs a readiness probe with its output captured and bounded.
	// The int is the process exit code; err is non-nil only when the
	// process could not be run or waited on at all.
	Check(dir, exe string, args []string) (int, string, error)
	// Apply runs the mutating argv with stdio INHERITED, so an auth hook's
	// device-code prompt or a package manager's progress reaches the same
	// terminal the human is sitting at.
	Apply(dir, exe string, args []string, in io.Reader, out, errw io.Writer) (int, error)
}

// Outcome is what happened to one hook.
type Outcome struct {
	ID string
	// State is one of the four honest answers below.
	State  string
	Detail string
}

// Hook outcome states. "ready" is only ever set after a check exited 0 —
// there is no path that reports readiness from a successful apply alone
// (safety invariant 12: success words are earned by a probe).
const (
	// StateAlreadyReady: the first check exited 0; nothing was mutated.
	StateAlreadyReady = "already-ready"
	// StateApplied: check failed, apply ran, and the RE-CHECK exited 0.
	StateApplied = "applied"
	// StateFailed: a required hook that is still not ready, or could not
	// be run at all.
	StateFailed = "failed"
	// StateSkipped: an auth hook that needs a terminal and did not get one.
	StateSkipped = "skipped"
)

// Result is Run's whole report.
type Result struct {
	Outcomes []Outcome
}

// Ready reports whether every REQUIRED hook ended in a probed-ready state.
// An optional hook that failed or was skipped leaves this true — it was
// warned about honestly, not silently swallowed.
func (r Result) Ready() bool {
	for _, o := range r.Outcomes {
		if o.State == StateFailed {
			return false
		}
	}
	return true
}

// Run executes hooks — the SetupHookFact slice from the ONE environment
// snapshot whose fingerprint the caller already proved is the accepted one
// — in the environment root dir. It returns an error for the first REQUIRED
// hook that cannot be made ready (including a refusal: a mutated
// executable, or an auth hook with no terminal); an optional hook's failure
// is written to Err as a warning and never fails the run.
//
// Idempotence is structural, not asserted: a hook whose check exits 0 is
// reported already-ready and nothing is executed beyond that probe, so a
// second `pix setup --env NAME` on a converged host mutates nothing.
func Run(dir string, hooks []Hook, opts Options) (Result, error) {
	var res Result
	ex := opts.Exec
	if ex == nil {
		ex = realExecutor{}
	}
	for _, h := range hooks {
		outcome, err := runOne(dir, h, opts, ex)
		res.Outcomes = append(res.Outcomes, outcome)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func runOne(root string, h Hook, opts Options, ex Executor) (Outcome, error) {
	label := fmt.Sprintf("%s (%s)", h.ID, h.Kind)

	// TOCTOU close: build a private, per-hook snapshot. takeSnapshot both
	// re-proves the executable and every declared input against the
	// accepted sha256 AND copies those exact verified bytes into a fresh
	// 0700 directory in the SAME read — never a separate hash-then-open-
	// again sequence, which is the gap a swap between the two calls could
	// still fit through.
	snap, exe, cleanup, err := takeSnapshot(root, h)
	if err != nil {
		var mm *mismatchError
		if errors.As(err, &mm) {
			// A CONTENT mismatch is a hard refusal whether or not the hook is
			// required: "optional" describes a tool this environment can live
			// without, never a licence to execute code no human ever reviewed.
			detail := mm.Error()
			return Outcome{ID: h.ID, State: StateFailed, Detail: detail},
				fmt.Errorf("pix setup --env %s: setup hook %s: %s; re-review it: pix env trust %s", opts.EnvName, label, detail, opts.EnvName)
		}
		return soft(h, opts, label, err.Error())
	}
	defer cleanup()

	code, out, err := ex.Check(snap, exe, h.CheckArgs)
	out = sys.TerminalSafe(out)
	if err != nil {
		return soft(h, opts, label, fmt.Sprintf("check could not run: %v", err))
	}
	if code == 0 {
		fmt.Fprintf(opts.Out, "pix setup: %s: already ready (check passed; nothing changed)\n", label)
		return Outcome{ID: h.ID, State: StateAlreadyReady}, nil
	}
	fmt.Fprintf(opts.Out, "pix setup: %s: check exited %d; applying\n", label, code)

	// An auth hook talks to a HUMAN. Without a terminal it would either
	// hang on a prompt nobody can answer or silently fail half way, so it
	// refuses by name and prints the exact command to run instead.
	if h.Kind == envinfo.SetupKindAuth && !opts.Interactive {
		return soft(h, opts, label, fmt.Sprintf("needs a terminal; rerun interactively: pix setup --env %s", opts.EnvName))
	}

	acode, aerr := ex.Apply(snap, exe, h.ApplyArgs, opts.In, opts.Out, opts.Err)
	if aerr != nil {
		return soft(h, opts, label, fmt.Sprintf("apply could not run: %v", aerr))
	}

	// The post-check is the ONLY thing that may produce a success word. A
	// zero exit from apply proves the installer ran, never that the tool is
	// present and working.
	pcode, pout, perr := ex.Check(snap, exe, h.CheckArgs)
	pout = sys.TerminalSafe(pout)
	if perr != nil {
		return soft(h, opts, label, fmt.Sprintf("post-check could not run: %v", perr))
	}
	if pcode == 0 {
		fmt.Fprintf(opts.Out, "pix setup: %s: ready (apply ran, post-check passed)\n", label)
		return Outcome{ID: h.ID, State: StateApplied}, nil
	}
	return soft(h, opts, label, fmt.Sprintf("apply exited %d, post-check still exited %d%s", acode, pcode, firstLineSuffix(pout, out)))
}

// snapshotDirPrefix names every private per-hook snapshot directory this
// package creates under the system temp root, so a stray one left behind
// after a crash (cleanup did not run: SIGKILL, a power loss) is
// identifiable by name alone.
const snapshotDirPrefix = "pix-setup-hook-"

// mismatchError is takeSnapshot's signal that a specific path's content no
// longer hashes to what was reviewed — as opposed to any other failure
// (missing file, permission error, a directory that vanished), which is a
// structural "cannot verify" runOne treats as soft(). Only a mismatchError
// is the unconditional "refuse even for an optional hook" case.
type mismatchError struct {
	path, accepted, found string
}

func (e *mismatchError) Error() string {
	return fmt.Sprintf("%s changed on disk since it was reviewed (accepted sha256:%s, found sha256:%s)", e.path, e.accepted, e.found)
}

// takeSnapshot creates a fresh, private 0700 directory, verifies h's
// executable and every declared input against their accepted sha256 in the
// SAME read that copies their bytes, and returns that snapshot's root plus
// the copied executable's own path inside it. cleanup always removes the
// snapshot — the caller defers it unconditionally on success; on any error
// takeSnapshot has already cleaned up itself, and returns a no-op cleanup.
//
// The executable is copied under its OWN basename so a rendered argv still
// names something recognizable; every input is copied preserving its
// declared relative path, so a hook's own relative reads resolve exactly
// as reviewed. Because the snapshot's cwd (the caller sets cmd.Dir to this
// same root) contains nothing but these reviewed files, an environment
// sibling the hook never declared as an input is simply absent — by
// default a hook can reach only what it named.
func takeSnapshot(root string, h Hook) (snapDir, exePath string, cleanup func(), err error) {
	noop := func() {}
	snap, err := os.MkdirTemp("", snapshotDirPrefix+"*")
	if err != nil {
		return "", "", noop, fmt.Errorf("creating a private setup-hook snapshot: %w", err)
	}
	remove := func() { os.RemoveAll(snap) }
	if err := os.Chmod(snap, 0o700); err != nil {
		remove()
		return "", "", noop, fmt.Errorf("setup-hook snapshot %s: %w", snap, err)
	}

	exeName := filepath.Base(h.Resolved)
	exeDst := filepath.Join(snap, exeName)
	if err := copyVerified(h.Resolved, exeDst, h.SHA, 0o700); err != nil {
		remove()
		return "", "", noop, err
	}

	for _, in := range h.Inputs {
		if filepath.Clean(in.Path) == exeName {
			remove()
			return "", "", noop, fmt.Errorf("setup hook %s: input %q collides with the hook executable's own snapshot name", h.ID, in.Path)
		}
		src := filepath.Join(root, in.Path)
		dst := filepath.Join(snap, in.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			remove()
			return "", "", noop, fmt.Errorf("setup hook %s: input %q: %w", h.ID, in.Path, err)
		}
		if err := copyVerified(src, dst, in.SHA, 0o600); err != nil {
			remove()
			return "", "", noop, err
		}
	}
	return snap, exeDst, remove, nil
}

// copyVerified reads src with hosttrust.ReadVerifiedFile (O_NOFOLLOW open,
// fstat-regular, on the SAME open descriptor — no separate Lstat a symlink
// swap could race), hashes those exact bytes, and only writes dst — the
// snapshot copy — once they match wantSHA. A mismatch returns *mismatchError
// (an unconditional refusal); any other failure (missing file, permission
// error, a write failure on the snapshot side) returns a plain error, which
// runOne treats as a structural "cannot verify" rather than a content
// mismatch.
func copyVerified(src, dst, wantSHA string, perm os.FileMode) error {
	data, err := hosttrust.ReadVerifiedFile(src)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	got := hosttrust.HashBytes(data)
	if got != wantSHA {
		return &mismatchError{path: src, accepted: wantSHA, found: got}
	}
	if err := os.WriteFile(dst, data, perm); err != nil {
		return fmt.Errorf("writing setup-hook snapshot %s: %w", dst, err)
	}
	return nil
}

// soft is every "this hook did not become ready" ending: fatal for a
// required hook, an HONEST warning (never silence, never a success word)
// for an optional one. Optional failures end as StateSkipped so Result.Ready
// keeps meaning "every required hook was probed ready". detail is rendered
// as-is by the caller — every caller of soft in this file already passes
// text built EITHER from this package's own fixed strings OR from captured
// process output that was already run through sys.TerminalSafe the moment
// it was captured (runOne's out/pout), so no raw terminal control byte from
// an environment-authored hook's stdout/stderr ever reaches here.
func soft(h Hook, opts Options, label, detail string) (Outcome, error) {
	if !h.Required {
		fmt.Fprintf(opts.Err, "pix setup: warning: optional setup hook %s is NOT ready: %s\n", label, detail)
		return Outcome{ID: h.ID, State: StateSkipped, Detail: detail}, nil
	}
	return Outcome{ID: h.ID, State: StateFailed, Detail: detail},
		fmt.Errorf("pix setup --env %s: required setup hook %s is not ready: %s", opts.EnvName, label, detail)
}

// firstLineSuffix appends the first non-empty line of whichever captured
// check output exists, so a failure names something concrete without
// dumping a hook's whole (bounded) transcript into the error. Callers pass
// already-sys.TerminalSafe'd text (runOne sanitizes out/pout the moment
// ex.Check returns them), so this never re-emits a raw control byte a
// hostile hook printed.
func firstLineSuffix(outs ...string) string {
	for _, s := range outs {
		for _, line := range strings.Split(s, "\n") {
			if t := strings.TrimSpace(line); t != "" {
				return ": " + t
			}
		}
	}
	return ""
}

// realExecutor is the production seam: os/exec, argv only.
type realExecutor struct{}

// Check runs the readiness argv with a BOUNDED capture of its combined
// output. cmd.Env is left nil, which means "inherit this process's
// environment unchanged": Pix injects nothing, resolves nothing, and adds
// no variable of its own.
func (realExecutor) Check(dir, exe string, args []string) (int, string, error) {
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	var buf boundedBuffer
	buf.limit = checkOutputLimit
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Stdin = nil
	err := cmd.Run()
	code, err := exitCode(err)
	return code, buf.String(), err
}

// Apply runs the mutating argv with stdio inherited from the caller's
// streams: an install's progress and an auth hook's prompt both belong on
// the human's terminal, not in a buffer nobody sees until it is over.
func (realExecutor) Apply(dir, exe string, args []string, in io.Reader, out, errw io.Writer) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errw
	return exitCode(cmd.Run())
}

// exitCode separates "the process ran and exited nonzero" (a fact the hook
// protocol is built on) from "the process could not be run at all" (a real
// error). Only the latter is returned as err.
func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// boundedBuffer is a Writer that keeps at most limit bytes and silently
// drops the rest — the process still runs to completion, it just cannot
// make the launcher hold its output.
// boundedBuffer's mu guards buf: realExecutor.Check wires the SAME
// *boundedBuffer as both cmd.Stdout and cmd.Stderr, and os/exec copies each
// pipe on its own goroutine, so two Write calls (one per stream) can and do
// land concurrently on a hook that writes to both descriptors — without a
// lock that is a data race on buf (caught by `go test -race`;
// TestBoundedBuffer_ConcurrentWritesAreRace-free pins it directly), not
// merely interleaved output.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
