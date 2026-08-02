package main

import (
	"bufio"
	"pix/host/secret"
	"strings"
	"testing"
)

// secret.ScanYN's contract (item 3): (line, ok). ok is false ONLY on a genuine scan
// failure (EOF or a Scanner error, including an oversized token past
// bufio.Scanner's default buffer) — NEVER treated as consent by any caller. A
// blank (Enter-only) line is a legitimate answer: ok=true, line="".

func TestScanYN_BlankLineIsOkWithEmptyAnswer(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("\n"))
	line, ok := secret.ScanYN(sc)
	if !ok {
		t.Fatal("a blank line must scan successfully (ok=true)")
	}
	if line != "" {
		t.Errorf("line = %q, want empty (caller applies its own default)", line)
	}
}

func TestScanYN_YesAndNoAnswers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"y\n", "y"},
		{"Y\n", "y"},
		{"yes\n", "yes"},
		{"n\n", "n"},
		{"No\n", "no"},
	} {
		sc := bufio.NewScanner(strings.NewReader(tc.in))
		line, ok := secret.ScanYN(sc)
		if !ok {
			t.Fatalf("input %q: expected ok=true", tc.in)
		}
		if line != tc.want {
			t.Errorf("input %q: line = %q, want %q", tc.in, line, tc.want)
		}
	}
}

// EOF (no input at all, not even a blank line) is a scan failure, NEVER
// consent — this is the exact bug the (answer, ok) API replaces: the old
// bool-returning secret.ScanYN(sc, true) collapsed EOF into "return the default",
// which for the reconcile overwrite prompt's default-YES meant a broken/EOF'd
// stdin was silently read as "yes, replace my sbx secrets".
func TestScanYN_EOFIsNotConsent(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader(""))
	line, ok := secret.ScanYN(sc)
	if ok {
		t.Fatal("EOF must report ok=false, never true")
	}
	if line != "" {
		t.Errorf("line = %q on EOF, want empty", line)
	}
}

// An oversized token (past bufio.Scanner's default ~64KB buffer) is a genuine
// Scanner error, not EOF, but must be treated identically by every caller:
// ok=false, not consent.
func TestScanYN_OversizedTokenIsScannerErrorNotConsent(t *testing.T) {
	huge := strings.Repeat("y", 128*1024) // well past bufio.MaxScanTokenSize default use
	sc := bufio.NewScanner(strings.NewReader(huge))
	line, ok := secret.ScanYN(sc)
	if ok {
		t.Fatal("an oversized token must report ok=false")
	}
	if line != "" {
		t.Errorf("line = %q on scanner error, want empty", line)
	}
	if sc.Err() == nil {
		t.Fatal("sanity: expected bufio.Scanner to report a real error (ErrTooLong) for the oversized token")
	}
}

// A second Scan() after EOF also reports ok=false (no state corruption/panic).
func TestScanYN_RepeatedCallsAfterEOFStayFalse(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader(""))
	for i := 0; i < 3; i++ {
		if _, ok := secret.ScanYN(sc); ok {
			t.Fatalf("call %d: expected ok=false after EOF", i)
		}
	}
}
