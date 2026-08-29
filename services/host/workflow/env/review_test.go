package env

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
)

// tempConfigAndState isolates BOTH $PIX_CONFIG and $XDG_STATE_HOME at fresh
// temp paths. review.go's trust store/lock live beside config and in the
// state dir respectively (environmentTrustStorePath/environmentTrustLockPath)
// — tempConfig alone (registry_test.go) only isolates the former, which
// would leave every Review test's lock file landing in the real
// ~/.local/state/pix on the machine running it.
func tempConfigAndState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
}

func reviewFixture(t *testing.T, name string) (string, *config.Config) {
	t.Helper()
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", root)
	if _, err := Register(cfg, name, root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Persist the registration: Use/Forget commit against a FRESH
	// under-lock reload of the live file (commit.go), exactly as
	// production always runs them after a persisted `pix env add`.
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return root, cfg
}

// ── non-TTY without --yes: exit 2, prints the bill plus the --yes line, writes nothing ──

func TestReview_NonTTYWithoutYesFailsClosedAndWritesNothing(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", root)
	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &out, TTY: false, Yes: false})
	if err == nil {
		t.Fatal("Review must fail closed on a non-TTY without --yes")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if !strings.Contains(out.String(), wantPRDBillDefault) {
		t.Errorf("non-TTY output must contain the same bill, got:\n%s", out.String())
	}
	// The retry command lives in the error's own three-part text now, never
	// a second, output-only line (C7): out carries the bill only.
	if strings.Contains(out.String(), "pix env review work --yes") {
		t.Errorf("non-TTY output must not ALSO print the retry command separately from the error, got:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "pix env review work --yes") {
		t.Errorf("non-TTY refusal error must name the --yes re-run command, got:\n%s", err.Error())
	}

	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Accepted) != 0 {
		t.Errorf("non-TTY refusal must write nothing, got %+v", ts.Accepted)
	}
}

// ── interactive prompt: yes / no / EOF ────────────────────────────────────

func TestReview_InteractivePromptYesAccepts(t *testing.T) {
	root, cfg := reviewFixture(t, "work")

	var out bytes.Buffer
	res, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{
		Out: &out, TTY: true, In: strings.NewReader("yes\n"),
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !res.Accepted || res.Fingerprint == "" {
		t.Fatalf("result = %+v, want Accepted with a non-empty Fingerprint", res)
	}
	wantSuccess := `pix: recorded acceptance for environment "work" (fingerprint ` + shortFingerprint(res.Fingerprint) + `).`
	if !strings.Contains(out.String(), wantSuccess) {
		t.Errorf("output missing exact success line %q, got:\n%s", wantSuccess, out.String())
	}
	for _, banned := range []string{"configured", "enabled", "ready", "verified"} {
		if strings.Contains(strings.ToLower(out.String()), banned) {
			t.Errorf("output must never say %q, got:\n%s", banned, out.String())
		}
	}

	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ts.Get(Subject(root))
	if !ok || got.Fingerprint != res.Fingerprint {
		t.Errorf("store record = %+v, ok=%v, want Fingerprint %q", got, ok, res.Fingerprint)
	}
}

func TestReview_InteractivePromptNoRefusesAndWritesNothing(t *testing.T) {
	_, cfg := reviewFixture(t, "work")

	var out bytes.Buffer
	res, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{
		Out: &out, TTY: true, In: strings.NewReader("no\n"),
	})
	if err == nil {
		t.Fatal("Review must refuse when the answer is no")
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Accepted) != 0 {
		t.Errorf("a declined review must write nothing, got %+v", ts.Accepted)
	}
}

func TestReview_InteractivePromptEOFRefusesAndWritesNothing(t *testing.T) {
	_, cfg := reviewFixture(t, "work")

	var out bytes.Buffer
	res, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{
		Out: &out, TTY: true, In: strings.NewReader(""),
	})
	if err == nil {
		t.Fatal("Review must refuse on EOF with no answer (default is No)")
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Accepted) != 0 {
		t.Errorf("an EOF-refused review must write nothing, got %+v", ts.Accepted)
	}
}

// ── Tier0: silence, no prompt, no store write ─────────────────────────────

