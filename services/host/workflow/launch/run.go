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

type CreatePoll struct {
	// Probe answers "does a sandbox by this name exist yet".
	Probe func(name string) SbxState
	// Interval is the gap between probes; Timeout bounds the whole poll.
	Interval time.Duration
	Timeout  time.Duration
}

const (
	SbxCreatePollInterval = 500 * time.Millisecond
	SbxCreatePollTimeout  = 15 * time.Minute
)

func SbxCreatePoll(env hostenv.Env) CreatePoll {
	return CreatePoll{
		Probe:    func(name string) SbxState { return ProbeTaskSandbox(env, name) },
		Interval: SbxCreatePollInterval,
		Timeout:  SbxCreatePollTimeout,
	}
}

// validate refuses a poll that cannot decide anything, BEFORE `sbx run` starts.
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

func sandboxAppeared(st SbxState) bool { return st == SbxRunning || st == SbxStopped }

type SessionChild struct {
	// Appeared reports that the create poll POSITIVELY saw this sandbox in
	// `sbx ls` — the only state in which a caller may record its own
	// create-time facts (lease record, fingerprint, invocation) against it.
	Appeared bool

	cmd     *exec.Cmd // kept only so Kill has something to signal
	waitCh  chan error
	drained bool  // the child already exited during the creation poll
	exitErr error // its exit result, replayed by Wait
}

func (c *SessionChild) Wait() error {
	if c.drained {
		return c.exitErr
	}
	return <-c.waitCh
}

// Kill is the abort path for a child that started but could not be handed a
// reference lease (see RunSession): a live, unreferenced session is worse
// than one killed outright, because a future reaper's zero-holder proof
// cannot tell the two apart. It signals the child (a no-op if it already
// exited) and then replays its exit the same way Wait does, so a caller
// always gets a definite result instead of racing its own drained/exitErr
// bookkeeping.
func (c *SessionChild) Kill() error {
	if c.drained {
		return c.exitErr
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	err := <-c.waitCh
	c.drained, c.exitErr = true, err
	return err
}

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

func startedChild(cmd *exec.Cmd) *SessionChild {
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	return &SessionChild{cmd: cmd, waitCh: waitCh}
}

// RecreateGuidance is the ONE recovery sentence for every "this sandbox is not
// the one you asked for" outcome: an attach whose create-time fingerprint
// diverged, and an attach that failed outright. It names the two steps that
// exist, in order, with the resolved sandbox name filled in — never a guessed
// or relative one.
func RecreateGuidance(name string) string {
	return fmt.Sprintf("remove it explicitly, then re-run: pix rm %s && pix run", sys.ShellQuote(name))
}

// ValidateRunWorkspace verifies a resolved run workspace is launchable: the cwd
// default (".") always is; any other value must name an existing directory. A
// non-directory token that matches a known verb gets a "did you mean" hint.
func ValidateRunWorkspace(ws string, knownVerb func(string) bool) error {
	err := workspace.Validate(ws)
	var nd workspace.ErrNotDirectory
	if errors.As(err, &nd) && knownVerb != nil && knownVerb(ws) {
		return fmt.Errorf("%q is not a directory. Did you mean `pix %s`?", ws, ws)
	}
	return err
}

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

func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pi-kit", "spec.yaml"))
	return err == nil
}

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

func ReadLocalImageTag(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "out", ".local-image-tag"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func WriteOllamaBridgeFile(ws, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = config.DefaultOllamaBridgeModel
	}
	_ = workspace.WriteStateFile(ws, "ollama-bridge.model", []byte(model+"\n"), 0o644)
}

func PrintJSONLauncher(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

const GeneratedInputMarker = "[pix-generated:onboarding] "
