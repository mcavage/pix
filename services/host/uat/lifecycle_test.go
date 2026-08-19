package uat

import (
	"os"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

func TestRegisterMCPRequiresGatewayVisibility(t *testing.T) {
	state := t.TempDir()
	var calls []string
	sys := &systest.Fake{
		RunFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			calls = append(calls, name+" "+joined)
			if joined == "mcp ls" {
				return "notion registered\n", nil
			}
			return "added\n", nil
		},
	}
	env := hostenv.Env{
		System:     sys,
		HostBinary: func() (string, error) { return "/usr/local/bin/pix-host", nil },
	}
	rec := &Registration{SessionID: "abc", MCPName: "pix-uat-abc"}
	if err := RegisterMCP(env, rec, "/repo", state); err == nil {
		t.Fatal("registration add that never becomes visible in sbx mcp ls must fail before sandbox creation")
	}
	if got := strings.Join(calls, "\n"); !strings.Contains(got, "sbx mcp rm -- pix-uat-abc") {
		t.Fatalf("failed registration did not roll back its exact UAT-owned name; calls:\n%s", got)
	}
}

func TestDeleteRegistrationRemovesSessionState(t *testing.T) {
	state := t.TempDir()
	recordDir := state + "/uat"
	sessionDir := recordDir + "/sessions/abc"
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionDir+"/artifact", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordPath := recordDir + "/pix-project.json"
	if err := os.WriteFile(recordPath, []byte(`{"session_id":"abc","mcp_name":"pix-uat-abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sys := &systest.Fake{
		StateDirFn: func() (string, error) { return state, nil },
		ReadFileFn: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
	}
	if err := DeleteRegistration(hostenv.Env{System: sys}, "pix-project"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{recordPath, sessionDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("UAT teardown left %s behind: %v", path, err)
		}
	}
}

func TestResolveAttachRegistration_DevRequiresRecordedUAT(t *testing.T) {
	tmpDir := t.TempDir()
	sys := &systest.Fake{
		StateDirFn: func() (string, error) { return tmpDir, nil },
		ReadFileFn: func(path string) (string, error) {
			return "", os.ErrNotExist
		},
	}
	env := hostenv.Env{System: sys}

	if _, err := ResolveAttachRegistration(env, "pix-project", true); err == nil {
		t.Fatal("--dev attach without a UAT registration must refuse instead of silently attaching without UAT tools")
	} else {
		msg := err.Error()
		for _, want := range []string{"--dev", "pix rm pix-project", "pix run --dev"} {
			if !strings.Contains(msg, want) {
				t.Errorf("refusal %q missing %q", msg, want)
			}
		}
	}
}

func TestResolveAttachRegistration_AllowsRecordedDevAndOrdinaryAttach(t *testing.T) {
	tmpDir := t.TempDir()
	recordPath := tmpDir + "/uat/pix-project.json"
	if err := os.MkdirAll(tmpDir+"/uat", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, []byte(`{"session_id":"abc","mcp_name":"pix-uat-abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sys := &systest.Fake{
		StateDirFn: func() (string, error) { return tmpDir, nil },
		ReadFileFn: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
		RunFn: func(name string, args ...string) (string, error) {
			return "pix-uat-abc registered\n", nil
		},
	}
	env := hostenv.Env{System: sys}

	rec, err := ResolveAttachRegistration(env, "pix-project", true)
	if err != nil || rec == nil || rec.MCPName != "pix-uat-abc" {
		t.Fatalf("recorded dev attach = (%+v, %v), want pix-uat-abc", rec, err)
	}

	// A record without its gateway registration is also not a usable dev
	// attach. The static name is baked into the sandbox, but there is no server
	// behind it and therefore no uat_capabilities tool.
	sys.RunFn = func(name string, args ...string) (string, error) { return "notion registered\n", nil }
	if _, err := ResolveAttachRegistration(env, "pix-project", true); err == nil {
		t.Fatal("--dev attach with a stale record but missing gateway registration must refuse")
	}

	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	rec, err = ResolveAttachRegistration(env, "pix-project", false)
	if err != nil || rec != nil {
		t.Fatalf("ordinary attach without UAT = (%+v, %v), want nil,nil", rec, err)
	}
}
