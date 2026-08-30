package corpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

var (
	buildBinOnce sync.Once
	buildBinPath string
	buildBinErr  error
)

// buildPixBinary compiles the real `pix` launcher (this package's parent
// directory) once per test process — every caller in the same `go test` run
// shares the same compiled binary, so a large corpus does not pay a `go
// build` per case. It exercises the actual shipped artifact, not a mock,
// which is the whole point of a golden CLI corpus.
//
// It manages its own scratch directory (outside any single test's t.TempDir())
// because the build is cached process-wide via sync.Once: if it lived inside
// the FIRST caller's t.TempDir(), that directory would be removed the moment
// that one test finished, breaking the binary path for every later test.
func buildPixBinary(t *testing.T) string {
	t.Helper()
	buildBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pix-corpus-bin-")
		if err != nil {
			buildBinErr = fmt.Errorf("corpus: scratch dir for pix build: %w", err)
			return
		}
		bin := dir + "/pix"
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = ".." // services/host/cmd/pix, this package's parent
		out, cmderr := cmd.CombinedOutput()
		if cmderr != nil {
			buildBinErr = fmt.Errorf("go build pix: %w\n%s", cmderr, out)
			return
		}
		buildBinPath = bin
	})
	if buildBinErr != nil {
		t.Fatalf("build pix binary: %v", buildBinErr)
	}
	return buildBinPath
}

// RunCase executes one Case against bin as a real subprocess: cwd=repoRoot
// (so verbs that read repo-relative state, e.g. `agent ls` reading ./agents,
// resolve the same way they do for a real user), and a fully isolated,
// per-call HOME so nothing this harness runs can read or write real user
// state — that is what makes even a "safe read-only" case safe to run
// unattended in CI.
func RunCase(bin, repoRoot, home string, c Case) (Result, error) {
	cmd := exec.Command(bin, c.Args...)
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		res.ExitCode = 0
		return res, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("run %s %v: %w", bin, c.Args, err)
}

// CheckCase asserts a Result satisfies a Case's contract, returning one error
// describing every mismatch (not just the first) so a failing corpus run
// tells the whole story in one read.
func CheckCase(c Case, r Result) error {
	var problems []string

	if r.ExitCode != c.ExitCode {
		problems = append(problems, fmt.Sprintf("exit code = %d, want %d", r.ExitCode, c.ExitCode))
	}

	stream := r.Stdout
	streamName := "stdout"
	if c.Stream == "stderr" {
		stream, streamName = r.Stderr, "stderr"
	}

	for _, want := range c.Contains {
		if !strings.Contains(stream, want) {
			problems = append(problems, fmt.Sprintf("%s does not contain %q", streamName, want))
		}
	}

	if len(c.JSONKeys) > 0 {
		if err := checkJSONKeys(stream, c.JSONKeys); err != nil {
			problems = append(problems, err.Error())
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// checkJSONKeys accepts either a JSON array of objects (a listing verb) or a
// single JSON object (a report verb, e.g. `status --json`), treating the
// latter as a one-row array. Both are "rows with an exact key set", which is
// what the contract guards; refusing the object form only meant report verbs
// went uncovered.
func checkJSONKeys(stream string, want []string) error {
	rows, err := jsonRows(stream)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("jsonKeys contract: output parsed as an empty JSON array (proves nothing)")
	}
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for i, row := range rows {
		if len(row) != len(wantSet) {
			return fmt.Errorf("jsonKeys contract: row %d has keys %v, want exactly %v", i, sortedKeys(row), want)
		}
		for k := range row {
			if !wantSet[k] {
				return fmt.Errorf("jsonKeys contract: row %d has unexpected key %q (want exactly %v)", i, k, want)
			}
		}
	}
	return nil
}

// jsonRows parses stream as an array of objects, falling back to a single
// object.
func jsonRows(stream string) ([]map[string]json.RawMessage, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stream), &rows); err == nil {
		return rows, nil
	}
	var one map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stream), &one); err != nil {
		return nil, fmt.Errorf("jsonKeys contract: output is neither a JSON array of objects nor a JSON object: %w", err)
	}
	return []map[string]json.RawMessage{one}, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
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
	// `pix secret ls` must read PIX_HOME (which resolves to $HOME/.pix when
	// PIX_HOME itself is unset, pixhome.Dir's only two rules) under the
	// isolated $HOME RunCase passes, never a real user's ~/.pix — this is the
	// "no destructive operations" guarantee: nothing this harness runs can
	// touch real state. Seeding a ref only the isolated home could contain and
	// asserting it comes back proves the isolation, not just its absence.
	bin := buildPixBinary(t)
	root := repoRoot(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".pix"), 0o700); err != nil {
		t.Fatal(err)
	}
	const seeded = "ANTHROPIC_API_KEY=op://isolated-vault/item/field\n"
	if err := os.WriteFile(filepath.Join(home, ".pix", "secrets.env"), []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := RunCase(bin, root, home, Case{
		Name:     "ls",
		Args:     []string{"secret", "ls"},
		ExitCode: 0,
	})
	if err != nil {
		t.Fatalf("RunCase: %v", err)
	}
	if !strings.Contains(res.Stdout, "op://isolated-vault/item/field") {
		t.Errorf("secret ls stdout = %q, want the ref seeded under the isolated HOME=%q", res.Stdout, home)
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
// network). Keep this in sync with root.go's rootCmd: any verb that mutates
// on-disk state, spawns a sandbox, or touches the network belongs here, not
// just the ones that happen to have a shard today. The v2 surface has no
// grouping verb whose OWN subcommands need a separate dangerous set (the v1
// `state`/`serve install` groups are gone with those verbs), so there is only
// this one flat map now.
var dangerousVerbs = map[string]bool{
	"run": true, "reset": true, "rm": true,
	"task": true, "setup": true,
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
	if !dangerousVerbs[verb] {
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
		{"safe verb untouched", []string{"env", "show"}, false},
		{"reset help", []string{"reset", "--help"}, false},
		{"reset bad-flag", []string{"reset", "--this-is-not-a-real-flag-9x7z"}, false},
		{"reset bare", []string{"reset"}, false},
		{"reset real flag mutates", []string{"reset", "--yes"}, true},
		{"rm bare is a usage error, not a mutation", []string{"rm"}, false},
		{"setup help", []string{"setup", "--help"}, false},
		{"setup bad-flag", []string{"setup", "--this-is-not-a-real-flag-9x7z"}, false},
		{"setup with a real dir launches provisioning", []string{"setup", "."}, true},
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
