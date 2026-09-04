package hosttrust

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"pix/host/sys"
)

// creationHMACKeyName is the ONE file this package ever stores the
// launcher's creation-fingerprint HMAC key under, beside every other
// launcher-owned trust document (pack-trust.json's sibling, in whatever
// config dir the caller passes — the same discipline SaveDocument already
// holds). E2.2 (envinfo/attribution.go) fingerprints an interpolated
// facet as its authored expression plus this key's HMAC of the resolved
// value — never the raw value, never an unkeyed hash — but envinfo (an L1
// capability) may not import this package (an L1 sibling), so the key
// itself, and the signing it enables, live here; envinfo only ever
// receives an already-signed digest through a caller-supplied resolver
// function.
const creationHMACKeyName = "creation-hmac.key"

// creationHMACKeyLen is 32 bytes (sha256's block size for HMAC) generated
// from crypto/rand only — never a low-entropy or derived value: a launcher
// key that were itself guessable would undo the whole point of keying the
// digest at all (hmackey_test.go's offline-guessing sentinel).
const creationHMACKeyLen = 32

// creationHMACKeyDoc is the on-disk shape: one hex-encoded field. This
// type is the ONLY thing in this package that ever marshals the raw key
// bytes, and nothing here ever formats a creationHMACKeyDoc (or the raw
// key) into an error string or a log line — see SignResolvedValue's own
// doc comment for the same discipline on the resolved value it signs.
type creationHMACKeyDoc struct {
	KeyHex string `json:"key_hex"`
}

// ErrCreationHMACKeyMissing is returned by LoadCreationHMACKey when no key
// record exists yet — a fresh host, or (the case E2.2 exists to handle
// without flooding recreatelog's cap) a host that just ran `pix reset`:
// reset moves the WHOLE config dir this key lives in aside, exactly like
// every other acceptance record (see reset.go's own doc comment and
// workflow/reset's hmackey reset test). A caller must turn this into
// envinfo.ErrHMACKeyMissing (and ultimately ONE
// envinfo.ResetInvalidatedDrift() record), never a per-interpolation
// refusal.
var ErrCreationHMACKeyMissing = errors.New(
	"hosttrust: creation HMAC key missing (reset invalidates acceptance; recreate required)")

// LoadCreationHMACKey reads the one stored key record from configDir, or
// ErrCreationHMACKeyMissing if none exists. It never logs, wraps, or
// otherwise surfaces the key bytes themselves in an error string.
func LoadCreationHMACKey(configDir string) ([]byte, error) {
	path := filepath.Join(configDir, creationHMACKeyName)
	b, err := ReadDocumentBytes(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCreationHMACKeyMissing
		}
		return nil, err
	}
	var doc creationHMACKeyDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("hosttrust: creation HMAC key record is corrupt: %w", err)
	}
	key, err := hex.DecodeString(doc.KeyHex)
	if err != nil || len(key) != creationHMACKeyLen {
		return nil, errors.New("hosttrust: creation HMAC key record is corrupt")
	}
	return key, nil
}

// EnsureCreationHMACKey loads the stored key, generating and persisting a
// fresh one exactly once if none exists — "generated once" (E2.2). The
// generate-then-save step runs under this key's own flock (a SEPARATE
// lock file, never the pack/environment acceptance store's lock — this is
// a different document with its own read-modify-write race to close), so
// two concurrent first launches can never each write their own key and
// leave the host with two candidates disagreeing about which is
// authoritative: the loser of the race re-reads under the lock and
// returns the winner's key instead of overwriting it.
func EnsureCreationHMACKey(configDir string) ([]byte, error) {
	if key, err := LoadCreationHMACKey(configDir); err == nil {
		return key, nil
	} else if !errors.Is(err, ErrCreationHMACKeyMissing) {
		return nil, err
	}

	lockPath := filepath.Join(configDir, creationHMACKeyName+".lock")
	var key []byte
	err := WithLock(lockPath, func() error {
		if k, err := LoadCreationHMACKey(configDir); err == nil {
			key = k
			return nil
		} else if !errors.Is(err, ErrCreationHMACKeyMissing) {
			return err
		}
		k := make([]byte, creationHMACKeyLen)
		if _, err := io.ReadFull(rand.Reader, k); err != nil {
			return fmt.Errorf("hosttrust: generate creation HMAC key: %w", err)
		}
		if err := saveCreationHMACKey(configDir, k); err != nil {
			return err
		}
		key = k
		return nil
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// saveCreationHMACKey writes the key record symlink-safe + atomic, at mode
// 0600 — PRIVATE, unlike SaveDocument's shared 0644: a trust acceptance
// document is meant to be inspected by anything that reviews trust, but
// this key must never be readable by anything but its owner.
func saveCreationHMACKey(configDir string, key []byte) error {
	dest := filepath.Join(configDir, creationHMACKeyName)
	if err := refuseSymlinkedDestination(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(creationHMACKeyDoc{KeyHex: hex.EncodeToString(key)})
	if err != nil {
		return err
	}
	return sys.AtomicWriteInDir(configDir, creationHMACKeyName, b, 0o600)
}

// SignResolvedValue computes the launcher-keyed HMAC-SHA256 of a resolved
// interpolation value, hex encoded. It is the ONLY function in this
// module that ever sees a raw resolved ${VAR} value; it returns an opaque
// digest and NEVER the value itself. A caller composing
// envinfo.InterpolationResolver wraps this directly — envinfo itself
// never holds the key or the raw value, only whatever digest this
// function returns (attribution.go's own doc comment).
//
// HMAC, not a plain (even salted) hash, is the load-bearing choice: a
// low-entropy resolved value (many real secrets are short tokens or
// simple strings) hashed WITHOUT a secret key is trivially brute-forced
// offline by anyone who obtains the persisted digest — the digest itself
// becomes an oracle. Keying it with a 32-byte random key this package
// alone generates and stores at mode 0600 removes that oracle: recovering
// the value from the digest requires the key, not just guessing power
// (hmackey_test.go's TestSignResolvedValue_LowEntropyValueNotOfflineGuessable).
func SignResolvedValue(key []byte, resolvedValue string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(resolvedValue))
	return hex.EncodeToString(mac.Sum(nil))
}
