//go:build unix

package launch

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/lease"
	"pix/host/sandbox"
	"pix/host/sys"
)

// isolateState points config.StateDir at a fresh tempdir for the duration of
// the test, so lease bookkeeping never touches a real host's state dir.
func isolateState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PIX_IDENTITY", "test@fixture")
}

// installFakeSbx writes a REAL executable shell script named "sbx" and
// prepends its directory to PATH, so every probe here genuinely execs a
// subprocess (env.Run -> exec.Command) rather than a mocked System. script is
// the script BODY (after the shebang). It returns the fixture's directory so
// a test can read the argv the script recorded.
func installFakeSbx(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-only fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	body := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SBX_FIXTURE_DIR", dir)
	return dir
}

func realEnv() hostenv.Env { return hostenv.Env{System: sys.Real{}} }

// fixtureSpawn builds the child process the way cmd/pix does (the real `sbx`
// on PATH — here the fixture script), with stdio discarded so a test's own
// output stays readable.
func fixtureSpawn(t *testing.T) func([]string) *exec.Cmd {
	t.Helper()
	return func(argv []string) *exec.Cmd {
		cmd := exec.Command("sbx", argv...)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		return cmd
	}
}

func TestSessionName_DeterministicDigest(t *testing.T) {
	dir := t.TempDir()
	a := SessionName(dir)
	if a != SessionName(dir) {
		t.Fatalf("SessionName(%q) is not deterministic", dir)
	}
	if !strings.HasPrefix(a, "pix-") {
		t.Fatalf("SessionName(%q) = %q, want a pix- prefix", dir, a)
	}
	if SessionName(t.TempDir()) == a {
		t.Fatalf("two different workspaces must not collide: %q", a)
	}
}

// TestLeaseDirFor_ModesAndRoot: the lease dir lives under the STATE dir (never
// the config dir — AGENTS.md safety invariant #4) and is 0700.
func TestLeaseDirFor_ModesAndRoot(t *testing.T) {
	isolateState(t)
	dir, err := leaseDirFor(SessionName(t.TempDir()))
	if err != nil {
		t.Fatalf("LeaseDirFor: %v", err)
	}
	state, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, filepath.Join(state, "sandboxes")+string(filepath.Separator)) {
		t.Errorf("lease dir %q is not under %q", dir, filepath.Join(state, "sandboxes"))
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("lease dir mode = %v (err %v), want 0700", fi.Mode().Perm(), err)
	}
}

// TestRecordSessionCreation_RecordsVerifiedInstance: a schema-verified running
// row with an instance id records all three create-time facts, and reports
// recorded=true (the only state in which -k may bind a keep).
func TestRecordSessionCreation_RecordsVerifiedInstance(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
  echo '[{"name":"pix-demo","state":"running","instance_id":"inst-123"}]'
  exit 0
fi
exit 1
`)
	key := SessionName(t.TempDir())
	fp := sandbox.Fingerprint{"static_mcp": "slack", "template": "local-1"}
	recorded, err := RecordSessionCreation(realEnv(), key, "pix-demo", fp, []string{"--model", "m"})
	if err != nil || !recorded {
		t.Fatalf("RecordSessionCreation = (%v, %v), want (true, nil)", recorded, err)
	}
	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := lease.ReadRecord(dir)
	if err != nil || rec.InstanceID != "inst-123" {
		t.Fatalf("record = %+v (err %v), want instance inst-123", rec, err)
	}
	if diverged, found := CheckSessionFingerprint(key, fp); !found || len(diverged) > 0 {
		t.Errorf("fingerprint round-trip: found=%v diverged=%v", found, diverged)
	}
	inv, found := readSessionInvocation(key)
	if !found || strings.Join(inv, " ") != "--model m" {
		t.Errorf("invocation round-trip = %v (found %v)", inv, found)
	}
	if !SessionRecorded(key) {
		t.Error("SessionRecorded = false after a verified record")
	}
}

// TestRecordSessionCreation_RecordsVerifiedInstance_V38Schema is the same
// explicit-named-create-record regression as
// TestRecordSessionCreation_RecordsVerifiedInstance, but against the sbx
// v0.38 `sbx ls --json` schema (object-wrapped `{"sandboxes": [...]}`, rows
// keyed name/id/agent/status/workspaces/workspace_missing) rather than the
// legacy bare array — proving sandbox.ParseList's v0.38 profile flows all the
// way through sbxEntry/FindByName into a real lease.Record with the UUID as
// InstanceID, not just that the pure parser accepts the shape.
func TestRecordSessionCreation_RecordsVerifiedInstance_V38Schema(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
  echo '{"sandboxes":[{"name":"pix-demo","id":"5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10","agent":"pi","status":"running","workspaces":["/workspace"],"workspace_missing":false}]}'
  exit 0
fi
exit 1
`)
	key := SessionName(t.TempDir())
	fp := sandbox.Fingerprint{"static_mcp": "slack", "template": "local-1"}
	recorded, err := RecordSessionCreation(realEnv(), key, "pix-demo", fp, []string{"--model", "m"})
	if err != nil || !recorded {
		t.Fatalf("RecordSessionCreation = (%v, %v), want (true, nil)", recorded, err)
	}
	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := lease.ReadRecord(dir)
	if err != nil || rec.InstanceID != "5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10" {
		t.Fatalf("record = %+v (err %v), want the v0.38 UUID instance id", rec, err)
	}
	if diverged, found := CheckSessionFingerprint(key, fp); !found || len(diverged) > 0 {
		t.Errorf("fingerprint round-trip: found=%v diverged=%v", found, diverged)
	}
	inv, found := readSessionInvocation(key)
	if !found || strings.Join(inv, " ") != "--model m" {
		t.Errorf("invocation round-trip = %v (found %v)", inv, found)
	}
	if !SessionRecorded(key) {
		t.Error("SessionRecorded = false after a verified v0.38 record")
	}
}

