package env

// edit_test.go — E1.12 `pix env edit NAME pix|sbxenv`, red-first: the
// target token table (AC-49), the TTY/non-TTY no-token behavior (AC-50),
// $VISUAL/$EDITOR resolution including the unset-both path and argv-with-
// spaces invocation (AC-51), editor failure, and the three PRD §5.4
// verdicts with the "never rolls back, never deletes a record" invariant
// (AC-52, AC-18's post-edit half).

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
	"pix/host/sys"
	"pix/host/sys/systest"
)

// tier0EditFixture registers a minimal, non-host-executing environment
// under name and returns its canonical root and the *config.Config it is
// registered against — the same shape show_test.go's tier0Fixture uses,
// isolated with tempConfigAndState since Edit reads the environment-trust
// store beside config.toml and its lock in the state dir.
func tier0EditFixture(t *testing.T, name string) (string, *config.Config) {
	t.Helper()
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(cfg, name, root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("cfg.Save: %v", err)
	}
	return root, cfg
}

// noEditorFake is a systest.Fake with $VISUAL and $EDITOR both unset
// (Getenv returns "" for everything) and RunInteractive refusing loudly if
// ever called — the fixture for every test proving "no editor configured"
// never reaches the editor-invocation step at all.
func noEditorFake() *systest.Fake {
	return &systest.Fake{GetenvFn: func(string) string { return "" }}
}

// failingReader errors on every Read, so a test that hands it as opts.In
// proves the code under test never attempts to read stdin at all — a
// silent success (zero bytes, no error) would not catch a caller that
// merely constructed a Scanner without calling Scan.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Fatal("must not read stdin: a target was already settled")
	return 0, errors.New("unreachable")
}

// ── AC-49: exact positional enum only, no --sbxenv flag ──────────────────

func TestEdit_TargetTokenTable(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string // "" means success; otherwise a substring of the error
	}{
		{name: "pix token", target: TargetPix, want: ""},
		{name: "sbxenv token", target: TargetSbxenv, want: ""},
		{name: "unrecognized token", target: "yaml", want: `unknown target "yaml"`},
		{name: "empty token, non-TTY", target: "", want: "needs a target file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg := tier0EditFixture(t, "work")
			var out bytes.Buffer
			opts := EditOptions{TTY: false, In: failingReader{t}, Out: &out}
			res, err := Edit(cfg, noEditorFake(), "work", tc.target, opts)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Edit(%q) = %v, want success", tc.target, err)
				}
				if res.Target != tc.target {
					t.Errorf("res.Target = %q, want %q", res.Target, tc.target)
				}
				return
			}
			if err == nil {
				t.Fatalf("Edit(%q) = nil error, want a refusal containing %q", tc.target, tc.want)
			}
			if got := cli.ExitCode(err); got != 2 {
				t.Errorf("cli.ExitCode(err) = %d, want 2", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err.Error(), tc.want)
			}
			assertNamesBothForms(t, err.Error(), "work")
		})
	}
}

func assertNamesBothForms(t *testing.T, msg, name string) {
	t.Helper()
	if !strings.Contains(msg, "pix env edit "+name+" pix") {
		t.Errorf("message = %q, want it to name the pix form", msg)
	}
	if !strings.Contains(msg, "pix env edit "+name+" sbxenv") {
		t.Errorf("message = %q, want it to name the sbxenv form", msg)
	}
}

// TestEdit_NoSbxenvFlag proves the ONLY way to select the native file is
// the positional token "sbxenv" — there is no separate flag spelling
// anywhere in this package's own LIVE (non-comment) source. Prose in a
// doc comment is free to write out "--sbxenv" to say it does not exist
// (this file's own doc comment above does); what must never appear is the
// literal flag declaration/reference on a CODE line.
func TestEdit_NoSbxenvFlag(t *testing.T) {
	src, err := os.ReadFile("edit.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, "--sbxenv") {
			t.Errorf("workflow/env/edit.go has a live (non-comment) --sbxenv reference: %q", line)
		}
	}
}

// ── AC-50: TTY selection vs non-TTY refusal, no token ─────────────────────

