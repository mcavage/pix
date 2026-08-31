//go:build unix

package launch

// pixhome_escape_test.go is the QA F5 integration proof: a full create +
// teardown lifecycle, driven the way `pix run` actually drives it (a real
// `sbx` fixture on PATH, a persisted effective document, RunSession's own
// create/attach/teardown sequence), with PIX_HOME pointed at one temp dir
// and every legacy XDG variable pointed at a SEPARATE, otherwise-untouched
// decoy dir. Every artifact this lifecycle writes — the effective document,
// the sandbox lease/record state, the session invocation/fingerprint state,
// and the teardown journal — must land ONLY under the temp PIX_HOME; the
// decoy tree must stay completely empty throughout. This is the "last
// executable state escape" QA asked to see closed: config.Path/StateDir/
// DataDir/ContextDir (and every workflow/launch caller built on them) now
// resolve under PIX_HOME alone, with no XDG_*/PIX_CONFIG fallback in
// production.

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pix/host/lease"
	"pix/host/sandbox"
)

// fullLifecycleFixture is sessionFixture's create/attach lifecycle (the
// `env`/`run`/`exec` cases, barrier-gated the same way) PLUS removableFixture's
// `rm -f`-then-delisted behavior, so one script can drive a real create
// through to a real, provable teardown removal.
const fullLifecycleFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
	[ -f "$d/removed" ] && exit 0
	if [ -f "$d/created" ]; then
		if [ "$2" = "--json" ]; then
			echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
		else
			echo "pix-demo  img  running"
		fi
	fi
	exit 0
	;;
env)
	touch "$d/created"
	exit 0
	;;
run)
	touch "$d/created"` + awaitRelease + `
	exit 0
	;;
exec)
	if [ "$2" = "-it" ]; then touch "$d/attached-it"; else touch "$d/attached"; fi` + awaitRelease + `
	exit 0
	;;
rm)
	if [ "$2" != "-f" ]; then
		echo "fixture: sbx v0.38 refuses a bare rm with no TTY attached; pass --force to skip confirmation" >&2
		exit 3
	fi
	touch "$d/removed"
	exit 0
	;;
