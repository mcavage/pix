package config

import (
	"os"
	"testing"
)

// TestMemoryCaptureValidation covers ValidMemoryCapture directly and through
// applyDefaults: absent/garbled (including the deleted "review" mode, and a
// hand-edited on-disk value) fails closed to explicit; every valid mode
// survives applyDefaults unchanged.
func TestMemoryCaptureValidation(t *testing.T) {
	for _, v := range MemoryCaptureModes {
		if !ValidMemoryCapture(v) {
			t.Errorf("ValidMemoryCapture(%q) = false, want true", v)
		}
		c := &Config{MemoryCapture: v}
		c.applyDefaults()
		if c.MemoryCapture != v {
			t.Errorf("applyDefaults changed a valid memory_capture %q to %q", v, c.MemoryCapture)
		}
	}
	for _, v := range []string{"", "bogus", "Explicit", "review"} {
		if ValidMemoryCapture(v) {
			t.Errorf("ValidMemoryCapture(%q) = true, want false", v)
		}
		c := &Config{MemoryCapture: v}
		c.applyDefaults()
		if c.MemoryCapture != MemoryCaptureExplicit {
			t.Errorf("applyDefaults(%q) = %q, want fail-closed default explicit", v, c.MemoryCapture)
		}
	}

	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("memory_capture = \"always\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryCapture != MemoryCaptureExplicit {
		t.Errorf("a hand-edited garbled value on disk: MemoryCapture = %q, want fail-closed default explicit", got.MemoryCapture)
	}
}
