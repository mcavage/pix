package sandbox

import "fmt"

// CreateOpts describes a create request. This package is argv-only: it
// never execs anything, so Image/Command are exactly the pieces needed to
// compose argv, not a full container spec.
type CreateOpts struct {
	Name    string   // required: the sandbox/container name (e.g. from Name)
	Image   string   // required: image ref to create from
	TTY     bool     // true -> "-it" (interactive foreground use); false -> "-i"
	Command []string // optional: command + args to run instead of the image default
}

// ExecOpts describes a request to exec into an ALREADY-RUNNING sandbox.
type ExecOpts struct {
	Name    string   // required: the sandbox/container name to exec into
	TTY     bool     // true -> "-it"; false -> "-i"
	Command []string // required: what to run (exec has no "default command")
}

// CreateArgv composes the argv for a create (everything after the program
// name, mirroring workflow/launch.BuildSbxArgs' own convention). TTY=true
// emits the combined "-it" flag; TTY=false emits "-i" so piped/scripted
// callers never block on a pty that isn't there.
func CreateArgv(o CreateOpts) ([]string, error) {
	if o.Name == "" {
		return nil, fmt.Errorf("sandbox: create requires a name")
	}
	if o.Image == "" {
		return nil, fmt.Errorf("sandbox: create requires an image")
	}
	args := []string{"create", "--name", o.Name}
	args = append(args, ttyFlag(o.TTY))
	args = append(args, o.Image)
	args = append(args, o.Command...)
	return args, nil
}

// ExecArgv composes the argv for exec-ing into name. A command is required —
// unlike create, exec has no implicit default to fall back on.
//
// The command is ALWAYS placed after a literal `--` separator
// (docs/design/pix-v2-architecture.md §6.3: "The command after `--` is
// inside the sandbox"). Without it, the first in-sandbox flag — `--model`,
// `--resume` — is ambiguous: sbx may claim it as its own. The separator is
// unconditional so every attach uses the byte-identical shape, rather than
// one argv for a bare entrypoint and another for an entrypoint that happens
// to carry options this session.
func ExecArgv(o ExecOpts) ([]string, error) {
	if o.Name == "" {
		return nil, fmt.Errorf("sandbox: exec requires a name")
	}
	if len(o.Command) == 0 {
		return nil, fmt.Errorf("sandbox: exec requires a command")
	}
	args := []string{"exec", ttyFlag(o.TTY), o.Name, "--"}
	args = append(args, o.Command...)
	return args, nil
}

func ttyFlag(tty bool) string {
	if tty {
		return "-it"
	}
	return "-i"
}

// Decision is PlanLaunch's outcome: which argv-builder it used, if any.
type Decision int

const (
	// DecisionNone is returned only alongside a non-nil error: never treat a
	// zero Decision as "nothing to do", always check the error first.
	DecisionNone Decision = iota
	DecisionCreate
	DecisionExec
)

// PlanLaunch decides CREATE vs EXEC for name, given `found` — the result of
// looking name up in a ParseList'd listing (nil when absent from it) — and
// composes the corresponding argv:
//
//   - found == nil                       -> CREATE (nothing exists yet)
//   - found.IdentityVerified == false    -> refuse: this package cannot
//     vouch for what "running"/"stopped" even means on an unverified row
//     (see list.go's Schema posture doc); guessing here is exactly the
//     mistake workflow/launch.PlanSandboxLaunch already refuses to make for
//     doctor.SbxUnknown.
//   - found.State == StateRunning        -> EXEC (there's something to
//     attach to)
//   - found.State == StateStopped        -> refuse: `exec` cannot attach to
//     a stopped container, and this package does not plan a `start` (out of
//     scope — see doc.go)
//   - found.State == StateUnknown        -> refuse: present but its own
//     liveness could not be read, same fail-closed posture as Unknown above
//
// create and exec are the CALLER-supplied option sets for the two argv
// builders; PlanLaunch only decides WHICH one applies and returns its argv.
func PlanLaunch(found *Entry, create CreateOpts, exec ExecOpts) (Decision, []string, error) {
	if found == nil {
		argv, err := CreateArgv(create)
		if err != nil {
			return DecisionNone, nil, err
		}
		return DecisionCreate, argv, nil
	}
	if !found.IdentityVerified {
		return DecisionNone, nil, fmt.Errorf("sandbox: %q found but its listing row could not be schema-verified — refusing to plan create/exec blind", found.Name)
	}
	switch found.State {
	case StateRunning:
		argv, err := ExecArgv(exec)
		if err != nil {
			return DecisionNone, nil, err
		}
		return DecisionExec, argv, nil
	case StateStopped:
		return DecisionNone, nil, fmt.Errorf("sandbox: %q exists but is stopped — exec requires a running sandbox; start or recreate it first", found.Name)
	default: // StateUnknown
		return DecisionNone, nil, fmt.Errorf("sandbox: %q found but its state is unknown — refusing to guess create vs exec", found.Name)
	}
}
