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
	"pix/host/mcp"
	"pix/host/sys"
	"pix/host/workflow/doctor"
	"pix/host/workflow/pack"
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

// RecordCreateReceipt commits the create receipt — called ONLY by
// ExecSbxRunAndRecordCreate, once its poll has positively seen the create
// appear. preloaded is the EXACT --static-mcp set launch emitted, so a later
// read never disagrees with what create requested. merge=true (the pre-create
// clear succeeded) preserves loads appended during the create window;
// merge=false replaces outright so a prior lifetime's loads cannot survive.
func RecordCreateReceipt(sandbox, ws string, preloaded []string, merge bool) error {
	fail := func(err error) error {
		return &workspace.ReceiptRecordError{Op: "create", Sandbox: sandbox, Err: err}
	}
	dir, err := workspace.MCPStateDirFn()
	if err != nil {
		return fail(fmt.Errorf("resolving pix state dir: %w", err))
	}
	var werr error
	if merge {
		werr = workspace.CommitCreateReceipt(dir, sandbox, ws, preloaded, nil)
	} else {
		werr = workspace.WriteCreateReceipt(dir, sandbox, ws, preloaded, nil)
	}
	if werr != nil {
		return fail(werr)
	}
	return nil
}

// ExecSbxRunAndRecordCreate runs cmd (the composed `sbx run ...`, stdio already
// wired) and owns the create-receipt lifecycle around it. writeReceipt=false (a
// plain re-attach, or an inconclusive probe — see DefinitelyCreating) is
// cmd.Run() and nothing else: a re-attach writes nothing and clears nothing.
// writeReceipt=true CLEARS any stale receipt, STARTS cmd, polls for the
// sandbox, commits the receipt the moment it appears (merging loads recorded
// since the clear), then waits the session out.
//
// Outcome contract: an exit BEFORE creation evidence returns its own error and
// writes no receipt (a final probe on a CLEAN exit still records — evidence
// found at exit is evidence). A receipt that cannot be recorded after the
// sandbox appeared, or a poll timeout with the session still running, still
// waits the session out and surfaces as *workspace.ReceiptRecordError, so the
// caller reports "launched, but state unrecorded" and exits non-zero rather
// than a silent success or a fake launch failure. The Wait goroutine always
// terminates and its result is always drained.
func ExecSbxRunAndRecordCreate(cmd *exec.Cmd, poll CreatePoll, writeReceipt bool, sandbox, ws string, preloaded []string) error {
	child, err := StartSbxRunAndRecordCreate(cmd, poll, writeReceipt, sandbox, ws, preloaded)
	if err != nil {
		return err
	}
	return child.Wait()
}

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
	// `sbx ls` — the only state in which anything at all was recorded, and
	// the only state in which a caller may record its own create-time facts
	// (lease record, fingerprint, invocation) against it.
	Appeared bool

	waitCh  chan error
	recErr  error // receipt outcome, surfaced by Wait on a clean session exit
	drained bool  // the child already exited during the creation poll
	exitErr error // its exit result, replayed by Wait
}

// Wait hands the terminal back and waits the session out. The child's own
// failure dominates; a receipt failure surfaces only on a clean session exit,
// so a caller can report "launched, but state unrecorded" without inventing a
// launch failure.
func (c *SessionChild) Wait() error {
	if c.drained {
		if c.exitErr != nil {
			return c.exitErr
		}
		return c.recErr
	}
	if werr := <-c.waitCh; werr != nil {
		return werr
	}
	return c.recErr
}

