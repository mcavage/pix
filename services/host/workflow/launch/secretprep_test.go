//go:build unix

package launch

// secretprep_test.go drives the credential hook against the REAL `sbx` fixture
// and real child processes, because every property it pins is about ORDER
// ACROSS PROCESSES: when the hook runs relative to the create receipt and the
// exec, and what happens to the sandbox when it fails.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/lease"
	"pix/host/sandbox"
)

// TestPrepareSecrets_CreateRunsAfterReceiptAndBeforeExec: on a create the hook
// fires once, AFTER the instance receipt has been promoted (the lease record
// exists when the hook is called) and BEFORE `sbx exec` — a sandbox is only
// given credentials once this host can prove it owns it, and the session never
// starts without them.
func TestPrepareSecrets_CreateRunsAfterReceiptAndBeforeExec(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)
	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}

	var recordSeen bool
	var argvAtHook []string
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- RunSession(SessionSpec{
			Key: key, Name: "pix-demo", Creating: true, AttachTTY: true,
			EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
			Fingerprint:   sandbox.Fingerprint{"static_mcp": ""},
			Invocation:    []string{"--model", "m"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t),
			PrepareSecrets: func(name string) error {
				calls++
				if name != "pix-demo" {
					t.Errorf("hook got sandbox %q, want pix-demo", name)
				}
				_, rerr := lease.ReadRecord(leaseDir)
				recordSeen = rerr == nil
				argvAtHook = argvLines(t, fixture)
				return nil
			}})
	}()

	waitForFile(t, filepath.Join(fixture, "attached-it"), 20*time.Second)
	if err := os.WriteFile(filepath.Join(fixture, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if rerr := <-done; rerr != nil {
		t.Fatalf("RunSession: %v", rerr)
	}

	if calls != 1 {
		t.Errorf("hook ran %d times, want exactly 1", calls)
	}
	if !recordSeen {
		t.Error("the hook ran before the instance receipt was promoted: no lease record was on disk")
	}
	for _, line := range argvAtHook {
		if strings.HasPrefix(line, "exec ") {
			t.Errorf("the hook ran AFTER the session exec (%q); credentials must be in place first", line)
		}
	}
}

// TestPrepareSecrets_FailedCreateTearsDownTheSandbox: a create whose
// credentials could not be prepared must not leave a credential-less orphan.
// The sandbox is removed through the SAME proof-gated teardown a normal exit
// uses, and no session is ever exec'd into it.
func TestPrepareSecrets_FailedCreateTearsDownTheSandbox(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)

	err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", Creating: true, Keep: true,
		EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
		Fingerprint:   sandbox.Fingerprint{"static_mcp": ""},
		Invocation:    []string{"--model", "m"},
	}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t),
		PrepareSecrets: func(string) error { return errTestPrep }})
	if err == nil {
		t.Fatal("a failed credential preparation must refuse the launch")
	}
	var prep *SecretPrepFailed
	if !asSecretPrepFailed(err, &prep) || !prep.Created {
		t.Fatalf("err = %v (%T), want a *SecretPrepFailed with Created=true", err, err)
	}

	lines := argvLines(t, fixture)
	joined := strings.Join(lines, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "exec ") {
			t.Errorf("a credential-less sandbox was exec'd into: %q", line)
		}
	}
	if !strings.Contains(joined, "rm") {
		t.Errorf("the created sandbox was not torn down; sbx saw:\n%s", joined)
	}
	// -k/--keep must not have been bound: a keep marker written before the
	// hook would be a proof that REFUSES the removal above.
	leaseDir, derr := leaseDirFor(key)
	if derr != nil {
		t.Fatal(derr)
	}
	if _, keepSet, kerr := lease.ReadKeep(leaseDir); kerr == nil && keepSet {
		t.Error("a keep marker was bound before the credentials were prepared")
	}
}

// TestPrepareSecrets_FailedAttachRetainsTheSandbox: the same failure on an
// ATTACH refuses the launch and KEEPS the sandbox — this transition did not
// create it, so removing it would destroy someone else's work.
func TestPrepareSecrets_FailedAttachRetainsTheSandbox(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, sessionFixture)
	ws := t.TempDir()
	key := SessionName(ws)
	// Make the sandbox visible to `sbx ls` (the fixture keys off this file).
	if err := os.WriteFile(filepath.Join(fixture, "created"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", AttachExec: true, AttachTTY: true,
		AttachArgs:        []string{"run", "--name", "pix-demo"},
		DefaultInvocation: []string{"--model", "m"},
	}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t),
		PrepareSecrets: func(string) error { return errTestPrep }})
	if err == nil {
		t.Fatal("a failed credential preparation must refuse the attach")
	}
	var prep *SecretPrepFailed
	if !asSecretPrepFailed(err, &prep) || prep.Created {
		t.Fatalf("err = %v (%T), want a *SecretPrepFailed with Created=false", err, err)
	}
	joined := strings.Join(argvLines(t, fixture), "\n")
	if strings.Contains(joined, "rm") {
		t.Errorf("an EXISTING sandbox was removed after a failed attach:\n%s", joined)
	}
	if strings.Contains(joined, "exec ") {
		t.Errorf("the attach spawned a child before its credentials were ready:\n%s", joined)
	}
}

// errTestPrep is the hook failure these tests inject.
var errTestPrep = errors.New("op is not signed in")

// asSecretPrepFailed is errors.As, spelled out so the assertions above read as
// one line each.
func asSecretPrepFailed(err error, target **SecretPrepFailed) bool {
	return errors.As(err, target)
}
