package corpus

import (
	"bytes"
	"encoding/json"
	"errors"
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
// to: it walks the commits that actually touched this path — the two most
// recent are [tip, previous] — and requires every line the PREVIOUS one
// committed to still be present, unchanged, in the same order, at the front
// of the file's current on-disk content.
//
// Deliberately not "HEAD~1": on a long-lived branch that carries merges (this
// repo's own history does), HEAD~1 follows the first-parent chain only, which
// can walk right past the commit that actually last touched this file — a
// false "nothing to compare" that would let a mutation landing on the other
// side of a merge through undetected. `git log -- path` instead walks every
// commit that touched the path, in any parent, so the comparison is anchored
// to the file's own history rather than to HEAD's immediate ancestry.
//
// A file with fewer than two such commits (brand new, or introduced in the
// very commit under test) is exempt — there is nothing to have mutated yet;
// so is a directory that git reports is not inside any repository, or a host
// with no git binary at all: this is a guard against LATER edits, not a hard
// dependency on git being present everywhere this package builds. Every OTHER
// git failure (a corrupt repo, a bad ref, a permission error) is a real error
// and is returned as one — it must never be folded into the same nil as
// "nothing to compare", or a git failure silently passes a mutation that a
// working git would have caught.
func CheckAppendOnly(path string) error {
	dir := filepath.Dir(path)
	topRes, err := runGit(dir, "rev-parse", "--show-toplevel")
	switch {
	case topRes.exempt:
		return nil // not a git repo, or git itself is unavailable: nothing to compare
	case err != nil:
		return fmt.Errorf("corpus: determine git toplevel for %s: %w", dir, err)
	}
	top := strings.TrimSpace(topRes.out)

	// Resolve symlinks before computing the path relative to top: git's
	// --show-toplevel always answers with the physical (symlink-resolved)
	// path, but `path` may still carry an unresolved one (e.g. a macOS
	// /tmp -> /private/tmp temp dir). Comparing an unresolved abs path against
	// a resolved top silently produces a bogus "rel" (a pile of ".." that does
	// not name the file in the repo at all), which makes every git lookup on
	// it fail — and a caller that treats every such failure as "bootstrap"
	// would then rubber-stamp any mutation. Resolve here instead so `rel` is
	// always the repo path git itself would recognize.
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("corpus: resolve %s: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("corpus: resolve symlinks for %s: %w", abs, err)
	}
	rel, err := filepath.Rel(top, resolved)
	if err != nil {
		return fmt.Errorf("corpus: relativize %s under %s: %w", resolved, top, err)
	}
	rel = filepath.ToSlash(rel)

	logRes, err := runGit(top, "log", "--format=%H", "-n", "2", "--", rel)
	switch {
	case logRes.exempt:
		return nil
	case err != nil:
		return fmt.Errorf("corpus: list commit history for %s: %w", rel, err)
	}
	hashes := nonEmptyLines(logRes.out)
	if len(hashes) < 2 {
		return nil // fewer than two commits ever touched this file: bootstrap case
	}

	prevRes, err := runGit(top, "show", hashes[1]+":"+rel)
	if err != nil {
		// hashes[1] came straight out of `git log` above for this exact path,
		// so a failure reading it back is a real error, never "no earlier
		// revision" — that case was already ruled out by the len(hashes) < 2
		// check.
		return fmt.Errorf("corpus: read %s at %s: %w", rel, hashes[1], err)
	}
	prevLines := nonEmptyLines(prevRes.out)

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

// gitResult is one bounded git invocation's classified outcome.
type gitResult struct {
	out string
	// exempt marks the two conditions CheckAppendOnly treats as "nothing to
	// compare" rather than a failure: git itself is not on PATH, or dir is
	// not inside any git repository. Every other non-nil error is real.
	exempt bool
}

// gitSanitizedEnv returns the current process environment with every
// GIT_-prefixed variable stripped: GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE,
// GIT_CEILING_DIRECTORIES and friends all override where git thinks the
// repository and working tree ARE, and they win over -C / cmd.Dir. Leaving
// one inherited from whatever wrapped the test process (a hook, a worktree
// helper, a prior GIT_DIR export left in a shell) means this guard can silently
// run every git command against a different repository than the scratch one
// (or the real one) it was just told to check — the append-only property
// would then be checked against the wrong history, or against none at all.
func gitSanitizedEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// runGit execs git under dir with a sanitized environment (see
// gitSanitizedEnv) and classifies the outcome: a genuine "not a git
// repository" or a missing git binary is exempt (CheckAppendOnly's callers
// treat that as nothing-to-compare); anything else that fails is returned as
// a real error, stderr and all, so a failure can never be mistaken for one of
// the two narrow conditions that are allowed to pass silently.
func runGit(dir string, args ...string) (gitResult, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitSanitizedEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return gitResult{out: stdout.String()}, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return gitResult{exempt: true}, nil
	}
	msg := strings.TrimSpace(stderr.String())
	if strings.Contains(msg, "not a git repository") {
		return gitResult{exempt: true}, nil
	}
	return gitResult{}, fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), dir, err, msg)
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

// gitTestEnv is gitSanitizedEnv plus a fixed, non-interactive identity: these
// scratch repos never touch the user's real gitconfig, and (like
// gitSanitizedEnv) never inherit a GIT_DIR/GIT_WORK_TREE/etc. from whatever
// wraps the test process — every commit these helpers make must land in the
// scratch repo they just created in dir, never in one named by an inherited
// variable.
func gitTestEnv() []string {
	return append(gitSanitizedEnv(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitTestEnv()
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
		cmd.Env = gitTestEnv()
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