func TestReview_Tier0IsSilentAndWritesNothing(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Review(cfg, "home", nil, nil, ReviewOptions{Out: &out, TTY: false, Yes: false})
	if err != nil {
		t.Fatalf("Review on a Tier0 environment must succeed with no gate, got: %v", err)
	}
	if !res.Accepted {
		t.Errorf("result = %+v, want Accepted (Tier0 needs no consent)", res)
	}
	if out.Len() != 0 {
		t.Errorf("Tier0 review must print nothing, got:\n%s", out.String())
	}

	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Accepted) != 0 {
		t.Errorf("Tier0 review must write no acceptance record, got %+v", ts.Accepted)
	}
}

// ── acceptance persistence, repoint, and change-regate ────────────────────

func TestReview_AcceptancePersistsRepointCannotInheritAndChangeRegates(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	oldRoot := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", oldRoot)
	if _, err := Register(cfg, "work", oldRoot); err != nil {
		t.Fatal(err)
	}

	// accept once
	res1, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &bytes.Buffer{}, Yes: true})
	if err != nil {
		t.Fatalf("first Review: %v", err)
	}

	// persistence: a brand-new process-equivalent load of the store still
	// reports the SAME fingerprint for oldRoot.
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := ts.Get(Subject(oldRoot))
	if !ok || rec.Fingerprint != res1.Fingerprint {
		t.Fatalf("persisted record = %+v, ok=%v, want Fingerprint %q", rec, ok, res1.Fingerprint)
	}

	// repoint: same NAME, a different root with its own (unaccepted) surface.
	newRoot := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", newRoot)
	if _, err := Register(cfg, "work", newRoot); err != nil {
		t.Fatal(err)
	}
	_, err = Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &bytes.Buffer{}, TTY: false, Yes: false})
	if err == nil {
		t.Fatal("a repointed name must not inherit the old root's acceptance — non-TTY review must fail closed")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	// the OLD root's record must be untouched by the repoint+failed review.
	ts2, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if rec2, ok := ts2.Get(Subject(oldRoot)); !ok || rec2.Fingerprint != res1.Fingerprint {
		t.Fatalf("old root's record changed after repoint: %+v, ok=%v", rec2, ok)
	}

	// change-regate: accept the new root, then mutate its surface (a new
	// host command) and accept again — the STORED fingerprint must move to
	// reflect the new surface, never silently keep the old one.
	res2, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &bytes.Buffer{}, Yes: true})
	if err != nil {
		t.Fatalf("Review on repointed root: %v", err)
	}
	sbxenvPath := filepath.Join(newRoot, ".sbxenv.yaml")
	data, err := os.ReadFile(sbxenvPath)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(data),
		"    - name: warehouse-mcp\n      command: warehouse-mcp-server\n",
		"    - name: warehouse-mcp\n      command: warehouse-mcp-server\n    - name: extra-mcp\n      command: extra-mcp-server\n",
		1)
	if rewritten == string(data) {
		t.Fatal("test setup error: fixture .sbxenv.yaml did not match the expected replace target")
	}
	if err := os.WriteFile(sbxenvPath, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	res3, err := Review(cfg, "work", prdMounts(), noBareLookPath, ReviewOptions{Out: &bytes.Buffer{}, Yes: true})
	if err != nil {
		t.Fatalf("Review after surface change: %v", err)
	}
	if res3.Fingerprint == res2.Fingerprint {
		t.Fatal("changing the host-exec surface must regate to a NEW fingerprint")
	}
	ts3, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	rec3, ok := ts3.Get(Subject(newRoot))
	if !ok || rec3.Fingerprint != res3.Fingerprint {
		t.Fatalf("stored record after change = %+v, ok=%v, want the NEW fingerprint %q", rec3, ok, res3.Fingerprint)
	}
}

// ── TOCTOU: a mutation introduced during the interactive wait must fail closed ──

