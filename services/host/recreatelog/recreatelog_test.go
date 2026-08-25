// recreatelog_test.go carries no build tag on purpose: it exercises only the
// public API (Append/Read) plus validation, none of which is unix-specific,
// so it is the platform-independent proof that Wave B review round 1 asked
// for — the SAME assertions (atomicity, the retention cap, restrictive
// permissions, fail-closed malformed/unknown-field reads, concurrent-append
// safety) hold whether the build picked lock_unix.go's flock or
// lock_windows.go's LockFileEx. symlink_test.go and
// environment_name_parity_test.go stay `//go:build unix`: the former needs
// real unix symlink semantics, the latter only ever runs where recreatelog
// and config are both built (see its own doc comment).
package recreatelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRead_MissingFileReturnsNoRecordsNoError(t *testing.T) {
	dir := t.TempDir()
	records, err := Read(dir)
	if err != nil {
		t.Fatalf("Read on a never-written dir: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected zero records, got %d", len(records))
	}
}

func TestRead_DeletedFileReturnsNoRecordsNoError(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", []string{"mcp.local.command"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("remove log file: %v", err)
	}
	records, err := Read(dir)
	if err != nil {
		t.Fatalf("Read after delete: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected zero records after delete, got %d", len(records))
	}
}

func TestAppendThenRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().UTC()
	// Deliberately unsorted, with a duplicate — canonicalization must sort and
	// dedupe so two callers that disagree on order still converge.
	if err := Append(dir, "work", []string{"mcp.local.args", "host.mcp.probe", "mcp.local.args"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	after := time.Now().UTC()

	records, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	rec := records[0]
	if rec.Environment != "work" {
		t.Errorf("Environment = %q, want %q", rec.Environment, "work")
	}
	want := []string{"host.mcp.probe", "mcp.local.args"}
	if len(rec.ChangedKeyPaths) != len(want) {
		t.Fatalf("ChangedKeyPaths = %v, want %v", rec.ChangedKeyPaths, want)
	}
	for i := range want {
		if rec.ChangedKeyPaths[i] != want[i] {
			t.Errorf("ChangedKeyPaths[%d] = %q, want %q", i, rec.ChangedKeyPaths[i], want[i])
		}
	}
	if rec.Timestamp.Before(before) || rec.Timestamp.After(after) {
		t.Errorf("Timestamp %v not within [%v, %v]", rec.Timestamp, before, after)
	}
}

// TestAppend_OnlyThreeFieldsSerialized pins the record shape at the wire
// level: a facet value, a credential name, or an argv slipped into a future
// caller's changed-key-path string is still just a string to this package,
// but the RECORD ITSELF may never grow a field to carry one deliberately.
func TestAppend_OnlyThreeFieldsSerialized(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", []string{"a.b"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("read raw log: %v", err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("unmarshal raw log: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	var keys []string
	for k := range rows[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"changed_key_paths", "environment", "timestamp"}
	if len(keys) != len(want) {
		t.Fatalf("record fields = %v, want exactly %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("record fields = %v, want exactly %v", keys, want)
		}
	}
}

func TestAppend_RetentionCapKeepsNewest100(t *testing.T) {
	dir := t.TempDir()
	const total = MaxRecords + 5
	for i := 0; i < total; i++ {
		key := "k" + itoa(i)
		if err := Append(dir, "home", []string{key}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
	records, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != MaxRecords {
		t.Fatalf("len(records) = %d, want exactly %d (newest kept, oldest dropped)", len(records), MaxRecords)
	}
	firstWant := "k" + itoa(total-MaxRecords)
	lastWant := "k" + itoa(total-1)
	if got := records[0].ChangedKeyPaths[0]; got != firstWant {
		t.Errorf("oldest surviving record = %q, want %q (the first %d were dropped)", got, firstWant, total-MaxRecords)
	}
	if got := records[len(records)-1].ChangedKeyPaths[0]; got != lastWant {
		t.Errorf("newest record = %q, want %q", got, lastWant)
	}
}

func TestAppend_ConcurrentAppendsAllPersist(t *testing.T) {
	dir := t.TempDir()
	const n = 150
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Append(dir, "home", []string{"k" + itoa(i)}); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Append failed: %v", err)
	}

	records, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(records) != MaxRecords {
		t.Fatalf("len(records) = %d, want exactly %d after %d concurrent appends", len(records), MaxRecords, n)
	}
	seen := map[string]bool{}
	for _, rec := range records {
		if len(rec.ChangedKeyPaths) != 1 {
			t.Fatalf("record has %d changed key paths, want 1: %v", len(rec.ChangedKeyPaths), rec)
		}
		k := rec.ChangedKeyPaths[0]
		if seen[k] {
			t.Fatalf("key %q recorded more than once; a concurrent append lost or duplicated a write", k)
		}
		seen[k] = true
	}
}

func TestRead_MalformedContentFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("Read on malformed content: expected an error, got nil (must fail closed, not silently return zero records)")
	}
}

func TestRead_UnknownFieldFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An extra field is exactly the shape a leaked facet value or credential
	// name would take: strict parsing must refuse it, not decode around it.
	bad := `[{"timestamp":"2026-01-01T00:00:00Z","environment":"home","changed_key_paths":["a"],"facet_value":"secret"}]`
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("Read with an undocumented field: expected an error, got nil")
	}
}

