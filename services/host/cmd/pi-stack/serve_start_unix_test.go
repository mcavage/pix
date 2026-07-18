//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H1: a symlinked serve.log must never be followed for WRITING — a planted
// `serve.log -> sensitive-file` would make the lazily-spawned daemon append its
// output into the target. openServeLogFile removes the symlink and creates a
// regular 0600 file; the target stays untouched.
func TestOpenServeLogFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("SECRET-CONTENT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "state", "serve.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, logPath); err != nil {
		t.Fatal(err)
	}

	f, err := openServeLogFile(logPath)
	if err != nil {
		t.Fatalf("openServeLogFile: %v", err)
	}
	if _, err := f.WriteString("daemon output\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// The victim file is untouched…
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "SECRET-CONTENT\n" {
		t.Errorf("victim file was written through the symlink: %q", got)
	}
	// …and the log path is now a REGULAR 0600 file holding the output.
	fi, err := os.Lstat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("log path is still a symlink")
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("log perms = %o, want 0600", perm)
	}
	logGot, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logGot), "daemon output") {
		t.Errorf("log content = %q, want the written output", logGot)
	}
}

// H1: the failed-start tail must never READ through a symlink either — it
// would echo the last lines of an attacker-chosen file to the terminal.
func TestTailFileLinesRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("line1\nSECRET-LAST-LINE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "serve.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := tailFileLines(link, 10); got != "" {
		t.Errorf("tailFileLines followed a symlink: %q", got)
	}
	// Sanity: a regular file still tails.
	regular := filepath.Join(dir, "regular.log")
	if err := os.WriteFile(regular, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailFileLines(regular, 2); got != "b\nc" {
		t.Errorf("regular tail = %q, want %q", got, "b\nc")
	}
}
