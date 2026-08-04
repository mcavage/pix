// task_cmd.go — the `pix task` dispatcher (new|run|ls|path|rm).
//
// Story06 simplification: `harvest` and `gc` were retired (see retired.go);
// the launcher no longer decides how a task clone rejoins its parent repo or
// when it is disposable — that is git's job and the user's call. This file
// composes two things that stay deliberately separate:
//
//   - the checkout itself (naming, metadata, clone/worktree mechanism, the
//     git-hygiene guard) — owned by the L1 "pix/host/workflow/task" package,
//     which never imports launch/sandbox/lease;
//   - the sandbox — launching is the EXISTING `pix run` path (runRun, in
//     run_cmd.go) with an explicit --name and a task's checkout dir as the
//     workspace; probing and tearing it down reuse launch.ProbeTaskSandbox
//     and launch.RemovePixSandbox (sandbox.go, the same helper `pix rm`
//     uses). There is no separate task-owned sandbox-lifecycle code.
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

const taskUsage = `usage: pix task <new|run|ls|path|rm> [args]

Run parallel tasks on one repo. Each task is a checkout of the repo (a local
clone by default, or a linked worktree with --worktree) with its own branch,
so tasks never collide. Commits land on the task's own branch and, on
` + "`rm`" + `, are persisted back into the main repo before the checkout goes away.

  new  <name> [--from REF] [--worktree] [-- pi-args]   create + launch
  run  <name> [-- pi-args]                             (re)launch an existing task
  ls   [--json]                                        tasks, branch, git + sandbox state
  path <name>                                          print the task's checkout dir (for cd)
  rm   <name> [--force]                                tear down sandbox + checkout (guarded)

Checkouts live under $XDG_STATE_HOME/pix/tasks/<repo>/co/<name>, outside your
repo. ` + "`pix task rm`" + ` refuses to drop a running sandbox, uncommitted changes,
untracked files, or any commit unreachable from the main repo — ` + "`--force`" + `
overrides only that last set of GIT-hygiene reasons, never a live sandbox.
A shorthand ` + "`pix run --task NAME`" + ` is equivalent to ` + "`pix task run NAME`" + `.
`

func runTaskCmd(argv []string) {
	if len(argv) > 0 {
		retiredIfRetired("task", argv[0])
	}
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		fmt.Print(taskUsage)
		return
	}
	switch argv[0] {
	case "new":
		runTaskNew(argv[1:])
	case "run":
		runTaskRunVerb(argv[1:])
	case "ls", "list":
		runTaskLs(argv[1:])
	case "path":
		runTaskPathVerb(argv[1:])
	case "rm", "remove":
		runTaskRmVerb(argv[1:])
	default:
		// `pix task <name> path` (name-then-verb) reads naturally for
		// `cd "$(pix task foo path)"`; the canonical form is `task path <name>`.
		if len(argv) == 2 && argv[1] == "path" {
			runTaskPathVerb(argv[:1])
			return
		}
		fmt.Fprintf(os.Stderr, "pix task: unknown subcommand %q\n\n%s", argv[0], taskUsage)
		os.Exit(2)
	}
}

// taskMainroot resolves the repo the current directory belongs to.
func taskMainroot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return task.ResolveMainroot(cwd)
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

// ---------------------------------------------------------------------------
// task new
// ---------------------------------------------------------------------------

func parseTaskNewArgs(argv []string) (name, from string, mechanism task.Mechanism, passthrough []string, err error) {
	mechanism = task.Clone
	pre := argv
	for i, a := range argv {
		if a == "--" {
			pre = argv[:i]
			passthrough = append([]string(nil), argv[i+1:]...)
			break
		}
	}
	nameSet := false
	for i := 0; i < len(pre); i++ {
		a := pre[i]
		n := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			n = a[:eq]
		}
		switch {
		case n == "--from":
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				from = a[eq+1:]
			} else {
				if i+1 >= len(pre) {
					return "", "", "", nil, fmt.Errorf("flag --from needs a value")
				}
				i++
				from = pre[i]
			}
		case a == "--worktree":
			mechanism = task.Worktree
		case a == "--clone":
			mechanism = task.Clone
		case strings.HasPrefix(a, "-"):
			return "", "", "", nil, fmt.Errorf("unknown flag %q", a)
		default:
			if nameSet {
				return "", "", "", nil, fmt.Errorf("unexpected extra argument %q (use -- for pi args)", a)
			}
			name = a
			nameSet = true
		}
	}
	if strings.HasPrefix(from, "-") {
		return "", "", "", nil, fmt.Errorf("--from value %q must not begin with '-'", from)
	}
	if !nameSet || strings.TrimSpace(name) == "" {
		return "", "", "", nil, fmt.Errorf("a task name is required")
	}
	return name, from, mechanism, passthrough, nil
}

