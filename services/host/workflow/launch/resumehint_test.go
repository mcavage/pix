package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResumeHintOnlyWhenThereIsSomethingToResume.
//
// pi prints "To resume this session: pi --session-dir .pi-sessions --session
// <id>" as it exits. Inside the sandbox every word of that is true; on the host
// the user is left holding it, the sandbox is gone, `pi` is not installed, and
// the path is relative to a directory that no longer exists.
//
// The tempting fix is to suppress it. That would be wrong: .pi-sessions lives in
// the WORKSPACE, which is a host bind-mount, so the session really did outlive
// the sandbox. Only the dialect is wrong. So pix prints the host command, and
// only when a session file provably exists.
func TestResumeHintOnlyWhenThereIsSomethingToResume(t *testing.T) {
	t.Run("persisted session prints a host-runnable command", func(t *testing.T) {
		ws := t.TempDir()
		dir := filepath.Join(ws, sessionDirName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "2026-08-09T14-22-03-608Z_019fe6e6.jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		printResumeHint(&buf, ws)
		got := buf.String()
		if !strings.Contains(got, "pix run -- -c") {
			t.Errorf("hint must name the command that works on the HOST, got %q", got)
		}
		if strings.Contains(got, "--session-dir") {
			t.Errorf("hint must not repeat pi's in-sandbox spelling, got %q", got)
		}
	})

	t.Run("no session dir says nothing", func(t *testing.T) {
		var buf bytes.Buffer
		printResumeHint(&buf, t.TempDir())
		if buf.Len() != 0 {
			t.Errorf("nothing was persisted; must stay silent, got %q", buf.String())
		}
	})

	t.Run("empty session dir says nothing", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, sessionDirName), 0o700); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		printResumeHint(&buf, ws)
		if buf.Len() != 0 {
			t.Errorf("a session dir with no sessions must stay silent, got %q", buf.String())
		}
	})

	t.Run("unknown workspace says nothing", func(t *testing.T) {
		var buf bytes.Buffer
		printResumeHint(&buf, "")
		if buf.Len() != 0 {
			t.Errorf("no workspace: must stay silent, got %q", buf.String())
		}
	})
}

// TestReportTeardownHintsOnlyOnRemoval: a KEPT sandbox is still there, so its
// session is reachable by just running pix again. The hint belongs to the case
// where the sandbox went away.
func TestReportTeardownHintsOnlyOnRemoval(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, sessionDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var removed, kept bytes.Buffer
	reportTeardown(&removed, TeardownResult{Verdict: TeardownRemoved, Detail: "removed x"}, ws)
	reportTeardown(&kept, TeardownResult{Verdict: TeardownKeptBusy, Detail: "still referenced", Sandbox: "x"}, ws)
	if !strings.Contains(removed.String(), "pix run -- -c") {
		t.Errorf("a removed sandbox should offer the resume command, got %q", removed.String())
	}
	if strings.Contains(kept.String(), "pix run -- -c") {
		t.Errorf("a kept sandbox is still there; no resume hint needed, got %q", kept.String())
	}
}
