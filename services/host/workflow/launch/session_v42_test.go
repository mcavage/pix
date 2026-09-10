//go:build unix

package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"pix/host/sandbox"
	"strings"
	"testing"
)

func TestRunSessionV42CreateExecAndStoppedCleanup(t *testing.T) {
	isolateState(t)
	row := `{"sandboxes":[{"name":"pix-demo","id":"943fa6d6-7c2e-4d8e-9886-0b2dcec1ca62","agent":"pix","status":"running","last_used_at":"2026-09-09T22:56:38.792733Z","workspaces":["/workspace"]}]}`
	script := strings.Replace(sessionFixture, `[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]`, row, 1)
	// After exec exits, sbx retains a stopped sandbox until Pix removes it.
	script = strings.Replace(script, "ls)\n", "ls)\n\t[ -f \"$d/removed\" ] && { echo '{\"sandboxes\":[]}'; exit 0; }\n", 1)
	script = strings.Replace(script, "echo '"+row+"'", "echo '"+row+"' | sed \"s/running/$(if [ -f \"$d/exited\" ]; then echo stopped; else echo running; fi)/\"", 1)
	script = strings.Replace(script, "esac", "rm)\n touch \"$d/removed\"\n exit 0\n ;;\nesac", 1)
	fixture := installFakeSbx(t, script)
	release(t, fixture)
	var warnings bytes.Buffer
	opts := fastTeardown(t)
	err := RunSession(SessionSpec{
		Key: SessionName(t.TempDir()), Name: "pix-demo", Creating: true,
		EnvCreateArgs: []string{"env", "create", "/state/effective.sbxenv.yaml"},
		Fingerprint:   sandbox.Fingerprint{"env.A": "1"},
		Invocation:    []string{"--model", "m"},
	}, SessionDeps{Env: realEnv(), Poll: fastPoll(), Warn: &warnings, Spawn: fixtureSpawn(t), Teardown: opts})
	if err != nil {
		t.Fatal(err)
	}
	var attached, removed bool
	for _, line := range argvLines(t, fixture) {
		if strings.HasPrefix(line, "exec ") {
			attached = true
		}
		if line == "rm -f pix-demo" {
			removed = true
		}
	}
	if !attached || !removed {
		t.Fatalf("attached=%v removed=%v warnings=%s journal=%+v", attached, removed, warnings.String(), lastVerdict(t, opts))
	}
	if _, err := os.Stat(filepath.Join(fixture, "removed")); err != nil {
		t.Fatal(err)
	}
}
