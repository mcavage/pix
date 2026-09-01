package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pix/host/container"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/workflow/launch"
)

// run_create_quiet_test.go — the user-visible half of the create: sbx 0.41
// renders its own plan and asks its own "Approve this plan?" on `sbx env
// create`, which for Pix is a SECOND approval of a document this launcher
// already rendered and trust-gated, and whose text carries the pix-memory
// URL with its bearer token in the query string.
//
// Everything here runs a REAL subprocess that refuses without
// --auto-approve and prints a token-bearing plan. A mocked spawn would prove
// nothing about the argv or which file descriptor the plan came out of.

const fakeCreateToken = "d34db33fd34db33fd34db33fd34db33f"

// quietCreateFixture is a real `sbx`: it prints a plan naming the
// token-bearing memory URL and refuses unless argv carries --auto-approve.
// `ls` reports the sandbox once created, so receipt ordering stays unchanged.
const quietCreateFixture = `
d="$(dirname "$0")"
echo "$@" >> "$d/argv.log"
case "$1" in
ls)
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
	echo "Plan: create pix-demo"
	echo "  mcp pix-memory http://127.0.0.1:9123/mcp?token=` + fakeCreateToken + `"
	case " $* " in
	  *" --auto-approve "*) ;;
	  *) echo "Approve them with --auto-approve" >&2; exit 1 ;;
	esac
	touch "$d/created"
	exit 0
	;;
exec)
	exit 0
	;;
esac
exit 0
`

// failingCreateFixture prints the same token-bearing plan and then fails,
// so the diagnostic path is exercised against real output.
const failingCreateFixture = `
d="$(dirname "$0")"
case "$1" in
ls) exit 0 ;;
env)
	echo "Plan: create pix-demo"
	echo "  mcp pix-memory http://127.0.0.1:9123/mcp?token=` + fakeCreateToken + `"
	echo "ANTHROPIC_API_KEY=sk-live-abcdefghijklmnop"
	echo "error: kit revision not found" >&2
	exit 2
	;;
esac
exit 0
`

func installQuietFixture(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-only fixture")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PIX_HOME", t.TempDir())
	t.Setenv("PIX_IDENTITY", "test@fixture")
	return path
}

// TestQuietCreate_NoPromptPlanOrTokenReachesUserOutput is the whole point:
// the create is approved internally (the fixture refuses to create without
// --auto-approve, so success proves the flag arrived), and none of its plan,
// its approval copy, or its token appears on any
// stream the user sees. The session exec still gets ordinary stdio.
func TestQuietCreate_NoPromptPlanOrTokenReachesUserOutput(t *testing.T) {
	bin := installQuietFixture(t, quietCreateFixture)
	var userOut bytes.Buffer
	var execStdio []any
	capture := &createCapture{}

	err := launch.RunSession(launch.SessionSpec{
		Key: launch.SessionName(t.TempDir()), Name: "pix-demo", Creating: true,
		EnvCreateArgs: launch.EnvCreateArgs("/state/effective.sbxenv.yaml"),
		Invocation:    []string{"--model", "m"},
	}, launch.SessionDeps{
		Env: hostenv.Env{System: sys.Real{}},
		Poll: launch.CreatePoll{Probe: func(name string) launch.SbxState {
			return launch.ProbeTaskSandbox(hostenv.Env{System: sys.Real{}}, name)
		}, Interval: 20 * time.Millisecond, Timeout: 3 * time.Second},
		Warn: &userOut,
		Spawn: func(argv []string) *exec.Cmd {
			cmd := exec.Command(bin, argv...)
			// The session child: whatever the command layer wires here is
			// the user's terminal. Recorded so the test can prove the quiet
			// wiring did NOT leak into it.
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, &userOut, &userOut
			execStdio = append(execStdio, cmd.Stdin)
			return cmd
		},
		SpawnCreate: quietCreateSpawn(bin, capture),
	})
	if err != nil {
		t.Fatalf("RunSession: %v (user output %q, captured %q)", err, userOut.String(), firstOf(capture))
	}

	seen := userOut.String()
	for _, forbidden := range []string{"Approve this plan?", "Plan: create", fakeCreateToken, "token="} {
		if strings.Contains(seen, forbidden) {
			t.Fatalf("user output contains %q; sbx's duplicate approval must never be shown: %q", forbidden, seen)
		}
	}
	// It WAS captured, not merely discarded: a failed create still has to
	// be able to say what happened.
	if got := firstOf(capture); !strings.Contains(got, "Plan: create") {
		t.Fatalf("create output = %q, want the suppressed plan captured for diagnostics", got)
	}
	// And the session exec kept ordinary inherited stdin.
	if len(execStdio) == 0 || execStdio[0] != os.Stdin {
		t.Fatalf("the session exec did not inherit normal stdio: %v", execStdio)
	}
}

