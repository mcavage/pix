// Zero-friction broker bearer helper. The launcher and host binary share one
// randomly-minted token, stored 0600 next to the config, so the sandbox can
// authenticate to host services without any manual setup.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// TokenPath resolves <config-dir>/broker-token (same dir as config.toml).
func TokenPath() string {
	dir, err := configDir()
	if err != nil {
		return "broker-token"
	}
	return filepath.Join(dir, "broker-token")
}

// mintToken returns a cryptographically-random 32-byte token, base64url encoded.
func mintToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// EnsureToken returns the existing broker token if present, else mints a random
// one, writes it 0600 (creating dirs 0700), and returns it. Idempotent.
func EnsureToken() (string, error) {
	path := TokenPath()
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tok, err := mintToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadToken returns the existing broker token, erroring if it is absent.
func ReadToken() (string, error) {
	b, err := os.ReadFile(TokenPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("broker token not found; run EnsureToken or setup first")
		}
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("broker token file is empty")
	}
	return tok, nil
}
