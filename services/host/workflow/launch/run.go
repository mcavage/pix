package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/sys"
	"pix/host/workspace"
)

// ApplyConfiguredSessionModel gives every interactive entry point (`run` and
// `task new`) the same top-level model, so a task sandbox cannot silently
// inherit pi's provider default. The bool reports whether a configured intent
// (including the explicit none/off opt-out) applied; callers decide how to
// present resolver errors.
func ApplyConfiguredSessionModel(o *RunOpts, cfg *config.Config) (bool, error) {
	if o == nil || cfg == nil || o.Model != "" || o.Intent != "" {
		return false, nil
	}
	intent := strings.TrimSpace(cfg.RunIntent)
	if intent == "" {
		return false, nil
	}
	if strings.EqualFold(intent, "none") || strings.EqualFold(intent, "off") {
		return true, nil
	}
	model, err := inference.ResolveSessionModel(intent)
	if err != nil {
		return true, err
	}
	o.Intent = intent
	o.Model = model
	return true, nil
}

// Creation-evidence poll. After `sbx run` is STARTED (not waited), the create
// path polls for the sandbox to become visible and records the receipt the
// moment it is, so status/doctor render preload provenance WHILE the session
// is alive. It is a PARAMETER, not a package var: the composition root alone
// knows which env answers the probe, and a test passes its own without
// mutating shared state. Every field is required — a create with an
// incomplete poll is an error, never a silent "record nothing".
type CreatePoll struct {
	// Probe answers "does a sandbox by this name exist yet".
	Probe func(name string) SbxState
	// Interval is the gap between probes; Timeout bounds the whole poll.
	Interval time.Duration
	Timeout  time.Duration
}

// SbxCreatePollInterval/Timeout are the production poll budget. The timeout is
// generous on purpose: a first create may pull the image for minutes, and the
// poll runs only while `sbx run` is still alive.
const (
	SbxCreatePollInterval = 500 * time.Millisecond
	SbxCreatePollTimeout  = 15 * time.Minute
)

// SbxCreatePoll is the real creation-evidence poll: `sbx ls` through env, on
// the production budget. The composition root builds it and hands it to
// ExecSbxRunAndRecordCreate; nothing in this package can conjure an env.
func SbxCreatePoll(env hostenv.Env) CreatePoll {
	return CreatePoll{
		Probe:    func(name string) SbxState { return ProbeTaskSandbox(env, name) },
		Interval: SbxCreatePollInterval,
		Timeout:  SbxCreatePollTimeout,
	}
}

// validate refuses a poll that cannot decide anything. Failing here — BEFORE
// `sbx run` starts — is the fail-closed half of deleting the package-var seam:
// an unwired poll used to degrade into "probe nothing, record nothing", which
// is the silent failure this whole receipt path exists to prevent.
func (p CreatePoll) validate() error {
	switch {
	case p.Probe == nil:
		return errors.New("create-receipt poll has no sandbox probe (pass launch.SbxCreatePoll(env))")
	case p.Interval <= 0:
		return fmt.Errorf("create-receipt poll interval must be positive, got %s", p.Interval)
	case p.Timeout <= 0:
		return fmt.Errorf("create-receipt poll timeout must be positive, got %s", p.Timeout)
	}
	return nil
}

// sandboxAppeared reports whether st is POSITIVE existence evidence. Absent
// keeps polling; unknown proves nothing and also keeps polling — never record
// a create receipt on an indeterminate read.
func sandboxAppeared(st SbxState) bool { return st == SbxRunning || st == SbxStopped }

