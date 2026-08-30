package pixhome

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeRunner records every call and answers success unless told otherwise —
// the injectable command adapter a test uses instead of ever spawning a real
// git process.
type fakeRunner struct {
	calls []string
	err   error
}

func (f *fakeRunner) Run(dir, name string, args ...string) (string, error) {
	f.calls = append(f.calls, dir+" | "+name+" "+joinArgs(args))
	if f.err != nil {
		return "", f.err
	}
	// Mirror real `git init`'s side effect (creating dir/.git) so a caller that
	// reruns Init against THIS fake sees the same idempotent skip a real git
	// binary would produce, rather than re-invoking on every call.
	if name == "git" && len(args) > 0 && args[0] == "init" {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
			return "", err
		}
	}
	return "", nil
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func TestInit_CreatesLayoutAndGitRepo(t *testing.T) {
	home := filepath.Join(t.TempDir(), "pixhome")
	p := New(home)
	runner := &fakeRunner{}

	res, err := Init(p, runner)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if !res.CreatedHome {
		t.Error("InitResult.CreatedHome = false, want true for a fresh home")
	}
	if !res.CreatedGitRepo {
		t.Error("InitResult.CreatedGitRepo = false, want true for a fresh home")
	}
	if !res.CreatedREADME || !res.CreatedGitignore {
		t.Errorf("InitResult = %+v, want README and gitignore both created", res)
	}

	for _, dir := range []string{
		p.Skills, p.Agents, p.OutputStyles, p.Envs, p.Pi, p.PiThemes, p.Runtime,
		p.State, p.StateEffective, p.StateMemory, p.StateMemoryBackups,
		p.StateSandboxes, p.StateSessions, p.StateTasks, p.StateTrust, p.StateTrustEnvironments,
	} {
		if !isDir(dir) {
			t.Errorf("expected directory %s to exist after Init", dir)
		}
	}

	if len(runner.calls) != 1 {
		t.Fatalf("runner.calls = %v, want exactly one git init call", runner.calls)
	}
	want := home + " | git init -b main"
	if runner.calls[0] != want {
		t.Errorf("runner call = %q, want %q", runner.calls[0], want)
	}
}

func TestInit_ExactGitignoreContent(t *testing.T) {
	home := t.TempDir()
	p := New(home)
	if _, err := Init(p, &fakeRunner{}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	got, err := os.ReadFile(p.Gitignore)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	want := "/config.toml\n/secrets.env\n/runtime/\n/state/\n"
	if string(got) != want {
		t.Errorf(".gitignore content = %q, want exactly %q", got, want)
	}
}

func TestInit_Idempotent_PreservesExistingFilesAndSkipsGitInit(t *testing.T) {
	home := t.TempDir()
	p := New(home)

	// Pre-populate: a real repo already exists, and the user customized
	// README/.gitignore.
	if err := os.MkdirAll(p.Git, dirMode); err != nil {
		t.Fatal(err)
	}
	customREADME := "# My own notes\n"
	customGitignore := "/my-own-stuff\n"
	if err := os.WriteFile(p.README, []byte(customREADME), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Gitignore, []byte(customGitignore), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	res, err := Init(p, runner)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if res.CreatedGitRepo {
		t.Error("InitResult.CreatedGitRepo = true, want false: .git already existed")
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner.calls = %v, want none: git init must not run against an existing repo", runner.calls)
	}
	if res.CreatedREADME || res.CreatedGitignore {
		t.Errorf("InitResult = %+v, want neither README nor gitignore reported as created", res)
	}

	gotREADME, err := os.ReadFile(p.README)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotREADME) != customREADME {
		t.Errorf("README.md = %q, want the preserved custom content %q", gotREADME, customREADME)
	}
	gotGitignore, err := os.ReadFile(p.Gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotGitignore) != customGitignore {
		t.Errorf(".gitignore = %q, want the preserved custom content %q", gotGitignore, customGitignore)
	}
}

func TestInit_RerunIsNoopAndFillsInMissingPieces(t *testing.T) {
	home := t.TempDir()
	p := New(home)
	runner := &fakeRunner{}

	if _, err := Init(p, runner); err != nil {
		t.Fatalf("first Init() error = %v", err)
	}
	// Simulate a partial install: remove one directory a package manager
	// might not have been able to create.
	if err := os.RemoveAll(p.StateTasks); err != nil {
		t.Fatal(err)
	}

	res, err := Init(p, runner)
	if err != nil {
		t.Fatalf("second Init() error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Errorf("runner.calls = %v, want still exactly one git init call across both runs", runner.calls)
	}
	if res.CreatedGitRepo || res.CreatedREADME || res.CreatedGitignore {
		t.Errorf("second InitResult = %+v, want no repo/README/gitignore recreated", res)
	}
	if !isDir(p.StateTasks) {
		t.Error("expected the second Init to recreate the missing state/tasks directory")
	}
	found := false
	for _, d := range res.CreatedDirs {
		if d == filepath.Join("state", "tasks") {
			found = true
		}
	}
	if !found {
		t.Errorf("res.CreatedDirs = %v, want it to report state/tasks recreated", res.CreatedDirs)
	}
}

func TestInit_DirectoryAndFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on windows")
	}
	home := t.TempDir()
	p := New(home)
	if _, err := Init(p, &fakeRunner{}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, dir := range []string{p.Home, p.State, p.StateTrustEnvironments, p.Skills} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("mode of %s = %o, want 0700", dir, perm)
		}
	}
	for _, f := range []string{p.README, p.Gitignore} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("mode of %s = %o, want 0644", f, perm)
		}
	}
}

func TestInit_GitInitFailure_PropagatesError(t *testing.T) {
	home := t.TempDir()
	p := New(home)
	runner := &fakeRunner{err: os.ErrPermission}

	_, err := Init(p, runner)
	if err == nil {
		t.Fatal("Init() error = nil, want the runner's failure propagated")
	}
	if len(runner.calls) != 1 {
		t.Errorf("runner.calls = %v, want exactly one attempted git init", runner.calls)
	}
	// Directories/files created before the git step must still be in place —
	// Init is not required to unwind partial progress.
	if !isDir(p.Skills) {
		t.Error("expected skills/ to exist even though git init failed")
	}
}

func TestInit_NilRunnerFallsBackToDefault(t *testing.T) {
	// DefaultRunner shells out to a real `git`; assert only that passing nil
	// does not panic and resolves to *some* runner (git must exist in the
	// dev/CI image per AGENTS.md's toolchain list).
	home := t.TempDir()
	p := New(home)
	if _, err := Init(p, nil); err != nil {
		t.Fatalf("Init(p, nil) error = %v", err)
	}
	if !isDir(p.Git) {
		t.Error("expected a real git repository after Init(p, nil)")
	}
}
