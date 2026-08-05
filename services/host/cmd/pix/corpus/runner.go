package corpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

var (
	buildOnce sync.Once
	buildBin  string
	buildErr  error
)

// BuildPixBinary compiles the real `pix` launcher (this package's parent
// directory) once per test process — every caller in the same `go test` run
// shares the same compiled binary, so a large corpus does not pay a `go
// build` per case. It exercises the actual shipped artifact, not a mock,
// which is the whole point of a golden CLI corpus.
//
// It manages its own scratch directory (outside any single test's t.TempDir())
// because the build is cached process-wide via sync.Once: if it lived inside
// the FIRST caller's t.TempDir(), that directory would be removed the moment
// that one test finished, breaking the binary path for every later test.
func BuildPixBinary() (string, error) {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pix-corpus-bin-")
		if err != nil {
			buildErr = fmt.Errorf("corpus: scratch dir for pix build: %w", err)
			return
		}
		bin := dir + "/pix"
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = ".." // services/host/cmd/pix, this package's parent
		out, cmderr := cmd.CombinedOutput()
		if cmderr != nil {
			buildErr = fmt.Errorf("go build pix: %w\n%s", cmderr, out)
			return
		}
		buildBin, buildErr = bin, nil
	})
	return buildBin, buildErr
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