// SessionChild is a STARTED child session whose create-time recording is
// already DECIDED but whose exit has not been waited for yet. It exists so a
// caller holding a lifecycle lock can do the two things in the right order:
// finish recording (Appeared says whether there is anything to record
// against), release the lock, and only THEN wait — a session's own lifetime
// is not a lifecycle transition and must never be serialized as one.
//
// Wait is safe to call exactly once and always terminates: the Wait goroutine
// started at Start writes its result to a buffered channel, and a result
// already consumed during the creation poll is replayed rather than waited
// for a second time.
type SessionChild struct {
	// Appeared reports that the create poll POSITIVELY saw this sandbox in
	// `sbx ls` — the only state in which a caller may record its own
	// create-time facts (lease record, fingerprint, invocation) against it.
	Appeared bool

	waitCh  chan error
	drained bool  // the child already exited during the creation poll
	exitErr error // its exit result, replayed by Wait
}

// Wait hands the terminal back and waits the session out, returning the
// child's own exit result and nothing else.
func (c *SessionChild) Wait() error {
	if c.drained {
		return c.exitErr
	}
	return <-c.waitCh
}

// StartSbxSession starts cmd (the composed `sbx ...` argv, stdio already
// wired) and runs every step that must complete BEFORE the session is waited
// for, returning the still-running child. creating=false (an attach) starts
// cmd and nothing else. creating=true then POLLS until the runtime positively
// lists the sandbox, so the caller — which is still holding the lifecycle lock
// — can record its create-time facts against evidence rather than hope (see
// SessionChild.Appeared and RecordSessionCreation).
//
// A non-nil error means the child never started, or the poll was unusable:
// nothing was created and nothing recorded. Everything else, including a child
// that already exited, is reported through the returned SessionChild's Wait.
//
// The poll's OWN outcome is deliberately not an error. An exit before creation
// evidence surfaces the child's own error; a clean exit gets ONE final probe,
// because evidence found at exit is still evidence; a timeout with the session
// still running leaves Appeared false, which the caller reads as "nothing to
// record against" — the same honest answer as never having seen it.
func StartSbxSession(cmd *exec.Cmd, poll CreatePoll, creating bool, sandbox string) (*SessionChild, error) {
	if !creating {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return startedChild(cmd), nil
	}
	if err := poll.validate(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	child := startedChild(cmd)

	deadline := time.Now().Add(poll.Timeout)
	ticker := time.NewTicker(poll.Interval)
	defer ticker.Stop()
	for {
		if sandboxAppeared(poll.Probe(sandbox)) {
			child.Appeared = true
			return child, nil
		}
		if time.Now().After(deadline) {
			return child, nil
		}
		select {
		case werr := <-child.waitCh:
			child.drained, child.exitErr = true, werr
			if werr == nil && sandboxAppeared(poll.Probe(sandbox)) {
				child.Appeared = true
			}
			return child, nil
		case <-ticker.C:
		}
	}
}

// startedChild wires the one Wait goroutine every started child gets: it
// always terminates and its result is always buffered, so a caller that never
// reaches Wait (a refused lifecycle transition) cannot leak it.
func startedChild(cmd *exec.Cmd) *SessionChild {
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	return &SessionChild{waitCh: waitCh}
}

// RecreateGuidance is the ONE recovery sentence for every "this sandbox is not
// the one you asked for" outcome: an attach whose create-time fingerprint
// diverged, and an attach that failed outright. It names the two steps that
// exist, in order, with the resolved sandbox name filled in — never a guessed
// or relative one.
//
// It replaces `pix run --replace`, which U04e retired. --replace was an `sbx rm
// -f` issued from the command layer, outside the lifecycle lock and with no
// zero-holder proof, which is precisely the forced removal U04d's teardown
// exists to prevent: it could destroy a sandbox another shell was live in.
// `pix rm` is the proof-gated removal, and it refuses (rather than racing) when
// somebody else still holds a reference — so the guidance points there.
func RecreateGuidance(name string) string {
	return fmt.Sprintf("remove it explicitly, then re-run: pix rm %s && pix run", sys.ShellQuote(name))
}

// ValidateRunWorkspace verifies a resolved run workspace is launchable: the cwd
// default (".") always is; any other value must name an existing directory. A
// non-directory token that matches a known verb gets a "did you mean" hint.
//
// knownVerb is the verb table, passed in because only cmd/pix has one: it
// turns `pix run doctro` into a suggestion instead of a junk sandbox. A caller
// with no verb table passes nil and loses the hint, nothing else — a choice
// visible at the call site, which is the point of it not being a package var.
func ValidateRunWorkspace(ws string, knownVerb func(string) bool) error {
	err := workspace.Validate(ws)
	var nd workspace.ErrNotDirectory
	if errors.As(err, &nd) && knownVerb != nil && knownVerb(ws) {
		return fmt.Errorf("%q is not a directory. Did you mean `pix %s`?", ws, ws)
	}
	return err
}

// ResolveRepoRoot finds a pix repo checkout for the local kit path, in order:
// $PIX_DEV_ROOT if set, else walking up from the cwd, else the launcher
// binary's own location (make install symlinks ~/.local/bin/pix ->
// <repo>/out/pix, so the repo is two levels up from the resolved binary).
func ResolveRepoRoot() (string, error) {
	if r := strings.TrimSpace(os.Getenv("PIX_DEV_ROOT")); r != "" {
		if isRepoRoot(r) {
			return r, nil
		}
		return "", fmt.Errorf("$PIX_DEV_ROOT=%q is not a pix checkout (no pi-kit/spec.yaml)", r)
	}
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; ; {
			if isRepoRoot(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if self, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = resolved
		}
		if repo := filepath.Dir(filepath.Dir(self)); isRepoRoot(repo) {
			return repo, nil
		}
	}
	return "", fmt.Errorf("no pix checkout found (set $PIX_DEV_ROOT or run from inside a checkout)")
}

// isRepoRoot reports whether dir looks like a pix repo checkout.
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pi-kit", "spec.yaml"))
	return err == nil
}