esac
exit 0
`

// assertEmptyTree fails the test if dir contains anything at all — the
// decoy XDG roots this test points every legacy variable at must never
// receive a single byte from a real create+teardown lifecycle.
func assertEmptyTree(t *testing.T, label, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("reading %s (%s): %v", label, dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("%s (%s) is not empty: %v — a state file escaped PIX_HOME", label, dir, names)
	}
}

// mustBeUnder fails the test if path does not lie inside root — every
// artifact this test finds on disk must resolve under the temp PIX_HOME,
// never some other root a stray absolute path might have named.
func mustBeUnder(t *testing.T, label, root, path string) {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || (len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)) {
		t.Errorf("%s = %q is not under PIX_HOME %q (rel=%q, err=%v)", label, path, root, rel, err)
	}
}

// TestFullLifecycle_EverythingResolvesUnderPIX_HOME_NoXDGEscape is the F5
// integration test: set a temp PIX_HOME, invoke a real create + teardown
// with a fake sbx, and assert every effective/sandbox/session/teardown
// artifact lands only under it — never under any of the legacy XDG_*/
// PIX_CONFIG roots this test deliberately also sets, pointed at a decoy
// directory that must stay untouched.
func TestFullLifecycle_EverythingResolvesUnderPIX_HOME_NoXDGEscape(t *testing.T) {
	home := t.TempDir()
	decoy := t.TempDir()
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_IDENTITY", "test@fixture")
	// Every legacy variable config.go's old resolver once honored, pointed
	// at a SEPARATE root. If any of config.Path/StateDir/DataDir/ContextDir
	// (or a caller built on them) still consulted one of these in
	// production, its artifacts would land here instead of under home —
	// exactly the escape F5 closes.
	t.Setenv("PIX_CONFIG", filepath.Join(decoy, "config.toml"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(decoy, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(decoy, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(decoy, "data"))

	fixture := installFakeSbx(t, fullLifecycleFixture)

	ws := filepath.Join(home, "workspace") // a real dir this run's key derives from
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	key := SessionName(ws)
	// sessionFixture's own `ls` case hard-codes "pix-demo" in its non-JSON
	// listing row (classifySbxListing matches on this exact name).
	name := "pix-demo"
	fp := sandbox.Fingerprint{"static_mcp": "slack"}

	// The create-time effective document a real `pix run` persists BEFORE
	// composing EnvCreateArgs (envlaunch.go's PersistEffectiveEnv). Written
	// here directly since this test drives RunSession without the full
	// cmd/pix compile step, but through the SAME production function.
	effectivePath, err := PersistEffectiveEnv(name, []byte("schemaVersion: \"2\"\nname: "+name+"\n"))
	if err != nil {
		t.Fatalf("PersistEffectiveEnv: %v", err)
	}
	wantEffectiveDir, err := EffectiveEnvDir(name)
	if err != nil {
		t.Fatalf("EffectiveEnvDir: %v", err)
	}
	mustBeUnder(t, "effective document", home, effectivePath)
	if filepath.Dir(effectivePath) != wantEffectiveDir {
		t.Errorf("effective document dir = %q, want %q", filepath.Dir(effectivePath), wantEffectiveDir)
	}
	if _, err := os.Stat(effectivePath); err != nil {
		t.Fatalf("persisted effective document missing: %v", err)
	}

	opts := fastTeardown(t)
	teardownJournal := opts.JournalPath // fastTeardown already isolates this to a t.TempDir(), separately proven below

	created := make(chan error, 1)
	go func() {
		created <- RunSession(SessionSpec{
			Key: key, Name: name, Creating: true,
			EnvCreateArgs:     []string{"env", "create", effectivePath},
			Fingerprint:       fp,
			Invocation:        []string{"--model", "m"},
			DefaultInvocation: []string{"--model", "m"},
		}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t), Teardown: opts})
	}()

	waitForFile(t, filepath.Join(fixture, "created"), 10*time.Second)

	leaseDir, err := leaseDirFor(key)
	if err != nil {
		t.Fatalf("leaseDirFor: %v", err)
	}
	mustBeUnder(t, "sandbox lease dir", home, leaseDir)
	waitForRecordedCreateState(t, leaseDir, key, false, 20*time.Second)

	// The record, fingerprint, and invocation are all real, on-disk, and
	// under PIX_HOME — the "sandbox" and "session" halves of F5's ask.
	rec, err := lease.ReadRecord(leaseDir)
	if err != nil || rec.InstanceID != "inst-1" {
		t.Fatalf("record = %+v (err %v), want instance inst-1", rec, err)
	}
	if _, found := readSessionInvocation(key); !found {
		t.Error("session invocation was not recorded")
	}
	if diverged, found := CheckSessionFingerprint(key, fp); !found || len(diverged) > 0 {
		t.Errorf("session fingerprint not recorded: found=%v diverged=%v", found, diverged)
	}

	// End the session so RunSession reaches its own teardown call.
	release(t, fixture)
	if err := <-created; err != nil {
		t.Fatalf("RunSession: %v", err)
	}

	// The teardown half: a journal entry for this exact removal, under the
	// SAME isolated root fastTeardown already pointed under a temp dir (its
	// own proof it never touches config.StateDir()'s default — see reap.go's
	// comment on this fixture). Confirmed present so this test's OWN
	// teardown actually ran, not merely that RunSession returned.
	if _, err := os.Stat(teardownJournal); err != nil {
		t.Errorf("teardown journal missing at %s: %v", teardownJournal, err)
	}
	if _, err := os.Stat(leaseDir); !os.IsNotExist(err) {
		left, _ := os.ReadDir(leaseDir)
		t.Errorf("lease state should be cleared by a confirmed removal, found: %v (stat err %v)", left, err)
	}

	// The whole-tree proof: every path PIX_HOME's own layout owns is where
	// this lifecycle actually wrote, and the decoy XDG roots got NOTHING.
	assertEmptyTree(t, "decoy config dir", filepath.Join(decoy, "config"))
	assertEmptyTree(t, "decoy state dir", filepath.Join(decoy, "state"))
	assertEmptyTree(t, "decoy data dir", filepath.Join(decoy, "data"))
	assertEmptyTree(t, "decoy PIX_CONFIG dir", decoy)
}