func runTaskNew(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	name, from, mechanism, passthrough, err := parseTaskNewArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task new: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}
	mainroot, err := taskMainroot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task new: not a git repository: %v\n", err)
		os.Exit(1)
	}
	if task.HasSubmodules(mainroot) {
		fmt.Fprintln(os.Stderr, "pix task new: note: submodules are not auto-initialized. "+
			"Run `git submodule update --init` in the sandbox and add submodule remotes to the network allowlist.")
	}
	m, err := task.New(task.NewOptions{
		StateRoot: workspace.TaskStateRoot(),
		Mainroot:  mainroot,
		Name:      name,
		Ref:       from,
		Mechanism: mechanism,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task new: %v\n", err)
		os.Exit(1)
	}
	co, err := task.Path(workspace.TaskStateRoot(), mainroot, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task new: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "pix: task %q ready at %s (branch %s, sandbox %s)\n", name, co, m.Branch, m.Sandbox)

	runArgv := []string{co, "--name", m.Sandbox}
	if len(passthrough) > 0 {
		runArgv = append(runArgv, "--")
		runArgv = append(runArgv, passthrough...)
	}
	runRun(runArgv)
}

// ---------------------------------------------------------------------------
// task run / --task NAME shorthand
// ---------------------------------------------------------------------------

// resolveTaskRunArgv resolves an existing task NAME to the argv `pix run`
// needs to (re)launch its sandbox: its checkout dir as the positional
// workspace, plus an explicit --name so `run` attaches to the SAME sandbox
// `task new` created (a bare directory would derive a different, colliding
// name). rest is appended after (e.g. a `-- pi-args` tail).
func resolveTaskRunArgv(name string, rest []string) ([]string, error) {
	mainroot, err := taskMainroot()
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	co, m, err := task.Resolve(workspace.TaskStateRoot(), mainroot, name)
	if err != nil {
		return nil, err
	}
	return append([]string{co, "--name", m.Sandbox}, rest...), nil
}

func runTaskRunVerb(argv []string) {
	if cli.WantsHelp(argv) || len(argv) == 0 {
		fmt.Print(taskUsage)
		if len(argv) == 0 {
			os.Exit(2)
		}
		return
	}
	name := argv[0]
	rest := argv[1:]
	runArgv, err := resolveTaskRunArgv(name, rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task run: %v\n", err)
		os.Exit(1)
	}
	runRun(runArgv)
}

// expandTaskFlag rewrites a leading `--task NAME` (or `--task=NAME`) in argv,
// before the `--` passthrough separator, into the resolved checkout + --name
// form resolveTaskRunArgv produces. ok=false means no --task flag was found
// and argv is returned unchanged.
func expandTaskFlag(argv []string) (out []string, ok bool, err error) {
	for i, a := range argv {
		if a == "--" {
			break
		}
		switch {
		case a == "--task":
			if i+1 >= len(argv) {
				return nil, true, fmt.Errorf("--task needs a NAME")
			}
			rest := append(append([]string(nil), argv[:i]...), argv[i+2:]...)
			out, err = resolveTaskRunArgv(argv[i+1], rest)
			return out, true, err
		case strings.HasPrefix(a, "--task="):
			rest := append(append([]string(nil), argv[:i]...), argv[i+1:]...)
			out, err = resolveTaskRunArgv(strings.TrimPrefix(a, "--task="), rest)
			return out, true, err
		}
	}
	return argv, false, nil
}

// ---------------------------------------------------------------------------
// task ls
// ---------------------------------------------------------------------------

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