func TestAppend_RestrictivePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", []string{"a.b"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file perm = %o, want 0600", perm)
	}
}

func TestAppend_RejectsEmptyEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "", []string{"a.b"}); err == nil {
		t.Fatal("expected an error for an empty environment name")
	}
}

func TestAppend_RejectsInvalidEnvironmentCharacters(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "../escape", []string{"a.b"}); err == nil {
		t.Fatal("expected an error for an environment name containing a path separator")
	}
}

func TestAppend_RejectsEmptyChangedKeyPaths(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", nil); err == nil {
		t.Fatal("expected an error for zero changed key paths")
	}
	if err := Append(dir, "home", []string{}); err == nil {
		t.Fatal("expected an error for zero changed key paths")
	}
}

func TestAppend_RejectsAbsoluteChangedKeyPath(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", []string{"/etc/passwd"}); err == nil {
		t.Fatal("expected an error for an absolute changed key path (a path outside the environment root)")
	}
}

func TestAppend_RejectsTraversalChangedKeyPath(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", []string{"mcp/../../../etc/passwd"}); err == nil {
		t.Fatal("expected an error for a changed key path containing '..'")
	}
}

func TestAppend_RejectsControlCharacterChangedKeyPath(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", []string{"mcp.local\ncommand"}); err == nil {
		t.Fatal("expected an error for a changed key path containing a control character")
	}
}

func TestMaxRecords_IsExactly100(t *testing.T) {
	if MaxRecords != 100 {
		t.Fatalf("MaxRecords = %d, want 100", MaxRecords)
	}
}

// TestFileName_IsExactlyRecreatesLog pins the on-disk diagnostic log's
// filename to PRD §5.9: `recreates.log`, no `.json` extension. This package
// still ENCODES the file as JSON (readRecordsFile/writeRecordsFile), but the
// PRD names the file, not the encoding, so the name carries no extension
// hint about the format inside it.
func TestFileName_IsExactlyRecreatesLog(t *testing.T) {
	if fileName != "recreates.log" {
		t.Fatalf("fileName = %q, want %q (PRD §5.9)", fileName, "recreates.log")
	}
}

// TestPath_JoinsDirAndFileName pins the exported accessor's exact shape:
// dir joined with the PRD-named file, nothing more. This is the sole
// accessor a later doctor wiring should call to locate the log — Read and
// Append use it internally too (see TestPath_MatchesWhereAppendWrites), so
// there is exactly one place the join happens.
func TestPath_JoinsDirAndFileName(t *testing.T) {
	dir := filepath.Join("some", "state", "dir")
	want := filepath.Join(dir, "recreates.log")
	if got := Path(dir); got != want {
		t.Fatalf("Path(%q) = %q, want %q", dir, got, want)
	}
}

// TestPath_DefaultStateDirLayout pins the exact absolute shape a default
// launcher state dir produces: ~/.local/state/pix/recreates.log. recreatelog
// itself never resolves $HOME or XDG_STATE_HOME — a caller always supplies
// dir — but this is the concrete path PRD §5.9 names, and the one a doctor
// wiring will actually see on a real host.
func TestPath_DefaultStateDirLayout(t *testing.T) {
	home := string(filepath.Separator) + filepath.Join("home", "example")
	dir := filepath.Join(home, ".local", "state", "pix")
	want := filepath.Join(home, ".local", "state", "pix", "recreates.log")
	if got := Path(dir); got != want {
		t.Fatalf("Path(%q) = %q, want %q", dir, got, want)
	}
}

// TestPath_MatchesWhereAppendWrites proves Path is not merely a parallel
// literal that happens to agree today: it names the exact file Append wrote
// and Read reads, so a future edit that changes fileName without updating
// Path (or vice versa) fails here, not silently in a caller.
func TestPath_MatchesWhereAppendWrites(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, "home", []string{"a.b"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(Path(dir)); err != nil {
		t.Fatalf("Path(dir) does not name the file Append wrote: %v", err)
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read via Path(dir): %v", err)
	}
	if !strings.Contains(string(raw), `"environment":"home"`) {
		t.Fatalf("content at Path(dir) does not look like the appended record: %s", raw)
	}
}

// TestNoStaleJSONExtensionFileName scans this package's OWN source (both
// production and test files, since a stale name in a comment is exactly as
// misleading as one in code) for the retired `recreate.log.json` /
// `recreate.log.lock` literals Wave B review round 1 shipped, so a future
// edit can never silently reintroduce the pre-PRD-§5.9 name.
func TestNoStaleJSONExtensionFileName(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	stale := []string{"recreate.log.json", "recreate.log.lock"}
	checked := 0
	for _, f := range files {
		if f == "recreatelog_test.go" {
			// this file names the stale strings above to describe them.
			continue
		}
		checked++
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stale {
			if strings.Contains(string(raw), s) {
				t.Errorf("%s contains stale name %q; the on-disk log is now named per PRD §5.9", f, s)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no other .go files to scan — did the package move?")
	}
}

// itoa avoids pulling in strconv just for test fixtures; it stays tiny and
// test-only so it does not count against the package's dependency surface.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
