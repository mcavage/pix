package corpus

import (
	"encoding/json"
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

// sanity: this package must never shell out to anything but the built pix
// binary — no docker/sbx/network client. This is a text-level guard against a
// case sneaking in a destructive/network argv (e.g. "reset", "rm --all", "run").
func TestShards_ForbidDangerousArgvPrefixes(t *testing.T) {
	shards, err := LoadShards(realShardsDir(t))
	if err != nil {
		t.Fatalf("LoadShards: %v", err)
	}
	dangerous := map[string]bool{"run": true, "reset": true, "rm": true, "restore": true, "backup": true, "task": true}
	for _, s := range shards {
		for _, c := range s.Cases {
			if len(c.Args) == 0 {
				continue
			}
			if dangerous[c.Args[0]] {
				// A dangerous verb's shard may ONLY exercise -h/--help/bad-flag
				// cases (never actually launch/mutate anything).
				for _, a := range c.Args[1:] {
					if a != "--help" && a != "-h" && a != "--this-is-not-a-real-flag-9x7z" {
						t.Errorf("shard %q case %q touches a destructive verb with a non-safe argument %q", s.Verb, c.Name, a)
					}
				}
			}
		}
	}
}