func TestEdit_TTYNoTokenPrintsSelectionAndReadsBoundedChoice(t *testing.T) {
	root, cfg := tier0EditFixture(t, "work")
	var out bytes.Buffer
	res, err := Edit(cfg, noEditorFake(), "work", "", EditOptions{
		TTY: true, In: strings.NewReader("sbxenv\n"), Out: &out,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got := out.String()
	wantSelection := "1) pix       pix.toml (the Pix sidecar)\n" +
		"2) sbxenv    .sbxenv.yaml (the native environment file)\n" +
		"which file? [pix/sbxenv]: "
	if !strings.HasPrefix(got, wantSelection) {
		t.Errorf("output = %q, want it to start with the exact two-line selection:\n%q", got, wantSelection)
	}
	if res.Target != TargetSbxenv {
		t.Errorf("res.Target = %q, want sbxenv", res.Target)
	}
	if want := filepath.Join(root, ".sbxenv.yaml") + "\n"; !strings.HasSuffix(got, want) {
		t.Errorf("output = %q, want it to end with the printed path %q", got, want)
	}
}

func TestEdit_TTYNumberedChoiceSelectsPix(t *testing.T) {
	_, cfg := tier0EditFixture(t, "work")
	res, err := Edit(cfg, noEditorFake(), "work", "", EditOptions{
		TTY: true, In: strings.NewReader("1\n"), Out: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Target != TargetPix {
		t.Errorf("res.Target = %q, want pix", res.Target)
	}
}

func TestEdit_TTYUnboundedAnswerRefusesNamingBothForms(t *testing.T) {
	_, cfg := tier0EditFixture(t, "work")
	_, err := Edit(cfg, noEditorFake(), "work", "", EditOptions{
		TTY: true, In: strings.NewReader("yaml please\n"), Out: &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("Edit must refuse an unbounded TTY answer")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	assertNamesBothForms(t, err.Error(), "work")
}

func TestEdit_NonTTYNoTokenExitsTwoNeverReadsStdin(t *testing.T) {
	_, cfg := tier0EditFixture(t, "work")
	var out bytes.Buffer
	_, err := Edit(cfg, noEditorFake(), "work", "", EditOptions{
		TTY: false, In: failingReader{t}, Out: &out,
	})
	if err == nil {
		t.Fatal("Edit must exit non-zero with no token on a non-TTY")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	assertNamesBothForms(t, err.Error(), "work")
	if out.String() != "" {
		t.Errorf("stdout = %q, want nothing printed on the refusal path", out.String())
	}
}

// ── AC-51: $VISUAL/$EDITOR resolution ─────────────────────────────────────

func TestEdit_BothUnsetPrintsOnlyAbsolutePath(t *testing.T) {
	root, cfg := tier0EditFixture(t, "work")
	var out bytes.Buffer
	fake := noEditorFake()
	res, err := Edit(cfg, fake, "work", TargetSbxenv, EditOptions{
		TTY: false, In: failingReader{t}, Out: &out,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	want := filepath.Join(root, ".sbxenv.yaml") + "\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want ONLY %q", out.String(), want)
	}
	if res.EditorRan {
		t.Error("EditorRan = true, want false: no editor is configured")
	}
	if res.Verdict != "" {
		t.Errorf("Verdict = %q, want empty: no edit happened, nothing to validate", res.Verdict)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("fake.Calls = %v, want none: RunInteractive must never be called", fake.Calls)
	}
}

func TestEdit_VisualWinsOverEditor(t *testing.T) {
	_, cfg := tier0EditFixture(t, "work")
	fake := &systest.Fake{
		GetenvFn: func(name string) string {
			switch name {
			case "VISUAL":
				return "visualcmd"
			case "EDITOR":
				return "editorcmd"
			}
			return ""
		},
		RunInteractiveFn: func(string, ...string) error { return nil },
	}
	_, err := Edit(cfg, fake, "work", TargetSbxenv, EditOptions{Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(fake.Calls) != 1 || !strings.HasPrefix(fake.Calls[0], "visualcmd") {
		t.Errorf("fake.Calls = %v, want the VISUAL command invoked, not EDITOR", fake.Calls)
	}
}

// TestEdit_ArgvSpacesSplitNoShell proves a multi-word $EDITOR is split into
// a real argv (never a single argument, never a shell -c invocation): the
// editor binary and its own flags are separate Calls entries in front of
// the target path.
func TestEdit_ArgvSpacesSplitNoShell(t *testing.T) {
	root, cfg := tier0EditFixture(t, "work")
	fake := &systest.Fake{
		GetenvFn:         func(string) string { return "myeditor --wait --flag" },
		RunInteractiveFn: func(string, ...string) error { return nil },
	}
	_, err := Edit(cfg, fake, "work", TargetPix, EditOptions{Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("fake.Calls = %v, want exactly one invocation", fake.Calls)
	}
	want := "myeditor --wait --flag " + filepath.Join(root, "pix.toml")
	if fake.Calls[0] != want {
		t.Errorf("fake.Calls[0] = %q, want %q", fake.Calls[0], want)
	}
}

// ── editor failure: operational, non-2 ─────────────────────────────────────

func TestEdit_EditorFailureIsOperationalNonTwo(t *testing.T) {
	_, cfg := tier0EditFixture(t, "work")
	fake := &systest.Fake{
		GetenvFn:         func(string) string { return "brokeneditor" },
		RunInteractiveFn: func(string, ...string) error { return errors.New("exit status 127") },
	}
	_, err := Edit(cfg, fake, "work", TargetSbxenv, EditOptions{Out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("Edit must fail when the editor itself fails")
	}
	if got := cli.ExitCode(err); got == 2 || got == 0 {
		t.Errorf("cli.ExitCode(err) = %d, want a non-zero, non-2 operational code", got)
	}
	if !strings.Contains(err.Error(), "brokeneditor") {
		t.Errorf("err = %q, want it to name the editor invocation", err.Error())
	}
}

// ── AC-52 / AC-18 post-edit half: the three verdicts ──────────────────────

// noopEditorFake never touches the file on disk: the "edit" is a no-op, so
// whatever Load/ComputeBoM produce afterward is byte-identical to before.
func noopEditorFake(editorValue string) *systest.Fake {
	return &systest.Fake{
		GetenvFn:         func(string) string { return editorValue },
		RunInteractiveFn: func(string, ...string) error { return nil },
	}
}

func TestEdit_VerdictOkWhenFootprintUnchangedAndAccepted(t *testing.T) {
	root, cfg := tier0EditFixture(t, "work")

	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(cfg, &ts.AcceptanceStore, "work", nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	bom, err := ComputeBoM(loaded, nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := Fingerprint(bom)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutateEnvironmentTrustStoreLocked(func(s *environmentTrustStore) error {
		s.Put(Subject(root), hosttrust.Record{Fingerprint: fp})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Edit(cfg, noopEditorFake("true"), "work", TargetSbxenv, EditOptions{Out: &out})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Verdict != "ok" {
		t.Errorf("Verdict = %q, want ok", res.Verdict)
	}
	if !strings.Contains(out.String(), "footprint unchanged") {
		t.Errorf("stdout = %q, want it to say the footprint is unchanged", out.String())
	}
	if !strings.Contains(out.String(), "pix env use work") {
		t.Errorf("stdout = %q, want the exact next: pix env use work line", out.String())
	}
}

func TestEdit_VerdictReviewWhenUnaccepted(t *testing.T) {
	_, cfg := tier0EditFixture(t, "work")

	var out bytes.Buffer
	res, err := Edit(cfg, noopEditorFake("true"), "work", TargetSbxenv, EditOptions{Out: &out})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Verdict != "review" {
		t.Errorf("Verdict = %q, want review", res.Verdict)
	}
	if !strings.Contains(out.String(), "pix env review work") {
		t.Errorf("stdout = %q, want the exact next: pix env review work line", out.String())
	}
}

func TestEdit_VerdictInvalidLeavesConfigAndTrustByteIdentical(t *testing.T) {
	root, cfg := tier0EditFixture(t, "work")

	configHashBefore := fileHashOrEmpty(t, config.Path())
	trustHashBefore := fileHashOrEmpty(t, environmentTrustStorePath())

	corrupt := &systest.Fake{
		GetenvFn: func(string) string { return "true" },
		RunInteractiveFn: func(string, ...string) error {
			return os.WriteFile(filepath.Join(root, ".sbxenv.yaml"),
				[]byte("schemaVersion: \"1\"\nagent: pix\nnot_a_real_field: true\n"), 0o644)
		},
	}

	var out bytes.Buffer
	res, err := Edit(cfg, corrupt, "work", TargetSbxenv, EditOptions{Out: &out})
	if err != nil {
		t.Fatalf("Edit itself must not fail on an invalid post-edit file: %v", err)
	}
	if res.Verdict != "invalid" {
		t.Errorf("Verdict = %q, want invalid", res.Verdict)
	}
	if !strings.Contains(out.String(), "next: pix env edit work sbxenv") {
		t.Errorf("stdout = %q, want the exact re-edit command", out.String())
	}

	configHashAfter := fileHashOrEmpty(t, config.Path())
	trustHashAfter := fileHashOrEmpty(t, environmentTrustStorePath())
	if configHashBefore != configHashAfter {
		t.Error("config.toml changed after an invalid post-edit reload; Edit must never rewrite it")
	}
	if trustHashBefore != trustHashAfter {
		t.Error("environment-trust store changed after an invalid post-edit reload; Edit must never mutate it")
	}
}

func fileHashOrEmpty(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sha256.Sum256(nil)
		}
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// compile-time proof Edit's second parameter really is sys.System, not a
// narrower ad hoc interface a test fake could satisfy by accident.
var _ = func() sys.System { return noEditorFake() }