func TestReview_MutationDuringPromptFailsClosedAtCommit(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	realKit := filepath.Join(root, "kit")
	if err := os.MkdirAll(realKit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realKit, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", "schemaVersion: \"1\"\nkits:\n  - ./kit\nsecrets:\n  anthropic:\n    ref: op://Personal/Anthropic/api-key\nbindings:\n  anthropic:\n    apiKey:\n      domains:\n        - api.anthropic.com\n")
	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatal(err)
	}

	mutated := false
	in := &mutateOnFirstRead{
		mutate: func() {
			mutated = true
			if err := os.RemoveAll(realKit); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, realKit); err != nil {
				t.Fatal(err)
			}
		},
		r: strings.NewReader("yes\n"),
	}

	var out bytes.Buffer
	res, err := Review(cfg, "work", nil, nil, ReviewOptions{Out: &out, TTY: true, In: in})
	if !mutated {
		t.Fatal("test setup error: the mutating reader was never invoked")
	}
	if err == nil {
		t.Fatal("Review must fail closed when the referenced kit becomes a symlink between render and commit")
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Accepted) != 0 {
		t.Errorf("a fail-closed commit must write nothing, got %+v", ts.Accepted)
	}
}

// mutateOnFirstRead wraps a reader and invokes mutate exactly once, on its
// very first Read call — placing a filesystem mutation at precisely the
// moment gate() blocks reading the user's answer, i.e. strictly BETWEEN
// Review's first (render) Load and its second (commit) Load.
type mutateOnFirstRead struct {
	mutate func()
	r      *strings.Reader
	done   bool
}

func (m *mutateOnFirstRead) Read(p []byte) (int, error) {
	if !m.done {
		m.done = true
		m.mutate()
	}
	return m.r.Read(p)
}

// ── finding C12: --verbose shows a digest only where one is resolvable ─────

// resolvingLookPath answers ok for exactly one bare command name (the
// fixture's resolvable service), pointing it at a real regular file this
// test controls the bytes of, and fails every other bare name exactly as
// noBareLookPath does — so a test can prove BOTH the resolvable and the
// unresolvable cases against the SAME bill without two different fixtures.
func resolvingLookPath(resolvable map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := resolvable[name]; ok {
			return p, nil
		}
		return "", errNotFound
	}
}

// TestRenderVerboseDetails_HostServiceDigestOnlyWhenResolvable is C12: a
// host service command that DOES resolve to a real local path shows its
// content digest ("sha256:<hex>", the exact hash of the bytes this test
// wrote); one that does NOT resolve at all shows the argv line and nothing
// else — no blank or placeholder "sha256:" line pretending a digest exists
// where none could be computed. This is the positive half
// bom_e18block_test.go's argv-only assertions never covered: every
// existing verbose test in this package runs noBareLookPath, so nothing
// before this proved a digest is ever actually PRINTED, only that its
// absence is tolerated.
func TestRenderVerboseDetails_HostServiceDigestOnlyWhenResolvable(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	binDir := t.TempDir()
	resolvedPath := filepath.Join(binDir, "warehouse-proxy")
	content := []byte("#!/bin/sh\necho warehouse-proxy\n")
	if err := os.WriteFile(resolvedPath, content, 0o755); err != nil {
		t.Fatal(err)
	}
	wantSHA := hosttrust.HashBytes(content)

	copyFixture(t, "testdata/hostexec-fixture", root)
	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	env, err := Load(cfg, &hosttrust.AcceptanceStore{}, "work", nil, noBareLookPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Resolvable: the fixture's "warehouse-proxy" host service resolves to
	// resolvedPath and gets a real digest.
	resolvedBoM, err := ComputeBoM(env, nil, resolvingLookPath(map[string]string{"warehouse-proxy": resolvedPath}))
	if err != nil {
		t.Fatalf("ComputeBoM (resolvable): %v", err)
	}
	var resolvedOut bytes.Buffer
	renderVerboseDetails(&resolvedOut, resolvedBoM)
	if !strings.Contains(resolvedOut.String(), "sha256:"+wantSHA) {
		t.Errorf("verbose (resolvable) = %q, want the digest sha256:%s", resolvedOut.String(), wantSHA)
	}

	// Unresolvable (noBareLookPath, exactly as every other test in this
	// package already runs): the argv line prints, no sha256 line at all.
	unresolvedBoM, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM (unresolvable): %v", err)
	}
	var unresolvedOut bytes.Buffer
	renderVerboseDetails(&unresolvedOut, unresolvedBoM)
	got := unresolvedOut.String()
	if !strings.Contains(got, "host service warehouse-proxy") {
		t.Errorf("verbose (unresolvable) = %q, want the host service argv line regardless", got)
	}
	if strings.Contains(got, "sha256:") {
		t.Errorf("verbose (unresolvable) = %q, must not print a sha256 line when nothing could be resolved", got)
	}
}
