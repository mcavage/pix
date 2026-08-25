package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// environment_test.go is the config-schema half of E1.5 (Story 1, native
// sandbox environments, docs/design/environments.md §5.3): `environment =
// "NAME"` (the machine default selection) and `[environments]` (the name ->
// canonical absolute local path registry). Wave C owns the `pix env` verbs
// that call these helpers; this file only proves the schema, canonicalization,
// and sparse-Save contract.

// ── canonicalization ─────────────────────────────────────────────────────

func TestAddEnvironmentCanonicalizesTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no resolvable home dir")
	}
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.AddEnvironment("home", "~/envs/mine")
	if err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	want := filepath.Join(home, "envs", "mine")
	if got != want {
		t.Errorf("AddEnvironment canonical path = %q, want %q", got, want)
	}
	if cfg.Environments["home"] != want {
		t.Errorf("Environments[home] = %q, want %q", cfg.Environments["home"], want)
	}
}

func TestAddEnvironmentCanonicalizesRelativePath(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.AddEnvironment("rel", "envs/rel")
	if err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("AddEnvironment canonical path = %q, want absolute", got)
	}
	if strings.Contains(got, "~") {
		t.Errorf("AddEnvironment canonical path = %q, must not retain ~", got)
	}
}

// ── empty/whitespace input is refused, never silently canonicalized ─────

func TestAddEnvironmentRejectsEmptyPath(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", ""); err == nil {
		t.Fatal("AddEnvironment with an empty path must be refused, not canonicalized to CWD")
	}
	if _, ok := cfg.Environments["home"]; ok {
		t.Error("a refused AddEnvironment must not register anything")
	}
}

func TestAddEnvironmentRejectsWhitespacePath(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "   "); err == nil {
		t.Fatal("AddEnvironment with a whitespace-only path must be refused, not canonicalized to CWD")
	}
	if _, ok := cfg.Environments["home"]; ok {
		t.Error("a refused AddEnvironment must not register anything")
	}
}

func TestAddEnvironmentRejectsWhitespaceName(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("   ", "/abs/home"); err == nil {
		t.Fatal("AddEnvironment with a whitespace-only name must be refused")
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a refused AddEnvironment must not register anything, got %v", cfg.Environments)
	}
}

// ── unsafe names are refused, not silently persisted ────────────────────
//
// AddEnvironment is the write-side boundary for [environments] keys: this is
// the ONLY place a name reaches config.toml, so it is the ONLY place that can
// refuse an unsafe one before it exists anywhere. The accepted shape mirrors
// recreatelog's documented environment-name pattern (start alnum, then alnum
// plus '.', '_', '-', <=128 bytes) byte-for-byte; see environment.go's
// validEnvironmentName doc for why it is duplicated rather than imported, and
// recreatelog's TestEnvironmentNameShapeMatchesConfig for the parity check.

func TestAddEnvironmentRejectsSpaceInName(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("my env", "/abs/home"); err == nil {
		t.Fatal("AddEnvironment with a space in the name must be refused")
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a refused AddEnvironment must not register anything, got %v", cfg.Environments)
	}
}

func TestAddEnvironmentRejectsSlashInName(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("my/env", "/abs/home"); err == nil {
		t.Fatal("AddEnvironment with a slash in the name must be refused")
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a refused AddEnvironment must not register anything, got %v", cfg.Environments)
	}
}

func TestAddEnvironmentRejectsControlCharInName(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("my\tenv", "/abs/home"); err == nil {
		t.Fatal("AddEnvironment with a control character in the name must be refused")
	}
	if _, err := cfg.AddEnvironment("my\nenv", "/abs/home"); err == nil {
		t.Fatal("AddEnvironment with a newline in the name must be refused")
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a refused AddEnvironment must not register anything, got %v", cfg.Environments)
	}
}

func TestAddEnvironmentRejectsLeadingPunctuationInName(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"-env", ".env", "_env"} {
		if _, err := cfg.AddEnvironment(name, "/abs/home"); err == nil {
			t.Errorf("AddEnvironment(%q, ...) with leading punctuation must be refused", name)
		}
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a refused AddEnvironment must not register anything, got %v", cfg.Environments)
	}
}

