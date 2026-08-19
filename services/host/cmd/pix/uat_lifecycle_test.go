package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/uat"
)

func TestUATLifecycle_SuccessHandsRegistrationToSandboxTeardown(t *testing.T) {
	source, err := os.ReadFile("run_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	runSession := strings.Index(string(source), "if xerr := launch.RunSession(spec, deps); xerr != nil")
	handoff := strings.Index(string(source), "launched = true")
	if runSession < 0 || handoff < runSession {
		t.Fatalf("successful RunSession must mark UAT registration handed off before the fallback defer")
	}
}

func TestUATLifecycle_CreateDev(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "pix-test-uat-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	var cmds []string

	var mcpList string
	sys := &systest.Fake{
		RunFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			cmds = append(cmds, name+" "+joined)
			if strings.HasPrefix(joined, "mcp add ") {
				mcpList = args[2] // the name is args[2]
			}
			if joined == "mcp ls" {
				return mcpList, nil
			}
			if strings.HasPrefix(joined, "mcp rm ") {
				mcpList = "" // clear it so the verify step passes
			}
			return "", nil
		},
		WriteFileFn: func(path string, data []byte, perm os.FileMode) error {
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			return os.WriteFile(path, data, perm)
		},
		ReadFileFn: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
		StateDirFn: func() (string, error) {
			return tmpDir, nil
		},
	}
	env := hostenv.Env{System: sys, HostBinary: func() (string, error) { return "/bin/pix-host", nil }}

	// Creating Dev:
	id, err := uat.GenerateSessionID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rec := &uat.Registration{SessionID: id, MCPName: "pix-uat-" + id}
	if err := uat.WriteRegistration(env, "test", rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	b, _ := os.ReadFile(filepath.Join(tmpDir, "uat", "test.json"))
	if len(b) == 0 {
		t.Error("expected state to be written")
	}

	err = uat.RegisterMCP(env, rec, "/repo", filepath.Join(tmpDir, "uat"))
	if err != nil {
		t.Error(err)
	}
	runnerState := filepath.Join(tmpDir, "uat", "sessions", id)
	if info, statErr := os.Stat(runnerState); statErr != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("runner state %q must exist with mode 0700 before gateway spawn: info=%v err=%v", runnerState, info, statErr)
	}

	foundAdd := false
	for _, c := range cmds {
		if strings.Contains(c, "sbx mcp add") {
			foundAdd = true
		}
	}
	if !foundAdd {
		t.Error("missing mcp add")
	}

	cmds = nil

	// Attach
	rec2, err := uat.ReadRegistration(env, "test")
	if err != nil || rec2 == nil {
		t.Error("expected to read back registration")
	} else if rec2.SessionID != id {
		t.Error("mismatch id")
	}

	// Teardown
	err = uat.UnregisterMCP(env, rec2.MCPName)
	if err != nil {
		t.Error(err)
	}

	foundRm := false
	for _, c := range cmds {
		if strings.Contains(c, "sbx mcp rm") {
			foundRm = true
		}
	}
	if !foundRm {
		t.Error("missing mcp rm")
	}
}
