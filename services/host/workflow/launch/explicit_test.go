// explicit_test.go — the guard for what U11g deleted, plus the behaviours that
// replaced it.
//
// launch used to carry two mutable package vars the composition root wrote at
// init (DefaultEnv, IsKnownVerb) and three more the tests swapped
// (SandboxAppearProbeFn + its two durations), and it printed straight to
// os.Stderr from five places. Both are the same defect wearing two hats: a
// fact the caller owns, resolved by a lower layer out of ambient state. An
// unwired DefaultEnv panicked mid-launch; an unwired IsKnownVerb silently
// answered "no"; a stderr write inside L3 could not be captured, routed, or
// asserted on.
//
// The guard is AST-based on purpose. A grep for "os.Stderr" hits doc comments
// (run.go says the words), and a grep for "var" hits locals. The parser sees
// declarations and selectors, which is what the rule is actually about.
package launch

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// parsePackageFiles returns this package's production files, parsed.
func parsePackageFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no production files parsed — the guard would pass vacuously")
	}
	return fset, files
}

// TestLaunch_NoPackageLevelVars: every fact this package needs arrives as a
// parameter. A package-level var is how DefaultEnv/IsKnownVerb happened: a
// seam with a default that is wrong for everyone, mutated at init by one
// caller and shared, unsynchronised, by all of them.
func TestLaunch_NoPackageLevelVars(t *testing.T) {
	fset, files := parsePackageFiles(t)
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					t.Errorf("%s: package-level var %q.\n"+
						"  workflow/launch declares no mutable globals. A fact only the composition root "+
						"knows (an env, the verb table, a probe) is a PARAMETER — a RunOpts field or a "+
						"function argument constructed in cmd/pix — never a var with a default that "+
						"silently applies to whoever forgot to set it.",
						fset.Position(id.Pos()), id.Name)
				}
			}
		}
	}
}

// TestLaunch_NoProcessStreamsOrExit: L3 writes to the writer it was given and
// returns errors. Choosing os.Stdout/os.Stderr, or calling os.Exit, takes a
// decision that belongs to the command layer (which stream `--json` may use,
// what exit code a failure earns) and makes the behaviour untestable.
func TestLaunch_NoProcessStreamsOrExit(t *testing.T) {
	banned := map[string]string{
		"Stdout": "write to an io.Writer the caller passed in (cli.Deps.Out)",
		"Stderr": "write to an io.Writer the caller passed in (cli.Deps.Err)",
		"Exit":   "return an error; only L4 (cmd/pix) owns the exit code",
	}
	fset, files := parsePackageFiles(t)
	for _, f := range files {
		// Only flag selectors on the real "os" package, so a local variable
		// named os, or an identically named field, is not a false positive.
		osName := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "os" {
				osName = "os"
				if imp.Name != nil {
					osName = imp.Name.Name
				}
			}
		}
		if osName == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != osName {
				return true
			}
			if why, bad := banned[sel.Sel.Name]; bad {
				t.Errorf("%s: %s.%s in workflow/launch.\n  %s.",
					fset.Position(sel.Pos()), osName, sel.Sel.Name, why)
			}
			return true
		})
	}
}

// TestStartSbxSession_UnwiredPollFailsClosedBeforeExec: the replacement for
// the panicking DefaultEnv. A create that must produce creation EVIDENCE but
// was handed no probe is a wiring bug, and it fails BEFORE `sbx run` starts —
// the old package var degraded instead into "poll nothing, record nothing",
// which is exactly the silent state loss the lease record exists to prevent.
func TestStartSbxSession_UnwiredPollFailsClosedBeforeExec(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	cmd := exec.Command("sh", "-c", "touch "+ran)
	_, err := StartSbxSession(cmd, CreatePoll{}, true, "pix-unwired")
	if err == nil || !strings.Contains(err.Error(), "no sandbox probe") {
		t.Fatalf("err = %v, want a poll-not-wired error", err)
	}
	if _, serr := os.Stat(ran); serr == nil {
		t.Error("`sbx run` must not be started when the create poll is unwired")
	}

	for _, tc := range []struct {
		name string
		poll CreatePoll
	}{
		{"no interval", CreatePoll{Probe: func(string) SbxState { return SbxRunning }, Timeout: time.Second}},
		{"no timeout", CreatePoll{Probe: func(string) SbxState { return SbxRunning }, Interval: time.Millisecond}},
	} {
		if _, err := StartSbxSession(exec.Command("true"), tc.poll, true, "pix-unwired"); err == nil {
			t.Errorf("%s: want an error, got nil", tc.name)
		}
	}
}

// TestSbxCreatePoll_ProbesThroughTheGivenEnv: the composition root's poll asks
// the env it was handed, on the shipped budget — no ambient DefaultEnv().
func TestSbxCreatePoll_ProbesThroughTheGivenEnv(t *testing.T) {
	var asked [][]string
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(cmd string, args ...string) (string, bool, error) {
		asked = append(asked, append([]string{cmd}, args...))
		return "pix-a  ws  running\n", false, nil
	}}}
	poll := SbxCreatePoll(env)
	if got := poll.Probe("pix-a"); got != SbxRunning {
		t.Errorf("probe = %v, want running", got)
	}
	if len(asked) != 1 || asked[0][0] != "sbx" || asked[0][1] != "ls" {
		t.Errorf("probe must go through the injected env's `sbx ls`, got %v", asked)
	}
	if poll.Interval != SbxCreatePollInterval || poll.Timeout != SbxCreatePollTimeout {
		t.Errorf("poll budget = %s/%s, want %s/%s", poll.Interval, poll.Timeout, SbxCreatePollInterval, SbxCreatePollTimeout)
	}
}

// TestValidateRunWorkspace_VerbHintComesFromTheCaller: the "did you mean"
// hint is driven by the table the caller passes. Passing nil is the explicit
// "no verb table" case and loses only the hint — the validation itself is
// unchanged, which is what the old silent default could never make visible.
func TestValidateRunWorkspace_VerbHintComesFromTheCaller(t *testing.T) {
	file := filepath.Join(t.TempDir(), "doctor")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	verbs := func(v string) bool { return filepath.Base(v) == "doctor" }

	err := ValidateRunWorkspace(file, verbs)
	if err == nil || !strings.Contains(err.Error(), "Did you mean") {
		t.Fatalf("err = %v, want a `did you mean` hint from the injected table", err)
	}
	err = ValidateRunWorkspace(file, nil)
	if err == nil {
		t.Fatal("a non-directory must still be rejected without a verb table")
	}
	if strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("no verb table must mean no hint, got %v", err)
	}
	if err := ValidateRunWorkspace(".", nil); err != nil {
		t.Errorf("the cwd default must always validate: %v", err)
	}
}

// TestPrintJSONLauncher_WritesToTheGivenWriter: JSON goes where the caller
// says (so `--json` reaches stdout and progress never pollutes it), and a
// marshal failure is RETURNED rather than printed as "null".
func TestPrintJSONLauncher_WritesToTheGivenWriter(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintJSONLauncher(&buf, map[string]int{"a": 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := buf.String(); got != "{\n  \"a\": 1\n}\n" {
		t.Errorf("output = %q", got)
	}
	if err := PrintJSONLauncher(io.Discard, make(chan int)); err == nil {
		t.Error("an unmarshalable value must return an error, not print nothing and claim success")
	}
	if err := PrintJSONLauncher(failWriter{}, 1); err == nil {
		t.Error("a write failure must be returned")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }
