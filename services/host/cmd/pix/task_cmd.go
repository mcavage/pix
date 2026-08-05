// task_cmd.go — the `pix task` verb tree (new|run|ls|path|rm). Domain logic
// (naming, clone/worktree mechanism, the git-hygiene guard) lives in
// pix/host/workflow/task; launching re-enters `pix run` with an explicit
// --name and the checkout as the workspace, so there is no task-owned
// sandbox-lifecycle code here.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/workflow/launch"
	"pix/host/workflow/task"
	"pix/host/workspace"
	"strings"
)

const taskDescription = `Run parallel tasks on one repo: each task is a checkout (clone by default,
or a linked worktree with --worktree) with its own branch, so tasks never
collide. 'rm' persists the branch back into the main repo before the
checkout goes away, and refuses a dirty/unpushed/live one without --force.
'pix run --task NAME' is a shorthand for 'pix task run NAME'.`

func (c *taskCmd) Help() string { return taskDescription }

// taskCmd is the verb tree; `list`/`remove` are kong aliases.
type taskCmd struct {
	Usage taskUsageCmd `cmd:"" default:"1" hidden:"" help:"Bare 'pix task' prints the group's usage."`
	New   taskNewCmd   `cmd:"" help:"Create + launch a new task checkout."`
	Run   taskRunCmd   `cmd:"" help:"(Re)launch an existing task's sandbox."`
	Ls    taskLsCmd    `cmd:"" aliases:"list" help:"Tasks, branch, git + sandbox state."`
	Path  taskPathCmd  `cmd:"" help:"Print the task's checkout dir (for cd)."`
	Rm    taskRmCmd    `cmd:"" aliases:"remove" help:"Tear down sandbox + checkout (guarded)."`
}

// taskUsageCmd is what bare `pix task` selects: the group's usage, exit 0.
type taskUsageCmd struct{}

func (c *taskUsageCmd) Run(d *cli.Deps) error {
	dispatch([]string{"task", "--help"}, d)
	return nil
}

func taskMainroot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return task.ResolveMainroot(cwd)
}

// taskRepo is what every subcommand starts with: the main checkout this task
// tree hangs off, plus the state root its metadata lives in. Not being in a
// git repo is the one failure they all report identically.
func taskRepo() (mainroot, stateRoot string, err error) {
	mainroot, err = taskMainroot()
	if err != nil {
		return "", "", fmt.Errorf("not a git repository: %w", err)
	}
	return mainroot, workspace.TaskStateRoot(), nil
}

func taskProbe() func(string) task.SandboxDisposition {
	env := defaultShellEnv()
	return func(name string) task.SandboxDisposition {
		switch launch.ProbeTaskSandbox(env, name) {
		case launch.SbxAbsent:
			return task.SandboxAbsent
		case launch.SbxRunning:
			return task.SandboxRunning
		case launch.SbxStopped:
			return task.SandboxStopped
		default:
			return task.SandboxUnknown
		}
	}
}

// taskNewCmd creates + launches a new task checkout.
type taskNewCmd struct {
	Name        string   `arg:"" help:"Task name."`
	From        string   `help:"Branch or ref to create the task from."`
	Worktree    bool     `help:"Use a linked worktree instead of a clone."`
	Clone       bool     `help:"Use a local clone (default)."`
	Passthrough []string `arg:"" optional:"" passthrough:"" help:"Args after -- forwarded to the launched pi session."`
}

func (c *taskNewCmd) Run(d *cli.Deps) error { return taskNew(d, c) }

// taskNewPassthrough splits kong's passthrough arg (which always includes the
// literal "--" it matched on) into the pi-args tail, rejecting a bare extra
// positional that arrived without one — kong itself accepts that (passthrough
// is exactly for a trailing positional), so the rejection is this guard's job.
func taskNewPassthrough(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] != "--" {
		return nil, cli.Usagef("unexpected extra argument %q (use -- for pi args)", raw[0])
	}
	return raw[1:], nil
}

func taskNewMechanism(worktree bool) task.Mechanism {
	if worktree {
		return task.Worktree
	}
	return task.Clone
}

func taskNew(d *cli.Deps, c *taskNewCmd) error {
	passthrough, err := taskNewPassthrough(c.Passthrough)
	if err != nil {
		return err
	}
	mainroot, stateRoot, err := taskRepo()
	if err != nil {
		return err
	}
	if task.HasSubmodules(mainroot) {
		fmt.Fprintln(d.Err, "note: submodules are not auto-initialized. "+
			"Run `git submodule update --init` in the sandbox and add submodule remotes to the network allowlist.")
	}
	m, err := task.New(task.NewOptions{
		StateRoot: stateRoot, Mainroot: mainroot,
		Name: c.Name, Ref: c.From, Mechanism: taskNewMechanism(c.Worktree),
	})
	if err != nil {
		return err
	}
	co, err := task.Path(stateRoot, mainroot, c.Name)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Err, "pix: task %q ready at %s (branch %s, sandbox %s)\n", c.Name, co, m.Branch, m.Sandbox)
	runArgv := []string{co, "--name", m.Sandbox}
	if len(passthrough) > 0 {
		runArgv = append(append(runArgv, "--"), passthrough...)
	}
	return dispatchRun(d, runArgv)
}

// resolveTaskRunArgv resolves an existing task NAME to the argv `pix run`
// needs to (re)launch its sandbox. Shared with run_cmd.go's `--task`
// shorthand (a different argv, outside this migration), so it stays
// argv-in/argv-out rather than a kong struct.
func resolveTaskRunArgv(name string, rest []string) ([]string, error) {
	co, sandboxName, err := resolveTaskTarget(name)
	if err != nil {
		return nil, err
	}
	return append([]string{co, "--name", sandboxName}, rest...), nil
}

// resolveTaskTarget resolves an existing task NAME to the two facts a launch
// needs: its checkout directory and its sandbox name. Shared with `pix run
// --task`, which fills them straight into RunOpts.
func resolveTaskTarget(name string) (dir, sandboxName string, err error) {
	mainroot, stateRoot, err := taskRepo()
	if err != nil {
		return "", "", err
	}
	co, m, err := task.Resolve(stateRoot, mainroot, name)
	if err != nil {
		return "", "", err
	}
	return co, m.Sandbox, nil
}

// taskRunCmd (re)launches an existing task's sandbox. Rest forwards
// VERBATIM, including a literal leading "--" — that is what `pix run`'s own
// argv parser expects to introduce its pi passthrough.
type taskRunCmd struct {
	Name string   `arg:"" help:"Task to (re)launch."`
	Rest []string `arg:"" optional:"" passthrough:"" help:"Forwarded to 'pix run' as-is."`
}

func (c *taskRunCmd) Run(d *cli.Deps) error {
	runArgv, err := resolveTaskRunArgv(c.Name, c.Rest)
	if err != nil {
		return err
	}
	return dispatchRun(d, runArgv)
}

type taskListRow struct {
	Name          string   `json:"name"`
	Branch        string   `json:"branch"`
	Mechanism     string   `json:"mechanism"`
	Sandbox       string   `json:"sandbox"`
	SandboxState  string   `json:"sandbox_state"`
	Dirty         bool     `json:"dirty"`
	Untracked     bool     `json:"untracked"`
	Unrecoverable int      `json:"unrecoverable_commits"`
	WouldRefuse   bool     `json:"would_refuse_rm"`
	Reasons       []string `json:"reasons,omitempty"`
	Path          string   `json:"path"`
	Unreadable    string   `json:"unreadable,omitempty"`
}

type taskLsCmd struct {
	JSON bool `help:"Emit machine-readable JSON instead of a table."`
}

func (c *taskLsCmd) Run(d *cli.Deps) error { return taskLs(d, c.JSON) }

