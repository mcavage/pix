package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"

	"pix/host/pixhome"
)

// IDLen is the number of lowercase hex characters an ID carries: 16 hex
// characters (64 bits) of a sha256 digest is far more entropy than two
// distinct PIX_HOME roots on the same host will ever collide on, while
// staying short enough to read (and to compose into a sandbox name still
// under the RFC1123 label cap sandbox.MaxNameLen enforces downstream).
const IDLen = 16

// idRe is the whole ID grammar: exactly IDLen lowercase hex characters,
// nothing else. Every naming helper in names.go checks an id against this
// before composing anything from it.
var idRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidID reports whether id is exactly IDLen lowercase hex characters —
// the only shape ID or Current ever produce, and the only shape a naming
// helper accepts.
func ValidID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("stack: invalid id %q: want exactly %d lowercase hex characters", id, IDLen)
	}
	return nil
}

// CanonicalPath resolves path to one canonical absolute form so two
// different spellings of the SAME location (a relative path, an absolute
// path, a path reached through a symlink) always resolve identically:
// filepath.Abs, then filepath.EvalSymlinks when the path actually resolves,
// then filepath.Clean. ID uses it to canonicalize PIX_HOME itself; a
// capability naming a resource from some OTHER path (e.g. sandbox.Name's
// workspace digest) reuses this SAME rule rather than duplicating the
// Abs/EvalSymlinks/Clean chain a second time, so the two packages can never
// silently diverge on what "the same directory" means. A failure to make
// path absolute is returned as an error: an identity that cannot be pinned
// down must never silently proceed under a different, wrong one.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("stack: resolve path %q: %w", path, err)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// HashPrefix returns the first n lowercase hex characters of sha256(s),
// clamped to the digest's full 64-character length. ID is HashPrefix(canonical
// home, IDLen); a capability wanting its own short, collision-resistant name
// component from a string (e.g. sandbox.Name's path digest) calls this
// directly rather than repeating crypto/sha256 + hex.EncodeToString itself —
// this package holds the ONE hashing routine every such caller needs.
func HashPrefix(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}

// ID derives the stable stack ID for home: the first IDLen lowercase hex
// characters of sha256(canonical home). Two calls for the same PIX_HOME
// (even spelled differently, or reached through a symlink) always agree;
// two different PIX_HOME roots always disagree.
func ID(home string) (string, error) {
	canon, err := CanonicalPath(home)
	if err != nil {
		return "", err
	}
	return HashPrefix(canon, IDLen), nil
}

// Current derives the stack ID for THIS process's resolved PIX_HOME
// (pixhome.Dir — $PIX_HOME when set, else ~/.pix; no XDG_* variable of any
// kind ever changes the answer, because pixhome.Dir itself never consults
// one).
func Current() (string, error) {
	home, err := pixhome.Dir()
	if err != nil {
		return "", err
	}
	return ID(home)
}