// LocalImageLoaded reports whether sbx's template store carries the local image
// tag, so a launch never makes sbx PULL a never-published local-* image. It
// matches the tag as a SUBSTRING of `sbx template ls` output: format
// independent, and it catches the fully-pruned case (no matching line ->
// refuse). The tag is a unique local-<unixts>, so a substring match cannot
// collide. It fails OPEN only when there is NO signal to judge from: no sbx, an
// ls error, a timeout, or empty output.
func LocalImageLoaded(env hostenv.Env, tag string) bool {
	if tag == "" {
		return true
	}
	if _, err := env.LookPath("sbx"); err != nil {
		return true
	}
	out, timedOut, err := env.RunTimed("sbx", "template", "ls")
	if timedOut || err != nil || strings.TrimSpace(out) == "" {
		return true // no signal -> don't block
	}
	return strings.Contains(out, tag)
}

// ReadLocalImageTag returns the trimmed contents of <root>/out/.local-image-tag
// (written by `make load`), or "" when absent — in which case the caller skips
// the --template pin.
func ReadLocalImageTag(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "out", ".local-image-tag"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteOllamaBridgeFile writes <workspace>/.pix/ollama-bridge.model: the local
// model tag the in-VM ollama-bridge should expose. Per-run, gitignored,
// best-effort — an absent file just means the bridge uses its default.
func WriteOllamaBridgeFile(ws, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = config.DefaultOllamaBridgeModel
	}
	_ = workspace.WriteStateFile(ws, "ollama-bridge.model", []byte(model+"\n"), 0o644)
}

// PrintJSONLauncher writes v as indented JSON to w and returns the marshal or
// write error instead of printing to a stream it chose itself. The caller owns
// the destination (`--json` output must reach the command's stdout and nowhere
// else) and the failure.
func PrintJSONLauncher(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// GeneratedInputMarker prefixes any user-role message `pix` itself synthesizes
// and hands to the agent as if typed by the user (currently the onboarding
// kickoff). It is NOT the user talking, so extensions that observe user turns
// (memory-capture.ts) must recognize it and skip capture — without it the
// watcher invents facts from the kickoff line. Keep this string and that
// extension's prefix check in sync. It is not user-visible: the kickoff travels
// as pi's initial CLI prompt argument, which transcripts do not render.
const GeneratedInputMarker = "[pix-generated:onboarding] "
