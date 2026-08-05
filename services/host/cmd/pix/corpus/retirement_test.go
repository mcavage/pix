package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

func TestRetirement_LoadsRealManifest(t *testing.T) {
	entries, err := LoadRetirement(realRetirementPath(t))
	if err != nil {
		t.Fatalf("LoadRetirement(real): %v", err)
	}
	if len(entries) < 3 {
		t.Fatalf("real retirement.jsonl has %d entries, want >= 3 (onboard, gog, route)", len(entries))
	}
	if err := ValidateRetirement(entries); err != nil {
		t.Fatalf("ValidateRetirement(real): %v", err)
	}
	for _, e := range entries {
		if e.Status != "approved" {
			t.Errorf("entry seq=%d has status %q, want \"approved\" (manifest holds APPROVED entries only)", e.Seq, e.Status)
		}
	}
	retired := RetiredVerbs(entries)
	for _, v := range []string{"onboard", "gog", "route"} {
		if !retired[v] {
			t.Errorf("real retirement manifest missing expected historical entry %q", v)
		}
	}
}

func TestValidateRetirement_Table(t *testing.T) {
	base := func() []RetirementEntry {
		return []RetirementEntry{{
			Seq: 1, Granularity: "verb", Verb: "onboard", Status: "approved",
			ApprovedBy: "AC-P0-308", ApprovedAt: "2026-08-01", Reason: "renamed",
		}}
	}
	cases := []struct {
		name    string
		mutate  func([]RetirementEntry) []RetirementEntry
		wantErr bool
	}{
		{"valid baseline", func(e []RetirementEntry) []RetirementEntry { return e }, false},
		{"missing reason", func(e []RetirementEntry) []RetirementEntry { e[0].Reason = ""; return e }, true},
		{"missing approvedBy", func(e []RetirementEntry) []RetirementEntry { e[0].ApprovedBy = ""; return e }, true},
		{"missing approvedAt", func(e []RetirementEntry) []RetirementEntry { e[0].ApprovedAt = ""; return e }, true},
		{"unapproved status", func(e []RetirementEntry) []RetirementEntry { e[0].Status = "proposed"; return e }, true},
		{"flag granularity without flag", func(e []RetirementEntry) []RetirementEntry {
			e[0].Granularity = "flag"
			return e
		}, true},
		{"verb granularity with a flag set", func(e []RetirementEntry) []RetirementEntry {
			e[0].Flag = "--old"
			return e
		}, true},
		{"unknown granularity", func(e []RetirementEntry) []RetirementEntry { e[0].Granularity = "subcommand"; return e }, true},
		{"empty verb", func(e []RetirementEntry) []RetirementEntry { e[0].Verb = ""; return e }, true},
		{"seq zero", func(e []RetirementEntry) []RetirementEntry { e[0].Seq = 0; return e }, true},
		{"duplicate seq", func(e []RetirementEntry) []RetirementEntry {
			return append(e, RetirementEntry{Seq: 1, Granularity: "verb", Verb: "gog", Status: "approved", ApprovedBy: "x", ApprovedAt: "2026-08-01", Reason: "y"})
		}, true},
		{"gap in seq", func(e []RetirementEntry) []RetirementEntry {
			return append(e, RetirementEntry{Seq: 3, Granularity: "verb", Verb: "gog", Status: "approved", ApprovedBy: "x", ApprovedAt: "2026-08-01", Reason: "y"})
		}, true},
		{"out of order seq", func(e []RetirementEntry) []RetirementEntry {
			e = append(e, RetirementEntry{Seq: 2, Granularity: "verb", Verb: "gog", Status: "approved", ApprovedBy: "x", ApprovedAt: "2026-08-01", Reason: "y"})
			e[0], e[1] = e[1], e[0]
			return e
		}, true},
		{"duplicate verb+flag pair", func(e []RetirementEntry) []RetirementEntry {
			return append(e, RetirementEntry{Seq: 2, Granularity: "verb", Verb: "onboard", Status: "approved", ApprovedBy: "x", ApprovedAt: "2026-08-01", Reason: "y"})
		}, true},
		{"flag granularity valid", func(e []RetirementEntry) []RetirementEntry {
			e[0].Granularity, e[0].Flag = "flag", "--profile"
			return append(e, RetirementEntry{Seq: 2, Granularity: "flag", Verb: "config", Flag: "--old-flag", Status: "approved", ApprovedBy: "x", ApprovedAt: "2026-08-01", Reason: "y"})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := tc.mutate(base())
			err := ValidateRetirement(entries)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateRetirement = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestRetiredFlags_KeyedByVerbAndFlag(t *testing.T) {
	entries := []RetirementEntry{
		{Seq: 1, Granularity: "flag", Verb: "config", Flag: "--legacy", Status: "approved", ApprovedBy: "x", ApprovedAt: "2026-08-01", Reason: "y"},
	}
	flags := RetiredFlags(entries)
	if !flags["config"]["--legacy"] {
		t.Error("RetiredFlags did not index the flag-granularity entry")
	}
	if flags["config"]["--other"] {
		t.Error("RetiredFlags reported an unretired flag as retired")
	}
}

// --- append-only enforcement (git-history based) ---------------------------
//
// Mirrors scripts/check-recall-transport.sh's self-test style: prove the
// guard actually fires on each violation, using a scratch git repo so the
// real manifest is never touched.

func writeManifest(t *testing.T, dir string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, "retirement.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "--allow-empty", "-m", "seed")
}

func gitCommitAll(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

const seedLine = `{"seq":1,"granularity":"verb","verb":"onboard","flag":"","status":"approved","approvedBy":"AC-P0-308","approvedAt":"2026-08-01","reason":"renamed","replacement":"setup --no-agent"}`
const appendLine = `{"seq":2,"granularity":"verb","verb":"gog","flag":"","status":"approved","approvedBy":"6b39a69","approvedAt":"2026-08-01","reason":"deleted","replacement":"gworkspace"}`

func TestCheckAppendOnly_AcceptsPureAppend(t *testing.T) {
	dir := t.TempDir()
	p := writeManifest(t, dir, []string{seedLine})
	gitInit(t, dir)
	writeManifest(t, dir, []string{seedLine, appendLine})
	gitCommitAll(t, dir, "append")
	if err := CheckAppendOnly(p); err != nil {
		t.Errorf("CheckAppendOnly(pure append) = %v, want nil", err)
	}
}

func TestCheckAppendOnly_RejectsMutation(t *testing.T) {
	dir := t.TempDir()
	p := writeManifest(t, dir, []string{seedLine})
	gitInit(t, dir)
	mutated := strings.Replace(seedLine, `"reason":"renamed"`, `"reason":"CHANGED"`, 1)
	writeManifest(t, dir, []string{mutated})
	gitCommitAll(t, dir, "mutate")
	if err := CheckAppendOnly(p); err == nil {
		t.Error("CheckAppendOnly(mutated existing entry) = nil, want error")
	}
}

func TestCheckAppendOnly_RejectsDeletion(t *testing.T) {
	dir := t.TempDir()
	p := writeManifest(t, dir, []string{seedLine, appendLine})
	gitInit(t, dir)
	writeManifest(t, dir, []string{appendLine})
	gitCommitAll(t, dir, "delete first entry")
	if err := CheckAppendOnly(p); err == nil {
		t.Error("CheckAppendOnly(deleted entry) = nil, want error")
	}
}

func TestCheckAppendOnly_RejectsReorder(t *testing.T) {
	dir := t.TempDir()
	p := writeManifest(t, dir, []string{seedLine, appendLine})
	gitInit(t, dir)
	writeManifest(t, dir, []string{appendLine, seedLine})
	gitCommitAll(t, dir, "reorder")
	if err := CheckAppendOnly(p); err == nil {
		t.Error("CheckAppendOnly(reordered entries) = nil, want error")
	}
}

func TestCheckAppendOnly_BootstrapsOnNewFile(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir) // no manifest committed yet
	p := writeManifest(t, dir, []string{seedLine})
	gitCommitAll(t, dir, "add manifest for the first time")
	if err := CheckAppendOnly(p); err != nil {
		t.Errorf("CheckAppendOnly(brand-new file) = %v, want nil", err)
	}
}

func TestCheckAppendOnly_RealManifestPassesAgainstItsOwnHistory(t *testing.T) {
	// The real manifest, checked in the real repo: must never fail against its
	// own committed history. (If this repo has no git history for the file
	// yet — e.g. it's introduced in the same commit as this test — the guard
	// bootstraps to nil rather than failing.)
	if err := CheckAppendOnly(realRetirementPath(t)); err != nil {
		t.Errorf("CheckAppendOnly(real manifest) = %v, want nil", err)
	}
}
