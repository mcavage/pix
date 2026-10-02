package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/workflow/task"
)

func taskTestRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main", repo},
		{"-C", repo, "-c", "user.name=Pix Test", "-c", "user.email=pix@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(repo)
	return repo
}

func taskTestDeps(t *testing.T, home string) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte("#!/bin/sh\ncase \"$1\" in\n --version|version) echo 'sbx version 9.9.9' ;;\n *) exit 1 ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PIX_HOME", home)
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb}, &out, &errb
}

func TestTaskNewMissingModelDoesNotCreateCheckout(t *testing.T) {
	taskTestRepo(t)
	home := keylessEnvHome(t, "plain", "")
	d, out, errb := taskTestDeps(t, home)
	if code := dispatch([]string{"task", "new", "needs-model"}, d); code == 0 {
		t.Fatalf("task new unexpectedly succeeded: %s%s", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "task not created: no model provider") || strings.Contains(errb.String(), "ready at") {
		t.Fatalf("missing-model result was not a pre-create refusal: %s", errb.String())
	}
	mainroot, state, err := taskRepo()
	if err != nil {
		t.Fatal(err)
	}
	stackID, err := taskStackID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := task.Resolve(state, mainroot, "needs-model", stackID); err == nil {
		t.Fatal("task metadata exists after the provider preflight refused")
	}
	if _, err := os.Stat(filepath.Join(state, task.RepoDir(mainroot), "co", "needs-model")); !os.IsNotExist(err) {
		t.Fatalf("task checkout was reserved before the provider preflight: %v", err)
	}
}

func TestTaskNewKeylessEnvIsBoundToLaterTaskRuns(t *testing.T) {
	taskTestRepo(t)
	home := keylessEnvHome(t, "local", "schema = 1\n[models]\nmain = \"ollama/qwen3.5:9b\"\n")
	d, out, errb := taskTestDeps(t, home)
	dispatch([]string{"task", "new", "keyless", "--env", "local"}, d)
	if !strings.Contains(errb.String(), `task "keyless" ready at`) {
		t.Fatalf("the task was not created: %s%s", out.String(), errb.String())
	}
	if strings.Contains(errb.String(), providerInterviewText) {
		t.Fatalf("a keyless task asked for a personal provider key: %s", errb.String())
	}
	mainroot, state, err := taskRepo()
	if err != nil {
		t.Fatal(err)
	}
	stackID, err := taskStackID()
	if err != nil {
		t.Fatal(err)
	}
	_, meta, err := task.Resolve(state, mainroot, "keyless", stackID)
	if err != nil {
		t.Fatalf("resolve task: %v; output: %s%s", err, out.String(), errb.String())
	}
	if !strings.HasPrefix(meta.Sandbox, "pix-"+stackID+"-") {
		t.Fatalf("task sandbox is not stack scoped: %q", meta.Sandbox)
	}
	if meta.Env != "local" {
		t.Fatalf("task environment = %q, want local", meta.Env)
	}
	opts, err := (&runCmd{Task: "keyless"}).opts()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Env != "local" || opts.Name != meta.Sandbox {
		t.Fatalf("run --task lost its environment or sandbox: %+v", opts)
	}
}