func TestAddEnvironmentRejectsOverlongName(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	toolong := strings.Repeat("a", 129)
	if _, err := cfg.AddEnvironment(toolong, "/abs/home"); err == nil {
		t.Fatal("AddEnvironment with a name over 128 bytes must be refused")
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a refused AddEnvironment must not register anything, got %v", cfg.Environments)
	}
}

// TestAddEnvironmentAcceptsWordEnvironments pins the one name that MUST stay
// valid despite sharing its spelling with the `[environments]` table itself —
// the safe-shape check is about characters, not about colliding with a TOML
// section name.
func TestAddEnvironmentAcceptsWordEnvironments(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("environments", "/abs/home"); err != nil {
		t.Fatalf("AddEnvironment(%q, ...) must be accepted, got %v", "environments", err)
	}
	if _, ok := cfg.Environments["environments"]; !ok {
		t.Errorf("Environments = %v, want it to include %q", cfg.Environments, "environments")
	}
}

func TestAddEnvironmentAlreadyAbsoluteStaysClean(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.AddEnvironment("home", "/abs/envs/home")
	if err != nil {
		t.Fatalf("AddEnvironment: %v", err)
	}
	if got != "/abs/envs/home" {
		t.Errorf("AddEnvironment canonical path = %q, want unchanged /abs/envs/home", got)
	}
}

// ── registry + selection round trip ─────────────────────────────────────

func TestEnvironmentsMapRoundTrips(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	homePath, err := cfg.AddEnvironment("home", "/abs/envs/home")
	if err != nil {
		t.Fatal(err)
	}
	workPath, err := cfg.AddEnvironment("work", "/abs/envs/work")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("work"); err != nil {
		t.Fatalf("UseEnvironment: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Environment != "work" {
		t.Errorf("Environment = %q, want work", got.Environment)
	}
	if len(got.Environments) != 2 || got.Environments["home"] != homePath || got.Environments["work"] != workPath {
		t.Errorf("Environments = %v, want home=%q work=%q", got.Environments, homePath, workPath)
	}
}

func TestUseEnvironmentRefusesUnregistered(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("nope"); err == nil {
		t.Fatal("expected an error selecting an unregistered environment")
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q, want unchanged empty after a refused selection", cfg.Environment)
	}
}

// TestUseEnvironmentUnknownErrorShape pins the Wave B / P0 subset of the
// PRD §5.1 actionable error copy verbatim, with known names sorted
// deterministically regardless of registration order. `closest:` is §5.1's
// structured Wave C presentation and is out of scope here.
func TestUseEnvironmentUnknownErrorShape(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("work", "/abs/work"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}

	err = cfg.UseEnvironment("hoem")
	if err == nil {
		t.Fatal("expected an error selecting an unregistered environment")
	}
	want := "pix: no environment named \"hoem\".\n     known: home, work\n     register one: pix env add <name> [path]"
	if got := err.Error(); got != want {
		t.Errorf("Error() =\n%s\nwant\n%s", got, want)
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q, want unchanged empty after a refused selection", cfg.Environment)
	}
}

// TestUseEnvironmentUnknownErrorIsTyped proves UseEnvironment returns a
// *UnknownEnvironmentError (Name/Known), not just an opaque error, so a
// future Wave C caller can render its own presentation without scraping
// Error()'s string.
func TestUseEnvironmentUnknownErrorIsTyped(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("work", "/abs/work"); err != nil {
		t.Fatal(err)
	}

	err = cfg.UseEnvironment("nope")
	var unknown *UnknownEnvironmentError
	if !errors.As(err, &unknown) {
		t.Fatalf("UseEnvironment error = %#v, want *UnknownEnvironmentError", err)
	}
	if unknown.Name != "nope" {
		t.Errorf("Name = %q, want nope", unknown.Name)
	}
	if want := []string{"work"}; !slices.Equal(unknown.Known, want) {
		t.Errorf("Known = %v, want %v", unknown.Known, want)
	}
}

// TestUseEnvironmentUnknownErrorNoneWhenEmpty proves the "known: none" case:
// an empty registry must not render as "known: " or a comma-joined blank.
func TestUseEnvironmentUnknownErrorNoneWhenEmpty(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	err = cfg.UseEnvironment("anything")
	want := "pix: no environment named \"anything\".\n     known: none\n     register one: pix env add <name> [path]"
	if err == nil || err.Error() != want {
		t.Errorf("Error() = %v, want %q", err, want)
	}
}

// TestUseEnvironmentUnknownErrorSpecialSafeName covers a name that is safe
// per validEnvironmentName (dots, underscore, hyphen) but still unregistered:
// the "no environment named" line must quote and repeat it unchanged, not
// choke on the punctuation, while the register-one line stays the fixed
// `<name>` placeholder regardless of what was typed.
func TestUseEnvironmentUnknownErrorSpecialSafeName(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}

	name := "stage-2.prod_env"
	err = cfg.UseEnvironment(name)
	want := "pix: no environment named \"stage-2.prod_env\".\n     known: home\n     register one: pix env add <name> [path]"
	if err == nil || err.Error() != want {
		t.Errorf("Error() = %v, want %q", err, want)
	}
}

