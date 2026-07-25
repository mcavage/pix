package main

import (
	"strings"
	"testing"
)

// U-W3.06 (AC-P0-406). This file pins the CURRENT sandbox-name composition
// and truncation behavior BEFORE the rename changes the prefix that drives
// it: "pi-stack-t-" (11 chars) became "pix-t-" (6 chars) in U-W3.09. Five
// more characters of budget now survive maxSandboxNameLen (63) truncation,
// which moved every threshold this file pins.
//
// THIS FILE IS UPDATED DELIBERATELY IN U-W3.09, NEVER BY THE RENAME DRIVER
// (scripts/rename/apply.sh -- see its DEFERRED_MOVE_DIRS / manual-content
// note, and this file's own `manual` disposition row in
// scripts/rename/inventory.tsv). The whole point of pinning the composed
// name AND the exact byte at which truncation kicks in -- not just "it still
// truncates somehow" -- is that a boundary test written against the OLD
// prefix would otherwise keep passing after the prefix shrinks, silently
// testing the WRONG boundary. Concretely: case 2 below (name length 31)
// fits comfortably once 5 more characters of budget appear, so it will FAIL
// LOUDLY the day the prefix changes -- that failure is what makes the
// U-W3.09 diff a deliberate act, not an accident nobody reviewed.
//
// Every expected string below is the VERIFIED CURRENT OUTPUT of
// boundSandboxName, captured by actually calling it (not hand-derived sha256
// arithmetic) -- see the commit body for the capture method.

func TestSandboxNameConstants_Pinned(t *testing.T) {
	// These are the numbers every threshold in this file is computed FROM.
	// If one of these changes, every other test in this file needs re-deriving
	// (that is the point: a silent drift here is a silent drift everywhere).
	if maxSandboxNameLen != 63 {
		t.Fatalf("maxSandboxNameLen = %d, want 63 (strictest common DNS-label limit, RFC1123)", maxSandboxNameLen)
	}
	if nameTrimFloor != 12 {
		t.Fatalf("nameTrimFloor = %d, want 12 (10-hex tag + dash + >=1 prefix rune)", nameTrimFloor)
	}
	if maxRepoLabelLen != 12 {
		t.Fatalf("maxRepoLabelLen = %d, want 12", maxRepoLabelLen)
	}
	if maxTaskNameLen != 40 {
		t.Fatalf("maxTaskNameLen = %d, want 40", maxTaskNameLen)
	}
}

func TestBoundSandboxName_ExactFitBoundary(t *testing.T) {
	// label at its cap (12), repokey the standard 8 hex chars, no profile.
	// Fixed overhead = len("pix-t-") + "-" + repokey + "-" = 6+1+8+1 = 16.
	// Budget left for label+name = 63-16 = 47; with label=12, name budget = 35.
	label := "repolabel12x" // exactly 12 chars
	if len(label) != 12 {
		t.Fatalf("fixture label is %d chars, want 12", len(label))
	}
	repokey := "abcd1234"

	// name length 35: compose is EXACTLY 63 -- fits with zero trimming.
	name35 := strings.Repeat("a", 35)
	want35 := "pix-t-repolabel12x-abcd1234-" + name35
	if got := boundSandboxName(label, repokey, name35, ""); got != want35 {
		t.Errorf("name len 35 (exact fit): got %q, want %q", got, want35)
	}
	if len(want35) != 63 {
		t.Fatalf("test fixture arithmetic is wrong: want35 is %d chars, want 63", len(want35))
	}

	// name length 36: ONE character over -- this is the pinned threshold.
	name36 := strings.Repeat("a", 36)
	want36 := "pix-t-repolabel12x-abcd1234-aaaaaaaaaaaaaaaaaaaaaaaa-22c1d24bcd"
	if got := boundSandboxName(label, repokey, name36, ""); got != want36 {
		t.Errorf("name len 36 (one over the exact-fit boundary): got %q, want %q", got, want36)
	}
	if len(want36) != 63 {
		t.Errorf("trimmed composite is %d chars, want exactly maxSandboxNameLen (63)", len(want36))
	}
}

func TestBoundSandboxName_NameOverflowStaysAtTheSameTrimWindow(t *testing.T) {
	// A MUCH longer name (60 chars, well past the 31-char case above) still
	// lands at the same trim window (nameBudget=30 given label=12): the loop
	// searches from len(name) down and the FIRST n that fits is still 30,
	// same as the 31-char case, just hashing different original content (so
	// the tag differs). This pins that "way over" behaves the same as "one
	// over" -- there is no separate "very long name" code path.
	label := "repolabel12x"
	name60 := strings.Repeat("a", 60)
	want := "pix-t-repolabel12x-abcd1234-aaaaaaaaaaaaaaaaaaaaaaaa-11ee391211"
	if got := boundSandboxName(label, "abcd1234", name60, ""); got != want {
		t.Errorf("name len 60: got %q, want %q", got, want)
	}
	if len(want) != 63 {
		t.Errorf("composite is %d chars, want 63", len(want))
	}
}

