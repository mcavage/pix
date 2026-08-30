package env

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/pixhome"
)

func homeWithEnvs(t *testing.T, names ...string) pixhome.Paths {
	t.Helper()
	home := pixhome.New(t.TempDir())
	for _, n := range names {
		dir := home.EnvironmentDir(n)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestResolveByDirectory(t *testing.T) {
	home := homeWithEnvs(t, "work", "home")
	sel, err := ResolveIn(home, "work")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	if sel.Root != home.EnvironmentDir("work") || sel.Symlinked {
		t.Fatalf("an ordinary directory resolves to itself, got %+v", sel)
	}
	if sel.SbxEnvPath() != filepath.Join(sel.Root, ".sbxenv.yaml") {
		t.Fatalf("unexpected native path %s", sel.SbxEnvPath())
	}

	var unknown *UnknownError
	_, err = ResolveIn(home, "nope")
	if !errors.As(err, &unknown) {
		t.Fatalf("an unknown name must be a typed refusal, got %v", err)
	}
	if len(unknown.Known) != 2 {
		t.Fatalf("the refusal must carry the known names, got %v", unknown.Known)
	}
}

func TestResolveFollowsExactlyOneSymlink(t *testing.T) {
	home := homeWithEnvs(t)
	external := filepath.Join(t.TempDir(), "external-env")
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home.Envs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, home.EnvironmentDir("ext")); err != nil {
		t.Fatal(err)
	}
	sel, err := ResolveIn(home, "ext")
	if err != nil {
		t.Fatalf("a symlinked environment must resolve: %v", err)
	}
	if sel.Root != external || !sel.Symlinked {
		t.Fatalf("resolution must use the one-hop target, got %+v", sel)
	}

	// A chain is refused, not chased.
	if err := os.Symlink(home.EnvironmentDir("ext"), home.EnvironmentDir("chain")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveIn(home, "chain"); err == nil {
		t.Fatalf("a symlink chain must be refused")
	}
	// A dangling symlink is refused.
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), home.EnvironmentDir("dangling")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveIn(home, "dangling"); err == nil {
		t.Fatalf("a dangling symlink must be refused")
	}
}

func TestResolveRefusesUnsafeNamesAndModes(t *testing.T) {
	home := homeWithEnvs(t, "work")
	for _, bad := range []string{"", ".", "..", "../etc", "a/b", "-leading"} {
		if _, err := ResolveIn(home, bad); err == nil {
			t.Fatalf("name %q must be refused", bad)
		}
	}
	if err := os.Chmod(home.EnvironmentDir("work"), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveIn(home, "work"); err == nil {
		t.Fatalf("a world-writable environment root must be refused")
	}
}

func TestListSkipsUnresolvableEntries(t *testing.T) {
	home := homeWithEnvs(t, "work", "home")
	if err := os.WriteFile(filepath.Join(home.Envs, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := List(home)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Name != "home" || got[1].Name != "work" {
		t.Fatalf("List must return the resolvable environments sorted, got %+v", got)
	}
}

func TestSelectPrecedence(t *testing.T) {
	if got := SelectName("explicit", "task", "default"); got != "explicit" {
		t.Fatalf("--env must win, got %q", got)
	}
	if got := SelectName("", "task", "default"); got != "task" {
		t.Fatalf("a task's recorded environment must beat the machine default, got %q", got)
	}
	if got := SelectName("", "", "default"); got != "default" {
		t.Fatalf("the machine default is the last resort, got %q", got)
	}
	if got := SelectName("", "", ""); got != "" {
		t.Fatalf("no selection is a legitimate empty answer, got %q", got)
	}
}

// TestSelectNeverFallsBackAfterInvalidExplicit is surface §3.1's exact
// rule: an explicit but unknown --env is an error, never a silent
// downgrade to the machine default.
func TestSelectNeverFallsBackAfterInvalidExplicit(t *testing.T) {
	home := homeWithEnvs(t, "work")
	_, ok, err := SelectIn(home, "typo", "", "work")
	if err == nil || ok {
		t.Fatalf("an unknown explicit --env must refuse, got ok=%v err=%v", ok, err)
	}
}