// TestRecordSessionCreation_UnverifiedRecordsNothing: an unverified row, or
// one with no instance id, records NOTHING and reports recorded=false — an
// invented identity is what would authorize a teardown nobody can justify.
func TestRecordSessionCreation_UnverifiedRecordsNothing(t *testing.T) {
	for _, tc := range []struct{ name, json string }{
		{"unverified schema", `[{"Sandbox":"pix-demo","state":"running","instance_id":"inst-1"}]`},
		{"no instance id", `[{"name":"pix-demo","state":"running"}]`},
		{"absent", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateState(t)
			installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
  echo '`+tc.json+`'
  exit 0
fi
exit 1
`)
			key := SessionName(t.TempDir())
			recorded, err := RecordSessionCreation(realEnv(), key, "pix-demo", sandbox.Fingerprint{"a": "b"}, []string{"-x"})
			if err != nil || recorded {
				t.Fatalf("RecordSessionCreation = (%v, %v), want (false, nil)", recorded, err)
			}
			if SessionRecorded(key) {
				t.Error("an unowned session must have no creation record")
			}
			if _, found := CheckSessionFingerprint(key, sandbox.Fingerprint{}); found {
				t.Error("an unowned session must have no stored fingerprint")
			}
		})
	}
}

// TestReadSessionInvocation_CorruptIsMissing: a CORRUPT record is treated
// exactly like a missing one — the attach falls back to the safe default
// invocation and stays unowned, never refusing and never authorizing removal.
func TestReadSessionInvocation_CorruptIsMissing(t *testing.T) {
	isolateState(t)
	key := SessionName(t.TempDir())
	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{sessionInvocationFileName, sessionFingerprintFileName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if inv, found := readSessionInvocation(key); found {
		t.Errorf("corrupt invocation reported as found (%v)", inv)
	}
	if _, found := CheckSessionFingerprint(key, sandbox.Fingerprint{"a": "b"}); found {
		t.Error("corrupt fingerprint reported as found — an attach would refuse on garbage")
	}
	if SessionRecorded(key) {
		t.Error("SessionRecorded must be false with no record.json")
	}
}

// TestRunSession_NeedsSpawn: the command layer owns process wiring; a
// RunSession without it is a programming error, reported not panicked.
func TestRunSession_NeedsSpawn(t *testing.T) {
	isolateState(t)
	if err := RunSession(SessionSpec{Key: SessionName(t.TempDir())}, SessionDeps{Warn: io.Discard}); err == nil {
		t.Fatal("expected an error when Spawn is nil")
	}
}

// TestRunSession_CreateRaceLost_RefusesWithoutStarting: the state under the
// lifecycle lock is the truth. A create whose sandbox already exists refuses —
// no child started, nothing recorded, and above all nothing removed.
func TestRunSession_CreateRaceLost_RefusesWithoutStarting(t *testing.T) {
	isolateState(t)
	dir := installFakeSbx(t, `
if [ "$1" = "ls" ]; then
  if [ "$2" = "--json" ]; then echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
  else echo "pix-demo  x  running"; fi
  exit 0
fi
echo "$@" >> "$(dirname "$0")/argv.log"
exit 0
`)
	err := RunSession(SessionSpec{
		Key: SessionName(t.TempDir()), Name: "pix-demo", Creating: true,
		EnvCreateArgs: []string{"env", "create", "/tmp/effective.sbxenv.yaml"},
	}, SessionDeps{Env: realEnv(), Poll: SbxCreatePoll(realEnv()), Warn: io.Discard, Spawn: fixtureSpawn(t)})

	var refused *SessionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("RunSession = %v, want a *SessionRefused", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "argv.log")); serr == nil {
		t.Error("a refused create must never start the child")
	}
	if !strings.Contains(refused.Error(), "nothing was created or removed") {
		t.Errorf("refusal %q must say nothing was created or removed", refused.Error())
	}
}

// TestRunSession_AttachVanished_Refuses: the mirror case — an attach whose
// sandbox disappeared under the lock refuses instead of falling through to a
// create path whose create-only flags were never resolved.
func TestRunSession_AttachVanished_Refuses(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ]; then exit 0; fi
exit 0
`)
	err := RunSession(SessionSpec{
		Key: SessionName(t.TempDir()), Name: "pix-demo", AttachArgs: []string{"run", "--name", "pix-demo"},
	}, SessionDeps{Env: realEnv(), Poll: SbxCreatePoll(realEnv()), Warn: io.Discard, Spawn: fixtureSpawn(t)})

	var refused *SessionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("RunSession = %v, want a *SessionRefused", err)
	}
}

