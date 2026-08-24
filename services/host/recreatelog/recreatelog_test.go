//go:build unix

package recreatelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
