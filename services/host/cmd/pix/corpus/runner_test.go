package corpus

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// buildPixBinary compiles the real `pix` launcher to a temp path so cases run
// against the actual compiled artifact, not a mock. Built once per test binary
// run via t.Helper caching (sync.Once at package scope avoids one build per
// subtest — see BuildPixBinary).
func buildPixBinary(t *testing.T) string {
	t.Helper()
	bin, err := BuildPixBinary()
	if err != nil {
		t.Fatalf("BuildPixBinary: %v", err)
	}
	return bin
}

// TestCorpusCases_MatchRealBinary is the golden test: every case in every
// shipped shard is run against the real compiled `pix` binary and must match
// its recorded exit code / output contract exactly. This is the guard: change
// a verb's help text, exit code, or JSON keys without updating the corpus and
// this goes red.
func TestCorpusCases_MatchRealBinary(t *testing.T) {
	bin := buildPixBinary(t)
	root := repoRoot(t)
	shards, err := LoadShards(realShardsDir(t))
	if err != nil {
		t.Fatalf("LoadShards: %v", err)
	}
	for _, s := range shards {
		s := s
		t.Run(s.Verb, func(t *testing.T) {
			for _, c := range s.Cases {
				c := c
				t.Run(c.Name, func(t *testing.T) {
					home := t.TempDir()
					res, err := RunCase(bin, root, home, c)
					if err != nil {
						t.Fatalf("RunCase(%s %v): %v", s.Verb, c.Args, err)
					}
					if err := CheckCase(c, res); err != nil {
						t.Errorf("%s %v: %v\n--- stdout ---\n%s\n--- stderr ---\n%s",
							s.Verb, c.Args, err, res.Stdout, res.Stderr)
					}
				})
			}
		})
	}
}