// StartSbxRunAndRecordCreate is ExecSbxRunAndRecordCreate's first half: it
// runs every step that must complete BEFORE the session is waited for, and
// returns the still-running child. writeReceipt=false (a plain re-attach, or
// an inconclusive probe — see DefinitelyCreating) starts cmd and nothing
// else: a re-attach writes nothing and clears nothing. writeReceipt=true
// CLEARS any stale receipt, STARTS cmd, polls for the sandbox, and commits
// the receipt the moment it appears (merging loads recorded since the clear).
//
// A non-nil error means the child never started (or the poll was unusable) —
// nothing was created, nothing recorded. Everything else, including a child
// that already exited and a receipt that could not be written, is reported
// through the returned SessionChild's Wait.
func StartSbxRunAndRecordCreate(cmd *exec.Cmd, poll CreatePoll, writeReceipt bool, sandbox, ws string, preloaded []string) (*SessionChild, error) {
	if !writeReceipt {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return startedChild(cmd), nil
	}
	if err := poll.validate(); err != nil {
		return nil, err
	}

	// Pre-create clear: under the same per-sandbox lock the writers use, drop
	// any receipt from a previous incarnation of this name. merge stays false
	// unless the clear POSITIVELY succeeded; an unproven clear degrades to a
	// plain replace, which cannot resurrect old loads.
	merge := false
	if stateDir, err := workspace.MCPStateDirFn(); err == nil {
		if err := workspace.ClearMCPReceipt(stateDir, sandbox); err == nil {
			merge = true
		}
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
			child.recErr = RecordCreateReceipt(sandbox, ws, preloaded, merge)
			return child, nil
		}
		if time.Now().After(deadline) {
			child.recErr = &workspace.ReceiptRecordError{Op: "create", Sandbox: sandbox,
				Err: fmt.Errorf("timed out after %s waiting for the sandbox to appear in `sbx ls`; its preloaded MCP set was not recorded", poll.Timeout)}
			return child, nil
		}
		select {
		case werr := <-child.waitCh:
			// Exited before creation evidence. A failed exec surfaces its OWN
			// error, receiptless. A clean exit gets ONE final probe (the
			// sandbox may have appeared exactly as it exited); still no
			// evidence means honestly no receipt.
			child.drained, child.exitErr = true, werr
			if werr != nil {
				return child, nil
			}
			if sandboxAppeared(poll.Probe(sandbox)) {
				child.Appeared = true
				child.recErr = RecordCreateReceipt(sandbox, ws, preloaded, merge)
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

// ApplyReplaceRm runs the plan's RmFirst step and MUST be checked by the
// caller: a failed rm means the old sandbox may still exist under that name,
// and creating against it is undefined (sbx may error, or silently reattach
// with stale kit/mcp flags — exactly what --replace was avoiding). Clearing
// the removed sandbox's receipt is best-effort; ExecSbxRunAndRecordCreate's
// pre-create clear is the correctness backstop.
//
// warn takes the best-effort clear's failure note; it is a caller-supplied
// stream (never os.Stderr) so a test reads it and a command routes it.
func ApplyReplaceRm(env hostenv.Env, warn io.Writer, plan RunLaunchPlan, name string) error {
	if !plan.RmFirst {
		return nil
	}
	if _, err := env.Run("sbx", "rm", "-f", name); err != nil {
		return fmt.Errorf("could not remove existing sandbox %q to replace it: %w", name, err)
	}
	if err := workspace.ClearRemovedReceipt(name); err != nil {
		fmt.Fprintf(warn, "pix: warning: removed sandbox %q but could not clear its mcp receipt: %v\n", name, err)
	}
	return nil
}

// SandboxPackMarkerPath is <workspace>/.pix/sandbox.pack: the pack root this
// workspace's sandbox was CREATED with. Written on every create (removed when
// created pack-less), never on a re-attach, so a later re-attach compares
// create-time truth against the CURRENT active pack instead of guessing.
func SandboxPackMarkerPath(ws string) string {
	return filepath.Join(ws, ".pix", "sandbox.pack")
}

// WriteSandboxPackMarker records the pack root a sandbox is being created with
// (or removes the marker when creating pack-less). Best-effort: a failed write
// only costs a future stale-pack reminder, never the launch. Symlink-safe via
// workspace.WriteStateFile/RemoveStateFile — a cloned repo can ship
// .pix/sandbox.pack, or .pix ITSELF, as a tracked symlink.
func WriteSandboxPackMarker(ws, packRoot string) {
	if strings.TrimSpace(packRoot) == "" {
		_ = workspace.RemoveStateFile(ws, "sandbox.pack")
		return
	}
	_ = workspace.WriteStateFile(ws, "sandbox.pack", []byte(pack.CanonicalizePackRoot(packRoot)+"\n"), 0o644)
}

// ReadSandboxPackMarker returns the create-time pack root recorded for this
// workspace's sandbox, or "" when no marker exists.
func ReadSandboxPackMarker(ws string) string {
	b, err := os.ReadFile(SandboxPackMarkerPath(ws))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// StalePackReattachWarning is the reminder `run` prints when RE-ATTACHING to a
// sandbox whose CREATE-TIME pack (the workspace marker) differs from the active
// pack. Comparing marker vs active is what makes it honest in BOTH directions:
// silent when the sandbox already carries the current pack, loud after `pack
// rm` (marker set, active empty) where the old sandbox still has the removed
// pack's bin/skills baked in. No marker => no warning; guessing from the active
// pack alone produced the old false positives. It deliberately says nothing
// about MCP — McpReattachWarning owns that claim precisely, via the receipt.
func StalePackReattachWarning(cfg *config.Config, o RunOpts, reattaching bool) string {
	if !reattaching || o.Replace {
		return ""
	}
	created := ReadSandboxPackMarker(o.Workspace)
	if created == "" {
		return ""
	}
	active := ""
	if root := pack.ActivePackRoot(cfg.Pack, o.Pack); root != "" {
		active = pack.CanonicalizePackRoot(root)
	}
	if created == active {
		return ""
	}
	if active == "" {
		return fmt.Sprintf("pix: re-attaching without --replace — this sandbox was created with pack %q (since detached); its bin/skills are still attached until you recreate: %s", created, RunReplaceCommand(o.Workspace))
	}
	return fmt.Sprintf("pix: re-attaching without --replace — this sandbox was created with pack %q, not the active pack %q; the active pack's bin/skills won't attach until you recreate: %s", created, active, RunReplaceCommand(o.Workspace))
}

// desiredMCPUniverse is the FULL set of MCP server names this invocation would
// preload at CREATE: cfg.MCP, the active/transient pack's integration servers,
// and any explicit --mcp. It is the read-only twin of ApplyPackToLaunch's
// pack-fold step, used ONLY for the reattach comparison: a re-attach never
// mounts a pack and must not trigger the mount/kit-synthesis side effects just
// to answer "what would this invocation want". A pack that fails to load
// degrades to cfg.MCP+o.MCP rather than blocking the comparison.
func desiredMCPUniverse(cfg *config.Config, o RunOpts) []string {
	names := append([]string(nil), cfg.MCP...)
	if root := pack.ActivePackRoot(cfg.Pack, o.Pack); root != "" {
		if p, err := pack.LoadPack(root); err == nil {
			names = append(names, pack.McpNames(p)...)
		}
	}
	names = append(names, o.MCP...)
	return mcp.AllPreloadedMCP(names)
}

// reattachGap is the one shape every MCP reattach warning takes: a reason, the
// per-name live-attach commands (`mcp load` attaches one server at a time, so
// N names need N commands), and the recreate path. Both remediations are
// always offered.
func reattachGap(reason string, names []string, ws string) string {
	cmds := make([]string, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, doctor.McpLoadCommand(n, ws))
	}
	return fmt.Sprintf("pix: re-attaching without --replace: %s. Attach live: %s. Or recreate with current context: %s",
		reason, strings.Join(cmds, "; "), RunReplaceCommand(ws))
}

// McpReattachWarning is `pix run`'s reattach honesty check: it compares the
// DESIRED MCP universe against the sandbox's own launcher receipt and warns,
// BEFORE reattaching, about any desired name the receipt cannot PROVE is
// attached (a positive preloaded/loaded claim is proof, anything else is a
// gap). It never auto-loads, only reports, and never mentions a receipted name
// that is no longer desired — that is legitimate history. An unresolvable state
// dir, an absent receipt, and an unverifiable one (corrupt / wrong schema /
// wrong sandbox identity) all mean the same honest thing: cannot verify.
func McpReattachWarning(cfg *config.Config, o RunOpts, reattaching bool) string {
	if !reattaching || o.Replace {
		return ""
	}
	desired := desiredMCPUniverse(cfg, o)
	if len(desired) == 0 {
		return ""
	}
	unverified := func(why string) string {
		return reattachGap(fmt.Sprintf("%s, so attachment for %s cannot be verified", why, strings.Join(desired, ", ")), desired, o.Workspace)
	}
	stateDir, err := workspace.MCPStateDirFn()
	if err != nil {
		return unverified(fmt.Sprintf("could not resolve local state (%v)", err))
	}
	receipt, rstatus, _ := workspace.ReadMCPReceipt(stateDir, o.Name)
	switch {
	case rstatus.Unverifiable():
		return unverified("this sandbox's MCP receipt is " + rstatus.String())
	case rstatus == workspace.MCPStateAbsent:
		return unverified("no MCP receipt for this sandbox")
	}
	var missing []string
	for _, name := range desired {
		if mcp.ReceiptClaim(receipt, rstatus, name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return reattachGap(strings.Join(missing, ", ")+" not proven attached to this sandbox (no receipted preload or load)", missing, o.Workspace)
}

// RunReplaceCommand returns the exact `pix run [WORKSPACE] --replace` recovery
// command, POSIX-shell-safe. Bare "pix run --replace" is only correct for the
// "." default; an EXPLICIT workspace must be echoed back verbatim, because
// omitting it would target whatever sandbox the CURRENT cwd derives — which
// can be a different sandbox than the one that just failed.
func RunReplaceCommand(ws string) string {
	if ws == "" || ws == "." {
		return "pix run --replace"
	}
	return "pix run " + sys.ShellQuote(ws) + " --replace"
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
