package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/monitor/tui"
	"pix/host/workflow/launch"
	"pix/host/workflow/reset"
	"pix/host/workspace"
)

// --- Story 1: naming --------------------------------------------------------

func TestTaskRepoLabel(t *testing.T) {
	// A normal repo's git-common-dir ends in /.git; the label is the parent name.
	dir := t.TempDir()
	repo := filepath.Join(dir, "my-api")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := launch.TaskRepoLabel(filepath.Join(repo, ".git")); got != "my-api" {
		t.Errorf("normal repo label = %q, want my-api", got)
	}
	// A bare repo dir ending .git keeps its base minus the suffix.
	bare := filepath.Join(dir, "svc.git")
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := launch.TaskRepoLabel(bare); got != "svc" {
		t.Errorf("bare repo label = %q, want svc", got)
	}
}

func TestTaskRepoLabel_SanitizeCapEmpty(t *testing.T) {
	dir := t.TempDir()
	// Overflow is a plain truncation to the cap.
	long := filepath.Join(dir, strings.Repeat("a", 40))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	got := launch.TaskRepoLabel(long)
	if len(got) > launch.MaxRepoLabelLen {
		t.Errorf("label %q len %d > cap %d", got, len(got), launch.MaxRepoLabelLen)
	}
	// Only safe runes survive.
	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Fatalf("unsafe rune %q in label %q", r, got)
		}
	}
}

func TestBoundSandboxName_TrimsNameThenLabel(t *testing.T) {
	// Short inputs: composed verbatim.
	if got := launch.BoundSandboxName("api", "abcd1234", "fix", "work"); got != "pix-t-api-abcd1234-fix-work" {
		t.Errorf("short: %q", got)
	}
	// A long name is trimmed (hash-tagged) but the repokey is always intact and
	// the whole name stays within the bound.
	longName := strings.Repeat("z", 90)
	got := launch.BoundSandboxName("api", "abcd1234", longName, "")
	if len(got) > launch.MaxSandboxNameLen {
		t.Errorf("bounded name too long: %d (%q)", len(got), got)
	}
	if !strings.Contains(got, "-abcd1234-") {
		t.Errorf("repokey missing from %q", got)
	}
	if !strings.HasPrefix(got, "pix-t-api-abcd1234-") {
		t.Errorf("label/prefix mangled: %q", got)
	}
	// Two different long names must not collide (hash tag distinguishes them).
	other := launch.BoundSandboxName("api", "abcd1234", strings.Repeat("z", 89)+"y", "")
	if got == other {
		t.Errorf("distinct long names collided: %q", got)
	}
	// A huge label is trimmed AFTER the name floor, repokey still intact + bound ok.
	hugeLabel := strings.Repeat("L", 60)
	g2 := launch.BoundSandboxName(hugeLabel, "abcd1234", longName, "")
	if len(g2) > launch.MaxSandboxNameLen {
		t.Errorf("huge-label case too long: %d (%q)", len(g2), g2)
	}
	if !strings.Contains(g2, "-abcd1234-") {
		t.Errorf("repokey trimmed away in huge-label case: %q", g2)
	}
}

