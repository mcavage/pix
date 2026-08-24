package uatenvmatrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// intendedPiInvocation is the exact "pi <flags>" argv a positively
// identified, name-based exec/reattach must carry once a native environment
// is live (docs/design/environments.md section 10.1/10.2, section 11 item
// 2): live skill trees, the exact session model, and the exact resume
// target via `--session`, in that order. Kit is DELIBERATELY absent: `--kit`
// is an sbx/create concern (docs/design/environments.md section 5), never a
// pi flag, and the create call already positively resolved it (see
// createOutputIdentifiesKit below) — passing it again into pi would be a
// fact this check never actually observed pi receiving. Resume is carried
// as `--session <id>`, an EXACT target, never `--resume` (a bare picker flag
// that takes no value and would silently drop the fixture's resume target
// entirely).
func intendedPiInvocation(f EnvironmentFixture) []string {
	args := []string{"pi"}
	for _, skill := range f.LiveSkills {
		args = append(args, "--skill", skill)
	}
	if f.Model != "" {
		args = append(args, "--model", f.Model)
	}
	if f.Resume != "" {
		args = append(args, "--session", f.Resume)
	}
	return args
}

// buildExecArgv composes the exact PRODUCTION `sbx exec` argv a positively-
// identified, name-based re-attach must receive: `exec -it <name> -- pi
// <skills> <model> <session>`. It is intentionally independent of both
// sandbox.ExecArgv (an L1 sibling this package must not import) and
// workflow/launch's real argv builder: this package proves the upstream
// contract from first principles, so an accidental agreement with the real
// builder is evidence, not a foregone conclusion.
//
// This is a PURE, pinned shape — this check never actually issues this
// exact `-it` command itself (see checkEnvironmentCreateThenExecInvocation's
// doc for why an interactive, non-terminating pi TUI cannot be treated as a
// transport probe). It exists so the exact facts a production reattach must
// carry are pinned and independently testable, and so the check's own
// deterministic probe (buildEchoProbeArgv) can assert it carries the exact
// SAME intended pi invocation this builder pins.
func buildExecArgv(f EnvironmentFixture) []string {
	args := []string{"exec", "-it", f.Name, "--"}
	return append(args, intendedPiInvocation(f)...)
}

// echoProbeScript is the terminating POSIX shell argv-echo probe this check
// hands to a real `sbx exec`: it never starts pi (no TUI, no API key
// needed), it only proves the exact argv sbx delivered into the sandbox by
// printing every positional argument it received, one per line, then
// exiting. This replaces treating an interactive, non-terminating pi TUI's
// bare process exit as transport proof — the read-only investigation's
// finding #1 and #4.
const echoProbeScript = `for a in "$@"; do printf '%s\n' "$a"; done`

// buildEchoProbeArgv composes the ACTUAL command this check issues through
// the injected Executor: non-TTY, name-based `sbx exec -i <name> --`
// (finding #4: the host daemon runs under a pty-less long-lived process, so
// `-it` is production's shape, never this check's), running a terminating
// POSIX shell that echoes back every one of intended's elements, one per
// line. Passing intended as POSITIONAL data (after `sh -c script sh`) proves
// name resolution and that every intended invocation facet arrived at the
// sandbox unchanged, without ever starting pi.
func buildEchoProbeArgv(f EnvironmentFixture, intended []string) []string {
	args := []string{"exec", "-i", f.Name, "--", "sh", "-c", echoProbeScript, "sh"}
	return append(args, intended...)
}

// expectedEchoProbeOutput is the exact stdout buildEchoProbeArgv's script
// must produce when every intended argv element survived transport
// unchanged: one element per line, in order.
func expectedEchoProbeOutput(intended []string) string {
	return strings.Join(intended, "\n") + "\n"
}

// echoProbeTimeout bounds the deterministic argv-echo probe itself (finding
// #5: a deterministic transport probe should terminate). It is a package
// variable, not a constant, so a test can inject a far smaller bound and
// prove the timeout/attribution branch in milliseconds rather than however
// long production actually waits — the same test-injectability pattern
// runningRowPollConfig uses below.
var echoProbeTimeout = 30 * time.Second

// runningRowPollConfig bounds checkEnvironmentCreateThenExecInvocation's
// post-create poll for a positively identified running row (finding #5:
// production design already says create -> poll for positively identified
// running instance -> exec; docs/design/environments.md section 10.1). It
// mirrors workflow/launch's own CreatePoll shape (Interval + Timeout)
// without importing it — this package's sibling L1 capabilities are never
// imported (matrix.go's package doc). Like echoProbeTimeout, it is a
// variable so a test can inject fast, deterministic bounds instead of
// waiting out the real production interval.
type pollConfig struct {
	Interval time.Duration
	Timeout  time.Duration
}

var runningRowPollConfig = pollConfig{
	Interval: 500 * time.Millisecond,
	Timeout:  30 * time.Second,
}

