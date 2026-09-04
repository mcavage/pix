package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestUnknownSlackTableTolerated: a leftover `[slack]` table from a
// pre-externalization config.toml decodes without error, does not fail the
// load, and is REPORTED as an unknown key so a caller can surface it -
func TestUnknownSlackTableTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const toml = "[slack]\nclient_id = \"123.456\"\nredirect_uri = \"https://example.com/cb\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !slices.Contains(c.UnknownKeys(), "slack") {
		t.Errorf("UnknownKeys() = %v, want it to include the ignored slack table", c.UnknownKeys())
	}
}
func TestSeedOpRefsAtNoClobberAndDirPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "op-refs.env")

	created, err := SeedOpRefsAt(path)
	if err != nil {
		t.Fatalf("SeedOpRefsAt first: %v", err)
	}
	if !created {
		t.Errorf("SeedOpRefsAt first: created = false, want true")
	}
	// An existing 0755 dir must be tightened to 0700.
	if fi, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir perms = %04o, want 0700", fi.Mode().Perm())
	}
	// File must be 0600.
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat file: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("file perms = %04o, want 0600", fi.Mode().Perm())
	}

	// Populate the file with user content, then seed again: it must NOT truncate.
	const userContent = "SLACK_TOKEN=op://Private/Slack/credential\n"
	if err := os.WriteFile(path, []byte(userContent), 0o600); err != nil {
		t.Fatalf("write user content: %v", err)
	}
	created, err = SeedOpRefsAt(path)
	if err != nil {
		t.Fatalf("SeedOpRefsAt second: %v", err)
	}
	if created {
		t.Errorf("SeedOpRefsAt second: created = true, want false (must not clobber)")
	}
	if got, _ := os.ReadFile(path); string(got) != userContent {
		t.Errorf("SeedOpRefsAt second truncated/modified existing file: %q", string(got))
	}
}

// TestSeedOpRefsAtConcurrent asserts that many concurrent seeders never truncate
// a populated op-refs.env: exactly one reports created, and the others leave the
// content intact.
func TestSeedOpRefsAtConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "op-refs.env")
	const n = 16
	var wg sync.WaitGroup
	createdCount := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c, err := SeedOpRefsAt(path)
			if err != nil {
				t.Errorf("SeedOpRefsAt goroutine %d: %v", i, err)
			}
			createdCount[i] = c
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range createdCount {
		if c {
			created++
		}
	}
	if created != 1 {
		t.Errorf("exactly one seeder should report created, got %d", created)
	}
	// The file must equal the template exactly (never a truncated/partial write).
	if got, _ := os.ReadFile(path); string(got) != OpRefsTemplate {
		t.Errorf("seeded file does not equal OpRefsTemplate after concurrent seed")
	}
}

// TestOpRefsTemplateHasNoActiveRefs covers F1 at the source: the seed template
// must have ZERO active (uncommented) KEY=VALUE lines.
func TestOpRefsTemplateHasNoActiveRefs(t *testing.T) {
	for _, ln := range strings.Split(OpRefsTemplate, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.IndexByte(trimmed, '=') > 0 {
			t.Errorf("OpRefsTemplate has an active (uncommented) entry: %q", ln)
		}
	}
}

func TestLoadAbsentReportsNoRetiredOrUnknownKeys(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %v, want none", got)
	}
}

// TestKnowledgeBundlesKeyIsRetired: an older config.toml carrying
// knowledge_bundles (the built-in OKF knowledge service, retired W2 U03A)
// must still Load cleanly — tolerated, never a hard error — and be reported
func TestKnowledgeBundlesKeyIsRetired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("knowledge_bundles = [\"/kb/acme\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_HOME", dir)

	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("Load must tolerate a retired knowledge_bundles key, got: %v", err)
	}
	if !slices.Contains(c.UnknownKeys(), "knowledge_bundles") {
		t.Errorf("UnknownKeys() = %v, want knowledge_bundles reported", c.UnknownKeys())
	}
}

// TestDataDirLayout locks the PIX_HOME resolution: DataDir is PIX_HOME
// itself, with NO XDG_DATA_HOME fallback (QA F5 — PIX_HOME is the single
// root; there is no second XDG data root to win or lose).
func TestDataDirLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)

	d, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := home; d != want {
		t.Errorf("DataDir = %q, want %q", d, want)
	}
}

// TestDataDirDefaultHome checks the ~/.pix fallback when PIX_HOME is unset
// (uses HOME).
func TestDataDirDefaultHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", "")
	t.Setenv("HOME", home)
	d, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join(home, ".pix"); d != want {
		t.Errorf("DataDir = %q, want %q", d, want)
	}
}

// TestUnknownKeysReportsATypo is the case the old retired-key allowlist could
// never cover: a MISTYPED key. It is not in any curated list, it decodes into
// nothing, and before this it was dropped in silence — so the setting simply
// never took effect and pix said nothing about why.
func TestUnknownKeysReportsATypo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// One real key (so the file is otherwise valid), one typo, one nested typo.
	const body = "ollama_bridge_model = \"code\"\nmemory_watchr_model = \"x\"\n\n[inference]\nbackendz = 1\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err) // an unknown key must never fail the load
	}
	if c.OllamaBridgeModel != "code" {
		t.Errorf("OllamaBridgeModel = %q, want the recognized key to still apply", c.OllamaBridgeModel)
	}
	for _, want := range []string{"memory_watchr_model", "inference.backendz"} {
		if !slices.Contains(c.UnknownKeys(), want) {
			t.Errorf("UnknownKeys() = %v, want it to include %q", c.UnknownKeys(), want)
		}
	}
}

// TestUnknownKeysEmptyForACleanConfig: the warning must stay silent on a file
// that is entirely understood, or it becomes noise everyone learns to ignore.
func TestUnknownKeysEmptyForACleanConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("ollama_bridge_model = \"code\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got := c.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %v, want none for a fully recognized config", got)
	}
}
