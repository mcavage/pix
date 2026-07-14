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
	"time"
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
// one, writes it 0600 (creating dirs 0700), and returns it. Idempotent AND
// concurrency-safe: any number of processes racing on first run converge on ONE
// token (the launcher, the host binary, and the shell all call this / the shell
// equivalent, so a divergence means host-has-A / VM-gets-B and auth fails).
//
// The election is atomic: exactly one caller creates the file via O_CREATE|O_EXCL
// and publishes the minted value with a temp+rename (so a reader never sees a
// half-written token); every other caller gets EEXIST and adopts the winner's
// value. No caller ever mints a token that another caller then overwrites.
func EnsureToken() (string, error) {
	path := TokenPath()

	// Fast path: a non-empty token already exists.
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}

	// Elect a single writer atomically. O_EXCL guarantees exactly one caller
	// creates the file; concurrent callers get EEXIST and adopt the winner's
	// token below, so all paths converge on one value.
	marker, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return adoptExistingToken(path)
		}
		return "", err
	}
	// We won the election. Publish the minted value via a temp file renamed over
	// the marker, so the token lands atomically at 0600 and a racing reader sees
	// either nothing-yet or the full value, never a partial write.
	_ = marker.Close()
	tok, err := mintToken()
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "broker-token.*.tmp")
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(tok); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	return tok, nil
}

// adoptExistingToken reads the token that the election winner is publishing.
// The winner creates the file (O_EXCL) a moment before it renames the minted
// value in, so a loser can briefly observe an empty file; retry for a short,
// bounded window until it fills rather than returning an empty token.
func adoptExistingToken(path string) (string, error) {
	for i := 0; i < 500; i++ { // ~5s worst case; the winner is sub-millisecond
		if b, err := os.ReadFile(path); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" {
				return tok, nil
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", errors.New("broker token file was created but never populated")
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