// runningRow is the minimal shape this package needs from one `sbx ls
// --json` row to decide "is this the exact fixture, currently running" —
// deliberately its own tiny parser rather than importing sandbox.ParseList
// (the L1 sibling this package must not import; matrix.go's package doc). It
// tolerates both the "status" and "state" keys sbx has been observed to use
// across versions, exactly the leniency cleanup.go's own fresh probe already
// affords by using a substring match rather than a hard schema.
type runningRow struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	State  string `json:"state"`
}

// findRunningRow parses lsOut (the raw `sbx ls --json` body, either a bare
// array or an object wrapped under "sandboxes") and reports whether it
// contains a schema-usable row for the exact name with a running status. A
// row this package cannot even parse into the minimal runningRow shape is
// skipped rather than failing the whole probe outright — the poll's caller
// treats "no running row yet" and "unparseable output" identically: keep
// polling until the timeout, never guess a row into existence.
func findRunningRow(lsOut, name string) (bool, error) {
	var raw any
	if err := json.Unmarshal([]byte(lsOut), &raw); err != nil {
		return false, fmt.Errorf("sbx ls --json: invalid JSON: %w", err)
	}
	var rows []any
	switch v := raw.(type) {
	case []any:
		rows = v
	case map[string]any:
		arr, ok := v["sandboxes"]
		if !ok {
			return false, fmt.Errorf("sbx ls --json: object missing %q", "sandboxes")
		}
		list, ok := arr.([]any)
		if !ok {
			return false, fmt.Errorf("sbx ls --json: %q is not an array", "sandboxes")
		}
		rows = list
	default:
		return false, fmt.Errorf("sbx ls --json: top level is neither a JSON array nor object")
	}
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		var row runningRow
		if err := json.Unmarshal(b, &row); err != nil {
			continue
		}
		if row.Name != name {
			continue
		}
		status := row.Status
		if status == "" {
			status = row.State
		}
		if strings.EqualFold(strings.TrimSpace(status), "running") {
			return true, nil
		}
	}
	return false, nil
}

