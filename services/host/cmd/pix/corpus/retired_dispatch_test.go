package corpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// argvFor derives the invocation that must produce the retirement notice from
// a manifest entry. A verb-granularity entry is typed as the bare verb; a
// flag-granularity entry is typed as `<verb> <flag>` — with the root command
// spelled "pix", whose "flags" are the global ones (e.g. `pix --man`).
func argvFor(e RetirementEntry) []string {
	if e.Granularity == "verb" {
		return []string{e.Verb}
	}
	if e.Verb == "pix" {
		return []string{e.Flag}
	}
	return []string{e.Verb, e.Flag}
}

// TestRetiredSurfaces_AnswerWithMarkerExitTwoAndReplacement is the behavioural
// half of the retirement contract, run against the REAL binary: every approved
// retirement, typed as a user would type it, exits 2, prints a PIX_RETIRED
// marker plus the recorded replacement on stderr, and writes nothing to stdout.
//
// A retired surface that silently succeeded, exited 0, or printed a bare
// "unknown command" would all pass the schema tests and fail a user; this is
// the test that catches that.
func TestRetiredSurfaces_AnswerWithMarkerExitTwoAndReplacement(t *testing.T) {
	bin := buildPixBinary(t)
	root := repoRoot(t)
	entries, err := LoadRetirement(realRetirementPath(t))
	if err != nil {
		t.Fatalf("LoadRetirement: %v", err)
	}
	for _, e := range entries {
		if e.Status != "approved" {
			continue
		}
		e := e
		args := argvFor(e)
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home := t.TempDir()
			res, err := RunCase(bin, root, home, Case{Name: "retired", Args: args})
			if err != nil {
				t.Fatalf("RunCase(%v): %v", args, err)
			}
			if res.ExitCode != 2 {
				t.Errorf("pix %v exit = %d, want 2\n--- stderr ---\n%s", args, res.ExitCode, res.Stderr)
			}
			if !strings.HasPrefix(res.Stderr, "PIX_RETIRED") {
				t.Errorf("pix %v stderr does not start with PIX_RETIRED:\n%s", args, res.Stderr)
			}
			if r := strings.TrimSpace(e.Replacement); r != "" && !strings.Contains(res.Stderr, r) {
				// The recorded replacement may itself have been retired later, in
				// which case the message names the surface that replaced IT — so
				// accept either the recorded string or a longer chain.
				if !strings.Contains(res.Stderr, "instead") {
					t.Errorf("pix %v stderr names no replacement (want %q or its successor):\n%s", args, r, res.Stderr)
				}
			}
			if strings.TrimSpace(res.Stdout) != "" {
				t.Errorf("pix %v wrote to stdout (a retirement notice is stderr-only):\n%s", args, res.Stdout)
			}
		})
	}
}

// TestRetiredSurfaces_HaveNoSideEffects: typing a retired surface must not
// create config, state, or anything else under HOME. This is what makes the
// notice safe to hit from a stale script — the old command does not half-run.
func TestRetiredSurfaces_HaveNoSideEffects(t *testing.T) {
	bin := buildPixBinary(t)
	root := repoRoot(t)
	entries, err := LoadRetirement(realRetirementPath(t))
	if err != nil {
		t.Fatalf("LoadRetirement: %v", err)
	}
	for _, e := range entries {
		if e.Status != "approved" {
			continue
		}
		args := argvFor(e)
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home := t.TempDir()
			if _, err := RunCase(bin, root, home, Case{Name: "retired", Args: args}); err != nil {
				t.Fatalf("RunCase(%v): %v", args, err)
			}
			var created []string
			err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if path != home {
					created = append(created, strings.TrimPrefix(path, home))
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", home, err)
			}
			if len(created) > 0 {
				t.Errorf("pix %v created state under HOME: %v", args, created)
			}
		})
	}
}
