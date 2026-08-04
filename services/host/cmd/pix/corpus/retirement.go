package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LoadRetirement parses a JSON-Lines retirement manifest (one RetirementEntry
// per line, blank lines ignored) in file order.
func LoadRetirement(path string) ([]RetirementEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("corpus: read retirement manifest %s: %w", path, err)
	}
	var entries []RetirementEntry
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e RetirementEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("corpus: %s line %d: %w", path, i+1, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// ValidateRetirement checks the manifest's schema as a whole: entries form a
// gap-free, in-order sequence starting at Seq 1 (the only valid edit to this
// file is appending the next Seq — see CheckAppendOnly for the git-history
// side of that same rule), every entry carries approval evidence, and no verb
// or (verb, flag) pair is retired twice.
func ValidateRetirement(entries []RetirementEntry) error {
	seen := map[string]bool{}
	for i, e := range entries {
		want := i + 1
		if e.Seq != want {
			return fmt.Errorf("entry %d: seq=%d, want %d (entries must be a gap-free, in-order, append-only sequence starting at 1)", i, e.Seq, want)
		}
		switch e.Granularity {
		case "verb":
			if e.Flag != "" {
				return fmt.Errorf("seq %d: granularity=verb but flag=%q is set (a verb-level retirement must not name a flag)", e.Seq, e.Flag)
			}
		case "flag":
			if strings.TrimSpace(e.Flag) == "" {
				return fmt.Errorf("seq %d: granularity=flag but flag is empty", e.Seq)
			}
		default:
			return fmt.Errorf("seq %d: unknown granularity %q (want \"verb\" or \"flag\")", e.Seq, e.Granularity)
		}
		if strings.TrimSpace(e.Verb) == "" {
			return fmt.Errorf("seq %d: empty verb", e.Seq)
		}
		if e.Status != "approved" {
			return fmt.Errorf("seq %d: status=%q, want \"approved\" (this manifest holds approved retirements only)", e.Seq, e.Status)
		}
		if strings.TrimSpace(e.ApprovedBy) == "" {
			return fmt.Errorf("seq %d: empty approvedBy", e.Seq)
		}
		if strings.TrimSpace(e.ApprovedAt) == "" {
			return fmt.Errorf("seq %d: empty approvedAt", e.Seq)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return fmt.Errorf("seq %d: empty reason", e.Seq)
		}
		key := e.Verb + "\x00" + e.Flag
		if seen[key] {
			return fmt.Errorf("seq %d: duplicate retirement for verb=%q flag=%q", e.Seq, e.Verb, e.Flag)
		}
		seen[key] = true
	}
	return nil
}

// RetiredVerbs returns the set of verbs retired at verb granularity (approved
// entries only).
func RetiredVerbs(entries []RetirementEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		if e.Granularity == "verb" && e.Status == "approved" {
			out[e.Verb] = true
		}
	}
	return out
}

// RetiredFlags returns verb -> set of retired flags, for approved
// flag-granularity entries.
func RetiredFlags(entries []RetirementEntry) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, e := range entries {
		if e.Granularity != "flag" || e.Status != "approved" {
			continue
		}
		if out[e.Verb] == nil {
			out[e.Verb] = map[string]bool{}
		}
		out[e.Verb][e.Flag] = true
	}
	return out
}

// CheckAppendOnly enforces that the manifest at path was only ever appended
// to: it compares the file as committed at HEAD~1 (the state before whatever
// most recent commit touched it) against the file's current on-disk content,
// and requires every previously-committed line to still be present,
// unchanged, in the same order, at the front of the current file.
//
// A file with no earlier committed revision to compare against (brand new,
// or the repo has no parent commit yet) is exempt — there is nothing to have
// mutated. A missing git binary or a directory outside any repo is exempt for
// the same reason: this is a guard against LATER edits, not a hard
// dependency on git being present everywhere this package builds.
func CheckAppendOnly(path string) error {
	dir := filepath.Dir(path)
	top, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil // not a git repo (or git unavailable): nothing to compare
	}
	top = strings.TrimSpace(top)

	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("corpus: resolve %s: %w", path, err)
	}
	rel, err := filepath.Rel(top, abs)
	if err != nil {
		return fmt.Errorf("corpus: relativize %s under %s: %w", abs, top, err)
	}
	rel = filepath.ToSlash(rel)

	prev, err := gitOutput(top, "show", "HEAD~1:"+rel)
	if err != nil {
		return nil // no earlier committed revision: bootstrap case
	}
	prevLines := nonEmptyLines(prev)

	cur, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("corpus: read %s: %w", abs, err)
	}
	curLines := nonEmptyLines(string(cur))

	if len(curLines) < len(prevLines) {
		return fmt.Errorf("retirement manifest %s shrank from %d to %d lines: entries were removed, which append-only forbids", path, len(prevLines), len(curLines))
	}
	for i, want := range prevLines {
		if curLines[i] != want {
			return fmt.Errorf("retirement manifest %s line %d no longer matches its previously committed content (entries are immutable once written):\n  was: %s\n  now: %s",
				path, i+1, want, curLines[i])
		}
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}