// TestInteractiveSessionSpawn_InheritsNormalStdio pins the production
// wiring of the OTHER child: the user's session is not suppressed.
func TestInteractiveSessionSpawn_InheritsNormalStdio(t *testing.T) {
	cmd := interactiveSessionSpawn("sbx")([]string{"exec", "-it", "pix-demo"})
	if cmd.Stdin != os.Stdin || cmd.Stdout != os.Stdout || cmd.Stderr != os.Stderr {
		t.Fatalf("session child stdio = (%v, %v, %v), want the real terminal", cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
}

// TestCreateFailureDiagnostic_RedactsTokenAndSecretValues is the failure
// half: the captured output is the diagnostic, and it is bounded and
// redacted. The raw token never appears; the plan text is not re-printed
// verbatim as a plan, it is quoted as what sbx said.
func TestCreateFailureDiagnostic_RedactsTokenAndSecretValues(t *testing.T) {
	bin := installQuietFixture(t, failingCreateFixture)
	home, _ := os.LookupEnv("PIX_HOME")
	if err := os.MkdirAll(filepath.Join(home, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "secrets.env"), []byte("ANTHROPIC_API_KEY=op://v/i/f\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	capture := &createCapture{}
	cmd := quietCreateSpawn(bin, capture)([]string{"env", "create", "/state/effective.sbxenv.yaml"})
	if err := cmd.Run(); err == nil {
		t.Fatal("the failing fixture must exit nonzero")
	}
	diag := createFailureDiagnostic(capture, []string{fakeCreateToken})

	if diag == "" || !strings.Contains(diag, "kit revision not found") {
		t.Fatalf("diagnostic = %q, want the real failure reason", diag)
	}
	if strings.Contains(diag, fakeCreateToken) {
		t.Fatalf("diagnostic leaked the memory bearer token: %q", diag)
	}
	if strings.Contains(diag, "sk-live-abcdefghijklmnop") {
		t.Fatalf("diagnostic leaked a configured secret's value: %q", diag)
	}
	if !strings.Contains(diag, container.RedactedTokenPlaceholder) {
		t.Fatalf("diagnostic = %q, want the redaction placeholder where a credential was", diag)
	}
}

// TestCreateCapture_IsBounded proves a runaway plan cannot flood a terminal
// through the diagnostic path.
func TestCreateCapture_IsBounded(t *testing.T) {
	c := &createCapture{}
	if _, err := io.WriteString(c, strings.Repeat("x", createCaptureLimit*3)); err != nil {
		t.Fatal(err)
	}
	got, overflow := c.text()
	if len(got) != createCaptureLimit || !overflow {
		t.Fatalf("captured %d bytes (overflow=%v), want a bound of %d and an overflow flag", len(got), overflow, createCaptureLimit)
	}
	if !strings.Contains(createFailureDiagnostic(c, nil), "output truncated") {
		t.Fatal("a truncated diagnostic must say so")
	}
}

// TestRedactCreateOutput_MasksEveryTokenOccurrence guards the whole-text
// redaction: a plan can name the memory endpoint more than once.
func TestRedactCreateOutput_MasksEveryTokenOccurrence(t *testing.T) {
	raw := "a http://x/mcp?token=aaaabbbbccccdddd b http://y/mcp?token=eeeeffff0000 c Bearer zzzzyyyyxxxx\n"
	got := redactCreateOutput(raw, nil)
	for _, leak := range []string{"aaaabbbbccccdddd", "eeeeffff0000", "zzzzyyyyxxxx"} {
		if strings.Contains(got, leak) {
			t.Fatalf("redacted output still contains %q: %q", leak, got)
		}
	}
}

func firstOf(c *createCapture) string {
	s, _ := c.text()
	return s
}
