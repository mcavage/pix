package task

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoKeyStableAndHex(t *testing.T) {
	dir := t.TempDir()
	k1 := RepoKey(dir)
	k2 := RepoKey(dir)
	if k1 != k2 {
		t.Errorf("RepoKey not stable: %q vs %q", k1, k2)
	}
	if len(k1) != 8 {
		t.Errorf("len = %d, want 8 (%q)", len(k1), k1)
	}
	if _, err := hex.DecodeString(k1); err != nil {
		t.Errorf("%q is not hex: %v", k1, err)
	}
	if other := RepoKey(filepath.Join(dir, "sub")); other == k1 {
		t.Error("a different path must produce a different key")
	}
}

func TestRepoLabel(t *testing.T) {
	if got := RepoLabel(filepath.Join("/x/my-api", ".git")); got != "my-api" {
		t.Errorf("RepoLabel = %q, want my-api", got)
	}
	long := "/x/" + strings.Repeat("z", 40)
	if got := RepoLabel(long); len(got) > MaxRepoLabelLen {
		t.Errorf("RepoLabel len = %d, want <= %d", len(got), MaxRepoLabelLen)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"fix-login": "fix-login",
		"fix login": "fix-login",
		"a/b":       "a-b",
		"":          "task",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("z", 100)
	got := SanitizeName(long)
	if len(got) > MaxNameLen {
		t.Errorf("len = %d, want <= %d", len(got), MaxNameLen)
	}
	for _, r := range got {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			t.Fatalf("unsafe rune %q in %q", r, got)
		}
	}
}

func TestBoundSandboxNameFits(t *testing.T) {
	if got := BoundSandboxName("api", "abcd1234", "fix"); got != "pix-t-api-abcd1234-fix" {
		t.Errorf("got %q", got)
	}
	longName := strings.Repeat("z", 89)
	got := BoundSandboxName("api", "abcd1234", longName)
	if len(got) > MaxSandboxNameLen {
		t.Errorf("len = %d, want <= %d: %q", len(got), MaxSandboxNameLen, got)
	}
	other := BoundSandboxName("api", "abcd1234", strings.Repeat("z", 89)+"y")
	if got == other {
		t.Error("two different overflowing names must not collide (hash tag)")
	}
	hugeLabel := strings.Repeat("l", 200)
	g2 := BoundSandboxName(hugeLabel, "abcd1234", longName)
	if len(g2) > MaxSandboxNameLen {
		t.Errorf("len = %d, want <= %d", len(g2), MaxSandboxNameLen)
	}
	if !strings.Contains(g2, "abcd1234") {
		t.Error("repokey must never be trimmed away")
	}
}

// Moved from the pre-Story06 cmd/pix/sandboxname_test.go (that file pinned
// launch.BoundSandboxName's PROFILE-suffixed formula; Story06 drops the
// profile dimension entirely -- see task.go's Meta doc -- so only the
// label/name/repokey boundaries are re-pinned here).
func TestBoundSandboxName_ExactFitBoundary(t *testing.T) {
	label := "repolabel12x" // exactly MaxRepoLabelLen (12)
	repokey := "abcd1234"
	// Fixed overhead = len("pix-t-")+"-"+repokey+"-" = 6+1+8+1 = 16; budget
	// left for label+name = 63-16 = 47; with label=12, name budget = 35.
	name35 := strings.Repeat("a", 35)
	want35 := "pix-t-repolabel12x-abcd1234-" + name35
	if got := BoundSandboxName(label, repokey, name35); got != want35 {
		t.Errorf("name len 35 (exact fit): got %q, want %q", got, want35)
	}
	if len(want35) != 63 {
		t.Fatalf("fixture arithmetic wrong: %d chars, want 63", len(want35))
	}
	name36 := strings.Repeat("a", 36)
	got := BoundSandboxName(label, repokey, name36)
	if len(got) != 63 {
		t.Errorf("one over the boundary: got %d chars, want exactly 63: %q", len(got), got)
	}
	if !strings.HasPrefix(got, "pix-t-repolabel12x-abcd1234-") {
		t.Errorf("got %q, want the label+repokey prefix intact", got)
	}
}

