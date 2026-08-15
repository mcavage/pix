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
)

type Registration struct {
	SessionID string `json:"session_id"`
	MCPName   string `json:"mcp_name"`
}

func StateDir(env hostenv.Env) (string, error) {
	if srv, ok := env.System.(interface{ StateDir() (string, error) }); ok {
		d, err := srv.StateDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "uat"), nil
	}
	return filepath.Join(env.Getenv("HOME"), ".local", "state", "pix", "uat"), nil
}

func ReadRegistration(env hostenv.Env, sandboxName string) (*Registration, error) {
	dir, err := StateDir(env)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sandboxName+".json")

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("registration file %s cannot be a symlink", path)
		}
	} else if os.IsNotExist(err) {
		return nil, nil
	} else {
		return nil, err
	}

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
	dir, err := StateDir(env)
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
	dir, err := StateDir(env)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, sandboxName+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func GenerateSessionID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func RegisterMCP(env hostenv.Env, rec *Registration, repoPath, statePath string) error {
	hostBin, err := env.HostBinary()
	if err != nil {
		return err
	}
	runnerStatePath := filepath.Join(statePath, "sessions", rec.SessionID)
	if err := os.MkdirAll(runnerStatePath, 0700); err != nil {
		return fmt.Errorf("create UAT runner state: %w", err)
	}
	info, err := os.Lstat(runnerStatePath)
	if err != nil {
		return fmt.Errorf("inspect UAT runner state: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("UAT runner state %s is not a real directory", runnerStatePath)
	}
	if err := os.Chmod(runnerStatePath, 0700); err != nil {
		return fmt.Errorf("secure UAT runner state: %w", err)
	}
	planner, err := NewMCPPlanner(hostBin, repoPath, runnerStatePath, rec.SessionID)
	if err != nil {
		return err
	}
	args := planner.PlanRegistrationAdd(rec.MCPName)
	out, err := env.Run("sbx", args...)
	if err != nil {
		_ = os.RemoveAll(runnerStatePath)
		return fmt.Errorf("sbx mcp add failed: %v, output: %s", err, out)
	}
	return nil
}

func hasMCPExact(out, mcpName string) bool {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) > 0 && parts[0] == mcpName {
			return true
		}
	}
	return false
}

func UnregisterMCP(env hostenv.Env, mcpName string) error {
	// First check if it exists using ls
	out, err := env.Run("sbx", "mcp", "ls")
	if err != nil {
		return err
	}
	if !hasMCPExact(out, mcpName) {
		// Not there, considered absent
		return nil
	}
	out, err = env.Run("sbx", "mcp", "rm", "--", mcpName)
	if err != nil {
		return fmt.Errorf("sbx mcp rm failed: %v, output: %s", err, out)
	}
	// Confirm absent
	out, err = env.Run("sbx", "mcp", "ls")
	if err == nil && hasMCPExact(out, mcpName) {
		return fmt.Errorf("sbx mcp rm succeeded but %s is still in ls", mcpName)
	}
	return nil
}
