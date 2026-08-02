// setup_keys_lock_test.go — the strict setup flow's provider-refs lock
// behaviour. These live here, not in secret/, because their subject is
// setupProvisionKeys (a cmd/pix workflow); the lock they observe belongs to
// secret, but the sequencing under test is the workflow's.
package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/secret"
	"pix/host/sys/systest"
)

// lockWindow returns the index of the first acquire and last release of
// lockPath in events, failing the test if either is missing.
func lockWindow(t *testing.T, events []string, lockPath string) (first, last int) {
	t.Helper()
	first, last = systest.LockWindow(events, lockPath)
	if first < 0 || last < 0 {
		t.Fatalf("no acquire/release of %s in events: %v", lockPath, events)
	}
	return first, last
}

// The strict flow must hold ONE provider-refs lock acquisition from the
// initial ref validation through the canonical both-file writes, the hostmode
// verification, and the sbx reconciliation — with every internal write via a
// *Locked variant (the recording flock fails the test on nesting).
func TestStrictFlowHoldsLockThroughReconcile(t *testing.T) {
	env, _ := stepEnv(t, allRefs("", "", ""), "", "tok-value")
	var events []string
	origRead, origWrite, origRun := systest.Of(env.System).ReadFileFn, systest.Of(env.System).WriteFileFn, systest.Of(env.System).RunFn
	systest.Of(env.System).ReadFileFn = func(p string) (string, error) {
		events = append(events, "read "+p)
		return origRead(p)
	}
	systest.Of(env.System).WriteFileFn = func(p string, d []byte, perm os.FileMode) error {
		events = append(events, "write "+p)
		return origWrite(p, d, perm)
	}
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		events = append(events, "run "+name+" "+strings.Join(args, " "))
		return origRun(name, args...)
	}
	systest.Of(env.System).LockFn = systest.NewLockRecorder(t.Fatalf, &events).Lock

	var out bytes.Buffer
	if !setupProvisionKeys(env, strings.NewReader(""), &out, false, true) {
		t.Fatalf("setupProvisionKeys strict flow failed: %s", out.String())
	}

	lockPath := secret.ProviderRefsLockPath(env)
	if got := systest.CountEvents(events, "acquire "); got != 1 {
		t.Fatalf("want exactly 1 provider-refs lock acquisition, got %d: %v", got, events)
	}
	first, last := lockWindow(t, events, lockPath)

	refsDir := filepath.Dir(secret.DefaultOpRefsPath(env))
	inWindow := func(i int) bool { return i > first && i < last }
	var sawRefsWrite, sawHostmodeRead, sawSbxSet bool
	for i, e := range events {
		switch {
		case strings.HasPrefix(e, "write "+refsDir):
			sawRefsWrite = true
			if !inWindow(i) {
				t.Errorf("refs-file write outside the lock window: %q (index %d, window %d..%d)", e, i, first, last)
			}
		case e == "read "+secret.HostModeRefsPath(env):
			sawHostmodeRead = true
			if !inWindow(i) {
				t.Errorf("hostmode verification read outside the lock window (index %d, window %d..%d)", i, first, last)
			}
		case strings.HasPrefix(e, "run sbx secret set"):
			sawSbxSet = true
			if !inWindow(i) {
				t.Errorf("sbx reconciliation outside the lock window: %q (index %d, window %d..%d)", e, i, first, last)
			}
		}
	}
	if !sawRefsWrite || !sawHostmodeRead || !sawSbxSet {
		t.Fatalf("expected refs write + hostmode read + sbx set events, got refsWrite=%v hostmodeRead=%v sbxSet=%v: %v",
			sawRefsWrite, sawHostmodeRead, sawSbxSet, events)
	}
}

func TestLockAcquisitionErrorFailsStrictSetup(t *testing.T) {
	env, _ := stepEnv(t, allRefs("", "", ""), "", "tok-value")
	systest.Of(env.System).LockFn = func(string, func() error) error { return errors.New("lock dir unwritable") }
	var out bytes.Buffer
	if setupProvisionKeys(env, strings.NewReader(""), &out, false, true) {
		t.Fatal("strict setup must fail when the provider-refs lock cannot be acquired")
	}
	if !strings.Contains(out.String(), "could not lock provider refs") {
		t.Errorf("setup output = %q, want a lock failure message", out.String())
	}
}