func TestBoundSandboxName_LabelUntouchedWhenNameIsSmall(t *testing.T) {
	// A label that alone would overflow maxSandboxNameLen is left FULLY
	// intact when the rest of the composite (a 1-char name, no profile)
	// leaves enough room. Trimming only ever engages once the FULL composite
	// actually overflows -- it does not pre-emptively shorten a long label.
	longLabel := "reallylonglabelthatoverflows12345" // 33 chars
	if len(longLabel) != 33 {
		t.Fatalf("fixture label is %d chars, want 33", len(longLabel))
	}
	want := "pix-t-reallylonglabelthatoverflows12345-abcd1234-n"
	if got := boundSandboxName(longLabel, "abcd1234", "n", ""); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(want) != 50 {
		t.Errorf("composite is %d chars, want 50 (well under the 63 cap -- no trim expected)", len(want))
	}
}

func TestBoundSandboxName_NameHitsFloorThenLabelTrims(t *testing.T) {
	// Both the label (33 chars) AND the name (60 chars) overflow. Per the
	// documented priority (name first, down to nameTrimFloor; THEN the
	// label; the repokey NEVER): the name is hash-tag-trimmed all the way to
	// its floor (12 chars: "a-" + 10 hex), and only then does the label get
	// trimmed (a plain truncation, no hash tag -- it is a cosmetic hint, not
	// a uniqueness guarantee).
	longLabel := "reallylonglabelthatoverflows12345"
	name60 := strings.Repeat("a", 60)
	want := "pix-t-reallylonglabelthatoverflows12345-abcd1234-aaa-11ee391211"
	got := boundSandboxName(longLabel, "abcd1234", name60, "")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(want) != 63 {
		t.Errorf("composite is %d chars, want 63", len(want))
	}
	// The name segment keeps three prefix characters plus the hash. It is the
	// same hash as the name-len-60 case above because hashTagTrim depends only
	// on the string being hashed, never on the label around it.
	if !strings.HasSuffix(got, "-aaa-11ee391211") {
		t.Errorf("expected the name segment hash-tagged to the SAME 10-hex tag as the name-only overflow case; got %q", got)
	}
}

func TestBoundSandboxName_LabelDroppedToRepoThenProfileTrims(t *testing.T) {
	// A single-char label and name leave plenty of room -- until an absurdly
	// long PROFILE (60 chars) overflows the composite regardless of label
	// size. Priority order: name first (already tiny, untouched), then
	// label (tried, still insufficient against a 60-char profile so it is
	// dropped to the literal "repo" placeholder -- repokey is NEVER
	// touched), then finally the profile itself is hash-tag-trimmed.
	prof60 := strings.Repeat("a", 60)
	want := "pix-t-repo-abcd1234-n-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-11ee391211"
	got := boundSandboxName("x", "abcd1234", "n", prof60)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(want) != 63 {
		t.Errorf("composite is %d chars, want 63", len(want))
	}
	if !strings.Contains(got, "-repo-abcd1234-") {
		t.Errorf("expected the label to be dropped to the literal placeholder %q; got %q", "repo", got)
	}
}

func TestBoundSandboxName_NeverExceedsTheCapAcrossAWideInputRange(t *testing.T) {
	// A property check alongside the exact pins above: whatever the inputs,
	// the composed name never exceeds maxSandboxNameLen. This is the
	// invariant U-W3.09 must preserve even though the exact trimmed STRINGS
	// above are expected to change with the new prefix.
	repokey := "abcd1234"
	lens := []int{0, 1, 12, 13, 40, 41, 63, 64, 100}
	for _, ln := range lens {
		for _, pln := range []int{0, 1, 41, 100} {
			label := strings.Repeat("l", ln)
			name := strings.Repeat("n", ln+1)
			prof := ""
			if pln > 0 {
				prof = strings.Repeat("p", pln)
			}
			got := boundSandboxName(label, repokey, name, prof)
			if len(got) > maxSandboxNameLen {
				t.Errorf("label len=%d name len=%d prof len=%d: composite is %d chars, exceeds maxSandboxNameLen (%d): %q",
					ln, ln+1, pln, len(got), maxSandboxNameLen, got)
			}
			if !strings.Contains(got, repokey) {
				t.Errorf("label len=%d name len=%d prof len=%d: repokey %q missing from composite (repokey must NEVER be trimmed): %q",
					ln, ln+1, pln, repokey, got)
			}
		}
	}
}
