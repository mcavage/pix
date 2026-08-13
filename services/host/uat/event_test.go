package uat

import (
	"os"
	"sync"
	"testing"
)

func setupTestDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "uat-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAppendEvent(t *testing.T) {
	dir := setupTestDir(t)
	defer os.RemoveAll(dir)
	tmpFile := dir + "/test.jsonl"

	store, err := NewEventStore(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Append("test-event", []byte(`{"foo":"bar"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verification - read it back
	f, _ := os.Open(tmpFile)
	defer f.Close()
	// Should contain one line of JSON
}

func TestEventStore_Sequence(t *testing.T) {
	dir := setupTestDir(t)
	defer os.RemoveAll(dir)
	tmpFile := dir + "/events.jsonl"

	store, err := NewEventStore(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	seq1, err := store.Append("type1", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 1 {
		t.Errorf("expected seq 1, got %d", seq1)
	}

	seq2, err := store.Append("type2", []byte(`{"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if seq2 != 2 {
		t.Errorf("expected seq 2, got %d", seq2)
	}
}

func TestEventStore_ConcurrentAppend(t *testing.T) {
	dir := setupTestDir(t)
	defer os.RemoveAll(dir)
	tmpFile := dir + "/events.jsonl"

	store, err := NewEventStore(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Append("test", []byte(`{"a":1}`))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	// Validate count
	events, _, err := store.Replay(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 10 {
		t.Errorf("expected 10 events, got %d", len(events))
	}
}

func TestEventStore_Validation(t *testing.T) {
	dir := setupTestDir(t)
	defer os.RemoveAll(dir)
	tmpFile := dir + "/events.jsonl"

	store, err := NewEventStore(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	// Non-monotonic
	f, _ := os.OpenFile(tmpFile, os.O_WRONLY, 0600)
	f.WriteString(`{"sequence": 1, "type": "t", "data": {}}` + "\n")
	f.WriteString(`{"sequence": 1, "type": "t", "data": {}}` + "\n")
	f.Close()
	_, err = store.Append("t", []byte(`{}`))
	if err == nil {
		t.Error("expected error for non-monotonic sequence")
	}

	// Malformed
	os.Truncate(tmpFile, 0)
	f, _ = os.OpenFile(tmpFile, os.O_WRONLY, 0600)
	f.WriteString("malformed" + "\n")
	f.Close()
	_, err = store.Append("t", []byte(`{}`))
	if err == nil {
		t.Error("expected error for malformed JSONL")
	}
}

func TestEventStore_Replay(t *testing.T) {
	dir := setupTestDir(t)
	defer os.RemoveAll(dir)
	tmpFile := dir + "/events.jsonl"

	store, err := NewEventStore(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		store.Append("t", []byte(`{}`))
	}

	events, lastSeq, err := store.Replay(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 events, got %d", len(events))
	}
	if lastSeq != 2 {
		t.Errorf("expected lastSeq 2, got %d", lastSeq)
	}
}

func TestEventStore_Permissions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "uat-dir")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Set parent dir to 0700
	if err := os.Chmod(tmpDir, 0700); err != nil {
		t.Fatal(err)
	}

	eventFile := tmpDir + "/events.jsonl"

	// Create with bad permissions first, should fail
	if _, err := os.OpenFile(eventFile, os.O_CREATE, 0666); err != nil {
		t.Fatal(err)
	}

	_, err = NewEventStore(eventFile)
	if err == nil {
		t.Error("expected error for existing file with bad permissions, got none")
	}

	os.Remove(eventFile)

	// Should create correctly
	_, err = NewEventStore(eventFile)
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(eventFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600, got %o", info.Mode().Perm())
	}

	// Test for symlink rejection
	os.Remove(eventFile)
	os.Symlink("/etc/passwd", eventFile)
	_, err = NewEventStore(eventFile)
	if err == nil {
		t.Error("expected error for symlink, got none")
	}
}