func taskLs(d *cli.Deps, jsonOut bool) error {
	mainroot, stateRoot, err := taskRepo()
	if err != nil {
		return err
	}
	names, err := task.SandboxNames(stateRoot, mainroot)
	if err != nil {
		return err
	}
	probe := taskProbe()
	dispositions := make(map[string]task.SandboxDisposition, len(names))
	for _, n := range names {
		dispositions[n] = probe(n)
	}
	entries, err := task.List(stateRoot, mainroot, dispositions)
	if err != nil {
		return err
	}
	var rows []taskListRow
	for _, e := range entries {
		co, _ := task.Path(stateRoot, mainroot, e.Meta.Name)
		if e.Unreadable != "" {
			rows = append(rows, taskListRow{Name: e.Meta.Name, Unreadable: e.Unreadable, WouldRefuse: true})
			continue
		}
		rows = append(rows, taskListRow{
			Name: e.Meta.Name, Branch: e.Meta.Branch, Mechanism: string(e.Meta.Mechanism),
			Sandbox: e.Meta.Sandbox, SandboxState: e.Sandbox.String(),
			Dirty: e.Git.Dirty, Untracked: e.Git.Untracked, Unrecoverable: e.Git.Unrecoverable,
			WouldRefuse: e.WouldRefuse, Reasons: e.Reasons, Path: co,
		})
	}
	if jsonOut {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Fprintln(d.Out, string(b))
		return nil
	}
	if len(rows) == 0 {
		fmt.Fprintln(d.Out, "No tasks for this repo. Start one with `pix task new <name>`.")
		return nil
	}
	for _, r := range rows {
		if r.Unreadable != "" {
			fmt.Fprintf(d.Out, "%s\t(unreadable metadata: %s)\n", r.Name, r.Unreadable)
			continue
		}
		flag := ""
		if r.WouldRefuse {
			flag = " [rm would refuse: " + strings.Join(r.Reasons, "; ") + "]"
		}
		fmt.Fprintf(d.Out, "%s\t%s\t%s\t%s%s\n", r.Name, r.Branch, r.Mechanism, r.SandboxState, flag)
	}
	return nil
}

type taskPathCmd struct {
	Name string `arg:"" help:"Task whose checkout dir to print."`
}

func (c *taskPathCmd) Run(d *cli.Deps) error { return taskPath(d, c.Name) }

func taskPath(d *cli.Deps, name string) error {
	mainroot, stateRoot, err := taskRepo()
	if err != nil {
		return err
	}
	co, err := task.Path(stateRoot, mainroot, name)
	if err != nil {
		return err
	}
	fmt.Fprintln(d.Out, co)
	return nil
}

type taskRmCmd struct {
	Name  string `arg:"" help:"Task to remove."`
	Force bool   `help:"Override GIT-hygiene refusals (never a live sandbox)."`
}

func (c *taskRmCmd) Run(d *cli.Deps) error { return taskRm(d, c.Name, c.Force) }

func taskRm(d *cli.Deps, name string, force bool) error {
	mainroot, stateRoot, err := taskRepo()
	if err != nil {
		return err
	}
	co, m, err := task.Resolve(stateRoot, mainroot, name)
	if err != nil {
		return err
	}
	git := task.GatherGitState(m.Mechanism, mainroot, co)
	disposition := taskProbe()(m.Sandbox)
	reasons, ok := task.RemoveGuard(git, disposition, force)
	if !ok {
		fmt.Fprintf(d.Err, "pix task rm: refusing to remove %q: %s\n", name, strings.Join(reasons, "; "))
		if disposition == task.SandboxRunning {
			fmt.Fprintf(d.Err, "Stop it first: sbx stop %s\n", m.Sandbox)
		}
		return cli.SilentError{Code: 2}
	}
	// Persist the branch BEFORE anything is torn down, so a mid-teardown
	// failure below never loses work the guard just proved safe to drop.
	if err := task.PersistBranch(m.Mechanism, mainroot, co, m.Branch); err != nil {
		return err
	}
	if disposition != task.SandboxAbsent {
		if err := launch.RemovePixSandbox(defaultShellEnv(), d.Err, m.Sandbox); err != nil {
			return fmt.Errorf("could not remove sandbox %s; leaving the checkout intact: %w", m.Sandbox, err)
		}
	}
	if err := task.RemoveCheckout(m.Mechanism, mainroot, co); err != nil {
		return fmt.Errorf("sandbox removed, but the checkout could not be deleted: %w", err)
	}
	if err := task.Forget(stateRoot, mainroot, name); err != nil {
		fmt.Fprintf(d.Err, "pix task rm: warning: could not remove metadata: %v\n", err)
	}
	fmt.Fprintf(d.Out, "pix: removed task %q (branch %s persisted in %s)\n", name, m.Branch, mainroot)
	return nil
}
