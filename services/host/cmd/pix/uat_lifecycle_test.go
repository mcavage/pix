package main

import (
	"os"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/uat"
)

func TestUATLifecycle_CreateDev(t *testing.T) {
	var cmds []string
	fsMap := make(map[string]string)

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
			fsMap[path] = string(data)
			return nil
		},
		ReadFileFn: func(path string) (string, error) {
			if v, ok := fsMap[path]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		},
		StateDirFn: func() (string, error) {
			return "/state", nil
		},
	}
	env := hostenv.Env{System: sys, HostBinary: func() (string, error) { return "/bin/pix-host", nil }}

	// Creating Dev:
	id := uat.GenerateSessionID()
	rec := &uat.Registration{SessionID: id, MCPName: "pix-uat-test-" + id}
	uat.WriteRegistration(env, "test", rec)

	if len(fsMap) == 0 {
		t.Error("expected state to be written")
	}

	err := uat.RegisterMCP(env, rec, "/repo", "/state/uat")
	if err != nil {
		t.Error(err)
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
