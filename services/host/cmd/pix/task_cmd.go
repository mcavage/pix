// task_cmd.go — the `pix task` verb tree (new|run|ls|path|rm). Domain logic
// (naming, clone/worktree mechanism, the git-hygiene guard) lives in
// pix/host/workflow/task; launching re-enters `pix run` with an explicit --name
// and the checkout as the workspace, so no sandbox-lifecycle code lives here.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/pixhome"
	"pix/host/workflow/launch"
	"pix/host/workflow/task"
	"strings"
)

const taskDescription = `Run parallel tasks on one repo: each task is a checkout (clone by default,
or a linked worktree with --worktree) with its own branch, so tasks never
collide. 'rm' persists the branch back into the main repo before the
checkout goes away, and refuses a dirty/unpushed/live one without --force.
'pix run --task NAME' is a shorthand for 'pix task run NAME'.`

func (c *taskCmd) Help() string { return taskDescription }

// taskCmd is the v2 four-verb tree: new | ls | path | rm
// (docs/design/pix-v2-surface.md §3.3). There is no separate 'task run':
// launch an existing task with 'pix run "$(pix task path NAME)"', or the
// 'pix run --task NAME' shorthand.
type taskCmd struct {
	Usage taskUsageCmd `cmd:"" default:"1" hidden:"" help:"Bare 'pix task' prints the group's usage."`
	New   taskNewCmd   `cmd:"" help:"Create + launch a new task checkout."`
	Ls    taskLsCmd    `cmd:"" help:"Tasks, branch, git + sandbox state."`
	Path  taskPathCmd  `cmd:"" help:"Print the task's checkout dir (for cd)."`
	Rm    taskRmCmd    `cmd:"" help:"Tear down sandbox + checkout (guarded)."`
}

// taskUsageCmd is what bare `pix task` selects: the group's usage, exit 0.
type taskUsageCmd struct{}

func (c *taskUsageCmd) Run(d *cli.Deps) error {
	dispatch([]string{"task", "--help"}, d)
	return nil
}

// taskRepo is what every subcommand starts with: the main checkout this task tree
// hangs off, plus the state root its metadata lives in. Not being in a git repo is
// the one failure they all report identically.
func taskRepo() (mainroot, stateRoot string, err error) {
	cwd, err := os.Getwd()
	if err == nil {
		mainroot, err = task.ResolveMainroot(cwd)
	}
	if err != nil {
		return "", "", fmt.Errorf("not a git repository: %w", err)
	}
	home, herr := pixhome.Resolve()
	if herr != nil {
		return "", "", herr
	}
	return mainroot, home.StateTasks, nil
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

func (c *taskNewCmd) Run(d *cli.Deps) error { return sbxAwareFail(d, taskNew(d, c)) }

// taskNewPassthrough splits kong's passthrough arg (which always includes the
// literal "--" it matched on) into the pi-args tail, rejecting a bare extra
// positional that arrived without one — kong accepts that, so refusing it is this
// guard's job.
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
	// Probe sbx BEFORE reserving a checkout: task.New below creates the
	// checkout dir, the branch, and the metadata file on disk, none of which
	// dispatchRun's own (later) sbx probe can undo. Without this, a missing
	// sbx left an orphan checkout+branch behind every time, discovered only
	// after `pix run` failed deep inside dispatchRun.
	if _, err := defaultShellEnv().LookPath("sbx"); err != nil {
		return launch.SbxUnavailableErr("create a task sandbox")
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
			Sandbox: e.Meta.Sandbox, SandboxState: e.Sandbox.String(), Dirty: e.Git.Dirty,
			Untracked: e.Git.Untracked, Unrecoverable: e.Git.Unrecoverable,
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
		switch disposition {
		case task.SandboxRunning:
			fmt.Fprintf(d.Err, "Stop it first: sbx stop %s\n", m.Sandbox)
		case task.SandboxUnknown:
			// "resolve, then retry" names no command a user can run. sbx entirely
			// absent gets the exact install fix (the SAME one ls/rm/doctor use);
			// sbx present but unable to answer gets the exact retry, since telling
			// someone to install a tool they already have is worse than useless.
			if _, lerr := defaultShellEnv().LookPath("sbx"); lerr != nil {
				fmt.Fprintf(d.Err, "%v\n", launch.SbxUnavailableErr("remove this task's sandbox"))
			} else {
				fmt.Fprintf(d.Err, "Run `sbx ls` to see why its state could not be read, then retry: pix task rm %s\n", name)
			}
		}
		return cli.SilentError{Code: 2}
	}
	// Persist the branch BEFORE anything is torn down, so a mid-teardown failure
	// never loses work the guard just proved safe to drop.
	if err := task.PersistBranch(m.Mechanism, mainroot, co, m.Branch); err != nil {
		return err
	}
	if disposition != task.SandboxAbsent {
		if err := launch.RemovePixSandbox(defaultShellEnv(), m.Sandbox); err != nil {
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
