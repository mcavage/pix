package uat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/hostenv"
	"pix/host/sys"
)

type Registration struct {
	SessionID string `json:"session_id"`
	MCPName   string `json:"mcp_name"`
}

func stateDir(env sys.Getenver, fs sys.FS) (string, error) {
	if srv, ok := env.(interface{ StateDir() (string, error) }); ok {
		d, err := srv.StateDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "uat"), nil
	}
	return filepath.Join(env.Getenv("HOME"), ".local", "state", "pix", "uat"), nil
}

func ReadRegistration(env hostenv.Env, sandboxName string) (*Registration, error) {
	dir, err := stateDir(env, env)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sandboxName+".json")
	data, err := env.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec Registration
	if err := json.Unmarshal([]byte(data), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func WriteRegistration(env hostenv.Env, sandboxName string, rec *Registration) error {
	dir, err := stateDir(env, env)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, sandboxName+".json")
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return env.WriteFile(path, data, 0600)
}

func DeleteRegistration(env hostenv.Env, sandboxName string) error {
	dir, err := stateDir(env, env)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, sandboxName+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func GenerateSessionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func RegisterMCP(env hostenv.Env, rec *Registration, repoPath, statePath string) error {
	hostBin, err := env.HostBinary()
	if err != nil {
		return err
	}
	planner, err := NewMCPPlanner(hostBin, repoPath, statePath, rec.SessionID)
	if err != nil {
		return err
	}
	args := planner.PlanRegistrationAdd(rec.MCPName)
	out, err := env.Run("sbx", args...)
	if err != nil && !strings.Contains(out, "already exists") { // Ignore if it exists maybe? No, we should fail if it fails.
		return fmt.Errorf("sbx mcp add failed: %v, output: %s", err, out)
	}
	return nil
}

func UnregisterMCP(env hostenv.Env, mcpName string) error {
	// First check if it exists using ls
	out, err := env.Run("sbx", "mcp", "ls")
	if err != nil {
		return err // Or ignore?
	}
	if !strings.Contains(out, mcpName) {
		// Not there, considered absent
		return nil
	}
	out, err = env.Run("sbx", "mcp", "rm", mcpName)
	if err != nil {
		return fmt.Errorf("sbx mcp rm failed: %v, output: %s", err, out)
	}
	// Confirm absent
	out, err = env.Run("sbx", "mcp", "ls")
	if err == nil && strings.Contains(out, mcpName) {
		return fmt.Errorf("sbx mcp rm succeeded but %s is still in ls", mcpName)
	}
	return nil
}