// pollForRunningInstance is the bounded, deterministic poll finding #5
// requires between a positive create receipt and any exec: it issues fresh
// `sbx ls --json` probes, at cfg.Interval apart, until findRunningRow
// reports a schema-usable running row for name or cfg.Timeout elapses. It
// never execs on the caller's behalf — a caller that never sees a nil
// return from this function must never proceed to exec.
func pollForRunningInstance(ctx context.Context, lw io.Writer, executor Executor, env []string, dir, name string, cfg pollConfig) error {
	deadline := time.Now().Add(cfg.Timeout)
	attempt := 0
	for {
		attempt++
		probeArgs := []string{"ls", "--json"}
		fmt.Fprintf(lw, "poll[%d]: $ sbx %s\n", attempt, strings.Join(probeArgs, " "))
		out, errOut, err := executor.Run(ctx, "sbx", probeArgs, env, dir)
		fmt.Fprintf(lw, "poll[%d]: stdout: %s\npoll[%d]: stderr: %s\npoll[%d]: err: %v\n", attempt, out, attempt, errOut, attempt, err)
		if err == nil {
			running, parseErr := findRunningRow(out, name)
			if parseErr != nil {
				fmt.Fprintf(lw, "poll[%d]: %v\n", attempt, parseErr)
			} else if running {
				fmt.Fprintf(lw, "poll: positively identified running row for %s after %d attempt(s)\n", name, attempt)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("poll: timed out after %s waiting for a positively identified running row for %s (%d attempt(s))", cfg.Timeout, name, attempt)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("poll: context canceled waiting for a running row for %s: %w", name, ctx.Err())
		case <-time.After(cfg.Interval):
		}
	}
}

// createOutputIdentifiesKit reports whether createOut positively identified
// every kit fixture declared under its own `kits:` list — the
// generated-kit facet the read-only investigation's finding #2 says must
// never be silently dropped: `--kit` is proven from create's own resolution,
// so this check must actually observe that resolution in the create
// receipt, not merely assume it happened because create otherwise
// succeeded. A fixture that declares no kits at all has nothing to require
// here.
func createOutputIdentifiesKit(createOut string, f EnvironmentFixture) bool {
	for _, kit := range f.RelativeKits {
		if !strings.Contains(createOut, kit) {
			return false
		}
	}
	return true
}

// checkEnvironmentCreateThenExecInvocation is Story 0's first named check:
// create a native environment fixture with the candidate Pix custom agent
// (`agent: pix`), poll for a positively identified running instance, then
// prove name-based `sbx exec` receives the exact intended pi invocation the
// fixture's typed facts demand. AFTER that primary proof succeeds, it also
// runs a bounded interpolation observation phase
// (check_create_exec_interpolation.go) that closes the final E0.7
// host-evidence gap: AC-7 requires the exact observed `${VAR}`,
// `${VAR:-default}`, and undefined-variable behavior, and this is added
// INSIDE this same first named check rather than as a seventh check name,
// so CheckNames() stays at exactly six entries — never a command sbx derived itself from the
// environment path, and never merely "the exec command exited 0".
//
// A read-only deep investigation (see this file's helpers) found the prior
// version of this check invalid on its own terms, not the upstream contract
// disproved: it launched an interactive, non-terminating pi TUI under a
// pty-less long-lived daemon and treated bare process exit 0 as transport
// proof (`ERROR: inspect exec: context deadline exceeded` is exactly what a
// TUI that never exits produces there); it passed pi a `--kit` flag pi does
// not have (kit is an sbx/create concern); and it used `--resume` with a
// value, when `--resume` is a bare picker flag that takes none. This version
// corrects all three: kit is proven from create's own resolution
// (createOutputIdentifiesKit), the actual transport proof is a bounded,
// terminating, non-TTY argv-echo probe (buildEchoProbeArgv) rather than a
// TUI launch, and the exact resume target travels as `--session
// <id>` (intendedPiInvocation). It also inserts the bounded poll finding #5
// says was missing between create and exec (pollForRunningInstance).
//
// Every host command goes through the injected Executor, so this check needs
// no real `sbx` binary to run under `go test`: production wires the real
// execExecutor, tests inject a fake that records and answers deterministically.
func checkEnvironmentCreateThenExecInvocation(ctx context.Context, lw io.Writer, executor Executor, phaseDir string) (retErr error) {
	env := hostToolExecEnv()

	// AC-7 / the E0.7 unit require this check's own bounded artifact to
	// carry the exact observed sbx version — captured HERE, before any
	// fixture mutation, so a version probe failure fails the check outright
	// rather than being silently skipped once fixture state already changed.
	if _, err := probeSbxVersion(ctx, lw, executor, env, phaseDir); err != nil {
		return fmt.Errorf("sbx version probe: %w", err)
	}

	fixture := customAgentFixture()

	fixturePath, err := writeAuthoredFixture(phaseDir, "authored.sbxenv.yaml", fixture)
	if err != nil {
		return err
	}
	fmt.Fprintf(lw, "authored fixture written to %s\n", fixturePath)

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	// createErr is deliberately its own named variable, never reused for the
	// exec call below: cleanupCreatedFixture is gated on the CREATE's own
	// receipt, and the deferred closure captures createOut/createErr by
	// reference, so reusing the same variable name for a later call (as this
	// package once did) would have the exec call's outcome silently
	// overwrite the create's by the time the deferred cleanup actually runs.
	createOut, createErrOut, createErr := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, createErr)
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, fixture.Name, createOut, createErr); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()
	if createErr != nil {
		return fmt.Errorf("sbx env create: %w", createErr)
	}
	if !strings.Contains(createOut, fixture.Name) {
		return fmt.Errorf("sbx env create did not report the expected positively-identified instance name %q (stdout=%q)", fixture.Name, createOut)
	}
	if !createOutputIdentifiesKit(createOut, fixture) {
		return fmt.Errorf("sbx env create did not identify the declared kit(s) %v in its output (stdout=%q); the generated-kit facet must never be silently dropped, since pi itself is never told --kit", fixture.RelativeKits, createOut)
	}

	if err := pollForRunningInstance(ctx, lw, executor, env, phaseDir, fixture.Name, runningRowPollConfig); err != nil {
		return err
	}

	intended := intendedPiInvocation(fixture)
	probeArgs := buildEchoProbeArgv(fixture, intended)
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(probeArgs, " "))
	probeCtx, cancel := context.WithTimeout(ctx, echoProbeTimeout)
	defer cancel()
	probeOut, probeErrOut, probeErr := executor.Run(probeCtx, "sbx", probeArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", probeOut, probeErrOut, probeErr)
	if probeErr != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("name-based sbx exec argv-echo probe: timed out after %s waiting for the sandbox to echo the intended pi invocation back: %w", echoProbeTimeout, probeErr)
		}
		return fmt.Errorf("name-based sbx exec argv-echo probe: %w", probeErr)
	}
	want := expectedEchoProbeOutput(intended)
	if probeOut != want {
		return fmt.Errorf("name-based sbx exec argv-echo probe did not echo the exact intended pi invocation unchanged: got %q, want %q", probeOut, want)
	}
	fmt.Fprintf(lw, "argv-echo probe confirmed all %d intended pi invocation facet(s) arrived unchanged\n", len(intended))

	// AC-7 / E0.7: the interpolation observation phase runs only after the
	// primary create/exec proof above has already succeeded, and never as a
	// seventh named check (checks.go still registers exactly six).
	if err := observeDefinedDefaultInterpolation(ctx, lw, executor, phaseDir); err != nil {
		return err
	}
	if err := observeUndefinedVariableBehavior(ctx, lw, executor, phaseDir); err != nil {
		return err
	}

	return nil
}