func TestExistingTaskLayouts_And_FindTaskLayout(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	repo := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	mainroot := filepath.Join(repo, ".git")
	newDir := launch.TaskRepoDir(mainroot)
	legacy := launch.TaskRepoKey(mainroot)
	writeMeta := func(dir, name string) {
		_, mp := launch.TaskPaths(dir, name)
		if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, mp, "{}\n")
	}

	// Nothing -> no layouts, launch.FindTaskLayout returns not-found (default new dir).
	if got := launch.ExistingTaskLayouts(mainroot); len(got) != 0 {
		t.Errorf("no dirs: layouts %+v, want none", got)
	}
	if lay, found, amb := launch.FindTaskLayout(mainroot, "x"); found || amb || lay.Dir != newDir {
		t.Errorf("not-found: lay=%+v found=%v amb=%v", lay, found, amb)
	}
	// A bare dir with NO meta/ must not read as a layout (e.g. a locks-only dir).
	if err := os.MkdirAll(filepath.Join(workspace.TaskStateRoot(), legacy, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := launch.ExistingTaskLayouts(mainroot); len(got) != 0 {
		t.Errorf("meta-less dir counted as layout: %+v", got)
	}
	// Legacy task present -> one legacy layout; launch.FindTaskLayout marks it legacy.
	writeMeta(legacy, "fix")
	lays := launch.ExistingTaskLayouts(mainroot)
	if len(lays) != 1 || lays[0].Dir != legacy || !lays[0].Legacy {
		t.Errorf("legacy only: %+v", lays)
	}
	if lay, found, _ := launch.FindTaskLayout(mainroot, "fix"); !found || !lay.Legacy {
		t.Errorf("find legacy: lay=%+v found=%v", lay, found)
	}
	// Add a NEW-layout task of the same name -> both layouts present; the same
	// name is now AMBIGUOUS and must be refused.
	writeMeta(newDir, "fix")
	if got := launch.ExistingTaskLayouts(mainroot); len(got) != 2 {
		t.Errorf("both Present: %+v, want 2", got)
	}
	if _, found, amb := launch.FindTaskLayout(mainroot, "fix"); !amb || found {
		t.Errorf("ambiguous name: found=%v amb=%v, want amb", found, amb)
	}
	// A name only in the new layout resolves unambiguously to new.
	writeMeta(newDir, "only-new")
	if lay, found, amb := launch.FindTaskLayout(mainroot, "only-new"); !found || amb || lay.Legacy {
		t.Errorf("new only: lay=%+v found=%v amb=%v", lay, found, amb)
	}
}

func TestLegacyTaskSandboxName(t *testing.T) {
	// Legacy tasks own the pre-label sandbox name (no repo label segment).
	if got := launch.LegacyTaskSandboxName("abcd1234", "fix", "default"); got != "pix-t-abcd1234-fix" {
		t.Errorf("legacy default: %q", got)
	}
	if got := launch.LegacyTaskSandboxName("abcd1234", "fix", "work"); got != "pix-t-abcd1234-fix" {
		t.Errorf("legacy work: %q", got)
	}
	// launch.HardenTaskMeta with legacy=true must derive the legacy name, not the labeled one.
	m, err := launch.HardenTaskMeta(launch.TaskMeta{Name: "fix", Profile: "default"}, "/main", "abcd1234", true, "fix")
	if err != nil {
		t.Fatal(err)
	}
	if m.Sandbox != "pix-t-abcd1234-fix" {
		t.Errorf("harden legacy sandbox = %q, want pre-label name", m.Sandbox)
	}
}

// --- Story 3: harvest -------------------------------------------------------

func TestIsArtifactPath(t *testing.T) {
	yes := []string{"README.md", "docs/design.md", "notes/scratch", "spec.prd", ".pix/artifacts/x", "deep/thing.txt"}
	no := []string{"main.go", "node_modules/pkg/readme.md", ".git/config", "vendor/x/doc.md", "image.png"}
	for _, p := range yes {
		if !launch.IsArtifactPath(p) {
			t.Errorf("launch.IsArtifactPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if launch.IsArtifactPath(p) {
			t.Errorf("launch.IsArtifactPath(%q) = true, want false", p)
		}
	}
}

func TestHarvestArtifacts_CopiesUncommittedDocsAndManifest(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/h", "HEAD")
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	// Untracked doc (should harvest), a committed doc (should NOT — leaves via git),
	// an ignored doc (should harvest), and a non-doc (should NOT).
	if err := os.MkdirAll(filepath.Join(co, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(co, "docs", "plan.md"), "scratch plan\n")
	writeFile(t, filepath.Join(co, "notes.md"), "loose notes\n")
	writeFile(t, filepath.Join(co, "main.go"), "package main\n")
	writeFile(t, filepath.Join(co, ".gitignore"), "secret.md\n")
	writeFile(t, filepath.Join(co, "secret.md"), "ignored doc\n")
	// A committed doc: add + commit so it is tracked/clean and excluded.
	writeFile(t, filepath.Join(co, "COMMITTED.md"), "tracked\n")
	tgit(t, co, "add", "COMMITTED.md")
	tgit(t, co, "commit", "-q", "-m", "doc")

	env := gitEnv(t, "", nil)
	meta := launch.TaskMeta{Name: "h", Repo: "main", Mainroot: main, Branch: "pix/h", Profile: "default"}
	dest, err := launch.HarvestArtifacts(env, io.Discard, meta, "main-abcd1234", co, "h")
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if dest == "" {
		t.Fatal("expected a destination dir, got empty")
	}
	must := []string{"docs/plan.md", "notes.md", "secret.md", "manifest.json"}
	for _, f := range must {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("expected harvested %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "main.go")); err == nil {
		t.Error("main.go should not be harvested")
	}
	if _, err := os.Stat(filepath.Join(dest, "COMMITTED.md")); err == nil {
		t.Error("committed doc should not be harvested")
	}
	// dest is under XDG_DATA_HOME, never the STATE tree.
	if !strings.HasPrefix(dest, filepath.Join(dataHome, "pix", "artifacts")) {
		t.Errorf("dest %q not under DATA_HOME artifacts", dest)
	}
	// Manifest names the task + repo + at least the harvested files.
	var man launch.HarvestManifest
	b, _ := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.Task != "h" || man.Repo != "main" || man.Branch != "pix/h" {
		t.Errorf("manifest fields wrong: %+v", man)
	}
	if len(man.Files) < 3 {
		t.Errorf("manifest files = %d, want >= 3", len(man.Files))
	}
}

func TestHarvestArtifacts_NothingToHarvest(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/e", "HEAD")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	meta := launch.TaskMeta{Name: "e", Mainroot: main, Branch: "pix/e", Profile: "default"}
	dest, err := launch.HarvestArtifacts(gitEnv(t, "", nil), io.Discard, meta, "r", co, "e")
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if dest != "" {
		t.Errorf("clean clone should harvest nothing, got %q", dest)
	}
}

// --- Story 2: gc ------------------------------------------------------------

func TestTaskAge_MaxOfCreatedAndMtime(t *testing.T) {
	co := t.TempDir() // fresh dir, mtime ~ now
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	// Created is old but the checkout was just touched: age tracks the recent
	// activity, not the stale created stamp.
	if age := launch.TaskAge(launch.TaskMeta{Created: old}, co); age > 24*time.Hour {
		t.Errorf("age = %v, want small (mtime recent)", age)
	}
	// No co dir + old created -> age reflects created.
	if age := launch.TaskAge(launch.TaskMeta{Created: old}, filepath.Join(co, "gone")); age < 20*24*time.Hour {
		t.Errorf("age = %v, want ~30d from created", age)
	}
	// Neither -> 0 (never over-age).
	if age := launch.TaskAge(launch.TaskMeta{}, filepath.Join(co, "gone")); age != 0 {
		t.Errorf("no timestamps: age = %v, want 0", age)
	}
}

func TestParseTaskGcArgs(t *testing.T) {
	o, err := launch.ParseTaskGcArgs(nil)
	if err != nil || o.Days != launch.TaskGCDefaultDays || o.ArtifactDays != launch.TaskArtifactDefaultDays || o.DryRun || o.NoHarvest {
		t.Fatalf("defaults wrong: %+v err=%v", o, err)
	}
	o, err = launch.ParseTaskGcArgs([]string{"--days", "14", "--dry-run", "--no-harvest", "--artifact-days=90"})
	if err != nil || o.Days != 14 || o.ArtifactDays != 90 || !o.DryRun || !o.NoHarvest {
		t.Fatalf("flags wrong: %+v err=%v", o, err)
	}
	if _, err := launch.ParseTaskGcArgs([]string{"--days", "-3"}); err == nil {
		t.Error("negative --days should error")
	}
	if _, err := launch.ParseTaskGcArgs([]string{"--bogus"}); err == nil {
		t.Error("unknown flag should error")
	}
	if _, err := launch.ParseTaskGcArgs([]string{"--days"}); err == nil {
		t.Error("missing value should error")
	}
}

func TestPruneArtifacts(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	repoDir := "proj-abcd1234"
	base := filepath.Join(workspace.TaskArtifactRoot(), repoDir, "task1")
	oldSnap := filepath.Join(base, "old")
	freshSnap := filepath.Join(base, "fresh")
	for _, d := range []string{oldSnap, freshSnap} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(oldSnap, "x.md"), "x\n")
	writeFile(t, filepath.Join(freshSnap, "y.md"), "y\n")
	// Backdate the old snapshot well past the retention window.
	past := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(oldSnap, past, past); err != nil {
		t.Fatal(err)
	}

	// Dry run counts but deletes nothing.
	if n := launch.PruneArtifacts(repoDir, 30, true); n != 1 {
		t.Errorf("dry-run pruned count = %d, want 1", n)
	}
	if _, err := os.Stat(oldSnap); err != nil {
		t.Error("dry-run must not delete")
	}
	// Real run deletes only the over-age snapshot.
	if n := launch.PruneArtifacts(repoDir, 30, false); n != 1 {
		t.Errorf("pruned count = %d, want 1", n)
	}
	if _, err := os.Stat(oldSnap); err == nil {
		t.Error("old snapshot should be gone")
	}
	if _, err := os.Stat(freshSnap); err != nil {
		t.Error("fresh snapshot should survive")
	}
	// days<=0 disables pruning.
	if n := launch.PruneArtifacts(repoDir, 0, false); n != 0 {
		t.Errorf("days=0 should prune nothing, got %d", n)
	}
}

func TestRunTaskGc_RemovesCleanSkipsDirty(t *testing.T) {
	main := newMainRepo(t)
	state := t.TempDir()
	data := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")

	env := gitEnv(t, "", nil) // sbx ls empty -> sandboxes absent -> guard passes on clean
	mainroot, err := launch.ResolveMainroot(env, main)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := launch.TaskRepoDir(mainroot)

	// Two tasks: "clean" (no changes) and "dirty" (uncommitted change). Both aged
	// past the threshold via an old Created and a backdated co mtime.
	mk := func(name string, dirty bool) {
		co, metaPath := launch.TaskPaths(repoDir, name)
		makeTaskClone(t, main, co, "pix/"+name, "HEAD")
		if dirty {
			writeFile(t, filepath.Join(co, "f"), "changed\n")
		}
		if err := launch.WriteTaskMeta(metaPath, launch.TaskMeta{
			Name: name, Mode: "localclone", Mainroot: mainroot,
			Branch: "pix/" + name, Profile: "default",
			Created: time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
		// Age = max(Created, mtime(co), mtime(.git/logs/HEAD), mtime(.git/index)),
		// so backdate the git activity markers too, not just the checkout root.
		past := time.Now().Add(-30 * 24 * time.Hour)
		for _, p := range []string{
			filepath.Join(co, ".git", "logs", "HEAD"),
			filepath.Join(co, ".git", "index"),
			co,
		} {
			_ = os.Chtimes(p, past, past)
		}
	}
	mk("clean", false)
	mk("dirty", true)

	t.Chdir(main)
	launch.RunTaskGc(env, nil)

	// clean gone, dirty kept.
	if _, mp := launch.TaskPaths(repoDir, "clean"); exists(mp) {
		t.Error("clean task meta should be removed by gc")
	}
	if _, mp := launch.TaskPaths(repoDir, "dirty"); !exists(mp) {
		t.Error("dirty task meta must survive gc (guard skip)")
	}
}

// --- Story 4: status summary ------------------------------------------------

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0B", 512: "512B", 1024: "1.0KB", 1536: "1.5KB", 1048576: "1.0MB"}
	for in, want := range cases {
		if got := tui.HumanBytes(in); got != want {
			t.Errorf("tui.HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestResetPlan_PurgeDataAddsArtifacts(t *testing.T) {
	cfg := &config.Config{}
	paths := reset.Paths{ConfigDir: "/c", DataRoot: "/d", MemoryDir: "/d/memory", ArtifactRoot: "/data/pix/artifacts"}
	hasArtifacts := func(a reset.Actions) bool {
		for _, b := range a.Backups {
			if b.Path == "/data/pix/artifacts" {
				return true
			}
		}
		return false
	}
	// Without --purge-data the artifacts dir is never in the plan (survives).
	if hasArtifacts(reset.Plan(cfg, paths, reset.Opts{})) {
		t.Error("artifacts must NOT be backed up without --purge-data")
	}
	// With --purge-data it is moved aside like any other data path.
	if !hasArtifacts(reset.Plan(cfg, paths, reset.Opts{PurgeData: true})) {
		t.Error("--purge-data must add the artifacts dir to the backup plan")
	}
}

// (writeFile + exists are shared test helpers defined in reset_test.go)

// --- review hardening: harvest safety ---------------------------------------

func TestSafeRelPath(t *testing.T) {
	ok := []string{"a.md", "docs/x.md", "a/b/c.txt"}
	bad := []string{"", "/etc/passwd", "../escape.md", "a/../../b.md"}
	for _, p := range ok {
		if !launch.SafeRelPath(p) {
			t.Errorf("launch.SafeRelPath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if launch.SafeRelPath(p) {
			t.Errorf("launch.SafeRelPath(%q) = true, want false", p)
		}
	}
}

func TestHarvest_SpacesAndSymlink(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/s", "HEAD")
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// A filename with a space (would be C-quoted by non-z porcelain and dropped).
	writeFile(t, filepath.Join(co, "my notes.md"), "spaced\n")
	// A symlink whose name looks like a doc: must be skipped, never counted.
	if err := os.Symlink("/etc/hostname", filepath.Join(co, "link.md")); err != nil {
		t.Skip("symlink unsupported")
	}
	env := gitEnv(t, "", nil)
	meta := launch.TaskMeta{Name: "s", Mainroot: main, Branch: "pix/s", Profile: "default"}
	dest, err := launch.HarvestArtifacts(env, io.Discard, meta, "r", co, "s")
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "my notes.md")); err != nil {
		t.Errorf("spaced filename not harvested: %v", err)
	}
	// The symlink must not appear in the manifest's copied list.
	var man launch.HarvestManifest
	b, _ := os.ReadFile(filepath.Join(dest, "manifest.json"))
	_ = json.Unmarshal(b, &man)
	for _, f := range man.Files {
		if f.Path == "link.md" {
			t.Error("symlink was recorded as harvested")
		}
	}
}

func TestFreshSnapshotDir_UniquePerCall(t *testing.T) {
	parent := t.TempDir()
	a, err := launch.FreshSnapshotDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	b, err := launch.FreshSnapshotDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two snapshots in the same second collided: %q", a)
	}
}

func TestHarvestToDir_RejectsCheckoutItself(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/x", "HEAD")
	writeFile(t, filepath.Join(co, "keep.md"), "precious\n")
	env := gitEnv(t, "", nil)
	// --to == the checkout must be refused (would O_TRUNC the source onto itself).
	if rc := launch.HarvestToDir(env, co, co, "x"); rc == 0 {
		t.Error("launch.HarvestToDir into the checkout should fail")
	}
	// A nested dir under the checkout is also refused.
	if rc := launch.HarvestToDir(env, co, filepath.Join(co, "sub"), "x"); rc == 0 {
		t.Error("launch.HarvestToDir into a nested checkout dir should fail")
	}
	// The source doc is untouched.
	if b, _ := os.ReadFile(filepath.Join(co, "keep.md")); string(b) != "precious\n" {
		t.Error("source doc was clobbered")
	}
	// A dir OUTSIDE the checkout works.
	out := filepath.Join(t.TempDir(), "out")
	if rc := launch.HarvestToDir(env, co, out, "x"); rc != 0 {
		t.Errorf("launch.HarvestToDir to an external dir should succeed, rc=%d", rc)
	}
	if _, err := os.Stat(filepath.Join(out, "keep.md")); err != nil {
		t.Errorf("external harvest missing keep.md: %v", err)
	}
}

// TestHarvestToDir_RejectsNestedDirUnderSymlinkedAncestor: the nesting guard
// must hold when the checkout is reached through a SYMLINKED ANCESTOR.
//
// launch.HarvestToDir EvalSymlinks's the checkout but --to may not exist yet, and
// EvalSymlinks fails outright on a missing path — so a merely Abs+Clean'd dest
// compared /real/link/co/sub against /real/target/co and concluded "not nested",
// copying straight into the checkout it was supposed to refuse.
//
// TestHarvestToDir_RejectsCheckoutItself only caught this where the PLATFORM
// temp root is itself a symlink (macOS /var -> /private/var). On Linux it passed
// with the bug present, so the guard could regress on CI unnoticed. This plants
// the symlink explicitly and therefore fails everywhere.
func TestHarvestToDir_RejectsNestedDirUnderSymlinkedAncestor(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	main := newMainRepo(t)
	co := filepath.Join(link, "co") // the checkout, reached VIA the symlink
	makeTaskClone(t, main, co, "pix/x", "HEAD")
	writeFile(t, filepath.Join(co, "keep.md"), "precious\n")
	env := gitEnv(t, "", nil)

	nested := filepath.Join(co, "sub") // does not exist yet
	if rc := launch.HarvestToDir(env, co, nested, "x"); rc == 0 {
		t.Error("launch.HarvestToDir into a nested dir reached through a symlinked ancestor should fail")
	}
	if _, err := os.Stat(nested); err == nil {
		t.Error("the refused harvest still created the nested dir inside the checkout")
	}
	if b, _ := os.ReadFile(filepath.Join(co, "keep.md")); string(b) != "precious\n" {
		t.Error("source doc was clobbered")
	}
}

func TestBoundSandboxName_LongProfileStaysBounded(t *testing.T) {
	got := launch.BoundSandboxName("api", "abcd1234", strings.Repeat("z", 50), strings.Repeat("p", 50))
	if len(got) > launch.MaxSandboxNameLen {
		t.Errorf("long profile: len %d > %d (%q)", len(got), launch.MaxSandboxNameLen, got)
	}
	if !strings.Contains(got, "abcd1234") {
		t.Errorf("repokey trimmed away: %q", got)
	}
}

func TestHarvest_IncludesModifiedTrackedDoc(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/m", "HEAD")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Commit a doc, then modify it in the working tree WITHOUT committing.
	writeFile(t, filepath.Join(co, "spec.md"), "v1\n")
	tgit(t, co, "add", "spec.md")
	tgit(t, co, "commit", "-q", "-m", "spec")
	writeFile(t, filepath.Join(co, "spec.md"), "v2 uncommitted\n")

	env := gitEnv(t, "", nil)
	meta := launch.TaskMeta{Name: "m", Mainroot: main, Branch: "pix/m", Profile: "default"}
	dest, err := launch.HarvestArtifacts(env, io.Discard, meta, "r", co, "m")
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "spec.md"))
	if err != nil || string(b) != "v2 uncommitted\n" {
		t.Errorf("modified tracked doc not harvested with working-tree bytes: %q err=%v", string(b), err)
	}
}

func TestParseTaskGcArgs_OverflowRejected(t *testing.T) {
	if _, err := launch.ParseTaskGcArgs([]string{"--days", "999999999999"}); err == nil {
		t.Error("absurd --days should be rejected (overflow guard)")
	}
	if _, err := launch.ParseTaskGcArgs([]string{"--artifact-days", "999999999999"}); err == nil {
		t.Error("absurd --artifact-days should be rejected (overflow guard)")
	}
}

func TestHarvestToDir_DescendantSymlinkAliasRefused(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/a", "HEAD")
	if err := os.MkdirAll(filepath.Join(co, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(co, "docs", "plan.md"), "precious\n")
	// External dest whose docs/ is a symlink back into the checkout.
	out := t.TempDir()
	if err := os.Symlink(filepath.Join(co, "docs"), filepath.Join(out, "docs")); err != nil {
		t.Skip("symlink unsupported")
	}
	env := gitEnv(t, "", nil)
	// The root check passes (out != co), but copying docs/plan.md must NOT truncate
	// the source through the descendant symlink: launch.CopyFilePreserve's SameFile guard
	// makes it an error, so launch.HarvestToDir returns non-zero.
	if rc := launch.HarvestToDir(env, co, out, "a"); rc == 0 {
		t.Error("descendant-symlink alias should fail launch.HarvestToDir")
	}
	if b, _ := os.ReadFile(filepath.Join(co, "docs", "plan.md")); string(b) != "precious\n" {
		t.Errorf("source doc was clobbered through the symlink: %q", string(b))
	}
}

func TestHarvestArtifacts_FailsClosedOnUnwritableDest(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pix/f", "HEAD")
	writeFile(t, filepath.Join(co, "notes.md"), "x\n")
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	// Make the artifacts root unwritable so the snapshot dir cannot be created.
	root := workspace.TaskArtifactRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o700)
	env := gitEnv(t, "", nil)
	meta := launch.TaskMeta{Name: "f", Mainroot: main, Branch: "pix/f", Profile: "default"}
	if _, err := launch.HarvestArtifacts(env, io.Discard, meta, "r", co, "f"); err == nil {
		t.Error("harvest must return an error when the dest cannot be created (callers fail closed)")
	}
}

func TestCopyFilePreserve_AtomicReplaceNoTempLeak(t *testing.T) {
	dir := t.TempDir()
	src1 := filepath.Join(dir, "s1")
	src2 := filepath.Join(dir, "s2")
	writeFile(t, src1, "first\n")
	writeFile(t, src2, "second\n")
	dst := filepath.Join(dir, "out", "d.md")
	if ok, err := launch.CopyFilePreserve(src1, dst); !ok || err != nil {
		t.Fatalf("first copy: ok=%v err=%v", ok, err)
	}
	// Re-copy a different source over the same dst: replaces content, no leftovers.
	if ok, err := launch.CopyFilePreserve(src2, dst); !ok || err != nil {
		t.Fatalf("second copy: ok=%v err=%v", ok, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "second\n" {
		t.Errorf("dst content = %q, want second", string(b))
	}
	// No .harvest-* temp files left behind in the dest dir.
	ents, _ := os.ReadDir(filepath.Dir(dst))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".harvest-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