func TestBoundSandboxName_NameFloorThenLabelTrims_RepokeyNeverTouched(t *testing.T) {
	longLabel := "reallylonglabelthatoverflows12345" // 33 chars
	name60 := strings.Repeat("a", 60)
	got := BoundSandboxName(longLabel, "abcd1234", name60)
	if len(got) > MaxSandboxNameLen {
		t.Errorf("len = %d, want <= %d: %q", len(got), MaxSandboxNameLen, got)
	}
	if !strings.Contains(got, "abcd1234") {
		t.Errorf("repokey must never be trimmed: %q", got)
	}
	// The name hits its floor (hash-tagged) before the label is touched at all.
	if !strings.Contains(got, "-aaa") {
		t.Errorf("want the name trimmed to its floor before the label: %q", got)
	}
}

func TestBoundSandboxName_NeverExceedsCapAcrossAWideRange(t *testing.T) {
	repokey := "abcd1234"
	for _, ln := range []int{0, 1, 12, 13, 40, 41, 63, 64, 100} {
		label := strings.Repeat("l", ln)
		name := strings.Repeat("n", ln+1)
		got := BoundSandboxName(label, repokey, name)
		if len(got) > MaxSandboxNameLen {
			t.Errorf("label len=%d name len=%d: %d chars, exceeds %d: %q", ln, ln+1, len(got), MaxSandboxNameLen, got)
		}
		if !strings.Contains(got, repokey) {
			t.Errorf("label len=%d name len=%d: repokey missing: %q", ln, ln+1, got)
		}
	}
}

func TestPaths(t *testing.T) {
	co, meta := Paths("/state", "abcd1234", "fix-login")
	if co != "/state/abcd1234/co/fix-login" {
		t.Errorf("co = %q", co)
	}
	if meta != "/state/abcd1234/meta/fix-login.json" {
		t.Errorf("meta = %q", meta)
	}
}

func TestHardenMetaRejectsTamperedName(t *testing.T) {
	if _, err := HardenMeta(Meta{Name: "evil"}, "/main", "abcd1234", "work"); err == nil {
		t.Fatal("want an error when the stored name does not sanitize back to the filename")
	}
	m, err := HardenMeta(Meta{Name: "work", Branch: "pix/../../heads/main", Sandbox: "sneaky"}, "/main", "abcd1234", "work")
	if err != nil {
		t.Fatalf("HardenMeta: %v", err)
	}
	if m.Branch != "pix/work" {
		t.Errorf("Branch = %q, want the RE-DERIVED value, not the stored one", m.Branch)
	}
	if m.Sandbox != SandboxName(RepoLabel("/main"), "abcd1234", "work") {
		t.Errorf("Sandbox = %q, want the re-derived name", m.Sandbox)
	}
	if m.Mechanism != Clone {
		t.Errorf("Mechanism = %q, want the default Clone when unset", m.Mechanism)
	}
}

func TestStripURLUserinfo(t *testing.T) {
	if got := StripURLUserinfo("https://user:token@host/org/repo.git"); got != "https://host/org/repo.git" {
		t.Errorf("got %q", got)
	}
	if got := StripURLUserinfo("git@host:org/repo.git"); got != "git@host:org/repo.git" {
		t.Errorf("scp-style remote must pass through unchanged: got %q", got)
	}
}

func TestRemoveGuard(t *testing.T) {
	cases := []struct {
		name    string
		git     GitState
		sandbox SandboxDisposition
		force   bool
		wantOK  bool
	}{
		{"clean", GitState{}, SandboxAbsent, false, true},
		{"running blocks", GitState{}, SandboxRunning, false, false},
		{"unknown sandbox blocks", GitState{}, SandboxUnknown, false, false},
		{"stopped is safe", GitState{}, SandboxStopped, false, true},
		{"dirty blocks", GitState{Dirty: true}, SandboxAbsent, false, false},
		{"untracked blocks", GitState{Untracked: true}, SandboxAbsent, false, false},
		{"unrecoverable blocks", GitState{Unrecoverable: 2}, SandboxAbsent, false, false},
		{"unknown git blocks", GitState{Unknown: true}, SandboxAbsent, false, false},
		{"ahead alone (upstream) is safe", GitState{HasUpstream: true, Ahead: 3}, SandboxAbsent, false, true},
		{"force overrides dirty", GitState{Dirty: true}, SandboxAbsent, true, true},
		{"force overrides unrecoverable", GitState{Unrecoverable: 2}, SandboxAbsent, true, true},
		{"force NEVER overrides running", GitState{}, SandboxRunning, true, false},
		{"force NEVER overrides unknown sandbox", GitState{}, SandboxUnknown, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reasons, ok := RemoveGuard(c.git, c.sandbox, c.force)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v (reasons: %v)", ok, c.wantOK, reasons)
			}
		})
	}
}