// TestRunSession_FingerprintDivergence_RefusesAttach: a RECORDED create-time
// fingerprint that no longer matches refuses, naming the diverged key. A
// sandbox with no record has nothing to compare against and attaches unowned
// (proven by TestRunSession_AttachUnowned_ExactArgv in the process tests).
func TestRunSession_FingerprintDivergence_RefusesAttach(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ]; then
  if [ "$2" = "--json" ]; then echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
  else echo "pix-demo  x  running"; fi
  exit 0
fi
exit 0
`)
	ws := t.TempDir()
	key := SessionName(ws)
	if err := writeSessionState(key, sessionFingerprintFileName, sandbox.Fingerprint{"static_mcp": "slack"}); err != nil {
		t.Fatal(err)
	}
	err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", AttachExec: true,
		Fingerprint: sandbox.Fingerprint{"static_mcp": "slack,notion"},
		AttachArgs:  []string{"run", "--name", "pix-demo"},
	}, SessionDeps{Env: realEnv(), Poll: SbxCreatePoll(realEnv()), Warn: io.Discard, Spawn: fixtureSpawn(t)})

	var refused *SessionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("RunSession = %v, want a *SessionRefused", err)
	}
	if !strings.Contains(refused.Error(), "static_mcp") {
		t.Errorf("refusal %q must name the diverged key", refused.Error())
	}
}

// TestRunSession_FingerprintDivergence_RefusesAttach_V38Schema is the same
// changed-MCP attach-refusal regression as
// TestRunSession_FingerprintDivergence_RefusesAttach, but against the sbx
// v0.38 `sbx ls --json` schema. A stored fingerprint that no longer matches
// must still refuse the attach even though the probe finding it came back
// through the v0.38 object-wrapped row shape rather than the legacy bare
// array.
func TestRunSession_FingerprintDivergence_RefusesAttach_V38Schema(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ]; then
  if [ "$2" = "--json" ]; then echo '{"sandboxes":[{"name":"pix-demo","id":"5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10","agent":"pi","status":"running","workspaces":["/workspace"],"workspace_missing":false}]}'
  else echo "pix-demo  x  running"; fi
  exit 0
fi
exit 0
`)
	ws := t.TempDir()
	key := SessionName(ws)
	if err := writeSessionState(key, sessionFingerprintFileName, sandbox.Fingerprint{"static_mcp": "slack"}); err != nil {
		t.Fatal(err)
	}
	err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", AttachExec: true,
		Fingerprint: sandbox.Fingerprint{"static_mcp": "slack,notion"},
		AttachArgs:  []string{"run", "--name", "pix-demo"},
	}, SessionDeps{Env: realEnv(), Poll: SbxCreatePoll(realEnv()), Warn: io.Discard, Spawn: fixtureSpawn(t)})

	var refused *SessionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("RunSession = %v, want a *SessionRefused", err)
	}
	if !strings.Contains(refused.Error(), "static_mcp") {
		t.Errorf("refusal %q must name the diverged key", refused.Error())
	}
}

// TestRunSession_KeepNeedsARecord: -k binds an identity-bound keep only when
// there is a verified creation record to bind it to; an unowned attach warns
// instead of writing a keep about nothing.
func TestRunSession_KeepNeedsARecord(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ]; then
  if [ "$2" = "--json" ]; then echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
  else echo "pix-demo  x  running"; fi
  exit 0
fi
exit 0
`)
	ws := t.TempDir()
	key := SessionName(ws)
	var warn strings.Builder
	if err := RunSession(SessionSpec{
		Key: key, Name: "pix-demo", Keep: true,
		AttachArgs: []string{"run", "--name", "pix-demo"},
	}, SessionDeps{Env: realEnv(), Poll: SbxCreatePoll(realEnv()), Warn: &warn, Spawn: fixtureSpawn(t)}); err != nil {
		t.Fatalf("RunSession: %v", err)
	}
	dir, err := leaseDirFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, set, kerr := lease.ReadKeep(dir); set || kerr != nil {
		t.Errorf("keep set=%v err=%v; an unowned session must not carry a keep", set, kerr)
	}
	if !strings.Contains(warn.String(), "-k/--keep not recorded") {
		t.Errorf("warn = %q, want it to say the keep was not recorded", warn.String())
	}
}
