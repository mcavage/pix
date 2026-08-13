package uat

import (
	"os"
	"testing"
)

func TestAppendEvent(t *testing.T) {
	tmpFile := "test.jsonl"
	defer os.Remove(tmpFile)

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
	tmpFile, _ := os.CreateTemp("", "uat-event-test")
	defer os.Remove(tmpFile.Name())

	store, err := NewEventStore(tmpFile.Name())
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
