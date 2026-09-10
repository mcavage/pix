//go:build unix

package launch

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/lease"
	"pix/host/sandbox"
)

const retiredInstance = "11111111-1111-4111-8111-111111111111"
const replacementInstance = "22222222-2222-4222-8222-222222222222"
const retiredSessionFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
 if [ "$2" = "--json" ]; then
  if [ -f "$d/created" ] && [ ! -f "$d/removed" ]; then
   echo '{"sandboxes":[{"name":"pix-demo","id":"22222222-2222-4222-8222-222222222222","agent":"pix","status":"stopped","last_used_at":"2026-09-09T22:00:00Z"}]}'
  else
   echo '{"sandboxes":[]}'
  fi
 elif [ -f "$d/created" ] && [ ! -f "$d/removed" ]; then
  echo 'pix-demo pix stopped'
 fi
 ;;
env) touch "$d/created" ;;
exec) exit 0 ;;
rm) touch "$d/removed" ;;
esac
`

func TestRunSessionRetiresAbsentPreviousLifetime(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup", true: "keep-new-lifetime"}[keep], func(t *testing.T) {
			isolateState(t)
			fixture := installFakeSbx(t, retiredSessionFixture)
			key := SessionName(t.TempDir())
			dir := seedRecordedSession(t, key, retiredInstance)
			if err := lease.SetKeep(dir, retiredInstance); err != nil {
				t.Fatal(err)
			}
			// Create the stable lock files before launch and retain their identities.
			lc, err := lease.OpenLifecycleLock(dir)
			if err != nil {
				t.Fatal(err)
			}
			lc.Close()
			rl, err := lease.OpenRefLease(dir)
			if err != nil {
				t.Fatal(err)
			}
			rl.Close()
			lifeBefore, _ := os.Stat(filepath.Join(dir, "lifecycle.lock"))
			refsBefore, _ := os.Stat(filepath.Join(dir, "refs.lock"))
			spec := SessionSpec{Key: key, Name: "pix-demo", Creating: true, Keep: keep,
				EnvCreateArgs: []string{"env", "create", "/state/effective.sbxenv.yaml"},
				Fingerprint:   sandbox.Fingerprint{"env.A": "new"}, Invocation: []string{"--model", "m"}}
			err = RunSession(spec, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: fixtureSpawn(t), Teardown: fastTeardown(t)})
			if err != nil {
				t.Fatal(err)
			}
			if keep {
				rec, err := lease.ReadRecord(dir)
				if err != nil || rec.InstanceID != replacementInstance {
					t.Fatalf("record=%+v err=%v", rec, err)
				}
				held, set, err := lease.ReadKeep(dir)
				if err != nil || !set || held.Identity != "test@fixture" {
					t.Fatalf("keep=%+v set=%v err=%v", held, set, err)
				}
				lifeAfter, _ := os.Stat(filepath.Join(dir, "lifecycle.lock"))
				refsAfter, _ := os.Stat(filepath.Join(dir, "refs.lock"))
				if !os.SameFile(lifeBefore, lifeAfter) || !os.SameFile(refsBefore, refsAfter) {
					t.Fatal("create replaced a held lock inode")
				}
			} else if _, err := os.Stat(filepath.Join(fixture, "removed")); err != nil {
				t.Fatal("replacement survived session exit", err)
			}
		})
	}
}

func TestRunSessionRetainsPreviousLifetimeWithoutPositiveAbsence(t *testing.T) {
	for _, scenario := range []string{"held", "present", "malformed", "probe-failed"} {
		t.Run(scenario, func(t *testing.T) {
			isolateState(t)
			script := retiredSessionFixture
			switch scenario {
			case "present":
				script = strings.ReplaceAll(script, `echo '{"sandboxes":[]}'`, `echo '{"sandboxes":[{"name":"pix-demo","id":"11111111-1111-4111-8111-111111111111","agent":"pix","status":"stopped"}]}'`)
			case "malformed":
				script = strings.ReplaceAll(script, `echo '{"sandboxes":[]}'`, `echo 'not-json'`)
			case "probe-failed":
				script = strings.ReplaceAll(script, `echo '{"sandboxes":[]}'`, `exit 1`)
			}
			installFakeSbx(t, script)
			key := SessionName(t.TempDir())
			dir := seedRecordedSession(t, key, retiredInstance)
			if scenario == "held" {
				ref, err := lease.OpenRefLease(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer ref.Close()
				if err := ref.AcquireShared(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			spawned := false
			err := RunSession(SessionSpec{Key: key, Name: "pix-demo", Creating: true, EnvCreateArgs: []string{"env", "create", "/state/effective.sbxenv.yaml"}}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: io.Discard, Spawn: func([]string) *exec.Cmd { spawned = true; return exec.Command("false") }})
			if err == nil || spawned {
				t.Fatalf("err=%v spawned=%v", err, spawned)
			}
			rec, rerr := lease.ReadRecord(dir)
			if rerr != nil || rec.InstanceID != retiredInstance {
				t.Fatalf("old record changed: %+v %v", rec, rerr)
			}
		})
	}
}
