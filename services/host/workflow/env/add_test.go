package env

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/pixhome"
)

func writeValidSbxenv(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ── name derivation ─────────────────────────────────────────────────────

func TestDeriveNameFromGitURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/org/repo.git":       "repo",
		"https://github.com/org/repo":           "repo",
		"git@github.com:org/repo.git":           "repo",
		"ssh://git@host/path/to/repo.git":       "repo",
		"https://example.com/repo.git?x=1#frag": "repo",
		"https://example.com/Weird Name!!.git":  "Weird-Name",
	}
	for in, want := range cases {
		if got := deriveNameFromGitURL(in); got != want {
			t.Errorf("deriveNameFromGitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveNameFromLocalPath(t *testing.T) {
	if got := deriveNameFromLocalPath("/home/user/my-project"); got != "my-project" {
		t.Errorf("got %q", got)
	}
	if got := deriveNameFromLocalPath("/home/user/.hidden"); got != "hidden" {
		t.Errorf("leading dot must be stripped (name must start alnum), got %q", got)
	}
}

func TestSanitizeCandidateName_EmptyWhenNothingSafeSurvives(t *testing.T) {
	if got := sanitizeCandidateName("---..."); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := sanitizeCandidateName(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestLooksLikeGitURL(t *testing.T) {
	yes := []string{"https://github.com/o/r.git", "ssh://git@host/o/r", "git@github.com:o/r.git", "file:///tmp/r"}
	for _, s := range yes {
		if !looksLikeGitURL(s) {
			t.Errorf("looksLikeGitURL(%q) = false, want true", s)
		}
	}
	no := []string{"not a url", "/absolute/path", "relative/path"}
	for _, s := range no {
		if looksLikeGitURL(s) {
			t.Errorf("looksLikeGitURL(%q) = true, want false", s)
		}
	}
}

// ── Add: local directory ────────────────────────────────────────────────

func TestAdd_LocalDirectory_SymlinksToCanonicalPathAndDerivesName(t *testing.T) {
	home := pixhome.New(t.TempDir())
	src := filepath.Join(t.TempDir(), "my-env")
	writeValidSbxenv(t, src)

	res, err := Add(AddOptions{Home: home, Source: src})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Name != "my-env" || res.Kind != "local" {
		t.Fatalf("got %+v", res)
	}
	target := home.EnvironmentDir("my-env")
	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("Lstat target: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("target is not a symlink: %v", fi.Mode())
	}
	linkTarget, err := os.Readlink(target)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSrc, _ := filepath.EvalSymlinks(src)
	if linkTarget != canonicalSrc {
		t.Fatalf("symlink target = %q, want canonical %q", linkTarget, canonicalSrc)
	}
	if !filepath.IsAbs(linkTarget) {
		t.Fatalf("symlink target %q is not absolute", linkTarget)
	}
}

func TestAdd_LocalDirectory_ExplicitName(t *testing.T) {
	home := pixhome.New(t.TempDir())
	src := filepath.Join(t.TempDir(), "source-dir")
	writeValidSbxenv(t, src)

	res, err := Add(AddOptions{Home: home, Source: src, Name: "custom"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Name != "custom" {
		t.Fatalf("got name %q", res.Name)
	}
	if _, err := os.Lstat(home.EnvironmentDir("custom")); err != nil {
		t.Fatalf("expected envs/custom to exist: %v", err)
	}
}

func TestAdd_RelativeLocalPath_ResolvesAgainstCWD(t *testing.T) {
	home := pixhome.New(t.TempDir())
	parent := t.TempDir()
	src := filepath.Join(parent, "relenv")
	writeValidSbxenv(t, src)
	t.Chdir(parent)

	res, err := Add(AddOptions{Home: home, Source: "relenv"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Name != "relenv" {
		t.Fatalf("got %q", res.Name)
	}
}

// ── Add: refusals ───────────────────────────────────────────────────────

func TestAdd_RefusesWhenTargetAlreadyExists_DoesNotOverwrite(t *testing.T) {
	home := pixhome.New(t.TempDir())
	existing := home.EnvironmentDir("taken")
	writeValidSbxenv(t, existing)
	before, _ := os.ReadFile(filepath.Join(existing, ".sbxenv.yaml"))

	src := filepath.Join(t.TempDir(), "other")
	writeValidSbxenv(t, src)

	_, err := Add(AddOptions{Home: home, Source: src, Name: "taken"})
	if err == nil {
		t.Fatal("Add: want a refusal, got nil error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v, want an 'already exists' refusal", err)
	}
	var usage cli.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is not a UsageError: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(existing, ".sbxenv.yaml"))
	if string(before) != string(after) {
		t.Fatal("the pre-existing environment's content changed — Add must never overwrite")
	}
	fi, _ := os.Lstat(existing)
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the pre-existing plain directory became a symlink — Add must never replace")
	}
}

func TestAdd_RefusesNonexistentPathThatIsNotAGitURL(t *testing.T) {
	home := pixhome.New(t.TempDir())
	_, err := Add(AddOptions{Home: home, Source: filepath.Join(t.TempDir(), "does-not-exist")})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "neither an existing local directory nor a recognized git URL") {
		t.Fatalf("got %v", err)
	}
}

func TestAdd_RefusesEmptySource(t *testing.T) {
	home := pixhome.New(t.TempDir())
	if _, err := Add(AddOptions{Home: home, Source: "   "}); err == nil {
		t.Fatal("want an error for an empty source")
	}
}

func TestAdd_RefusesInvalidExplicitName(t *testing.T) {
	home := pixhome.New(t.TempDir())
	src := filepath.Join(t.TempDir(), "src")
	writeValidSbxenv(t, src)
	if _, err := Add(AddOptions{Home: home, Source: src, Name: "../escape"}); err == nil {
		t.Fatal("want an error for an invalid name")
	}
}

func TestAdd_RefusesWhenNoSafeNameCanBeDerived(t *testing.T) {
	home := pixhome.New(t.TempDir())
	parent := t.TempDir()
	src := filepath.Join(parent, "---")
	writeValidSbxenv(t, src)
	_, err := Add(AddOptions{Home: home, Source: src})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "could not derive a safe environment name") {
		t.Fatalf("got %v", err)
	}
}

func TestAdd_RefusesLinkingPixHomeItself(t *testing.T) {
	homeDir := t.TempDir()
	home := pixhome.New(homeDir)
	if _, err := Add(AddOptions{Home: home, Source: homeDir, Name: "loop"}); err == nil {
		t.Fatal("want a refusal to link PIX_HOME itself")
	}
	if _, err := os.Lstat(home.EnvironmentDir("loop")); !os.IsNotExist(err) {
		t.Fatal("no target should have been created")
	}
}

// ── Add: missing/invalid .sbxenv.yaml -> cleanup ────────────────────────

func TestAdd_LocalDirectory_MissingSbxenv_CleansUpTheSymlinkOnly(t *testing.T) {
	home := pixhome.New(t.TempDir())
	src := t.TempDir() // no .sbxenv.yaml written

	_, err := Add(AddOptions{Home: home, Source: src, Name: "bad"})
	if err == nil {
		t.Fatal("want an error for a missing .sbxenv.yaml")
	}
	if !strings.Contains(err.Error(), "did not pass validation") {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Lstat(home.EnvironmentDir("bad")); !os.IsNotExist(statErr) {
		t.Fatal("the symlink this call created must be removed on validation failure")
	}
	// The source directory itself must survive untouched.
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("the source directory must never be removed: %v", statErr)
	}
}

func TestAdd_LocalDirectory_InvalidSbxenv_CleansUp(t *testing.T) {
	home := pixhome.New(t.TempDir())
	src := filepath.Join(t.TempDir(), "invalid-src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".sbxenv.yaml"), []byte("not: [valid: yaml::"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Add(AddOptions{Home: home, Source: src, Name: "brokenenv"})
	if err == nil {
		t.Fatal("want an error for invalid .sbxenv.yaml")
	}
	if _, statErr := os.Lstat(home.EnvironmentDir("brokenenv")); !os.IsNotExist(statErr) {
		t.Fatal("the symlink must be removed on validation failure")
	}
}

// ── Add: git clone (fake runner, no network) ────────────────────────────

func TestAdd_GitURL_ClonesAndDerivesNameFromRepo(t *testing.T) {
	home := pixhome.New(t.TempDir())
	orig := GitClone
	defer func() { GitClone = orig }()

	var gotURL, gotDest string
	GitClone = func(url, dest string) (string, error) {
		gotURL, gotDest = url, dest
		writeValidSbxenv(t, dest)
		return "", nil
	}

	res, err := Add(AddOptions{Home: home, Source: "https://example.com/org/myrepo.git"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Name != "myrepo" || res.Kind != "git" {
		t.Fatalf("got %+v", res)
	}
	if gotURL != "https://example.com/org/myrepo.git" {
		t.Fatalf("clone called with url %q", gotURL)
	}
	if gotDest != home.EnvironmentDir("myrepo") {
		t.Fatalf("clone called with dest %q", gotDest)
	}
}

func TestAdd_GitURL_CloneFailure_RemovesPartialTarget(t *testing.T) {
	home := pixhome.New(t.TempDir())
	orig := GitClone
	defer func() { GitClone = orig }()

	GitClone = func(url, dest string) (string, error) {
		// Simulate a partial clone leaving files behind before failing.
		_ = os.MkdirAll(dest, 0o700)
		_ = os.WriteFile(filepath.Join(dest, "partial"), []byte("x"), 0o600)
		return "fatal: could not read from remote repository", errAddTestClone
	}

	_, err := Add(AddOptions{Home: home, Source: "https://example.com/org/failrepo.git"})
	if err == nil {
		t.Fatal("want an error")
	}
	if _, statErr := os.Lstat(home.EnvironmentDir("failrepo")); !os.IsNotExist(statErr) {
		t.Fatal("a failed clone's partial directory must be removed")
	}
}

func TestAdd_GitURL_ClonedButMissingSbxenv_RemovesClonedTree(t *testing.T) {
	home := pixhome.New(t.TempDir())
	orig := GitClone
	defer func() { GitClone = orig }()

	GitClone = func(url, dest string) (string, error) {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return "", err
		}
		return "", os.WriteFile(filepath.Join(dest, "README.md"), []byte("no sbxenv here"), 0o600)
	}

	_, err := Add(AddOptions{Home: home, Source: "https://example.com/org/nosbx.git"})
	if err == nil {
		t.Fatal("want an error for a clone with no .sbxenv.yaml")
	}
	if _, statErr := os.Lstat(home.EnvironmentDir("nosbx")); !os.IsNotExist(statErr) {
		t.Fatal("the cloned tree must be removed when it fails validation")
	}
}

func TestAdd_GitURL_ExplicitName(t *testing.T) {
	home := pixhome.New(t.TempDir())
	orig := GitClone
	defer func() { GitClone = orig }()
	GitClone = func(url, dest string) (string, error) {
		writeValidSbxenv(t, dest)
		return "", nil
	}

	res, err := Add(AddOptions{Home: home, Source: "git@github.com:org/repo.git", Name: "picked"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Name != "picked" {
		t.Fatalf("got %q", res.Name)
	}
}

// errAddTestClone is a stand-in git-clone failure for tests.
type addTestCloneErr struct{}

func (addTestCloneErr) Error() string { return "clone failed" }

var errAddTestClone error = addTestCloneErr{}