func TestRunCase_ExitCodeAndStreams(t *testing.T) {
	bin := buildPixBinary(t)
	root := repoRoot(t)
	home := t.TempDir()
	res, err := RunCase(bin, root, home, Case{
		Name:     "version",
		Args:     []string{"version"},
		ExitCode: 0,
		Contains: []string{},
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if res.Stdout == "" {
		t.Error("stdout empty for `pix version`")
	}
}

func TestRunCase_IsolatesHome(t *testing.T) {
	// `pix config path` must resolve under the isolated $HOME passed to
	// RunCase, never a real user's config — this is the "no destructive
	// operations" guarantee: nothing this harness runs can touch real state.
	bin := buildPixBinary(t)
	root := repoRoot(t)
	home := t.TempDir()
	res, err := RunCase(bin, root, home, Case{
		Name:     "path",
		Args:     []string{"config", "path"},
		ExitCode: 0,
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	want := filepath.Join(home, ".config", "pix", "config.toml")
	got := res.Stdout
	if len(got) == 0 || got[len(got)-1] == '\n' {
		got = got[:max(0, len(got)-1)]
	}
	if got != want {
		t.Errorf("config path = %q, want %q (isolated HOME=%q)", got, want, home)
	}
}

func TestCheckCase_JSONKeysContract(t *testing.T) {
	c := Case{Name: "x", Args: []string{"y"}, ExitCode: 0, JSONKeys: []string{"Name", "Model"}}
	good := Result{ExitCode: 0, Stdout: mustJSON(t, []map[string]any{{"Name": "a", "Model": "b"}})}
	if err := CheckCase(c, good); err != nil {
		t.Errorf("CheckCase(matching keys) = %v, want nil", err)
	}
	missingKey := Result{ExitCode: 0, Stdout: mustJSON(t, []map[string]any{{"Name": "a"}})}
	if err := CheckCase(c, missingKey); err == nil {
		t.Error("CheckCase(missing key) = nil, want error")
	}
	extraKey := Result{ExitCode: 0, Stdout: mustJSON(t, []map[string]any{{"Name": "a", "Model": "b", "Extra": "c"}})}
	if err := CheckCase(c, extraKey); err == nil {
		t.Error("CheckCase(extra key) = nil, want error (a key rename/addition should be a reviewed corpus change)")
	}
	empty := Result{ExitCode: 0, Stdout: "[]"}
	if err := CheckCase(c, empty); err == nil {
		t.Error("CheckCase(empty array) = nil, want error (an empty result proves nothing)")
	}
}

func TestCheckCase_ExitCodeMismatch(t *testing.T) {
	c := Case{Name: "x", Args: []string{"y"}, ExitCode: 0}
	if err := CheckCase(c, Result{ExitCode: 2}); err == nil {
		t.Error("CheckCase(exit code mismatch) = nil, want error")
	}
}

func TestCheckCase_ContainsOnWrongStream(t *testing.T) {
	c := Case{Name: "x", Args: []string{"y"}, ExitCode: 0, Stream: "stderr", Contains: []string{"boom"}}
	if err := CheckCase(c, Result{ExitCode: 0, Stdout: "boom", Stderr: ""}); err == nil {
		t.Error("CheckCase should check the declared stream (stderr), not stdout")
	}
	if err := CheckCase(c, Result{ExitCode: 0, Stderr: "boom"}); err != nil {
		t.Errorf("CheckCase(match on declared stream) = %v, want nil", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// dangerousVerbs is the flat set of pix verbs whose corpus shard may ONLY
// exercise -h/--help/bad-flag cases (never actually launch/mutate/reach the
// network). Keep this in sync with runVerb's switch in main.go: any verb that
// mutates on-disk state, spawns a sandbox, or touches the network belongs
// here, not just the ones that happen to have a shard today.
var dangerousVerbs = map[string]bool{
	"run": true, "reset": true, "rm": true, "restore": true, "backup": true,
	"task": true, "setup": true,
}

// groupedDangerousSubcommands maps a "grouping" verb (one whose subcommands
// fan out to already-dangerous flat aliases, e.g. `pix state reset` runs the
// exact same runReset as the top-level `pix reset` — see state.go) to the set
// of its OWN subcommands that are dangerous. A grouping verb itself is safe
// to invoke bare or with -h (it only prints group usage), so it is
// deliberately absent from dangerousVerbs; only once the subcommand token
// resolves to a dangerous one does the same safe-tail rule apply, starting
// one position later.
var groupedDangerousSubcommands = map[string]map[string]bool{
	"state": {"backup": true, "restore": true, "reset": true},
}

// safeTailArg reports whether a single argv token, appearing after a
// resolved dangerous verb (or grouped dangerous subcommand), is safe: it can
// only ever print help or a usage error, never launch/mutate/reach the
// network.
func safeTailArg(a string) bool {
	return a == "--help" || a == "-h" || a == "--this-is-not-a-real-flag-9x7z"
}

// dangerousArgvViolation returns a non-empty reason if args reaches a
// dangerous verb (flat, e.g. `reset`, or a grouped subcommand, e.g. `state
// reset`) with anything beyond a safe -h/--help/bad-flag tail. An empty args,
// or a verb/subcommand not in either dangerous set, or a dangerous verb with
// no tail at all, is not a violation (matches the existing `rm` bare-argument
// corpus case, whose bare invocation is a "missing argument" usage error, not
// a mutation).
func dangerousArgvViolation(args []string) string {
	if len(args) == 0 {
		return ""
	}
	verb, tail := args[0], args[1:]
	if subs, ok := groupedDangerousSubcommands[verb]; ok {
		if len(args) < 2 || !subs[args[1]] {
			// Bare group noun, group -h/--help, or an unresolved/unknown
			// subcommand — none of these dispatch to a dangerous action.
			return ""
		}
		verb, tail = verb+" "+args[1], args[2:]
	} else if !dangerousVerbs[verb] {
		return ""
	}
	for _, a := range tail {
		if !safeTailArg(a) {
			return fmt.Sprintf("touches a destructive verb %q with a non-safe argument %q", verb, a)
		}
	}
	return ""
}

// TestDangerousArgvViolation locks the classifier's contract directly
// (independent of whatever cases the shipped corpus happens to contain
// today), including the two gaps W0 review found: "setup" was missing from
// the flat set entirely, and a grouping verb's dangerous subcommand (`state
// reset`/`state restore`/`state backup`) was invisible because the original
// guard only ever inspected args[0].
func TestDangerousArgvViolation(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantViolation bool
	}{
		{"empty", nil, false},
		{"safe verb untouched", []string{"config", "show"}, false},
		{"reset help", []string{"reset", "--help"}, false},
		{"reset bad-flag", []string{"reset", "--this-is-not-a-real-flag-9x7z"}, false},
		{"reset bare", []string{"reset"}, false},
		{"reset real flag mutates", []string{"reset", "--yes"}, true},
		{"rm bare is a usage error, not a mutation", []string{"rm"}, false},
		{"setup help", []string{"setup", "--help"}, false},
		{"setup bad-flag", []string{"setup", "--this-is-not-a-real-flag-9x7z"}, false},
		{"setup with a real dir launches provisioning", []string{"setup", "."}, true},
		{"state group bare", []string{"state"}, false},
		{"state group help", []string{"state", "--help"}, false},
		{"state bad-invocation", []string{"state", "--this-is-not-a-real-flag-9x7z"}, false},
		{"state reset help is fine", []string{"state", "reset", "--help"}, false},
		{"state reset with a real flag mutates", []string{"state", "reset", "--yes"}, true},
		{"state restore with an archive path restores", []string{"state", "restore", "/tmp/x.tar.gz"}, true},
		{"state backup with an out path writes", []string{"state", "backup", "--out", "/tmp/x"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dangerousArgvViolation(c.args) != ""
			if got != c.wantViolation {
				t.Errorf("dangerousArgvViolation(%v) violation=%v, want %v", c.args, got, c.wantViolation)
			}
		})
	}
}

// sanity: this package must never shell out to anything but the built pix
// binary — no docker/sbx/network client. This is a text-level guard against a
// case sneaking in a destructive/network argv (e.g. "reset", "rm --all", "run",
// "state reset --yes", "setup .").
func TestShards_ForbidDangerousArgvPrefixes(t *testing.T) {
	shards, err := LoadShards(realShardsDir(t))
	if err != nil {
		t.Fatalf("LoadShards: %v", err)
	}
	for _, s := range shards {
		for _, c := range s.Cases {
			if reason := dangerousArgvViolation(c.Args); reason != "" {
				t.Errorf("shard %q case %q %s", s.Verb, c.Name, reason)
			}
		}
	}
}
