package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-stack/host/config"
)

// --- Story 1: naming --------------------------------------------------------

func TestTaskRepoLabel(t *testing.T) {
	// A normal repo's git-common-dir ends in /.git; the label is the parent name.
	dir := t.TempDir()
	repo := filepath.Join(dir, "my-api")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := taskRepoLabel(filepath.Join(repo, ".git")); got != "my-api" {
		t.Errorf("normal repo label = %q, want my-api", got)
	}
	// A bare repo dir ending .git keeps its base minus the suffix.
	bare := filepath.Join(dir, "svc.git")
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := taskRepoLabel(bare); got != "svc" {
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
	got := taskRepoLabel(long)
	if len(got) > maxRepoLabelLen {
		t.Errorf("label %q len %d > cap %d", got, len(got), maxRepoLabelLen)
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
	if got := boundSandboxName("api", "abcd1234", "fix", "work"); got != "pi-stack-t-api-abcd1234-fix-work" {
		t.Errorf("short: %q", got)
	}
	// A long name is trimmed (hash-tagged) but the repokey is always intact and
	// the whole name stays within the bound.
	longName := strings.Repeat("z", 90)
	got := boundSandboxName("api", "abcd1234", longName, "")
	if len(got) > maxSandboxNameLen {
		t.Errorf("bounded name too long: %d (%q)", len(got), got)
	}
	if !strings.Contains(got, "-abcd1234-") {
		t.Errorf("repokey missing from %q", got)
	}
	if !strings.HasPrefix(got, "pi-stack-t-api-abcd1234-") {
		t.Errorf("label/prefix mangled: %q", got)
	}
	// Two different long names must not collide (hash tag distinguishes them).
	other := boundSandboxName("api", "abcd1234", strings.Repeat("z", 89)+"y", "")
	if got == other {
		t.Errorf("distinct long names collided: %q", got)
	}
	// A huge label is trimmed AFTER the name floor, repokey still intact + bound ok.
	hugeLabel := strings.Repeat("L", 60)
	g2 := boundSandboxName(hugeLabel, "abcd1234", longName, "")
	if len(g2) > maxSandboxNameLen {
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
	newDir := taskRepoDir(mainroot)
	legacy := taskRepoKey(mainroot)
	writeMeta := func(dir, name string) {
		_, mp := taskPaths(dir, name)
		if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, mp, "{}\n")
	}

	// Nothing -> no layouts, findTaskLayout returns not-found (default new dir).
	if got := existingTaskLayouts(mainroot); len(got) != 0 {
		t.Errorf("no dirs: layouts %+v, want none", got)
	}
	if lay, found, amb := findTaskLayout(mainroot, "x"); found || amb || lay.dir != newDir {
		t.Errorf("not-found: lay=%+v found=%v amb=%v", lay, found, amb)
	}
	// A bare dir with NO meta/ must not read as a layout (e.g. a locks-only dir).
	if err := os.MkdirAll(filepath.Join(taskStateRoot(), legacy, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := existingTaskLayouts(mainroot); len(got) != 0 {
		t.Errorf("meta-less dir counted as layout: %+v", got)
	}
	// Legacy task present -> one legacy layout; findTaskLayout marks it legacy.
	writeMeta(legacy, "fix")
	lays := existingTaskLayouts(mainroot)
	if len(lays) != 1 || lays[0].dir != legacy || !lays[0].legacy {
		t.Errorf("legacy only: %+v", lays)
	}
	if lay, found, _ := findTaskLayout(mainroot, "fix"); !found || !lay.legacy {
		t.Errorf("find legacy: lay=%+v found=%v", lay, found)
	}
	// Add a NEW-layout task of the same name -> both layouts present; the same
	// name is now AMBIGUOUS and must be refused.
	writeMeta(newDir, "fix")
	if got := existingTaskLayouts(mainroot); len(got) != 2 {
		t.Errorf("both present: %+v, want 2", got)
	}
	if _, found, amb := findTaskLayout(mainroot, "fix"); !amb || found {
		t.Errorf("ambiguous name: found=%v amb=%v, want amb", found, amb)
	}
	// A name only in the new layout resolves unambiguously to new.
	writeMeta(newDir, "only-new")
	if lay, found, amb := findTaskLayout(mainroot, "only-new"); !found || amb || lay.legacy {
		t.Errorf("new only: lay=%+v found=%v amb=%v", lay, found, amb)
	}
}

func TestLegacyTaskSandboxName(t *testing.T) {
	// Legacy tasks own the pre-label sandbox name (no repo label segment).
	if got := legacyTaskSandboxName("abcd1234", "fix", "default"); got != "pi-stack-t-abcd1234-fix" {
		t.Errorf("legacy default: %q", got)
	}
	if got := legacyTaskSandboxName("abcd1234", "fix", "work"); got != "pi-stack-t-abcd1234-fix-work" {
		t.Errorf("legacy work: %q", got)
	}
	// hardenTaskMeta with legacy=true must derive the legacy name, not the labeled one.
	m, err := hardenTaskMeta(taskMeta{Name: "fix", Profile: config.DefaultProfile}, "/main", "abcd1234", true, "fix")
	if err != nil {
		t.Fatal(err)
	}
	if m.Sandbox != "pi-stack-t-abcd1234-fix" {
		t.Errorf("harden legacy sandbox = %q, want pre-label name", m.Sandbox)
	}
}

// --- Story 3: harvest -------------------------------------------------------

func TestIsArtifactPath(t *testing.T) {
	yes := []string{"README.md", "docs/design.md", "notes/scratch", "spec.prd", ".pi-stack/artifacts/x", "deep/thing.txt"}
	no := []string{"main.go", "node_modules/pkg/readme.md", ".git/config", "vendor/x/doc.md", "image.png"}
	for _, p := range yes {
		if !isArtifactPath(p) {
			t.Errorf("isArtifactPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isArtifactPath(p) {
			t.Errorf("isArtifactPath(%q) = true, want false", p)
		}
	}
}

func TestHarvestArtifacts_CopiesUncommittedDocsAndManifest(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pi-stack/h", "HEAD")
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
	meta := taskMeta{Name: "h", Repo: "main", Mainroot: main, Branch: "pi-stack/h", Profile: "default"}
	dest, err := harvestArtifacts(env, io.Discard, meta, "main-abcd1234", co, "h")
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
	if !strings.HasPrefix(dest, filepath.Join(dataHome, "pi-stack", "artifacts")) {
		t.Errorf("dest %q not under DATA_HOME artifacts", dest)
	}
	// Manifest names the task + repo + at least the harvested files.
	var man harvestManifest
	b, _ := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if man.Task != "h" || man.Repo != "main" || man.Branch != "pi-stack/h" {
		t.Errorf("manifest fields wrong: %+v", man)
	}
	if len(man.Files) < 3 {
		t.Errorf("manifest files = %d, want >= 3", len(man.Files))
	}
}

func TestHarvestArtifacts_NothingToHarvest(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pi-stack/e", "HEAD")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	meta := taskMeta{Name: "e", Mainroot: main, Branch: "pi-stack/e", Profile: "default"}
	dest, err := harvestArtifacts(gitEnv(t, "", nil), io.Discard, meta, "r", co, "e")
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
	if age := taskAge(taskMeta{Created: old}, co); age > 24*time.Hour {
		t.Errorf("age = %v, want small (mtime recent)", age)
	}
	// No co dir + old created -> age reflects created.
	if age := taskAge(taskMeta{Created: old}, filepath.Join(co, "gone")); age < 20*24*time.Hour {
		t.Errorf("age = %v, want ~30d from created", age)
	}
	// Neither -> 0 (never over-age).
	if age := taskAge(taskMeta{}, filepath.Join(co, "gone")); age != 0 {
		t.Errorf("no timestamps: age = %v, want 0", age)
	}
}

func TestParseTaskGcArgs(t *testing.T) {
	o, err := parseTaskGcArgs(nil)
	if err != nil || o.days != taskGCDefaultDays || o.artifactDays != taskArtifactDefaultDays || o.dryRun || o.noHarvest {
		t.Fatalf("defaults wrong: %+v err=%v", o, err)
	}
	o, err = parseTaskGcArgs([]string{"--days", "14", "--dry-run", "--no-harvest", "--artifact-days=90"})
	if err != nil || o.days != 14 || o.artifactDays != 90 || !o.dryRun || !o.noHarvest {
		t.Fatalf("flags wrong: %+v err=%v", o, err)
	}
	if _, err := parseTaskGcArgs([]string{"--days", "-3"}); err == nil {
		t.Error("negative --days should error")
	}
	if _, err := parseTaskGcArgs([]string{"--bogus"}); err == nil {
		t.Error("unknown flag should error")
	}
	if _, err := parseTaskGcArgs([]string{"--days"}); err == nil {
		t.Error("missing value should error")
	}
}

func TestPruneArtifacts(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	repoDir := "proj-abcd1234"
	base := filepath.Join(taskArtifactRoot(), repoDir, "task1")
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
	if n := pruneArtifacts(repoDir, 30, true); n != 1 {
		t.Errorf("dry-run pruned count = %d, want 1", n)
	}
	if _, err := os.Stat(oldSnap); err != nil {
		t.Error("dry-run must not delete")
	}
	// Real run deletes only the over-age snapshot.
	if n := pruneArtifacts(repoDir, 30, false); n != 1 {
		t.Errorf("pruned count = %d, want 1", n)
	}
	if _, err := os.Stat(oldSnap); err == nil {
		t.Error("old snapshot should be gone")
	}
	if _, err := os.Stat(freshSnap); err != nil {
		t.Error("fresh snapshot should survive")
	}
	// days<=0 disables pruning.
	if n := pruneArtifacts(repoDir, 0, false); n != 0 {
		t.Errorf("days=0 should prune nothing, got %d", n)
	}
}

func TestRunTaskGc_RemovesCleanSkipsDirty(t *testing.T) {
	main := newMainRepo(t)
	state := t.TempDir()
	data := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PI_STACK_PROFILE", "")

	env := gitEnv(t, "", nil) // sbx ls empty -> sandboxes absent -> guard passes on clean
	mainroot, err := resolveMainroot(env, main)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := taskRepoDir(mainroot)

	// Two tasks: "clean" (no changes) and "dirty" (uncommitted change). Both aged
	// past the threshold via an old Created and a backdated co mtime.
	mk := func(name string, dirty bool) {
		co, metaPath := taskPaths(repoDir, name)
		makeTaskClone(t, main, co, "pi-stack/"+name, "HEAD")
		if dirty {
			writeFile(t, filepath.Join(co, "f"), "changed\n")
		}
		if err := writeTaskMeta(metaPath, taskMeta{
			Name: name, Mode: "localclone", Mainroot: mainroot,
			Branch: "pi-stack/" + name, Profile: "default",
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
	runTaskGc(env, nil)

	// clean gone, dirty kept.
	if _, mp := taskPaths(repoDir, "clean"); exists(mp) {
		t.Error("clean task meta should be removed by gc")
	}
	if _, mp := taskPaths(repoDir, "dirty"); !exists(mp) {
		t.Error("dirty task meta must survive gc (guard skip)")
	}
}

// --- Story 4: status summary ------------------------------------------------

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0B", 512: "512B", 1024: "1.0KB", 1536: "1.5KB", 1048576: "1.0MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTaskStateSummary(t *testing.T) {
	state := t.TempDir()
	data := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_DATA_HOME", data)
	// Two task metas across one repo dir.
	metaDir := filepath.Join(taskStateRoot(), "proj-abcd1234", "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(metaDir, "a.json"), "{}\n")
	writeFile(t, filepath.Join(metaDir, "b.json"), "{}\n")
	// An artifact file to size.
	artDir := filepath.Join(taskArtifactRoot(), "proj-abcd1234", "a", "ts")
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(artDir, "doc.md"), strings.Repeat("x", 100))

	tasks, bytes := taskStateSummary()
	if tasks != 2 {
		t.Errorf("tasks = %d, want 2", tasks)
	}
	if bytes < 100 {
		t.Errorf("artifact bytes = %d, want >= 100", bytes)
	}
}

// --- Story 4: uninstall --purge-data ----------------------------------------

func TestResetPlan_PurgeDataAddsArtifacts(t *testing.T) {
	cfg := &config.Config{}
	paths := resetPaths{configDir: "/c", dataRoot: "/d", memoryDir: "/d/memory", artifactRoot: "/data/pi-stack/artifacts"}
	hasArtifacts := func(a resetActions) bool {
		for _, b := range a.Backups {
			if b.Path == "/data/pi-stack/artifacts" {
				return true
			}
		}
		return false
	}
	// Without --purge-data the artifacts dir is never in the plan (survives).
	if hasArtifacts(resetPlan(cfg, paths, resetOpts{})) {
		t.Error("artifacts must NOT be backed up without --purge-data")
	}
	// With --purge-data it is moved aside like any other data path.
	if !hasArtifacts(resetPlan(cfg, paths, resetOpts{purgeData: true})) {
		t.Error("--purge-data must add the artifacts dir to the backup plan")
	}
}

// (writeFile + exists are shared test helpers defined in reset_test.go)

// --- review hardening: harvest safety ---------------------------------------

func TestSafeRelPath(t *testing.T) {
	ok := []string{"a.md", "docs/x.md", "a/b/c.txt"}
	bad := []string{"", "/etc/passwd", "../escape.md", "a/../../b.md"}
	for _, p := range ok {
		if !safeRelPath(p) {
			t.Errorf("safeRelPath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if safeRelPath(p) {
			t.Errorf("safeRelPath(%q) = true, want false", p)
		}
	}
}

func TestHarvest_SpacesAndSymlink(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pi-stack/s", "HEAD")
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// A filename with a space (would be C-quoted by non-z porcelain and dropped).
	writeFile(t, filepath.Join(co, "my notes.md"), "spaced\n")
	// A symlink whose name looks like a doc: must be skipped, never counted.
	if err := os.Symlink("/etc/hostname", filepath.Join(co, "link.md")); err != nil {
		t.Skip("symlink unsupported")
	}
	env := gitEnv(t, "", nil)
	meta := taskMeta{Name: "s", Mainroot: main, Branch: "pi-stack/s", Profile: "default"}
	dest, err := harvestArtifacts(env, io.Discard, meta, "r", co, "s")
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "my notes.md")); err != nil {
		t.Errorf("spaced filename not harvested: %v", err)
	}
	// The symlink must not appear in the manifest's copied list.
	var man harvestManifest
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
	a, err := freshSnapshotDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	b, err := freshSnapshotDir(parent)
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
	makeTaskClone(t, main, co, "pi-stack/x", "HEAD")
	writeFile(t, filepath.Join(co, "keep.md"), "precious\n")
	env := gitEnv(t, "", nil)
	// --to == the checkout must be refused (would O_TRUNC the source onto itself).
	if rc := harvestToDir(env, co, co, "x"); rc == 0 {
		t.Error("harvestToDir into the checkout should fail")
	}
	// A nested dir under the checkout is also refused.
	if rc := harvestToDir(env, co, filepath.Join(co, "sub"), "x"); rc == 0 {
		t.Error("harvestToDir into a nested checkout dir should fail")
	}
	// The source doc is untouched.
	if b, _ := os.ReadFile(filepath.Join(co, "keep.md")); string(b) != "precious\n" {
		t.Error("source doc was clobbered")
	}
	// A dir OUTSIDE the checkout works.
	out := filepath.Join(t.TempDir(), "out")
	if rc := harvestToDir(env, co, out, "x"); rc != 0 {
		t.Errorf("harvestToDir to an external dir should succeed, rc=%d", rc)
	}
	if _, err := os.Stat(filepath.Join(out, "keep.md")); err != nil {
		t.Errorf("external harvest missing keep.md: %v", err)
	}
}

func TestBoundSandboxName_LongProfileStaysBounded(t *testing.T) {
	got := boundSandboxName("api", "abcd1234", strings.Repeat("z", 50), strings.Repeat("p", 50))
	if len(got) > maxSandboxNameLen {
		t.Errorf("long profile: len %d > %d (%q)", len(got), maxSandboxNameLen, got)
	}
	if !strings.Contains(got, "abcd1234") {
		t.Errorf("repokey trimmed away: %q", got)
	}
}

func TestStatusRender_ArtifactsWithoutTasks(t *testing.T) {
	var sb strings.Builder
	statusReport{Version: "v", Tasks: 0, ArtifactB: 4096}.render(&sb)
	if !strings.Contains(sb.String(), "artifacts 4.0KB") {
		t.Errorf("artifact-only status did not render the tasks line:\n%s", sb.String())
	}
}

// --- review loop 2 hardening ------------------------------------------------

func TestHarvest_IncludesModifiedTrackedDoc(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pi-stack/m", "HEAD")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Commit a doc, then modify it in the working tree WITHOUT committing.
	writeFile(t, filepath.Join(co, "spec.md"), "v1\n")
	tgit(t, co, "add", "spec.md")
	tgit(t, co, "commit", "-q", "-m", "spec")
	writeFile(t, filepath.Join(co, "spec.md"), "v2 uncommitted\n")

	env := gitEnv(t, "", nil)
	meta := taskMeta{Name: "m", Mainroot: main, Branch: "pi-stack/m", Profile: "default"}
	dest, err := harvestArtifacts(env, io.Discard, meta, "r", co, "m")
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "spec.md"))
	if err != nil || string(b) != "v2 uncommitted\n" {
		t.Errorf("modified tracked doc not harvested with working-tree bytes: %q err=%v", string(b), err)
	}
}

func TestParseTaskGcArgs_OverflowRejected(t *testing.T) {
	if _, err := parseTaskGcArgs([]string{"--days", "999999999999"}); err == nil {
		t.Error("absurd --days should be rejected (overflow guard)")
	}
	if _, err := parseTaskGcArgs([]string{"--artifact-days", "999999999999"}); err == nil {
		t.Error("absurd --artifact-days should be rejected (overflow guard)")
	}
}

func TestHarvestToDir_DescendantSymlinkAliasRefused(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pi-stack/a", "HEAD")
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
	// the source through the descendant symlink: copyFilePreserve's SameFile guard
	// makes it an error, so harvestToDir returns non-zero.
	if rc := harvestToDir(env, co, out, "a"); rc == 0 {
		t.Error("descendant-symlink alias should fail harvestToDir")
	}
	if b, _ := os.ReadFile(filepath.Join(co, "docs", "plan.md")); string(b) != "precious\n" {
		t.Errorf("source doc was clobbered through the symlink: %q", string(b))
	}
}

func TestHarvestArtifacts_FailsClosedOnUnwritableDest(t *testing.T) {
	main := newMainRepo(t)
	co := filepath.Join(t.TempDir(), "co")
	makeTaskClone(t, main, co, "pi-stack/f", "HEAD")
	writeFile(t, filepath.Join(co, "notes.md"), "x\n")
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	// Make the artifacts root unwritable so the snapshot dir cannot be created.
	root := taskArtifactRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o700)
	env := gitEnv(t, "", nil)
	meta := taskMeta{Name: "f", Mainroot: main, Branch: "pi-stack/f", Profile: "default"}
	if _, err := harvestArtifacts(env, io.Discard, meta, "r", co, "f"); err == nil {
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
	if ok, err := copyFilePreserve(src1, dst); !ok || err != nil {
		t.Fatalf("first copy: ok=%v err=%v", ok, err)
	}
	// Re-copy a different source over the same dst: replaces content, no leftovers.
	if ok, err := copyFilePreserve(src2, dst); !ok || err != nil {
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