func runTaskLs(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	jsonOut := false
	for _, a := range argv {
		if a == "--json" {
			jsonOut = true
		}
	}
	mainroot, err := taskMainroot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task ls: not a git repository: %v\n", err)
		os.Exit(1)
	}
	stateRoot := workspace.TaskStateRoot()
	entries, err := task.List(stateRoot, mainroot, taskProbe())
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task ls: %v\n", err)
		os.Exit(1)
	}
	var rows []taskListRow
	for _, e := range entries {
		co, _ := task.Path(stateRoot, mainroot, e.Meta.Name)
		if e.Unreadable != "" {
			rows = append(rows, taskListRow{Name: e.Meta.Name, Unreadable: e.Unreadable, WouldRefuse: true})
			continue
		}
		rows = append(rows, taskListRow{
			Name:          e.Meta.Name,
			Branch:        e.Meta.Branch,
			Mechanism:     string(e.Meta.Mechanism),
			Sandbox:       e.Meta.Sandbox,
			SandboxState:  e.Sandbox.String(),
			Dirty:         e.Git.Dirty,
			Untracked:     e.Git.Untracked,
			Unrecoverable: e.Git.Unrecoverable,
			WouldRefuse:   e.WouldRefuse,
			Reasons:       e.Reasons,
			Path:          co,
		})
	}
	if jsonOut {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return
	}
	if len(rows) == 0 {
		fmt.Println("No tasks for this repo. Start one with `pix task new <name>`.")
		return
	}
	for _, r := range rows {
		if r.Unreadable != "" {
			fmt.Printf("%s\t(unreadable metadata: %s)\n", r.Name, r.Unreadable)
			continue
		}
		flag := ""
		if r.WouldRefuse {
			flag = " [rm would refuse: " + strings.Join(r.Reasons, "; ") + "]"
		}
		fmt.Printf("%s\t%s\t%s\t%s%s\n", r.Name, r.Branch, r.Mechanism, r.SandboxState, flag)
	}
}

// ---------------------------------------------------------------------------
// task path
// ---------------------------------------------------------------------------

func runTaskPathVerb(argv []string) {
	if cli.WantsHelp(argv) || len(argv) != 1 {
		fmt.Print(taskUsage)
		if len(argv) != 1 {
			os.Exit(2)
		}
		return
	}
	mainroot, err := taskMainroot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task path: not a git repository: %v\n", err)
		os.Exit(1)
	}
	co, err := task.Path(workspace.TaskStateRoot(), mainroot, argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task path: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(co)
}

// ---------------------------------------------------------------------------
// task rm
// ---------------------------------------------------------------------------

func parseTaskRmArgs(argv []string) (name string, force bool, err error) {
	for _, a := range argv {
		switch {
		case a == "--force":
			force = true
		case strings.HasPrefix(a, "-"):
			return "", false, fmt.Errorf("unknown flag %q", a)
		default:
			if name != "" {
				return "", false, fmt.Errorf("unexpected extra argument %q", a)
			}
			name = a
		}
	}
	if name == "" {
		return "", false, fmt.Errorf("a task name is required")
	}
	return name, force, nil
}

func runTaskRmVerb(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	name, force, err := parseTaskRmArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}
	mainroot, err := taskMainroot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: not a git repository: %v\n", err)
		os.Exit(1)
	}
	stateRoot := workspace.TaskStateRoot()
	co, m, err := task.Resolve(stateRoot, mainroot, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: %v\n", err)
		os.Exit(1)
	}

	git := task.GatherGitState(m.Mechanism, mainroot, co)
	disposition := taskProbe()(m.Sandbox)
	reasons, ok := task.RemoveGuard(git, disposition, force)
	if !ok {
		fmt.Fprintf(os.Stderr, "pix task rm: refusing to remove %q: %s\n", name, strings.Join(reasons, "; "))
		if disposition == task.SandboxRunning {
			fmt.Fprintf(os.Stderr, "Stop it first: sbx stop %s\n", m.Sandbox)
		}
		os.Exit(2)
	}

	// Persist the branch into the main repo BEFORE anything is torn down, so
	// even a mid-teardown failure below never loses the work the guard just
	// proved is safe to drop from the checkout.
	if err := task.PersistBranch(m.Mechanism, mainroot, co, m.Branch); err != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: %v\n", err)
		os.Exit(1)
	}

	// Tear the sandbox down via the SAME helper `pix rm` uses (no duplicated
	// sandbox-lifecycle code here). Absent needs no removal call at all.
	if disposition != task.SandboxAbsent {
		if err := launch.RemovePixSandbox(defaultShellEnv(), m.Sandbox); err != nil {
			fmt.Fprintf(os.Stderr, "pix task rm: could not remove sandbox %s; leaving the checkout intact: %v\n", m.Sandbox, err)
			os.Exit(1)
		}
	}

	if err := task.RemoveCheckout(m.Mechanism, mainroot, co); err != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: sandbox removed, but the checkout could not be deleted: %v\n", err)
		os.Exit(1)
	}
	if err := task.Forget(stateRoot, mainroot, name); err != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: warning: could not remove metadata: %v\n", err)
	}
	fmt.Printf("pix: removed task %q (branch %s persisted in %s)\n", name, m.Branch, mainroot)
}
