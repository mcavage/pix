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
	if err := validateRegistration(&rec); err != nil {
		return nil, fmt.Errorf("invalid UAT registration %s: %w", path, err)
	}
	return &rec, nil
}

// ResolveAttachRegistration reads the create-time UAT record for an existing
// sandbox. A caller explicitly asking for --dev must never be silently attached
// to a sandbox without that record: static MCP servers cannot be retrofitted,
// so the resulting session would look like dev mode while lacking every UAT
// tool. Ordinary attaches legitimately return nil when the sandbox was not
// created in dev mode.
func ResolveAttachRegistration(env hostenv.Env, sandboxName string, devRequested bool) (*Registration, error) {
	rec, err := ReadRegistration(env, sandboxName)
	if err != nil {
		return nil, fmt.Errorf("read UAT registration for %s: %w", sandboxName, err)
	}
	if !devRequested {
		return rec, nil
	}
	if rec == nil {
		return nil, devAttachRecreateError(sandboxName, "it was not created with the session UAT server")
	}
	out, err := env.Run("sbx", "mcp", "ls")
	if err != nil {
		return nil, fmt.Errorf("verify dev UAT registration %s: %w", rec.MCPName, err)
	}
	if !hasMCPExact(out, rec.MCPName) {
		return nil, devAttachRecreateError(sandboxName, "its session UAT server is no longer registered")
	}
	return rec, nil
}

func devAttachRecreateError(sandboxName, reason string) error {
	return fmt.Errorf("--dev cannot attach sandbox %q because %s; static MCP tools attach only at creation. Recreate it: pix rm %s && pix run --dev", sandboxName, reason, sandboxName)
}

func WriteRegistration(env hostenv.Env, sandboxName string, rec *Registration) error {
	if err := validateRegistration(rec); err != nil {
		return err
	}
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
	rec, err := ReadRegistration(env, sandboxName)
	if err != nil {
		return err
	}
	dir, err := StateDir(env)
	if err != nil {
		return err
	}
	if rec != nil {
		if err := removeSessionState(dir, rec.SessionID); err != nil {
			return err // keep the registration so a later teardown can retry
		}
	}
	err = os.Remove(filepath.Join(dir, sandboxName+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validateRegistration(rec *Registration) error {
	if rec == nil {
		return fmt.Errorf("registration is nil")
	}
	if err := ValidateID(rec.SessionID); err != nil {
		return fmt.Errorf("session id: %w", err)
	}
	if want := "pix-uat-" + rec.SessionID; rec.MCPName != want {
		return fmt.Errorf("MCP name %q does not match session id (want %q)", rec.MCPName, want)
	}
	return nil
}

func removeSessionState(uatDir, sessionID string) error {
	if err := ValidateID(sessionID); err != nil {
		return fmt.Errorf("refuse unsafe UAT session path: %w", err)
	}
	sessionsDir := filepath.Join(uatDir, "sessions")
	if info, err := os.Lstat(sessionsDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("UAT sessions path %s is not a real directory", sessionsDir)
		}
	} else if os.IsNotExist(err) {
		return nil
	} else {
		return err
	}
	sessionDir := filepath.Join(sessionsDir, sessionID)
	if info, err := os.Lstat(sessionDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("UAT session path %s is not a real directory", sessionDir)
		}
	} else if os.IsNotExist(err) {
		return nil
	} else {
		return err
	}
	return os.RemoveAll(sessionDir)
}

func GenerateSessionID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func RegisterMCP(env hostenv.Env, rec *Registration, repoPath, statePath string) error {
	if err := validateRegistration(rec); err != nil {
		return err
	}
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
	listed, lerr := env.Run("sbx", "mcp", "ls")
	if lerr != nil || !hasMCPExact(listed, rec.MCPName) {
		// add may have mutated the gateway despite an unusable/ambiguous result.
		// Roll back by exact UAT-owned name before the caller creates a sandbox
		// whose static MCP set can never produce uat_capabilities.
		_, _ = env.Run("sbx", "mcp", "rm", "--", rec.MCPName)
		_ = os.RemoveAll(runnerStatePath)
		if lerr != nil {
			return fmt.Errorf("verify UAT MCP registration %s: %w", rec.MCPName, lerr)
		}
		return fmt.Errorf("sbx mcp add succeeded but %s is absent from sbx mcp ls", rec.MCPName)
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