func TestUseEnvironmentEmptyClearsDefault(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment(""); err != nil {
		t.Fatalf("UseEnvironment(\"\"): %v", err)
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q, want cleared", cfg.Environment)
	}
}

func TestRemoveEnvironmentClearsMatchingDefault(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("home"); err != nil {
		t.Fatal(err)
	}
	if !cfg.RemoveEnvironment("home") {
		t.Fatal("RemoveEnvironment(home) reported no change")
	}
	if _, ok := cfg.Environments["home"]; ok {
		t.Errorf("Environments still has home after RemoveEnvironment")
	}
	if cfg.Environment != "" {
		t.Errorf("Environment = %q, want cleared (its registration was removed)", cfg.Environment)
	}
}

func TestRemoveEnvironmentLeavesUnrelatedDefaultAlone(t *testing.T) {
	tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("work", "/abs/work"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseEnvironment("work"); err != nil {
		t.Fatal(err)
	}
	if !cfg.RemoveEnvironment("home") {
		t.Fatal("RemoveEnvironment(home) reported no change")
	}
	if cfg.Environment != "work" {
		t.Errorf("Environment = %q, want unchanged work", cfg.Environment)
	}
}

// ── sparse Save: no default noise, exactly one added key ────────────────

func TestSaveWithNoEnvironmentAddsNoKeys(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = "acme" // force a write, unrelated to environments
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	if strings.Contains(raw, "environment") {
		t.Errorf("raw file petrified environment/environments with nothing set:\n%s", raw)
	}
}

// TestSelectingEnvironmentAddsExactlyOneKey is the byte-diff proof: choosing a
// default among an ALREADY-registered environment changes the file by exactly
// one line, `environment = "home"`. Registration itself is a separate, prior
// write (AddEnvironment/`pix env add`), so this isolates what selection alone
// costs.
func TestSelectingEnvironmentAddsExactlyOneKey(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment("home", "/abs/home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	before := rawFile(t, path)

	if err := cfg.UseEnvironment("home"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	after := rawFile(t, path)

	beforeLines := map[string]bool{}
	for _, l := range strings.Split(strings.TrimRight(before, "\n"), "\n") {
		beforeLines[l] = true
	}
	var added []string
	for _, l := range strings.Split(strings.TrimRight(after, "\n"), "\n") {
		if !beforeLines[l] {
			added = append(added, l)
		}
	}
	if len(added) != 1 || added[0] != `environment = "home"` {
		t.Fatalf("selection diff = %v, want exactly [environment = \"home\"]\nbefore:\n%s\nafter:\n%s", added, before, after)
	}
}

// ── malformed/noncanonical persisted paths fail closed ──────────────────

// TestLoadDropsNoncanonicalEnvironmentPaths: a hand-edited config.toml is the
// only way a `~`-bearing or relative environments path reaches disk (Save()
// only ever writes AddEnvironment's already-canonical output). Loading one
// must not trust it as a local root: it is dropped, surfaced as an unknown
// key (the same "tell them" contract as a retired [plugins.*] slot), and a
// default naming the dropped entry resolves to no default rather than a
// dangling selection.
func TestLoadDropsNoncanonicalEnvironmentPaths(t *testing.T) {
	path := tempConfig(t)
	const toml = "environment = \"home\"\n\n[environments]\n" +
		"home = \"~/envs/home\"\n" +
		"work = \"relative/work\"\n" +
		"good = \"/abs/canonical/good\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Environments["home"]; ok {
		t.Errorf("Environments kept a ~-bearing path: %v", got.Environments)
	}
	if _, ok := got.Environments["work"]; ok {
		t.Errorf("Environments kept a relative path: %v", got.Environments)
	}
	if p, ok := got.Environments["good"]; !ok || p != "/abs/canonical/good" {
		t.Errorf("Environments[good] = %q, ok=%v, want the untouched canonical entry", p, ok)
	}
	if got.Environment != "" {
		t.Errorf("Environment = %q, want cleared (its registration was dropped as noncanonical)", got.Environment)
	}
	unknown := got.UnknownKeys()
	for _, want := range []string{"environments.home", "environments.work"} {
		if !slices.Contains(unknown, want) {
			t.Errorf("UnknownKeys() = %v, want it to include %q", unknown, want)
		}
	}
	if slices.Contains(unknown, "environments.good") {
		t.Errorf("UnknownKeys() wrongly flagged the canonical entry: %v", unknown)
	}
}

// TestSaveErasesHandEditedNoncanonicalEnvironment is the explicit regression
// test for the fail-closed behavior TestLoadDropsNoncanonicalEnvironmentPaths
// documents at the in-memory level: a hand-edited noncanonical [environments]
// entry does not just fail to round-trip through Load, it is PERMANENTLY
// ERASED the moment anything calls Save() on that loaded Config, because
// Save() always writes the in-memory (already-dropped) Environments map back
// to disk. That is deliberate — AddEnvironment is the only writer trusted to
// produce a canonical path, so a value that could only have reached the file
// by hand is never round-tripped — but "deliberate" must never mean "silent":
// this test proves the diagnostic (UnknownKeys) is already populated the
// instant Load returns, strictly BEFORE the destructive Save() call, so a
// caller that checks UnknownKeys() first (see cli.Deps.Config's
// warnUnknownConfigKeys, which does exactly this on every command) sees the
// warning with time to back up the file before the entry becomes
// unrecoverable.
func TestSaveErasesHandEditedNoncanonicalEnvironment(t *testing.T) {
	path := tempConfig(t)
	const before = "environment = \"home\"\n\n[environments]\n" +
		"home = \"~/envs/home\"\n" +
		"good = \"/abs/canonical/good\"\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}

	// The diagnostic must exist BEFORE Save is ever called — this is the
	// window in which the drop is still recoverable (the raw file on disk
	// still has the hand-edited entry; only the in-memory Config forgot it).
	if !slices.Contains(cfg.UnknownKeys(), "environments.home") {
		t.Fatalf("UnknownKeys() = %v, want it to already include %q before Save is ever called",
			cfg.UnknownKeys(), "environments.home")
	}
	raw := rawFile(t, path)
	if !strings.Contains(raw, "~/envs/home") {
		t.Fatalf("file on disk no longer has the hand-edited entry before Save was called: %s", raw)
	}

	// Now the destructive step: Save() persists the in-memory (already-
	// dropped) state, which is the point of no return for the hand-edited
	// entry.
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after := rawFile(t, path)
	if strings.Contains(after, "~") || strings.Contains(after, "envs/home") {
		t.Errorf("Save must have erased the noncanonical entry from disk, got:\n%s", after)
	}
	if !strings.Contains(after, "/abs/canonical/good") {
		t.Errorf("Save must keep the canonical sibling entry, got:\n%s", after)
	}

	// The erasure is now permanent and, on a FRESH load of the rewritten
	// file, silent: there is nothing left to flag. This is exactly why the
	// pre-Save diagnostic above is the only chance a user gets to notice.
	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Environments["home"]; ok {
		t.Errorf("Environments still has the erased entry after Save+reload: %v", reloaded.Environments)
	}
	if slices.Contains(reloaded.UnknownKeys(), "environments.home") {
		t.Errorf("UnknownKeys() = %v, want no trace after the entry was already erased by Save", reloaded.UnknownKeys())
	}
}

// TestLoadDropsInvalidEnvironmentNames is the Wave B review round-1 gap in
// TestLoadDropsNoncanonicalEnvironmentPaths above: that test hand-edits an
// unsafe PATH under a well-formed name. AddEnvironment is the only writer
// that ever checks validEnvironmentName, so a hand-edited config.toml is
// also the only way an unsafe NAME — one AddEnvironment would have refused
// outright — reaches [environments], and Load must fail closed on it exactly
// like a noncanonical path: dropped, never trusted as a registration, and
// surfaced via UnknownKeys so the drop is diagnosable before it becomes
// permanent at the next Save. Every entry below carries an otherwise-valid
// CANONICAL path, isolating the name check from the path check.
func TestLoadDropsInvalidEnvironmentNames(t *testing.T) {
	path := tempConfig(t)
	overlong := strings.Repeat("a", 129)
	raw := "environment = \"bad/name\"\n\n[environments]\n" +
		"\"bad/name\" = \"/abs/canonical/slash\"\n" +
		"\"../evil\" = \"/abs/canonical/traversal\"\n" +
		"\"bad\\tname\" = \"/abs/canonical/control\"\n" +
		"\"" + overlong + "\" = \"/abs/canonical/overlong\"\n" +
		"good = \"/abs/canonical/good\"\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	badNames := []string{"bad/name", "../evil", "bad\tname", overlong}
	for _, badName := range badNames {
		if _, ok := got.Environments[badName]; ok {
			t.Errorf("Environments kept an invalid name %q: %v", badName, got.Environments)
		}
	}
	if len(got.Environments) != 1 {
		t.Errorf("Environments = %v, want exactly the one valid entry", got.Environments)
	}
	if p, ok := got.Environments["good"]; !ok || p != "/abs/canonical/good" {
		t.Errorf("Environments[good] = %q, ok=%v, want the untouched canonical entry", p, ok)
	}
	if got.Environment != "" {
		t.Errorf("Environment = %q, want cleared (its registration had an invalid name)", got.Environment)
	}
	unknown := got.UnknownKeys()
	for _, badName := range badNames {
		want := "environments." + badName
		if !slices.Contains(unknown, want) {
			t.Errorf("UnknownKeys() = %v, want it to include %q", unknown, want)
		}
	}
	if slices.Contains(unknown, "environments.good") {
		t.Errorf("UnknownKeys() wrongly flagged the valid entry: %v", unknown)
	}
}

// TestSaveErasesHandEditedInvalidEnvironmentName is the name-side sibling of
// TestSaveErasesHandEditedNoncanonicalEnvironment: the diagnostic
// (UnknownKeys) must already be populated the instant Load returns, strictly
// BEFORE the destructive Save() call that permanently erases the entry —
// proving the invalid-name drop does not survive a save, and does not
// silently survive undetected before one either.
func TestSaveErasesHandEditedInvalidEnvironmentName(t *testing.T) {
	path := tempConfig(t)
	const before = "environment = \"home\"\n\n[environments]\n" +
		"\"../evil\" = \"/abs/canonical/evil\"\n" +
		"good = \"/abs/canonical/good\"\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.UnknownKeys(), "environments.../evil") {
		t.Fatalf("UnknownKeys() = %v, want it to already include %q before Save is ever called",
			cfg.UnknownKeys(), "environments.../evil")
	}
	raw := rawFile(t, path)
	if !strings.Contains(raw, "../evil") {
		t.Fatalf("file on disk no longer has the hand-edited entry before Save was called: %s", raw)
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after := rawFile(t, path)
	if strings.Contains(after, "evil") {
		t.Errorf("Save must have erased the invalid-name entry from disk, got:\n%s", after)
	}
	if !strings.Contains(after, "/abs/canonical/good") {
		t.Errorf("Save must keep the valid sibling entry, got:\n%s", after)
	}

	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Environments["../evil"]; ok {
		t.Errorf("Environments still has the erased entry after Save+reload: %v", reloaded.Environments)
	}
	if slices.Contains(reloaded.UnknownKeys(), "environments.../evil") {
		t.Errorf("UnknownKeys() = %v, want no trace after the entry was already erased by Save", reloaded.UnknownKeys())
	}
}

func TestLoadDropsDanglingEnvironmentDefault(t *testing.T) {
	path := tempConfig(t)
	const toml = "environment = \"ghost\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Environment != "" {
		t.Errorf("Environment = %q, want cleared (names no registered environment)", got.Environment)
	}
}
